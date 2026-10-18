package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

// Retours de l'application sur la synchronisation : échos écartés,
// notification des droits, couverture d'un point de reprise.
func TestSyncFeedback(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	us := openUsers(t, db, "proprio", "editeur", "lecteur")
	owner, editor, reader := us[0], us[1], us[2]
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var wb, tid, schema, channel string
	must(db.AsUser(t, owner.PGRole, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.create_workbook('Synchro', 'collaborative')::text`).Scan(&wb); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_table($1::uuid, 'T', '[{"name":"v","type":"integer"}]')::text`, wb).Scan(&tid); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid) ->> 'schema', lattis.my_capabilities($1::uuid) ->> 'channel'`, wb).Scan(&schema, &channel); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.finish_publish($1::uuid)`, wb).Scan(new(int)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT lattis.invite($1, $2::uuid, '{editor}')`, editor.Email, wb)
		return err
	}))
	tbl := pgx.Identifier{schema, "T"}.Sanitize()

	// Échos : l'éditeur écarte sa propre transaction, pas celle d'un autre.
	var mine string
	must(db.AsUser(t, editor.PGRole, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO `+tbl+` (lattis_id, lattis_pos, v) VALUES (gen_random_uuid(), 'a0', 1)`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT pg_current_xact_id()::text`).Scan(&mine)
	}))
	must(db.AsUser(t, owner.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO `+tbl+` (lattis_id, lattis_pos, v) VALUES (gen_random_uuid(), 'a1', 2), (gen_random_uuid(), 'a2', 3)`)
		return err
	}))
	count := func(skip []string) (n int) {
		t.Helper()
		must(db.AsUser(t, editor.PGRole, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM lattis.changes_since($1::uuid, $2::uuid, '0', $3::xid8[])`, wb, tid, skip).Scan(&n)
		}))
		return n
	}
	if n := count(nil); n != 3 {
		t.Fatalf("sans filtre : %d lignes", n)
	}
	if n := count([]string{mine}); n != 2 {
		t.Fatalf("échos écartés : %d lignes", n)
	}

	// Écho qui recouvre l'écriture d'un autre pas encore reçue : la ligne
	// revient. Depuis un point de reprise postérieur à cette écriture : non.
	var since0, mine2 string
	must(db.AsUser(t, editor.PGRole, func(tx pgx.Tx) error { return tx.QueryRow(ctx, `SELECT lattis.sync_point()::text`).Scan(&since0) }))
	must(db.AsUser(t, owner.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE `+tbl+` SET v = 20 WHERE v = 2`)
		return err
	}))
	var since1 string
	must(db.AsUser(t, editor.PGRole, func(tx pgx.Tx) error { return tx.QueryRow(ctx, `SELECT lattis.sync_point()::text`).Scan(&since1) }))
	must(db.AsUser(t, editor.PGRole, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lattis.device = 'poste-1'`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE `+tbl+` SET v = 21 WHERE v = 20`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT pg_current_xact_id()::text`).Scan(&mine2)
	}))
	echoes := func(since string, skip []string) (n int) {
		t.Helper()
		must(db.AsUser(t, editor.PGRole, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM lattis.changes_since($1::uuid, $2::uuid, $3::xid8, $4::xid8[]) WHERE (row_data ->> 'v')::int = 21`,
				wb, tid, since, skip).Scan(&n)
		}))
		return n
	}
	if n := echoes(since0, []string{mine, mine2}); n != 1 {
		t.Fatalf("écriture recouverte masquée : %d", n)
	}
	if n := echoes(since1, []string{mine, mine2}); n != 0 {
		t.Fatalf("écho déjà couvert renvoyé : %d", n)
	}

	// Droits : une invitation notifie « *acl » sur le canal, une seule fois.
	conn, err := db.Super.Acquire(ctx)
	must(err)
	defer conn.Release()
	_, err = conn.Exec(ctx, `LISTEN `+pgx.Identifier{channel}.Sanitize())
	must(err)
	must(db.AsUser(t, owner.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.invite($1, $2::uuid, '{reader}')`, reader.Email, wb)
		return err
	}))
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	n, err := conn.Conn().WaitForNotification(wctx)
	if err != nil || n.Payload != "*acl" {
		t.Fatalf("notification de droits : %+v %v", n, err)
	}
	short, cancel2 := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel2()
	if n, err := conn.Conn().WaitForNotification(short); err == nil {
		t.Fatalf("notification en double : %+v", n)
	}

	// Pierres tombales : un since antérieur à la purge n'est plus couvert.
	var since string
	must(db.AsUser(t, editor.PGRole, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.sync_point()::text`).Scan(&since); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM `+tbl+` WHERE v = 1`)
		return err
	}))
	covered := func(s string) (ok bool) {
		t.Helper()
		must(db.AsUser(t, reader.PGRole, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT lattis.since_covered($1::xid8)`, s).Scan(&ok)
		}))
		return ok
	}
	if !covered(since) || !covered("0") {
		t.Fatal("couvert avant la purge")
	}
	_, err = db.Super.Exec(ctx, `UPDATE lattis.tombstones SET deleted_at = now() - interval '400 days'`)
	must(err)
	must(db.Store.Purge(ctx))
	if covered(since) || !covered("0") {
		t.Fatal("since toujours couvert après la purge")
	}
	var after string
	must(db.AsUser(t, editor.PGRole, func(tx pgx.Tx) error { return tx.QueryRow(ctx, `SELECT lattis.sync_point()::text`).Scan(&after) }))
	if !covered(after) {
		t.Fatal("point de reprise récent non couvert")
	}

	// Transaction longue pendant un rechargement : sync_point reste sous la
	// purge, l'instantané du rechargement dit que tout était déjà pris.
	long, err := db.Super.Begin(ctx)
	must(err)
	defer long.Rollback(ctx)
	_, err = long.Exec(ctx, `SELECT pg_current_xact_id()`)
	must(err)
	must(db.AsUser(t, editor.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM `+tbl+` WHERE v = 3`)
		return err
	}))
	reload := func() (since, snap string) {
		t.Helper()
		must(db.AsUser(t, editor.PGRole, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT lattis.sync_point()::text, pg_current_snapshot()::text`).Scan(&since, &snap)
		}))
		return since, snap
	}
	_, before := reload()
	_, err = db.Super.Exec(ctx, `UPDATE lattis.tombstones SET deleted_at = now() - interval '400 days'`)
	must(err)
	must(db.Store.Purge(ctx))
	since2, snap := reload()
	coveredFrom := func(s, snap string) (ok bool) {
		t.Helper()
		must(db.AsUser(t, reader.PGRole, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT lattis.since_covered($1::xid8, $2::pg_snapshot)`, s, snap).Scan(&ok)
		}))
		return ok
	}
	if covered(since2) {
		t.Fatal("transaction longue : since2 devrait être sous la purge")
	}
	if !coveredFrom(since2, snap) {
		t.Fatal("rechargement postérieur à la purge non couvert")
	}
	if coveredFrom(since2, before) {
		t.Fatal("rechargement antérieur à la purge couvert")
	}
}
