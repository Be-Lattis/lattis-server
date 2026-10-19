package store_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

// Data Lake Lattis : un tableau partagé dans le catalogue, lisible selon
// l'audience (équipe + filtre), colonnes masquées, pierres tombales, retrait.
func TestDatasets(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	us := openUsers(t, db, "publieur", "anne", "bruno")
	pub, anne, bruno := us[0], us[1], us[2]
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	service := func(fn func(pgx.Tx) error) error { return db.Store.AsActor(ctx, "test", fn) }
	as := func(role string, fn func(pgx.Tx) error) error { return db.AsUser(t, role, fn) }

	var nord string
	must(service(func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT lattis.set_data_publisher($1::uuid, true)`, pub.UserID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_team('Nord', NULL)::text`).Scan(&nord); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT lattis.add_team_member($1::uuid, $2::uuid)`, nord, anne.UserID)
		return err
	}))

	var wb, tid, schema string
	must(as(pub.PGRole, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.create_workbook('Référentiel clients', 'collaborative')::text`).Scan(&wb); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_table($1::uuid, 'Clients', '[{"name":"Nom","type":"text"},{"name":"Région","type":"text"},{"name":"Marge","type":"decimal"}]')::text`, wb).Scan(&tid); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid) ->> 'schema'`, wb).Scan(&schema); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "Clients"}.Sanitize()+` (lattis_id, lattis_pos, "Nom", "Région", "Marge") VALUES
			('00000000-0000-7000-8000-000000000001', 'a0', 'Lille SA', 'Nord', 12),
			('00000000-0000-7000-8000-000000000002', 'a1', 'Nice SARL', 'Sud', 30)`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT lattis.finish_publish($1::uuid)`, wb).Scan(new(int))
	}))

	// Un non-publieur ne partage pas.
	if err := as(bruno.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.create_data_folder('Pirate', NULL)`)
		return err
	}); pgCode(err) != "42501" {
		t.Fatalf("dossier créé par un non-publieur : %v", err)
	}

	var folder, sub, ds, sqlName string
	must(as(pub.PGRole, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.create_data_folder('Commercial', NULL)::text`).Scan(&folder); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_data_folder('Clients', $1::uuid)::text`, folder).Scan(&sub); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_dataset($1::uuid, $2::uuid, $3::uuid, 'Clients', 'Référentiel', NULL, ARRAY['Marge'])::text`,
			wb, tid, sub).Scan(&ds); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT sql_name FROM jsonb_to_recordset(lattis.data_catalog() -> 'datasets') AS x(id uuid, sql_name text) WHERE id = $1::uuid`, ds).Scan(&sqlName); err != nil {
			return err
		}
		// Audience posée sur le dossier parent : la donnée en hérite.
		_, err := tx.Exec(ctx, `SELECT lattis.add_data_audience(NULL, $1::uuid, $2::uuid, $3)`, folder, nord, `"Région" = 'Nord'`)
		return err
	}))
	// Un autre publieur, sans rôle sur le classeur source, ne peut pas ouvrir
	// la donnée par l'audience du dossier.
	must(service(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.set_data_publisher($1::uuid, true)`, bruno.UserID)
		return err
	}))
	if err := as(bruno.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.add_data_audience(NULL, $1::uuid, $2::uuid, NULL)`, folder, bruno.UserID)
		return err
	}); pgCode(err) != "42501" {
		t.Fatalf("audience de dossier posée par un autre publieur : %v", err)
	}
	must(service(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.set_data_publisher($1::uuid, false)`, bruno.UserID)
		return err
	}))
	// Historique de l'audience : l'équipe Nord figure parmi les lecteurs.
	must(as(pub.PGRole, func(tx pgx.Tx) error {
		var hist string
		if err := tx.QueryRow(ctx, `SELECT lattis.dataset_audience_history($1::uuid)::text`, ds).Scan(&hist); err != nil {
			return err
		}
		if !strings.Contains(hist, "Équipe Nord") {
			t.Errorf("historique de l'audience : %s", hist)
		}
		return nil
	}))
	if sqlName != "clients" {
		t.Fatalf("nom SQL : %q", sqlName)
	}
	view := pgx.Identifier{"donnees", sqlName}.Sanitize()

	// Anne (équipe Nord) : une ligne, sans la colonne masquée.
	var rows int
	var cols []string
	must(as(anne.PGRole, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+view).Scan(&rows); err != nil {
			return err
		}
		r, err := tx.Query(ctx, `SELECT * FROM `+view+` LIMIT 0`)
		if err != nil {
			return err
		}
		for _, f := range r.FieldDescriptions() {
			cols = append(cols, f.Name)
		}
		r.Close()
		var n int
		if err := tx.QueryRow(ctx, `SELECT jsonb_array_length(lattis.data_catalog() -> 'folders')`).Scan(&n); err != nil {
			return err
		}
		if n != 2 {
			t.Errorf("dossiers visibles pour Anne : %d", n)
		}
		return nil
	}))
	if rows != 1 {
		t.Fatalf("lignes visibles pour Anne : %d", rows)
	}
	for _, c := range cols {
		if c == "Marge" || c == "lattis_author" {
			t.Fatalf("colonne exposée à tort : %v", cols)
		}
	}

	// Audience par adresse : adresse inconnue refusée.
	if err := as(pub.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.add_data_audience_by_email($1::uuid, NULL, 'inconnu@example.test', NULL)`, ds)
		return err
	}); pgCode(err) != "P0002" && pgCode(err) != "02000" {
		t.Fatalf("adresse inconnue : %v", err)
	}

	// Bruno, hors audience : pas de lecture, catalogue vide.
	if err := as(bruno.PGRole, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM `+view).Scan(new(int))
	}); pgCode(err) != "42501" {
		t.Fatalf("lecture hors audience : %v", err)
	}
	must(as(bruno.PGRole, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT jsonb_array_length(lattis.data_catalog() -> 'datasets') + jsonb_array_length(lattis.data_catalog() -> 'folders')`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("catalogue de Bruno : %d éléments", n)
		}
		return nil
	}))

	// Sortie d'audience : Lille passe au Sud, la ligne n'est plus rendue mais
	// son identifiant figure parmi les lignes changées.
	var since string
	must(as(anne.PGRole, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.sync_point()::text`).Scan(&since)
	}))
	must(as(pub.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE `+pgx.Identifier{schema, "Clients"}.Sanitize()+` SET "Région" = 'Sud' WHERE "Nom" = 'Lille SA'`)
		return err
	}))
	// since peut remonter avant des écritures plus anciennes (transactions
	// ouvertes ailleurs dans la grappe) : on vérifie que Lille figure parmi
	// les lignes changées, sans en exiger le nombre exact.
	var changedIDs []string
	must(as(anne.PGRole, func(tx pgx.Tx) error {
		var visible int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+view+` WHERE lattis_xid >= $1::xid8 AND "Nom" = 'Lille SA'`, since).Scan(&visible); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT coalesce(array_agg(row_id::text), '{}') FROM lattis.dataset_changed_ids($1::uuid, $2::xid8)`, ds, since).Scan(&changedIDs); err != nil {
			return err
		}
		if visible != 0 {
			t.Errorf("sortie d'audience : Lille encore visible")
		}
		return nil
	}))
	var lille string
	must(db.Super.QueryRow(ctx, `SELECT lattis_id::text FROM `+pgx.Identifier{schema, "Clients"}.Sanitize()+` WHERE "Nom" = 'Lille SA'`).Scan(&lille))
	if !slices.Contains(changedIDs, lille) {
		t.Errorf("sortie d'audience : Lille absente des lignes changées %v", changedIDs)
	}
	must(as(pub.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE `+pgx.Identifier{schema, "Clients"}.Sanitize()+` SET "Région" = 'Nord' WHERE "Nom" = 'Lille SA'`)
		return err
	}))

	// Suppression dans la source : visible par les pierres tombales.
	must(as(pub.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM `+pgx.Identifier{schema, "Clients"}.Sanitize()+` WHERE "Nom" = 'Lille SA'`)
		return err
	}))
	must(as(anne.PGRole, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM lattis.dataset_tombstones($1::uuid, '0'::xid8)`, ds).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("pierres tombales : %d", n)
		}
		return nil
	}))

	// Équipe supprimée : la vue est refaite sans elle et reste lisible par personne.
	must(service(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.drop_team($1::uuid)`, nord)
		return err
	}))
	if err := as(anne.PGRole, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM `+view).Scan(new(int))
	}); pgCode(err) != "42501" {
		t.Fatalf("lecture après suppression de l'équipe : %v", err)
	}

	// Retrait du partage : la vue disparaît.
	must(as(pub.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.drop_dataset($1::uuid)`, ds)
		return err
	}))
	must(service(func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'donnees'`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("vues restantes : %d", n)
		}
		return nil
	}))
}

