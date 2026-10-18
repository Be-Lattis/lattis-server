package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

// openUsers inscrit des utilisateurs en entrée libre.
func openUsers(t *testing.T, db *testpg.DB, names ...string) []store.Enrollment {
	t.Helper()
	ctx := context.Background()
	var google string
	if err := db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT lattis.set_config('access_mode', '"open"')`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT lattis.add_provider('google', 'Google', NULL, 'cid', NULL, NULL)::text`).Scan(&google)
	}); err != nil {
		t.Fatal(err)
	}
	var out []store.Enrollment
	for _, n := range names {
		e, err := db.Store.Enroll(ctx, store.Identity{ProviderID: google, Subject: n, Email: testpg.Email(n), EmailVerified: true})
		if err != nil || e.Status != "ok" {
			t.Fatal(e, err)
		}
		out = append(out, e)
	}
	return out
}

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func TestFrozenWorkbookLifecycle(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	us := openUsers(t, db, "auteur", "lecteur", "intrus")
	author, reader, stranger := us[0], us[1], us[2]
	as := func(u store.Enrollment, fn func(pgx.Tx) error) error { return db.AsUser(t, u.PGRole, fn) }

	var wb, tid, schema string
	if err := as(author, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.create_workbook('Ventes 2026 — Été', 'frozen')::text`).Scan(&wb); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT lattis.create_table($1::uuid, 'Clients', '[{"name":"Nom","type":"text"},{"name":"CA","type":"decimal"},{"name":"Actif","type":"boolean"}]')::text`, wb).Scan(&tid)
	}); err != nil {
		t.Fatal(err)
	}
	if err := as(author, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT schema_name FROM lattis.workbooks`).Scan(&schema)
	}); pgCode(err) != "42501" {
		t.Fatalf("le catalogue ne doit pas être lisible par un membre : %v", err)
	}
	_ = db.Super.QueryRow(ctx, `SELECT schema_name FROM lattis.workbooks WHERE id = $1::uuid`, wb).Scan(&schema)
	// Les rôles sont communs au serveur de test : suffixe possible (_2, _3…).
	if !strings.HasPrefix(schema, "wb_ventes_2026_ete") {
		t.Fatalf("schéma : %q", schema)
	}
	tbl := pgx.Identifier{schema, "Clients"}.Sanitize()

	// Brouillon : l'auteur remplit par COPY.
	if err := as(author, func(tx pgx.Tx) error {
		_, err := tx.CopyFrom(ctx, pgx.Identifier{schema, "Clients"}, []string{"lattis_id", "lattis_pos", "Nom", "CA", "Actif"},
			pgx.CopyFromRows([][]any{
				{"01920000-0000-7000-8000-000000000001", "a0", "Durand", "1200.50", true},
				{"01920000-0000-7000-8000-000000000002", "a1", "Martin", "80", false},
			}))
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT lattis.finish_publish($1::uuid)`, wb).Scan(new(int))
	}); err != nil {
		t.Fatal(err)
	}

	// Invitation d'un lecteur déjà inscrit : appliquée tout de suite.
	if err := as(author, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.invite($1, $2::uuid, '{reader}')`, reader.Email, wb)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := as(reader, func(tx pgx.Tx) error { return tx.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n) }); err != nil || n != 2 {
		t.Fatalf("lecteur : %d lignes, %v", n, err)
	}
	var caps map[string]any
	_ = as(reader, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid)`, wb).Scan(&caps)
	})
	if caps["can_design"] != false || caps["tables"].(map[string]any)[tid].(map[string]any)["level"] != "reader" {
		t.Fatalf("capacités : %v", caps)
	}
	var list []map[string]any
	_ = as(reader, func(tx pgx.Tx) error { return tx.QueryRow(ctx, `SELECT lattis.my_workbooks()`).Scan(&list) })
	if len(list) != 1 || list[0]["title"] != "Ventes 2026 — Été" {
		t.Fatalf("liste : %v", list)
	}

	// Figé : personne n'écrit, l'intrus ne lit rien.
	for who, u := range map[string]store.Enrollment{"auteur": author, "lecteur": reader} {
		err := as(u, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE `+tbl+` SET "CA" = 0`)
			return err
		})
		if pgCode(err) != "42501" {
			t.Errorf("%s écrit dans un classeur figé : %v", who, err)
		}
	}
	if err := as(stranger, func(tx pgx.Tx) error { return tx.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n) }); pgCode(err) != "42501" {
		t.Fatalf("intrus : %v", err)
	}
	if err := as(reader, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, `SELECT lattis.republish($1::uuid)`, wb); return err }); pgCode(err) != "42501" {
		t.Fatalf("lecteur qui republie : %v", err)
	}

	// Les lecteurs écoutent le canal : rien avant la bascule.
	var capsJ struct {
		Channel string `json:"channel"`
		Title   string `json:"title"`
	}
	if err := as(reader, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid)`, wb).Scan(&capsJ)
	}); err != nil || capsJ.Title == "" {
		t.Fatalf("capacités : %+v %v", capsJ, err)
	}
	listen, err := db.Super.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer listen.Release()
	if _, err := listen.Exec(ctx, `LISTEN `+pgx.Identifier{capsJ.Channel}.Sanitize()); err != nil {
		t.Fatal(err)
	}

	// Republication : version de travail, bascule atomique.
	var staging string
	if err := as(author, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.republish($1::uuid)`, wb).Scan(&staging); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT lattis.create_table($1::uuid, 'Clients', '[{"name":"Nom","type":"text"},{"name":"CA","type":"decimal"}]')`, wb); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{staging, "Clients"}.Sanitize()+` (lattis_id, "Nom", "CA") VALUES (gen_random_uuid(), 'Petit', 5)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Pendant ce temps, le lecteur voit toujours la version 1 et pas la version de travail.
	if err := as(reader, func(tx pgx.Tx) error { return tx.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n) }); err != nil || n != 2 {
		t.Fatalf("lecture pendant la republication : %d %v", n, err)
	}
	if err := as(reader, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{staging, "Clients"}.Sanitize()).Scan(&n)
	}); pgCode(err) != "42501" {
		t.Fatalf("version de travail visible : %v", err)
	}
	var version int
	if err := as(author, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.finish_publish($1::uuid)`, wb).Scan(&version)
	}); err != nil || version != 2 {
		t.Fatalf("republication : %d %v", version, err)
	}
	if note, err := listen.Conn().WaitForNotification(ctx); err != nil || note.Payload != "*version:2" {
		t.Fatalf("première notification : %+v %v", note, err)
	}
	if err := as(reader, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{staging, "Clients"}.Sanitize()).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("nouvelle version : %d %v", n, err)
	}
	// L'ancienne version est supprimée après le délai de grâce.
	if _, err := db.Super.Exec(ctx, `UPDATE lattis.pending_drops SET drop_after = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Store.Purge(ctx); err != nil {
		t.Fatal(err)
	}
	var exists bool
	_ = db.Super.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, schema).Scan(&exists)
	if exists {
		t.Fatal("ancienne version non supprimée")
	}

	// Suppression du classeur : schémas et rôles disparaissent.
	if err := as(reader, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, `SELECT lattis.drop_workbook($1::uuid)`, wb); return err }); pgCode(err) != "42501" {
		t.Fatalf("lecteur qui supprime : %v", err)
	}
	if err := as(author, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, `SELECT lattis.drop_workbook($1::uuid)`, wb); return err }); err != nil {
		t.Fatal(err)
	}
	_ = db.Super.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname LIKE $1 || '\_%')`, schema).Scan(&exists)
	if exists {
		t.Fatal("rôles du classeur restants")
	}
}

