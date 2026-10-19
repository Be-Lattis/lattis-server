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

// Pipeline de bout en bout : fichier envoyé, mise à jour par clé sous
// l'identité technique, lignes rejetées, puis remplacement.
func TestPipelineUpload(t *testing.T) {
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
		return tx.QueryRow(ctx, `SELECT lattis.add_provider('google', 'Google', NULL, 'cid', NULL, NULL)::text`).Scan(&google)
	}))
	author, err := db.Store.Enroll(ctx, store.Identity{ProviderID: google, Subject: "auteur", Email: testpg.Email("auteur"), EmailVerified: true})
	must(err)

	var wb, tid, schema string
	must(db.AsUser(t, author.PGRole, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.create_workbook('Catalogue', 'collaborative')::text`).Scan(&wb); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_table($1::uuid, 'Articles', '[{"name":"Code","type":"text"},{"name":"Libellé","type":"text"},{"name":"Prix","type":"decimal"},{"name":"Depuis","type":"date"}]')::text`, wb).Scan(&tid); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid) ->> 'schema'`, wb).Scan(&schema); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "Articles"}.Sanitize()+` (lattis_id, lattis_pos, "Code", "Libellé", "Prix")
			VALUES (gen_random_uuid(), 'a0', 'A1', 'Ancien libellé', 1)`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT lattis.finish_publish($1::uuid)`, wb).Scan(new(int))
	}))

	var pid string
	cfg, _ := json.Marshal(map[string]any{"name": "Tarifs", "workbook_id": wb, "table_id": tid, "source_kind": "upload",
		"format": "csv", "mode": "upsert", "key_columns": []string{"Code"}, "column_map": map[string]string{"Prix": "PRIX_HT"}})
	must(service(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.create_pipeline($1::jsonb)::text`, string(cfg)).Scan(&pid)
	}))

	dir := t.TempDir()
	r := &pipeline.Runner{Store: db.Store, Users: userdb.SessionAuth{Pool: db.Super}, Dir: dir}
	run := func(file, content string) map[string]any {
		t.Helper()
		up := filepath.Join(dir, pid, "uploads")
		must(os.MkdirAll(up, 0o750))
		must(os.WriteFile(filepath.Join(up, file), []byte(content), 0o640))
		must(service(func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT lattis.run_pipeline($1::uuid, $2)`, pid, file)
			return err
		}))
		ran, err := r.RunDue(ctx)
		if err != nil || !ran {
			t.Fatalf("exécution : %v %v", ran, err)
		}
		var res map[string]any
		must(db.Super.QueryRow(ctx, `SELECT to_jsonb(r) FROM lattis.pipeline_runs r WHERE pipeline_id = $1 ORDER BY started_at DESC LIMIT 1`, pid).Scan(&res))
		return res
	}

	// Simulation : même bilan, rien d'écrit.
	up := filepath.Join(dir, pid, "uploads")
	must(os.MkdirAll(up, 0o750))
	must(os.WriteFile(filepath.Join(up, "essai.csv"), []byte("Code;Libellé;PRIX_HT\nA1;Vis inox;1,50\nA9;Neuf;2\n"), 0o640))
	sim, err := r.Simulate(ctx, pid, "essai.csv")
	if err != nil || sim.Status != "ok" || sim.Inserted != 1 || sim.Updated != 1 {
		t.Fatalf("simulation : %+v %v", sim, err)
	}
	var count int
	must(db.Super.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{schema, "Articles"}.Sanitize()).Scan(&count))
	if count != 1 {
		t.Fatalf("la simulation a écrit : %d lignes", count)
	}

	res := run("tarifs.csv", "Code;Libellé;PRIX_HT;Depuis\nA1;Vis inox;1,50;18/10/2026\nA2;Écrou;0,20;\nA3;Rondelle;pas un prix;\n")
	if res["status"] != "warning" || res["inserted"].(float64) != 1 || res["updated"].(float64) != 1 || res["rejected"].(float64) != 1 {
		t.Fatalf("première exécution : %v", res)
	}
	tbl := pgx.Identifier{schema, "Articles"}.Sanitize()
	var label, by string
	must(db.Super.QueryRow(ctx, `SELECT "Libellé", lattis_updated_by FROM `+tbl+` WHERE "Code" = 'A1'`).Scan(&label, &by))
	var identity string
	must(db.Super.QueryRow(ctx, `SELECT u.pg_role FROM lattis.pipelines p JOIN lattis.users u ON u.id = p.user_id WHERE p.id = $1`, pid).Scan(&identity))
	if label != "Vis inox" || by != identity {
		t.Fatalf("mise à jour : %q par %q (identité %q)", label, by, identity)
	}
	if _, err := os.Stat(filepath.Join(dir, pid, "uploads", "tarifs.csv")); !os.IsNotExist(err) {
		t.Fatal("fichier traité non rangé")
	}
	// Même fichier relu : rien ne change (aucune mise à jour inutile).
	res = run("tarifs2.csv", "Code;Libellé;PRIX_HT\nA1;Vis inox;1,50\n")
	if res["status"] != "ok" || res["updated"].(float64) != 0 {
		t.Fatalf("relecture : %v", res)
	}

	// Remplacement par clé : A2 disparaît.
	must(service(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.update_pipeline($1::uuid, '{"mode":"replace"}')`, pid)
		return err
	}))
	res = run("tarifs3.csv", "Code;Libellé;PRIX_HT\nA1;Vis inox;1,60\nA4;Boulon;0,90\n")
	if res["status"] != "ok" || res["inserted"].(float64) != 1 || res["updated"].(float64) != 1 || res["deleted"].(float64) != 1 {
		t.Fatalf("remplacement : %v", res)
	}
	var n int
	must(db.Super.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n))
	if n != 2 {
		t.Fatalf("lignes après remplacement : %d", n)
	}

	// Suppression : l'identité technique est désactivée.
	must(service(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.drop_pipeline($1::uuid)`, pid)
		return err
	}))
	var disabled bool
	must(db.Super.QueryRow(ctx, `SELECT disabled FROM lattis.users WHERE pg_role = $1`, identity).Scan(&disabled))
	if !disabled {
		t.Fatal("identité technique encore active")
	}
}
