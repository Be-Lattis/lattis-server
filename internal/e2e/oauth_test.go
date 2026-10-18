// Package e2e vérifie de bout en bout la connexion OAUTHBEARER à un
// PostgreSQL 18 équipé du module lattis_oauth (image be-lattis/server).
//
// Variables : LATTIS_E2E_PG_URL (lattis_service, TLS), LATTIS_E2E_KEY_FILE
// (clé de signature dont la clé publique est chargée par le serveur),
// LATTIS_E2E_ISSUER (lattis_oauth.issuer du serveur).
package e2e

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/migrate"
	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/tokens"
)

func TestOAuthBearerLogin(t *testing.T) {
	svcURL, keyFile, issuer := os.Getenv("LATTIS_E2E_PG_URL"), os.Getenv("LATTIS_E2E_KEY_FILE"), os.Getenv("LATTIS_E2E_ISSUER")
	if svcURL == "" || keyFile == "" || issuer == "" {
		t.Skip("LATTIS_E2E_PG_URL, LATTIS_E2E_KEY_FILE et LATTIS_E2E_ISSUER requis")
	}
	ctx := context.Background()
	keys, err := tokens.LoadKeyFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, svcURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := migrate.Up(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	var google string
	if err := st.AsActor(ctx, "e2e", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT lattis.set_config('access_mode', '"open"')`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT id::text FROM lattis.identity_providers WHERE kind = 'google'
			UNION ALL SELECT lattis.add_provider('google', 'Google', NULL, 'cid', NULL, NULL)::text LIMIT 1`).Scan(&google)
	}); err != nil {
		t.Fatal(err)
	}
	email := "e2e." + strings.ToLower(tokens.NewID()[:8]) + "@example.test"
	e, err := st.Enroll(ctx, store.Identity{ProviderID: google, Subject: "e2e-" + email, Email: email, EmailVerified: true})
	if err != nil || e.Status != "ok" {
		t.Fatalf("inscription : %+v %v", e, err)
	}

	sign := func(mut func(*tokens.Claims), ks *tokens.KeySet) string {
		now := time.Now()
		c := tokens.Claims{Issuer: issuer, Subject: e.UserID, Audience: tokens.Audience{tokens.AudiencePostgres},
			IssuedAt: now.Unix(), NotBefore: now.Unix(), ExpiresAt: now.Add(5 * time.Minute).Unix(),
			Scope: "openid lattis", PGRole: e.PGRole}
		if mut != nil {
			mut(&c)
		}
		tok, err := ks.Sign(tokens.TypeAccess, c)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	connect := func(role, token string) (*pgx.Conn, error) {
		u, _ := url.Parse(svcURL)
		u.User = url.User(role)
		cfg, err := pgx.ParseConfig(u.String())
		if err != nil {
			t.Fatal(err)
		}
		cfg.OAuthTokenProvider = func(context.Context) (string, error) { return token, nil }
		return pgx.ConnectConfig(ctx, cfg)
	}

	conn, err := connect(e.PGRole, sign(nil, keys))
	if err != nil {
		t.Fatalf("connexion OAUTHBEARER : %v", err)
	}
	var sysUser, cur string
	if err := conn.QueryRow(ctx, `SELECT system_user, current_user`).Scan(&sysUser, &cur); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	if sysUser != "oauth:lattis:"+e.UserID || cur != e.PGRole {
		t.Fatalf("identité : system_user=%q current_user=%q", sysUser, cur)
	}

	otherPEM, _ := tokens.GenerateKeyPEM()
	other, _ := tokens.ParseKeys(otherPEM)
	for name, tok := range map[string]string{
		"autre rôle":     sign(func(c *tokens.Claims) { c.PGRole = "lattis_service" }, keys),
		"expiré":         sign(func(c *tokens.Claims) { c.ExpiresAt = time.Now().Add(-10 * time.Minute).Unix() }, keys),
		"autre émetteur": sign(func(c *tokens.Claims) { c.Issuer = "https://evil.example" }, keys),
		"sans scope":     sign(func(c *tokens.Claims) { c.Scope = "openid" }, keys),
		"autre audience": sign(func(c *tokens.Claims) { c.Audience = tokens.Audience{"lattis-desktop"} }, keys),
		"autre clé":      sign(nil, other),
		"pas un JWT":     "abc",
	} {
		conn, err := connect(e.PGRole, tok)
		if err == nil {
			conn.Close(ctx)
			t.Errorf("%s : connexion acceptée", name)
			continue
		}
		if !strings.Contains(err.Error(), "OAuth") && !strings.Contains(err.Error(), "authentication") {
			t.Errorf("%s : erreur inattendue %v", name, err)
		}
	}

	// Compte désactivé : NOLOGIN, même avec un jeton valide.
	if err := st.AsActor(ctx, "e2e", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.disable_user($1::uuid)`, e.UserID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if conn, err := connect(e.PGRole, sign(nil, keys)); err == nil {
		conn.Close(ctx)
		t.Fatal("compte désactivé accepté")
	} else if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
