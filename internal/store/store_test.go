package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Be-Lattis/lattis-server/internal/migrate"
	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

func passwordProvider(t *testing.T, st *store.Store) string {
	t.Helper()
	var id string
	if err := st.DB.QueryRow(context.Background(),
		`SELECT id::text FROM lattis.identity_providers WHERE kind = 'password'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func canLogin(t *testing.T, db *testpg.DB, role string) bool {
	t.Helper()
	var ok bool
	if err := db.Super.QueryRow(context.Background(), `SELECT rolcanlogin FROM pg_roles WHERE rolname = $1`, role).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func isOwner(t *testing.T, db *testpg.DB, role string) bool {
	t.Helper()
	var ok bool
	if err := db.Super.QueryRow(context.Background(),
		`SELECT pg_has_role($1, 'lattis_server_owners', 'MEMBER')`, role).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestMigrationsIdempotent(t *testing.T) {
	db := testpg.New(t)
	n, err := migrate.Up(context.Background(), db.Store.DB)
	if err != nil || n != 0 {
		t.Fatalf("deuxième passage : %d migrations, %v", n, err)
	}
	v, err := migrate.Current(context.Background(), db.Store.DB)
	if err != nil || v != migrate.Latest() {
		t.Fatalf("version %d, attendu %d (%v)", v, migrate.Latest(), err)
	}
}

// La personne qui tient le lien choisit son adresse : le compte, son rôle et
// l'invitation de propriétaire la suivent.
func TestSetupChangesEmail(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	st := db.Store
	token, err := st.Bootstrap(ctx, testpg.Email("planned"))
	if err != nil {
		t.Fatal(err)
	}
	chosen := testpg.Email("chosen")
	uid, err := st.SetPasswordWithToken(ctx, token, "$argon2id$fake", "  "+strings.ToUpper(chosen)+" ")
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.User(ctx, uid)
	if err != nil || u.Email != chosen || u.PGRole != chosen {
		t.Fatalf("compte : %+v %v", u, err)
	}
	acct, err := st.PasswordAccount(ctx, chosen)
	if err != nil || acct.UserID != uid {
		t.Fatalf("compte à mot de passe : %+v %v", acct, err)
	}
	e, err := st.Enroll(ctx, store.Identity{ProviderID: acct.ProviderID, Subject: uid, Email: chosen, EmailVerified: true})
	if err != nil || e.Status != "ok" || !isOwner(t, db, e.PGRole) {
		t.Fatalf("inscription : %+v %v", e, err)
	}
	var verified bool
	if err := db.Super.QueryRow(ctx, `SELECT email_verified FROM lattis.users WHERE id = $1`, uid).Scan(&verified); err != nil || verified {
		t.Fatalf("adresse choisie marquée vérifiée : %v %v", verified, err)
	}
	// Une adresse déjà portée par un compte est refusée.
	var token2 string
	if err := st.AsActor(ctx, "test", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.create_password_account($1, '')`, testpg.Email("autre")).Scan(&token2)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetPasswordWithToken(ctx, token2, "$argon2id$fake", chosen); pgCode(err) != "23505" {
		t.Fatalf("adresse déjà prise acceptée : %v", err)
	}
	// Le dernier moyen de connexion actif ne se désactive pas.
	if err := st.AsActor(ctx, "test", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE lattis.identity_providers SET enabled = false WHERE kind = 'password'`)
		return err
	}); pgCode(err) != "55000" {
		t.Fatalf("dernier fournisseur désactivé : %v", err)
	}
}

func TestBootstrapOwnerWithPassword(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	st := db.Store
	email := testpg.Email("owner")

	token, err := st.Bootstrap(ctx, email)
	if err != nil || token == "" {
		t.Fatalf("amorçage : %q, %v", token, err)
	}
	// Une seconde fois : rien à faire, l'invitation est en attente.
	if again, err := st.Bootstrap(ctx, email); err != nil || again != "" {
		t.Fatalf("second amorçage : %q, %v", again, err)
	}
	if got, ok, err := st.SetupTokenValid(ctx, token); err != nil || !ok || got != email {
		t.Fatalf("jeton : %q %v %v", got, ok, err)
	}
	uid, err := st.SetPasswordWithToken(ctx, token, "$argon2id$fake", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetPasswordWithToken(ctx, token, "$argon2id$fake", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("jeton réutilisé : %v", err)
	}
	acct, err := st.PasswordAccount(ctx, email)
	if err != nil || acct.UserID != uid || acct.Hash != "$argon2id$fake" {
		t.Fatalf("compte : %+v %v", acct, err)
	}
	u, _ := st.User(ctx, uid)
	if canLogin(t, db, u.PGRole) {
		t.Fatal("le rôle ne doit pas pouvoir se connecter avant la première connexion")
	}

	e, err := st.Enroll(ctx, store.Identity{ProviderID: acct.ProviderID, Subject: uid, Email: email, EmailVerified: true})
	if err != nil || e.Status != "ok" || e.PGRole != email {
		t.Fatalf("inscription : %+v %v", e, err)
	}
	if !canLogin(t, db, e.PGRole) || !isOwner(t, db, e.PGRole) {
		t.Fatal("le premier propriétaire doit pouvoir se connecter et être propriétaire du serveur")
	}
	// Plus d'amorçage une fois un propriétaire en place.
	if again, err := st.Bootstrap(ctx, testpg.Email("other")); err != nil || again != "" {
		t.Fatalf("amorçage après propriétaire : %q, %v", again, err)
	}
}

func TestEnrollAccessModes(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	st := db.Store
	var google string
	err := st.AsActor(ctx, "test", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.add_provider('google', 'Google', NULL, 'cid', NULL, NULL)::text`).Scan(&google)
	})
	if err != nil {
		t.Fatal(err)
	}

	stranger := store.Identity{ProviderID: google, Subject: "g-1", Email: testpg.Email("stranger"), EmailVerified: true}
	if e, err := st.Enroll(ctx, stranger); err != nil || e.Status != "not_invited" {
		t.Fatalf("sur invitation, inconnu : %+v %v", e, err)
	}

	// Invitation : le compte existe dès l'invitation, relié à la première connexion.
	invited := store.Identity{ProviderID: google, Subject: "g-2", Email: testpg.Email("invited"), EmailVerified: true, DisplayName: "Invitée"}
	if err := st.AsActor(ctx, "test", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.invite($1, NULL, '{}')`, invited.Email)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	unverified := invited
	unverified.EmailVerified = false
	if e, _ := st.Enroll(ctx, unverified); e.Status != "not_invited" {
		t.Fatalf("adresse non vérifiée acceptée : %+v", e)
	}
	e, err := st.Enroll(ctx, invited)
	if err != nil || e.Status != "ok" || e.Mode != "invitation" || e.DisplayName != "Invitée" {
		t.Fatalf("invitée : %+v %v", e, err)
	}
	again, err := st.Enroll(ctx, invited)
	if err != nil || again.UserID != e.UserID || again.Mode != "existing" {
		t.Fatalf("reconnexion : %+v %v", again, err)
	}

	// Entrée libre.
	if err := st.AsActor(ctx, "test", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.set_config('access_mode', '"open"')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if e, err := st.Enroll(ctx, stranger); err != nil || e.Status != "ok" || e.Mode != "open" || !canLogin(t, db, e.PGRole) {
		t.Fatalf("entrée libre : %+v %v", e, err)
	}

	// Désactivation : plus de connexion, plus de jeton.
	if err := st.AsActor(ctx, "test", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.disable_user($1::uuid)`, e.UserID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if d, _ := st.Enroll(ctx, invited); d.Status != "disabled" {
		t.Fatalf("désactivé : %+v", d)
	}
	if canLogin(t, db, e.PGRole) {
		t.Fatal("rôle désactivé encore LOGIN")
	}
	var n int
	_ = db.Super.QueryRow(ctx, `SELECT count(*) FROM lattis.audit WHERE action IN ('enroll', 'disable_user', 'set_config', 'invite')`).Scan(&n)
	if n < 4 {
		t.Fatalf("journal incomplet : %d lignes", n)
	}
}

