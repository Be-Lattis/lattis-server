package pipeline_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/config"
	"github.com/Be-Lattis/lattis-server/internal/httpserver"
	"github.com/Be-Lattis/lattis-server/internal/pipeline"
	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
	"github.com/Be-Lattis/lattis-server/internal/tokens"
	"github.com/Be-Lattis/lattis-server/internal/userdb"
)

// Tableau publié seul au Data Lake : source poussée créée par un publieur,
// contenu envoyé par POST /api/push, écrit par le pipeline, puis supprimé.
func TestPushedSource(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	service := func(fn func(pgx.Tx) error) error { return db.Store.AsActor(ctx, "test", fn) }
	var google string
	must(service(func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT lattis.set_config('access_mode', '"open"')`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT lattis.set_config('workbook_creators', '"owners"')`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT lattis.add_provider('google', 'Google', NULL, 'cid', NULL, NULL)::text`).Scan(&google)
	}))
	enroll := func(n string) store.Enrollment {
		u, err := db.Store.Enroll(ctx, store.Identity{ProviderID: google, Subject: n, Email: testpg.Email(n), EmailVerified: true})
		must(err)
		return u
	}
	author, other := enroll("publieur"), enroll("autre")
	must(service(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.set_data_publisher($1::uuid, true)`, author.UserID)
		return err
	}))
	as := func(u store.Enrollment, fn func(pgx.Tx) error) error { return db.AsUser(t, u.PGRole, fn) }

	// Création par un publieur qui n'a pas le droit de créer des classeurs.
	var created map[string]string
	must(as(author, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.create_pushed_source($1::jsonb)`, `{"name": "Clients partagés", "mode": "upsert",
			"key_columns": ["Code"], "columns": [{"name": "Code", "type": "text"}, {"name": "CA", "type": "decimal"}],
			"origin": {"workbook": "Ventes.lattis", "table": "Clients"}}`).Scan(&created)
	}))
	pid := created["pipeline_id"]
	var mine, sources string
	must(as(author, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.my_workbooks()::text`).Scan(&mine); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT lattis.my_pushed_sources()::text`).Scan(&sources)
	}))
	if strings.Contains(mine, created["workbook_id"]) || !strings.Contains(sources, pid) || !strings.Contains(sources, "Ventes.lattis") {
		t.Fatalf("listes : classeurs %s ; sources %s", mine, sources)
	}
	if err := as(other, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.create_pushed_source('{"name": "X", "columns": [{"name": "A", "type": "text"}]}')`)
		return err
	}); err == nil {
		t.Fatal("source poussée créée par un non-publieur")
	}

	// Envoi par l'API, comme l'application.
	dir := t.TempDir()
	runner := &pipeline.Runner{Store: db.Store, Users: userdb.SessionAuth{Pool: db.Super}, Dir: dir}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go runner.Run(rctx)
	pem, _ := tokens.GenerateKeyPEM()
	keys, _ := tokens.ParseKeys(pem)
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	defer srv.Close()
	pub, _ := url.Parse(srv.URL)
	cfg := &config.Config{PublicURL: pub, PipelineDir: dir, RateLimit: 100}
	h = (&httpserver.Server{Config: cfg, Store: db.Store, Keys: keys, Users: userdb.SessionAuth{Pool: db.Super}, Pipelines: runner}).Handler()
	token := func(u store.Enrollment) string {
		now := time.Now()
		s, err := keys.Sign(tokens.TypeAccess, tokens.Claims{Issuer: cfg.Issuer(), Subject: u.UserID,
			Audience: tokens.Audience{tokens.AudiencePostgres}, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
			Scope: "openid lattis", PGRole: u.PGRole})
		must(err)
		return s
	}
	push := func(u store.Enrollment, body string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/push/"+pid+"?wait=30", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token(u))
		req.Header.Set("Content-Type", "text/csv")
		resp, err := http.DefaultClient.Do(req)
		must(err)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		return resp.StatusCode, out
	}
	code, res := push(author, "Code;CA\nC1;1200,50\nC2;80\n")
	if code != http.StatusOK || res["status"] != "ok" || res["inserted"].(float64) != 2 || res["trigger"] != "push" {
		t.Fatalf("premier envoi : %d %v", code, res)
	}
	get := func(path string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+token(author))
		resp, err := http.DefaultClient.Do(req)
		must(err)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/api/push/" + pid + "?run=" + res["id"].(string)); code != http.StatusOK || !strings.Contains(body, `"inserted": 2`) {
		t.Fatalf("suivi de l'envoi : %d %s", code, body)
	}
	code, res = push(author, "Code;CA\nC1;1300\n")
	if code != http.StatusOK || res["updated"].(float64) != 1 {
		t.Fatalf("second envoi : %d %v", code, res)
	}
	if code, _ := push(other, "Code;CA\nX;1\n"); code != http.StatusForbidden {
		t.Fatalf("envoi par un autre : %d", code)
	}
	// Mode et clé changés par le créateur, pas par un autre.
	must(as(author, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.update_pushed_source($1::uuid, '{"mode": "replace", "key_columns": []}')`, pid)
		return err
	}))
	if err := as(other, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.update_pushed_source($1::uuid, '{"enabled": false}')`, pid)
		return err
	}); err == nil {
		t.Fatal("source modifiée par un autre")
	}
	var origin string
	must(as(author, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT d -> 'source' ->> 'origin' FROM jsonb_array_elements(lattis.data_catalog() -> 'datasets') d
			WHERE d ->> 'id' = $1`, created["dataset_id"]).Scan(&origin)
	}))
	if !strings.Contains(origin, "Ventes.lattis") {
		t.Fatalf("origine au catalogue : %s", origin)
	}
	var schema string
	must(db.Super.QueryRow(ctx, `SELECT schema_name FROM lattis.workbooks WHERE id = $1`, created["workbook_id"]).Scan(&schema))
	var total float64
	must(db.Super.QueryRow(ctx, `SELECT sum("CA")::float8 FROM `+pgx.Identifier{schema, "Clients partagés"}.Sanitize()).Scan(&total))
	if total != 1380 {
		t.Fatalf("contenu de la source : %v", total)
	}
	if code, body := get("/api/push/" + pid); code != http.StatusOK || !strings.Contains(body, `"last_run"`) || !strings.Contains(body, `"mode":"replace"`) {
		t.Fatalf("état : %d %s", code, body)
	}

	// Suppression : donnée, pipeline et classeur technique.
	must(as(author, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.drop_pushed_source($1::uuid)`, pid)
		return err
	}))
	var left int
	must(db.Super.QueryRow(ctx, `SELECT (SELECT count(*) FROM lattis.datasets WHERE id = $1) + (SELECT count(*) FROM lattis.pipelines WHERE id = $2)
		+ (SELECT count(*) FROM lattis.workbooks WHERE id = $3)`, created["dataset_id"], pid, created["workbook_id"]).Scan(&left))
	if left != 0 {
		t.Fatalf("restes après suppression : %d", left)
	}
}