// Une donnée tirée d'un classeur figé suit ses republications : la vue est
// refaite sur la nouvelle version et survit à la suppression de l'ancienne.
func TestDatasetFrozenRepublish(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	us := openUsers(t, db, "publieur", "anne")
	pub, anne := us[0], us[1]
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	as := func(role string, fn func(pgx.Tx) error) error { return db.AsUser(t, role, fn) }
	must(db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.set_data_publisher($1::uuid, true)`, pub.UserID)
		return err
	}))
	cols := `[{"name":"Article","type":"text","table_local_id":"t1"}]`
	var wb, tid, schema, ds string
	must(as(pub.PGRole, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.create_workbook('Tarifs', 'frozen')::text`).Scan(&wb); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_table($1::uuid, 'Tarifs', $2::jsonb)::text`, wb, cols).Scan(&tid); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid) ->> 'schema'`, wb).Scan(&schema); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "Tarifs"}.Sanitize()+` (lattis_id, "Article") VALUES (gen_random_uuid(), 'Vis')`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.finish_publish($1::uuid)`, wb).Scan(new(int)); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_dataset($1::uuid, $2::uuid, NULL, 'Tarifs', '', 'tarifs', NULL)::text`, wb, tid).Scan(&ds); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT lattis.add_data_audience($1::uuid, NULL, $2::uuid, NULL)`, ds, anne.UserID)
		return err
	}))
	count := func() int {
		t.Helper()
		var n int
		must(as(anne.PGRole, func(tx pgx.Tx) error { return tx.QueryRow(ctx, `SELECT count(*) FROM donnees.tarifs`).Scan(&n) }))
		return n
	}
	if n := count(); n != 1 {
		t.Fatalf("avant republication : %d", n)
	}
	must(as(pub.PGRole, func(tx pgx.Tx) error {
		var staging string
		if err := tx.QueryRow(ctx, `SELECT lattis.republish($1::uuid)`, wb).Scan(&staging); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT lattis.create_table($1::uuid, 'Tarifs', $2::jsonb)`, wb, cols); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{staging, "Tarifs"}.Sanitize()+` (lattis_id, "Article") VALUES (gen_random_uuid(), 'Vis'), (gen_random_uuid(), 'Écrou')`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT lattis.finish_publish($1::uuid)`, wb).Scan(new(int))
	}))
	if n := count(); n != 2 {
		t.Fatalf("après republication : %d", n)
	}
	if _, err := db.Super.Exec(ctx, `UPDATE lattis.pending_drops SET drop_after = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	must(db.Store.Purge(ctx))
	if n := count(); n != 2 {
		t.Fatalf("après suppression de l'ancienne version : %d", n)
	}
}
