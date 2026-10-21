package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

// Même adresse chez plusieurs fournisseurs : Google et Microsoft (adresse
// vérifiée) se relient au compte existant ; les autres, ou une adresse non
// vérifiée, sont refusés avec le moyen de connexion d'origine.
func TestLinkedIdentities(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	var oidc, google, ms string
	if err := db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT lattis.set_config('access_mode', '"open"')`); err != nil {
			return err
		}
		for kind, dst := range map[string]*string{"oidc": &oidc, "google": &google, "microsoft": &ms} {
			if err := tx.QueryRow(ctx, `SELECT lattis.add_provider($1, $2, 'https://idp.example', 'cid', NULL, NULL)::text`, kind, "Fournisseur "+kind).Scan(dst); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	email := testpg.Email("claire")
	enroll := func(p, sub string, verified bool) store.Enrollment {
		t.Helper()
		e, err := db.Store.Enroll(ctx, store.Identity{ProviderID: p, Subject: sub, Email: email, EmailVerified: verified})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	first := enroll(oidc, "o1", true)
	if first.Status != "ok" {
		t.Fatalf("premier compte : %+v", first)
	}
	// Google, adresse vérifiée : même compte, sans en créer un second.
	g := enroll(google, "g1", true)
	if g.Status != "ok" || g.UserID != first.UserID || g.Mode != "linked" {
		t.Fatalf("liaison Google : %+v", g)
	}
	if again := enroll(google, "g1", true); again.UserID != first.UserID || again.Mode != "existing" {
		t.Fatalf("reconnexion Google : %+v", again)
	}
	// Microsoft sans adresse garantie : refus nommé.
	if m := enroll(ms, "m1", false); m.Status != "email_taken" || m.Provider != "Fournisseur oidc" {
		t.Fatalf("Microsoft non vérifié : %+v", m)
	}
	// Autre fournisseur générique : refus, même si l'adresse se dit vérifiée.
	var other string
	if err := db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.add_provider('keycloak', 'Keycloak', 'https://kc.example', 'cid', NULL, NULL)::text`).Scan(&other)
	}); err != nil {
		t.Fatal(err)
	}
	if k := enroll(other, "k1", true); k.Status != "email_taken" {
		t.Fatalf("fournisseur générique : %+v", k)
	}
	var n int
	if err := db.Super.QueryRow(ctx, `SELECT count(*) FROM lattis.users WHERE email = $1`, email).Scan(&n); err != nil || n != 1 {
		t.Fatalf("comptes pour l'adresse : %d %v", n, err)
	}
}
