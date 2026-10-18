package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

func TestCollaborativeWorkbook(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	us := openUsers(t, db, "proprio", "editeur", "participant", "lecteur")
	owner, editor, part, reader := us[0], us[1], us[2], us[3]
	as := func(u store.Enrollment, fn func(pgx.Tx) error) error { return db.AsUser(t, u.PGRole, fn) }
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	var wb, tid, schema string
	must(as(owner, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.create_workbook('Suivi', 'collaborative')::text`).Scan(&wb); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_table($1::uuid, 'Tâches', '[{"name":"Titre","type":"text"},{"name":"Fait","type":"boolean"}]')::text`, wb).Scan(&tid); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid) ->> 'schema'`, wb).Scan(&schema); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "Tâches"}.Sanitize()+` (lattis_id, lattis_pos, "Titre") VALUES ('00000000-0000-7000-8000-000000000001', 'a0', 'Préparer')`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.finish_publish($1::uuid)`, wb).Scan(new(int)); err != nil {
			return err
		}
		for u, r := range map[string]string{editor.Email: "editor", part.Email: "participant", reader.Email: "reader"} {
			if _, err := tx.Exec(ctx, `SELECT lattis.invite($1, $2::uuid, ARRAY[$3])`, u, wb, r); err != nil {
				return err
			}
		}
		return nil
	}))
	tbl := pgx.Identifier{schema, "Tâches"}.Sanitize()
	rows := func(u store.Enrollment) int {
		var n int
		must(as(u, func(tx pgx.Tx) error { return tx.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n) }))
		return n
	}
	exec := func(u store.Enrollment, q string, args ...any) (int64, error) {
		var n int64
		err := as(u, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, q, args...)
			n = tag.RowsAffected()
			return err
		})
		return n, err
	}
	var since string
	must(as(reader, func(tx pgx.Tx) error { return tx.QueryRow(ctx, `SELECT lattis.sync_point()::text`).Scan(&since) }))

	// Lecteur : lit, n'écrit pas.
	if rows(reader) != 1 {
		t.Fatal("lecteur : ligne invisible")
	}
	if _, err := exec(reader, `UPDATE `+tbl+` SET "Fait" = true`); pgCode(err) != "42501" {
		t.Fatalf("lecteur qui écrit : %v", err)
	}

	// Participant : ajoute ses lignes, ne touche pas celles des autres.
	if _, err := exec(part, `INSERT INTO `+tbl+` (lattis_id, lattis_pos, "Titre") VALUES ('00000000-0000-7000-8000-000000000002', 'a1', 'Relire')`); err != nil {
		t.Fatal(err)
	}
	if n, err := exec(part, `UPDATE `+tbl+` SET "Fait" = true WHERE lattis_id = '00000000-0000-7000-8000-000000000001'`); err != nil || n != 0 {
		t.Fatalf("participant modifie une ligne d'autrui : %d %v", n, err)
	}
	var why map[string]any
	must(as(part, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.why_not($1::uuid, $2::uuid, '00000000-0000-7000-8000-000000000001')`, wb, tid).Scan(&why)
	}))
	if why["reason"] != "forbidden" {
		t.Fatalf("why_not participant : %v", why)
	}
	if n, err := exec(part, `UPDATE `+tbl+` SET "Fait" = true WHERE lattis_id = '00000000-0000-7000-8000-000000000002'`); err != nil || n != 1 {
		t.Fatalf("participant modifie sa ligne : %d %v", n, err)
	}
	if _, err := exec(part, `UPDATE `+tbl+` SET lattis_author = 'autre' WHERE lattis_id = '00000000-0000-7000-8000-000000000002'`); pgCode(err) != "42501" {
		t.Fatalf("lattis_author modifiable : %v", err)
	}

	// Éditeur : modification conditionnelle à la valeur vue (§10).
	if n, err := exec(editor, `UPDATE `+tbl+` SET "Titre" = 'Préparer la réunion'
		WHERE lattis_id = '00000000-0000-7000-8000-000000000001' AND "Titre" IS NOT DISTINCT FROM 'Préparer'`); err != nil || n != 1 {
		t.Fatalf("éditeur : %d %v", n, err)
	}
	// Second envoi sur la même valeur vue : conflit.
	if n, _ := exec(editor, `UPDATE `+tbl+` SET "Titre" = 'Autre'
		WHERE lattis_id = '00000000-0000-7000-8000-000000000001' AND "Titre" IS NOT DISTINCT FROM 'Préparer'`); n != 0 {
		t.Fatal("conflit non détecté")
	}
	must(as(editor, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.why_not($1::uuid, $2::uuid, '00000000-0000-7000-8000-000000000001')`, wb, tid).Scan(&why)
	}))
	if why["reason"] != "modified" || why["updated_by"] != editor.PGRole || why["row"].(map[string]any)["Titre"] != "Préparer la réunion" {
		t.Fatalf("why_not éditeur : %v", why)
	}
	if n, err := exec(editor, `DELETE FROM `+tbl+` WHERE lattis_id = '00000000-0000-7000-8000-000000000002'`); err != nil || n != 1 {
		t.Fatalf("suppression : %d %v", n, err)
	}
	must(as(editor, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.why_not($1::uuid, $2::uuid, '00000000-0000-7000-8000-000000000002')`, wb, tid).Scan(&why)
	}))
	if why["reason"] != "deleted" || why["deleted_by"] != editor.PGRole {
		t.Fatalf("why_not supprimée : %v", why)
	}

	// Deltas vus par le lecteur depuis son point de reprise.
	var changed, deleted int
	must(as(reader, func(tx pgx.Tx) error {
		r, err := tx.Query(ctx, `SELECT deleted FROM lattis.changes_since($1::uuid, $2::uuid, $3::xid8)`, wb, tid, since)
		if err != nil {
			return err
		}
		for r.Next() {
			var d bool
			if err := r.Scan(&d); err != nil {
				return err
			}
			if d {
				deleted++
			} else {
				changed++
			}
		}
		return r.Err()
	}))
	if changed != 1 || deleted != 1 {
		t.Fatalf("deltas : %d modifiées, %d supprimées", changed, deleted)
	}

	// Un intrus n'a rien.
	stranger := openUsers(t, db, "intrus")[0]
	if err := as(stranger, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT * FROM lattis.changes_since($1::uuid, $2::uuid, '0')`, wb, tid)
		return err
	}); pgCode(err) != "42501" {
		t.Fatalf("intrus : %v", err)
	}

	// Concepteur absent : un éditeur ne change pas la structure.
	if _, err := exec(editor, `SELECT lattis.create_table($1::uuid, 'Autre', '[{"name":"x","type":"text"}]')`, wb); pgCode(err) != "42501" {
		t.Fatalf("éditeur qui crée un tableau : %v", err)
	}
	// Le propriétaire ajoute un tableau à chaud : droits compilés aussitôt.
	if _, err := exec(owner, `SELECT lattis.create_table($1::uuid, 'Notes', '[{"name":"Texte","type":"text"}]')`, wb); err != nil {
		t.Fatal(err)
	}
	if _, err := exec(part, `INSERT INTO `+pgx.Identifier{schema, "Notes"}.Sanitize()+` (lattis_id, "Texte") VALUES (gen_random_uuid(), 'ok')`); err != nil {
		t.Fatalf("participant sur un nouveau tableau : %v", err)
	}
	var caps map[string]any
	must(as(part, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid)`, wb).Scan(&caps)
	}))
	if caps["tables"].(map[string]any)[tid].(map[string]any)["level"] != "participant" || caps["channel"] != schema {
		t.Fatalf("capacités : %v", caps)
	}
}

func TestNotifyOnChange(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	owner := openUsers(t, db, "p")[0]
	var wb, tid, channel string
	if err := db.AsUser(t, owner.PGRole, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.create_workbook('Canal', 'collaborative')::text`).Scan(&wb); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid) ->> 'channel'`, wb).Scan(&channel); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_table($1::uuid, 'T', '[{"name":"v","type":"integer"}]')::text`, wb).Scan(&tid); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT lattis.finish_publish($1::uuid)`, wb).Scan(new(int))
	}); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Super.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `LISTEN `+pgx.Identifier{channel}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if err := db.AsUser(t, owner.PGRole, func(tx pgx.Tx) error {
		tbl := pgx.Identifier{channel, "T"}.Sanitize() // canal = schéma tant qu'il n'y a pas eu de republication
		_, err := tx.Exec(ctx, `INSERT INTO `+tbl+` (lattis_id, v) SELECT gen_random_uuid(), g FROM generate_series(1, 3) g;
			UPDATE `+tbl+` SET v = v + 1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	n, err := conn.Conn().WaitForNotification(ctx)
	if err != nil || n.Payload != tid {
		t.Fatalf("notification : %+v %v", n, err)
	}
}
