package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/migrate"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

// Mise à jour d'une installation existante : une base à la version
// précédente, avec un classeur, une équipe, une donnée du Data Lake et son
// audience, passe à la dernière version sans rien perdre (lecture filtrée,
// droits, écarts).
func TestUpgradeFromPrevious(t *testing.T) {
	ctx := context.Background()
	db := testpg.NewAt(t, migrate.Latest()-1)
	us := openUsers(t, db, "proprio", "lecteur")
	owner, reader := us[0], us[1]
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var team, wb, tid, schema, ds string
	must(db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT lattis.set_data_publisher($1::uuid, true)`, owner.UserID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_team('Nord', NULL)::text`).Scan(&team); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT lattis.add_team_member($1::uuid, $2::uuid)`, team, reader.UserID)
		return err
	}))
	must(db.AsUser(t, owner.PGRole, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.create_workbook('Avant mise à jour', 'collaborative')::text`).Scan(&wb); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_table($1::uuid, 'Clients', '[{"name":"Nom","type":"text"},{"name":"Région","type":"text"}]')::text`, wb).Scan(&tid); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid) ->> 'schema'`, wb).Scan(&schema); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "Clients"}.Sanitize()+` (lattis_id, lattis_pos, "Nom", "Région")
			VALUES (gen_random_uuid(), 'a0', 'Lille', 'Nord'), (gen_random_uuid(), 'a1', 'Nice', 'Sud')`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.finish_publish($1::uuid)`, wb).Scan(new(int)); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_dataset($1::uuid, $2::uuid, NULL, 'Clients', '', 'clients_maj', NULL)::text`, wb, tid).Scan(&ds); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT lattis.add_data_audience($1::uuid, NULL, $2::uuid, '"Région" = ''Nord''')`, ds, team)
		return err
	}))

	if _, err := migrate.Up(ctx, db.Store.DB); err != nil {
		t.Fatalf("mise à jour : %v", err)
	}

	var n int
	must(db.AsUser(t, reader.PGRole, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM donnees.clients_maj`).Scan(&n)
	}))
	if n != 1 {
		t.Fatalf("lecture filtrée après mise à jour : %d lignes", n)
	}
	var drift int
	must(db.AsUser(t, owner.PGRole, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT jsonb_array_length(lattis.check_drift($1::uuid))`, wb).Scan(&drift)
	}))
	if drift != 0 {
		t.Fatalf("écarts après mise à jour : %d", drift)
	}
	var verified bool
	must(db.Super.QueryRow(ctx, `SELECT bool_and(email_verified) FROM lattis.users`).Scan(&verified))
	if !verified {
		t.Fatal("comptes existants marqués non vérifiés")
	}
}
