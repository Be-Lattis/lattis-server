package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

// Comptes techniques (pipelines, formulaires) : aucune connexion ni
// invitation possible à leur nom, même avec un fournisseur qui affirme
// l'adresse.
func TestTechnicalAccounts(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	var google, tech string
	if err := db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT lattis.set_config('access_mode', '"open"')`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.add_provider('google', 'Google', NULL, 'cid', NULL, NULL)::text`).Scan(&google); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT (lattis.create_user('pipeline+abc@lattis.invalid', 'Pipeline : test', NULL, NULL)).id::text`).Scan(&tech)
	}); err != nil {
		t.Fatal(err)
	}
	// Un fournisseur qui affirme l'adresse technique : nouveau compte à part
	// (entrée libre), jamais le compte technique.
	u, err := db.Store.Enroll(ctx, store.Identity{ProviderID: google, Subject: "x", Email: "pipeline+abc@lattis.invalid", EmailVerified: true})
	if err == nil && u.UserID == tech || err != nil && !strings.Contains(err.Error(), "users_technical_no_login") {
		t.Fatalf("identité reliée au compte technique : %v", err)
	}
	if _, err := db.Super.Exec(ctx, `UPDATE lattis.users SET provider_id = $1, subject = 's' WHERE id = $2`, google, tech); err == nil || !strings.Contains(err.Error(), "users_technical_no_login") {
		t.Fatal("fournisseur relié à un compte technique")
	}
	if _, err := db.Super.Exec(ctx, `INSERT INTO lattis.invitations (email, roles, invited_by, expires_at) VALUES ('form+abc@lattis.invalid', '{}', 'test', now() + interval '1 day')`); err == nil || !strings.Contains(err.Error(), "invitations_not_technical") {
		t.Fatalf("invitation adressée à un compte technique : %v", err)
	}
}
