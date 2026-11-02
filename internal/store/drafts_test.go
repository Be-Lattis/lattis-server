package store_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

// Brouillons de session : propres à chaque personne, remplacés à chaque
// enregistrement, sous une limite qui laisse le précédent intact.
func TestSessionDrafts(t *testing.T) {
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
		if _, err := tx.Exec(ctx, `SELECT lattis.set_config('draft_quota_mb', '1')`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT lattis.add_provider('google', 'Google', NULL, 'cid', NULL, NULL)::text`).Scan(&google)
	}))
	enroll := func(n string) store.Enrollment {
		u, err := db.Store.Enroll(ctx, store.Identity{ProviderID: google, Subject: n, Email: testpg.Email(n), EmailVerified: true})
		must(err)
		return u
	}
	alice, bob := enroll("alice"), enroll("bob")
	const a, b = "00000000-0000-0000-0000-00000000000a", "00000000-0000-0000-0000-00000000000b"
	save := func(u store.Enrollment, id string, data []byte) error {
		return db.AsUser(t, u.PGRole, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT lattis.draft_save($1::uuid, 'Ventes', $2)`, id, data)
			return err
		})
	}
	read := func(u store.Enrollment, id string) ([]byte, error) {
		var out []byte
		err := db.AsUser(t, u.PGRole, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT lattis.draft_read($1::uuid)`, id).Scan(&out)
		})
		return out, err
	}
	code := func(err error) string {
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			return pe.Code
		}
		return ""
	}

	// Enregistré, remplacé, relu.
	must(save(alice, a, []byte("un")))
	must(save(alice, a, []byte("deux")))
	got, err := read(alice, a)
	must(err)
	if string(got) != "deux" {
		t.Fatalf("relu : %q", got)
	}
	// Le même identifiant chez une autre personne est un autre brouillon, et
	// celui d'Alice lui reste invisible.
	if _, err := read(bob, a); code(err) != "P0002" {
		t.Fatalf("brouillon d'une autre personne : %v", err)
	}
	must(save(bob, a, []byte("bob")))
	if got, _ := read(alice, a); string(got) != "deux" {
		t.Fatalf("brouillon écrasé par une autre personne : %q", got)
	}
	// Pas d'accès direct à la table.
	if err := db.AsUser(t, alice.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT count(*) FROM lattis.session_drafts`)
		return err
	}); code(err) != "42501" {
		t.Fatalf("table lisible : %v", err)
	}
	// Liste : sans le contenu.
	must(db.AsUser(t, alice.PGRole, func(tx pgx.Tx) error {
		var id, title string
		var size int64
		if err := tx.QueryRow(ctx, `SELECT id::text, title, size FROM lattis.draft_list()`).Scan(&id, &title, &size); err != nil {
			return err
		}
		if id != a || title != "Ventes" || size != 4 {
			t.Fatalf("liste : %s %s %d", id, title, size)
		}
		return nil
	}))
	// Limite (1 Mo) : refus nommé, brouillon précédent intact ; remplacer le
	// même brouillon ne compte pas deux fois.
	big := bytes.Repeat([]byte("x"), 700<<10)
	must(save(alice, a, big))
	must(save(alice, a, big))
	err = save(alice, b, big)
	if code(err) != "LT001" || !strings.Contains(err.Error(), "limite de 1 Mo") {
		t.Fatalf("limite : %v", err)
	}
	if got, _ := read(alice, a); len(got) != len(big) {
		t.Fatalf("brouillon après un refus : %d", len(got))
	}
	// Retrait, sans erreur la seconde fois ; la place est rendue.
	for i := 0; i < 2; i++ {
		must(db.AsUser(t, alice.PGRole, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT lattis.draft_delete($1::uuid)`, a)
			return err
		}))
	}
	must(save(alice, b, big))
	// Le contenu n'est pas recompressé par PostgreSQL.
	var storage string
	must(db.Super.QueryRow(ctx, `SELECT attstorage::text FROM pg_attribute WHERE attrelid = 'lattis.session_drafts'::regclass AND attname = 'data'`).Scan(&storage))
	if storage != "e" {
		t.Fatalf("stockage : %q", storage)
	}
}
