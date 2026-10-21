package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

// Limite d'utilisateurs connectés : une nouvelle personne est refusée
// au-delà ; une personne déjà connectée et les propriétaires entrent.
func TestConnectedUsersLimit(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	var google string
	if err := db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT lattis.set_config('access_mode', '"open"')`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT lattis.set_config('max_connected_users', '1')`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT lattis.add_provider('google', 'Google', NULL, 'cid', NULL, NULL)::text`).Scan(&google)
	}); err != nil {
		t.Fatal(err)
	}
	enroll := func(n string) store.Enrollment {
		u, err := db.Store.Enroll(ctx, store.Identity{ProviderID: google, Subject: n, Email: testpg.Email(n), EmailVerified: true})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	alice, bob := enroll("alice"), enroll("bob")
	if max, _, err := db.Store.ConnectedUsersFull(ctx, bob.PGRole); err != nil || max != 0 {
		t.Fatalf("personne connectée : %d %v", max, err)
	}
	// Un poste connecté (application Lattis) : la place est prise.
	conn, err := db.Super.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET application_name = 'Lattis/0.3.5/test'`); err != nil {
		t.Fatal(err)
	}
	if max, n, err := db.Store.ConnectedUsersFull(ctx, bob.PGRole); err != nil || max != 1 || n < 1 {
		t.Fatalf("serveur complet non vu : %d %d %v", max, n, err)
	}
	// Propriétaire du serveur : entre toujours.
	if _, err := db.Super.Exec(ctx, `GRANT lattis_server_owners TO `+pgx.Identifier{alice.PGRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if max, _, err := db.Store.ConnectedUsersFull(ctx, alice.PGRole); err != nil || max != 0 {
		t.Fatalf("propriétaire refusé : %d %v", max, err)
	}
}
