package pipeline_test

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/pipeline"
	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
	"github.com/Be-Lattis/lattis-server/internal/userdb"
)

// Pipelines OUT : un tableau exporté en fichier (CSV, JSON, XLSX), envoyé
// par HTTP, et copié dans une table PostgreSQL externe (ajout, remplacement).
func TestPipelineExport(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var google string
	must(db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT lattis.set_config('access_mode', '"open"')`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT lattis.add_provider('google', 'Google', NULL, 'cid', NULL, NULL)::text`).Scan(&google)
	}))
	author, err := db.Store.Enroll(ctx, store.Identity{ProviderID: google, Subject: "auteur", Email: testpg.Email("auteur"), EmailVerified: true})
	must(err)
	var wb, tid, schema string
	must(db.AsUser(t, author.PGRole, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.create_workbook('Ventes', 'collaborative')::text`).Scan(&wb); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_table($1::uuid, 'Lignes', '[{"name":"Article","type":"text"},{"name":"Qté","type":"integer"},{"name":"Prix","type":"decimal"},{"name":"Jour","type":"date"},{"name":"Payé","type":"boolean"}]')::text`, wb).Scan(&tid); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid) ->> 'schema'`, wb).Scan(&schema); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "Lignes"}.Sanitize()+` (lattis_id, lattis_pos, "Article", "Qté", "Prix", "Jour", "Payé")
			VALUES (gen_random_uuid(), 'a0', 'Vis; inox', 3, 1.5, '2026-10-21', true), (gen_random_uuid(), 'a1', 'Écrou', NULL, 2, NULL, false)`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT lattis.finish_publish($1::uuid)`, wb).Scan(new(int))
	}))

	secrets := t.TempDir()
	dir := t.TempDir()
	var mu sync.Mutex
	var posted, ctype string
	hsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		posted, ctype = string(b), r.Header.Get("Content-Type")
		mu.Unlock()
	}))
	defer hsrv.Close()
	r := &pipeline.Runner{Store: db.Store, Users: userdb.SessionAuth{Pool: db.Super}, Dir: dir, SecretsDir: secrets, HTTP: hsrv.Client()}

	create := func(name, kind, format, mode string, src map[string]any, secret string) string {
		t.Helper()
		cfg, _ := json.Marshal(map[string]any{"name": name, "workbook_id": wb, "table_id": tid, "source_kind": kind, "format": format,
			"mode": mode, "role": "reader", "source": src, "secret_ref": secret})
		var id string
		must(db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT lattis.create_pipeline($1::jsonb)::text`, string(cfg)).Scan(&id); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `SELECT lattis.set_pipeline_direction($1::uuid, 'out')`, id)
			return err
		}))
		return id
	}
	run := func(id string) {
		t.Helper()
		must(db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT lattis.run_pipeline($1::uuid, NULL)`, id)
			return err
		}))
		if ran, err := r.RunDue(ctx); err != nil || !ran {
			t.Fatalf("exécution : %v %v", ran, err)
		}
		var status, msg string
		var read, ins int
		must(db.Super.QueryRow(ctx, `SELECT status, coalesce(error, ''), rows_read, inserted FROM lattis.pipeline_runs WHERE pipeline_id = $1 ORDER BY started_at DESC LIMIT 1`, id).Scan(&status, &msg, &read, &ins))
		if status != "ok" || read != 2 || ins != 2 {
			t.Fatalf("export : %s %s (%d lues, %d écrites)", status, msg, read, ins)
		}
	}
	read := func(id string) string {
		t.Helper()
		p := r.LatestExport(id)
		if p == "" {
			t.Fatal("pas de fichier exporté")
		}
		b, err := os.ReadFile(p)
		must(err)
		return string(b)
	}

	// Fichier CSV (séparateur ;, champ protégé).
	csvID := create("Export CSV", "file", "csv", "replace", nil, "")
	run(csvID)
	if got := read(csvID); !strings.HasPrefix(got, "Article;Qté;Prix;Jour;Payé\n\"Vis; inox\";3;1.5;2026-10-21;t\nÉcrou;;2;;f\n") {
		t.Fatalf("CSV : %q", got)
	}
	// Simulation : rien d'écrit en plus.
	if res, err := r.Simulate(ctx, csvID, ""); err != nil || res.Status != "ok" || res.Read != 2 {
		t.Fatalf("simulation : %+v %v", res, err)
	}
	// JSON typé.
	jsonID := create("Export JSON", "file", "json", "replace", nil, "")
	run(jsonID)
	var rows []map[string]any
	must(json.Unmarshal([]byte(read(jsonID)), &rows))
	if len(rows) != 2 || rows[0]["Qté"] != float64(3) || rows[0]["Payé"] != true || rows[1]["Qté"] != nil || rows[1]["Article"] != "Écrou" {
		t.Fatalf("JSON : %v", rows)
	}
	// XLSX : feuille lisible, nombres en cellules numériques.
	xlsxID := create("Export XLSX", "file", "xlsx", "replace", nil, "")
	run(xlsxID)
	zr, err := zip.OpenReader(r.LatestExport(xlsxID))
	must(err)
	var sheet string
	for _, f := range zr.File {
		if f.Name == "xl/worksheets/sheet1.xml" {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			sheet = string(b)
		}
	}
	zr.Close()
	if !strings.Contains(sheet, "<c><v>3</v></c>") || !strings.Contains(sheet, "Vis; inox") || !strings.Contains(sheet, "VRAI") {
		t.Fatalf("XLSX : %s", sheet)
	}
	// HTTP (PUT, JSON).
	httpID := create("Export HTTP", "http", "json", "replace", map[string]any{"url": hsrv.URL + "/recevoir", "method": "PUT"}, "")
	run(httpID)
	mu.Lock()
	if ctype != "application/json" || !strings.Contains(posted, `"Article":"Écrou"`) {
		t.Fatalf("HTTP : %q %s", ctype, posted)
	}
	mu.Unlock()
	// PostgreSQL externe : ajout puis remplacement.
	_, err = db.Super.Exec(ctx, `CREATE TABLE public.recu ("Article" text, "Qté" bigint, "Prix" numeric, "Jour" date, "Payé" boolean)`)
	must(err)
	must(os.WriteFile(filepath.Join(secrets, "base-externe"), []byte(db.SuperURL), 0o600))
	sqlID := create("Export SQL", "sql", "sql", "append", map[string]any{"table": "public.recu"}, "base-externe")
	run(sqlID)
	run(sqlID)
	var n int
	must(db.Super.QueryRow(ctx, `SELECT count(*) FROM public.recu`).Scan(&n))
	if n != 4 {
		t.Fatalf("ajout : %d lignes", n)
	}
	_, err = db.Super.Exec(ctx, `UPDATE lattis.pipelines SET mode = 'replace' WHERE id = $1`, sqlID)
	must(err)
	run(sqlID)
	var qty int
	must(db.Super.QueryRow(ctx, `SELECT count(*), sum("Qté") FROM public.recu`).Scan(&n, &qty))
	if n != 2 || qty != 3 {
		t.Fatalf("remplacement : %d lignes, quantité %d", n, qty)
	}
	// Export depuis un fichier envoyé : refusé.
	cfg, _ := json.Marshal(map[string]any{"name": "Mauvais", "workbook_id": wb, "table_id": tid, "source_kind": "upload", "format": "csv", "mode": "replace", "role": "reader"})
	err = db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, `SELECT lattis.create_pipeline($1::jsonb)::text`, string(cfg)).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT lattis.set_pipeline_direction($1::uuid, 'out')`, id)
		return err
	})
	if err == nil {
		t.Fatal("export d'un fichier envoyé accepté")
	}
}
