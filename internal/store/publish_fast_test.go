package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

// Publication : pas de déclencheur par ligne sur le brouillon (chargement
// rapide), colonnes système remplies par leurs valeurs par défaut ;
// déclencheurs posés à la publication.
func TestDraftWithoutRowTriggers(t *testing.T) {
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
	u, err := db.Store.Enroll(ctx, store.Identity{ProviderID: google, Subject: "a", Email: testpg.Email("auteur"), EmailVerified: true})
	must(err)
	var wb, schema string
	triggers := func() int {
		t.Helper()
		var n int
		must(db.Super.QueryRow(ctx, `SELECT count(*) FROM pg_trigger WHERE tgrelid = $1::regclass AND tgname IN ('lattis_stamp', 'lattis_tombstone')`,
			pgx.Identifier{schema, "Lignes"}.Sanitize()).Scan(&n))
		return n
	}
	must(db.AsUser(t, u.PGRole, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.create_workbook('Gros', 'collaborative')::text`).Scan(&wb); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT lattis.create_table($1::uuid, 'Lignes', '[{"name":"A","type":"text"}]')`, wb); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid) ->> 'schema'`, wb).Scan(&schema); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "Lignes"}.Sanitize()+` (lattis_id, "A") VALUES (gen_random_uuid(), 'x')`)
		return err
	}))
	if n := triggers(); n != 0 {
		t.Fatalf("déclencheurs sur le brouillon : %d", n)
	}
	var author, by, pos string
	must(db.Super.QueryRow(ctx, `SELECT lattis_author, lattis_updated_by, lattis_pos FROM `+pgx.Identifier{schema, "Lignes"}.Sanitize()).Scan(&author, &by, &pos))
	if author != u.PGRole || by != u.PGRole || pos != "" {
		t.Fatalf("colonnes système : %q %q %q", author, by, pos)
	}
	must(db.AsUser(t, u.PGRole, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.finish_publish($1::uuid)`, wb).Scan(new(int))
	}))
	if n := triggers(); n != 2 {
		t.Fatalf("déclencheurs après publication : %d", n)
	}
}
