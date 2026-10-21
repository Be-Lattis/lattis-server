package pipeline_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/pipeline"
	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
	"github.com/Be-Lattis/lattis-server/internal/userdb"
)

// Cible « Créer automatiquement » : donnée neuve du Data Lake, colonnes
// créées d'après la source (types devinés), colonnes nouvelles ajoutées aux
// exécutions suivantes ; la donnée est proposée ensuite comme cible.
func TestPipelineAutoTarget(t *testing.T) {
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
	admin, err := db.Store.Enroll(ctx, store.Identity{ProviderID: google, Subject: "admin", Email: testpg.Email("admin"), EmailVerified: true})
	must(err)
	_, err = db.Super.Exec(ctx, `GRANT lattis_server_owners TO `+pgx.Identifier{admin.PGRole}.Sanitize())
	must(err)

	var target struct {
		WorkbookID string `json:"workbook_id"`
		TableID    string `json:"table_id"`
		DatasetID  string `json:"dataset_id"`
	}
	var pid string
	must(db.AsUser(t, admin.PGRole, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.create_datalake_target('{"name": "Ventes du jour"}')`).Scan(&target); err != nil {
			return err
		}
		cfg, _ := json.Marshal(map[string]any{"name": "Ventes", "workbook_id": target.WorkbookID, "table_id": target.TableID,
			"source_kind": "upload", "format": "csv", "mode": "append", "role": "designer", "source": map[string]any{"auto_columns": true}})
		return tx.QueryRow(ctx, `SELECT lattis.create_pipeline($1::jsonb)::text`, string(cfg)).Scan(&pid)
	}))

	dir := t.TempDir()
	r := &pipeline.Runner{Store: db.Store, Users: userdb.SessionAuth{Pool: db.Super}, Dir: dir}
	run := func(file, content string) {
		t.Helper()
		up := filepath.Join(dir, pid, "uploads")
		must(os.MkdirAll(up, 0o750))
		must(os.WriteFile(filepath.Join(up, file), []byte(content), 0o640))
		must(db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT lattis.run_pipeline($1::uuid, $2)`, pid, file)
			return err
		}))
		if ran, err := r.RunDue(ctx); err != nil || !ran {
			t.Fatalf("exécution : %v %v", ran, err)
		}
		var status, msg string
		must(db.Super.QueryRow(ctx, `SELECT status, coalesce(error, '') FROM lattis.pipeline_runs WHERE pipeline_id = $1 ORDER BY started_at DESC LIMIT 1`, pid).Scan(&status, &msg))
		if status != "ok" {
			t.Fatalf("exécution %s : %s %s", file, status, msg)
		}
	}
	columns := func() map[string]string {
		t.Helper()
		var schema string
		must(db.Super.QueryRow(ctx, `SELECT schema_name FROM lattis.workbooks WHERE id = $1`, target.WorkbookID).Scan(&schema))
		rows, err := db.Super.Query(ctx, `SELECT name, type FROM `+pgx.Identifier{schema, "lattis_meta_columns"}.Sanitize()+` WHERE table_id = $1`, target.TableID)
		must(err)
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var n, ty string
			must(rows.Scan(&n, &ty))
			out[n] = ty
		}
		return out
	}

	run("j1.csv", "Code;Quantité;Prix;Jour;Payé\n0042;3;1,50;2026-10-21;oui\nB7;12;2;2026-10-20;non\n")
	cols := columns()
	want := map[string]string{"Code": "text", "Quantité": "integer", "Prix": "decimal", "Jour": "date", "Payé": "boolean"}
	if len(cols) != len(want) {
		t.Fatalf("colonnes créées : %v", cols)
	}
	for n, ty := range want {
		if cols[n] != ty {
			t.Fatalf("colonne %s : %q, attendu %q (%v)", n, cols[n], ty, cols)
		}
	}
	// Colonne nouvelle dans la source : ajoutée.
	run("j2.csv", "Code;Quantité;Prix;Jour;Payé;Magasin\nC1;1;3;2026-10-22;oui;Lille\n")
	if columns()["Magasin"] != "text" {
		t.Fatalf("colonne ajoutée : %v", columns())
	}
	var n int
	must(db.Super.QueryRow(ctx, `SELECT count(*) FROM lattis.datasets WHERE id = $1`, target.DatasetID).Scan(&n))
	if n != 1 {
		t.Fatal("donnée du Data Lake absente")
	}
	// Proposée comme cible, sous son nom de donnée.
	var targets []map[string]any
	must(db.AsUser(t, admin.PGRole, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.pipeline_targets()`).Scan(&targets)
	}))
	found := false
	for _, tg := range targets {
		found = found || (tg["datalake"] == true && tg["dataset"] == "Ventes du jour")
	}
	if !found {
		t.Fatalf("cible du Data Lake absente : %v", targets)
	}
}