func TestCreateTableValidation(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	author := openUsers(t, db, "a")[0]
	var wb string
	if err := db.AsUser(t, author.PGRole, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.create_workbook('Essai', 'frozen')::text`).Scan(&wb)
	}); err != nil {
		t.Fatal(err)
	}
	for name, cols := range map[string]string{
		"type inconnu":   `[{"name":"x","type":"money"}]`,
		"nom réservé":    `[{"name":"lattis_id","type":"text"}]`,
		"doublon":        `[{"name":"Nom","type":"text"},{"name":"nom","type":"text"}]`,
		"aucune colonne": `[]`,
	} {
		err := db.AsUser(t, author.PGRole, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT lattis.create_table($1::uuid, 'T', $2::jsonb)`, wb, cols)
			return err
		})
		if err == nil {
			t.Errorf("%s : accepté", name)
		}
	}
}

// Création réservée : le rôle « créateur de classeurs » suffit, sans être
// propriétaire du serveur.
func TestWorkbookCreatorRole(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	us := openUsers(t, db, "createur", "autre")
	creator, other := us[0], us[1]
	if err := db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT lattis.set_config('workbook_creators', '"owners"')`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT lattis.set_workbook_creator($1::uuid, true)`, creator.UserID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	create := func(u store.Enrollment, title string) error {
		return db.AsUser(t, u.PGRole, func(tx pgx.Tx) error {
			var can bool
			if err := tx.QueryRow(ctx, `SELECT (lattis.login_summary() ->> 'can_create_workbooks')::boolean`).Scan(&can); err != nil {
				return err
			}
			if can != (u.UserID == creator.UserID) {
				t.Errorf("can_create_workbooks de %s : %v", u.PGRole, can)
			}
			_, err := tx.Exec(ctx, `SELECT lattis.create_workbook($1, 'frozen')`, title)
			return err
		})
	}
	if err := create(creator, "Créé par le créateur"); err != nil {
		t.Fatalf("créateur : %v", err)
	}
	if err := create(other, "Refusé"); pgCode(err) != "42501" {
		t.Fatalf("sans le rôle : %v", err)
	}
	if err := db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.set_workbook_creator($1::uuid, false)`, creator.UserID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.AsUser(t, creator.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.create_workbook('Après retrait', 'frozen')`)
		return err
	}); pgCode(err) != "42501" {
		t.Fatalf("rôle retiré : %v", err)
	}
}