func TestOwnerOnlyFunctions(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	st := db.Store
	var google string
	if err := st.AsActor(ctx, "test", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.add_provider('google', 'Google', NULL, 'cid', NULL, NULL)::text`).Scan(&google)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AsActor(ctx, "test", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.set_config('access_mode', '"open"')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	user, err := st.Enroll(ctx, store.Identity{ProviderID: google, Subject: "u", Email: testpg.Email("user"), EmailVerified: true})
	if err != nil || user.Status != "ok" {
		t.Fatal(user, err)
	}

	// Un utilisateur ordinaire ne lit pas le catalogue et n'administre rien.
	for _, q := range []string{
		`SELECT lattis.set_config('access_mode', '"invitation"')`,
		`SELECT lattis.invite('x@example.test', NULL, '{server_owner}')`,
		`SELECT count(*) FROM lattis.users`,
		`SELECT count(*) FROM lattis.local_credentials`,
		`SELECT lattis.enroll(NULL, 's', 'e@x.y', true, '', NULL)`,
	} {
		err := db.AsUser(t, user.PGRole, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err })
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "42501" {
			t.Errorf("%s : attendu insufficient_privilege, obtenu %v", q, err)
		}
	}
	// Il voit son propre résumé.
	var summary map[string]any
	if err := db.AsUser(t, user.PGRole, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.login_summary()`).Scan(&summary)
	}); err != nil {
		t.Fatal(err)
	}
	if summary["server_owner"] != false || summary["user"].(map[string]any)["pg_role"] != user.PGRole {
		t.Fatalf("résumé : %v", summary)
	}

	// Promu propriétaire par invitation : les rôles s'appliquent tout de suite.
	if err := st.AsActor(ctx, "test", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.invite($1, NULL, '{server_owner}')`, user.Email)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !isOwner(t, db, user.PGRole) {
		t.Fatal("invitation server_owner non appliquée")
	}
	err = db.AsUser(t, user.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.set_config('display_name', '"Serveur Acme"')`)
		return err
	})
	if err != nil {
		t.Fatalf("propriétaire : %v", err)
	}
	var actor string
	_ = db.Super.QueryRow(ctx, `SELECT actor FROM lattis.audit WHERE action = 'set_config' ORDER BY id DESC LIMIT 1`).Scan(&actor)
	if actor != user.PGRole {
		t.Fatalf("auteur journalisé : %q", actor)
	}
	// Valeurs refusées.
	err = db.AsUser(t, user.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.set_config('access_mode', '"tout"')`)
		return err
	})
	if err == nil {
		t.Fatal("access_mode invalide accepté")
	}
}

func TestRevokeInvitationDropsUnusedRole(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	email := testpg.Email("revoked")
	var id string
	if err := db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.invite($1, NULL, '{}')::text`, email).Scan(&id)
	}); err != nil {
		t.Fatal(err)
	}
	var exists bool
	_ = db.Super.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, email).Scan(&exists)
	if !exists {
		t.Fatal("rôle non créé à l'invitation")
	}
	if err := db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.revoke_invitation($1::uuid)`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_ = db.Super.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, email).Scan(&exists)
	if exists {
		t.Fatal("rôle inutilisé non supprimé")
	}
}

func TestCodesAndRefreshTokens(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	st := db.Store
	tok, _ := st.Bootstrap(ctx, testpg.Email("o"))
	uid, err := st.SetPasswordWithToken(ctx, tok, "h", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := st.SaveCode(ctx, "code-1", store.Code{UserID: uid, ClientID: "c", RedirectURI: "r", CodeChallenge: "x", Scope: "openid lattis"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if c, err := st.ConsumeCode(ctx, "code-1"); err != nil || c.UserID != uid {
		t.Fatalf("code : %+v %v", c, err)
	}
	if err := st.SaveRefresh(ctx, "rt-1", store.Refresh{UserID: uid, ClientID: "c", Scope: "s"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	// Le code rejoué est refusé et révoque les jetons émis depuis.
	if _, err := st.ConsumeCode(ctx, "code-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("code rejoué : %v", err)
	}
	if _, err := st.UseRefresh(ctx, "rt-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("jeton après code rejoué : %v", err)
	}

	// Rotation puis réutilisation : toute la famille tombe.
	if err := st.SaveRefresh(ctx, "rt-2", store.Refresh{UserID: uid, ClientID: "c", Scope: "s"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	r2, err := st.UseRefresh(ctx, "rt-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRefresh(ctx, "rt-3", store.Refresh{Family: r2.Family, UserID: uid, ClientID: "c", Scope: "s"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UseRefresh(ctx, "rt-2"); !errors.Is(err, store.ErrReused) {
		t.Fatalf("réutilisation : %v", err)
	}
	if _, err := st.UseRefresh(ctx, "rt-3"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("famille non révoquée : %v", err)
	}
}
