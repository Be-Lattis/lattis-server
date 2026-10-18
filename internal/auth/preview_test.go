package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
	"github.com/Be-Lattis/lattis-server/internal/tokens"
)

func TestPreviewEndpoint(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	var google string
	if err := e.db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT lattis.set_config('access_mode', '"open"')`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT lattis.add_provider('google', 'Google', NULL, 'cid', NULL, NULL)::text`).Scan(&google)
	}); err != nil {
		t.Fatal(err)
	}
	enroll := func(n string) store.Enrollment {
		u, err := e.db.Store.Enroll(ctx, store.Identity{ProviderID: google, Subject: n, Email: testpg.Email(n), EmailVerified: true})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	owner, reader := enroll("auteur"), enroll("lecteur")
	var wb, tid string
	if err := e.db.AsUser(t, owner.PGRole, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.create_workbook('Budget', 'collaborative')::text`).Scan(&wb); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_table($1::uuid, 'Lignes', '[{"name":"Poste","type":"text"}]')::text`, wb).Scan(&tid); err != nil {
			return err
		}
		var schema string
		if err := tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid) ->> 'schema'`, wb).Scan(&schema); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "Lignes"}.Sanitize()+` (lattis_id, "Poste") VALUES (gen_random_uuid(), 'Loyer')`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.finish_publish($1::uuid)`, wb).Scan(new(int)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT lattis.invite($1, $2::uuid, ARRAY['reader'])`, reader.Email, wb)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	token := func(u store.Enrollment) string {
		now := time.Now()
		s, err := e.keys.Sign(tokens.TypeAccess, tokens.Claims{Issuer: e.srv.URL, Subject: u.UserID,
			Audience: tokens.Audience{tokens.AudiencePostgres}, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
			Scope: "openid lattis", PGRole: u.PGRole})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	call := func(tok string) (*http.Response, map[string]any) {
		body, _ := json.Marshal(map[string]string{"workbook_id": wb, "user_id": reader.UserID, "table_id": tid})
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/preview", strings.NewReader(string(body)))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := e.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp, out
	}
	if resp, _ := call(""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("sans jeton : %d", resp.StatusCode)
	}
	if resp, _ := call(token(reader)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("lecteur : %d", resp.StatusCode)
	}
	resp, out := call(token(owner))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("propriétaire : %d %v", resp.StatusCode, out)
	}
	rows := out["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["Poste"] != "Loyer" {
		t.Fatalf("lignes : %v", out)
	}
}