// Activité d'un classeur : dernier éditeur (lignes des tableaux) et dernier
// lecteur (record_open), visibles dans admin_workbooks.
func TestWorkbookActivity(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	us := openUsers(t, db, "auteur", "lecteur", "intrus")
	author, reader, stranger := us[0], us[1], us[2]
	var wb, schema string
	if err := db.AsUser(t, author.PGRole, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.create_workbook('Activité', 'collaborative')::text`).Scan(&wb); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT lattis.create_table($1::uuid, 'Notes', '[{"name":"Texte","type":"text"}]')`, wb); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid) ->> 'schema'`, wb).Scan(&schema); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.finish_publish($1::uuid)`, wb).Scan(new(int)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT lattis.invite($1, $2::uuid, ARRAY['reader'])`, reader.Email, wb); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "Notes"}.Sanitize()+` (lattis_id, lattis_pos, "Texte") VALUES (gen_random_uuid(), 'a0', 'ok')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.AsUser(t, reader.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.record_open($1::uuid)`, wb)
		return err
	}); err != nil {
		t.Fatalf("ouverture : %v", err)
	}
	if err := db.AsUser(t, stranger.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.record_open($1::uuid)`, wb)
		return err
	}); pgCode(err) != "42501" {
		t.Fatalf("ouverture sans accès : %v", err)
	}
	var edit, read string
	if err := db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT w -> 'last_edit' ->> 'email', w -> 'last_read' ->> 'email'
			FROM jsonb_array_elements(lattis.admin_workbooks()) w WHERE w ->> 'id' = $1`, wb).Scan(&edit, &read)
	}); err != nil {
		t.Fatal(err)
	}
	if edit != author.Email || read != reader.Email {
		t.Fatalf("activité : modifié par %q, lu par %q", edit, read)
	}
}
