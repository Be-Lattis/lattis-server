package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/httpserver"
)

// TestComposeLogin rejoue la connexion d'un poste contre un Serveur Lattis
// lancé (Docker Compose) : découverte, mot de passe du premier propriétaire,
// PKCE, jeton, puis connexion PostgreSQL en OAUTHBEARER.
//
// Variables : LATTIS_E2E_LATTISD_URL, LATTIS_E2E_SETUP_TOKEN (lien affiché
// au premier démarrage) et LATTIS_E2E_OWNER (LATTIS_BOOTSTRAP_OWNER).
func TestComposeLogin(t *testing.T) {
	base, setup, owner := os.Getenv("LATTIS_E2E_LATTISD_URL"), os.Getenv("LATTIS_E2E_SETUP_TOKEN"), os.Getenv("LATTIS_E2E_OWNER")
	if base == "" || setup == "" || owner == "" {
		t.Skip("LATTIS_E2E_LATTISD_URL, LATTIS_E2E_SETUP_TOKEN et LATTIS_E2E_OWNER requis")
	}
	ctx := context.Background()
	jar, _ := cookiejar.New(nil)
	const redirect = "http://127.0.0.1:53682/callback"
	c := &http.Client{Jar: jar, CheckRedirect: func(r *http.Request, _ []*http.Request) error {
		if r.URL.Host == "127.0.0.1:53682" {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	read := func(resp *http.Response, err error) (*http.Response, string) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}
	field := func(body, name string) string {
		m := regexp.MustCompile(`name="` + name + `" value="([^"]*)"`).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("champ %s absent", name)
		}
		return m[1]
	}

	_, body := read(c.Get(base + "/.well-known/lattis.json"))
	var d httpserver.Discovery
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatal(err)
	}

	pw := "mot de passe de démonstration"
	// Lien déjà utilisé lors d'un passage précédent : le mot de passe est en place.
	if resp, body := read(c.Get(base + "/setup?token=" + setup)); resp.StatusCode == 200 {
		if resp, _ := read(c.PostForm(base+"/setup", url.Values{"token": {setup}, "csrf": {field(body, "csrf")},
			"password": {pw}, "confirm": {pw}})); resp.StatusCode != 200 {
			t.Fatalf("setup : %d", resp.StatusCode)
		}
	}

	verifier := strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	resp, body := read(c.Get(d.AuthorizationURL + "?" + url.Values{
		"client_id": {d.ClientID}, "redirect_uri": {redirect}, "response_type": {"code"}, "scope": {d.Scope},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "state": {"s"},
	}.Encode()))
	resp, _ = read(c.PostForm(base+resp.Request.URL.Path, url.Values{"req": {field(body, "req")}, "csrf": {field(body, "csrf")},
		"email": {owner}, "password": {pw}}))
	loc, _ := url.Parse(resp.Header.Get("Location"))
	resp, body = read(c.PostForm(d.TokenURL, url.Values{"grant_type": {"authorization_code"}, "client_id": {d.ClientID},
		"code": {loc.Query().Get("code")}, "redirect_uri": {redirect}, "code_verifier": {verifier}}))
	var tr struct {
		AccessToken string `json:"access_token"`
		PGRole      string `json:"pg_role"`
	}
	if err := json.Unmarshal([]byte(body), &tr); err != nil || tr.AccessToken == "" {
		t.Fatalf("jeton : %d %s", resp.StatusCode, body)
	}

	// Le rôle contient « @ » : il se pose dans la configuration, pas dans l'URL.
	cfg, err := pgx.ParseConfig(fmt.Sprintf("postgres://%s:%d/%s?sslmode=%s",
		d.Postgres.RW.Host, d.Postgres.RW.Port, d.Postgres.Database, d.Postgres.SSLMode))
	if err != nil {
		t.Fatal(err)
	}
	cfg.User = tr.PGRole
	cfg.OAuthTokenProvider = func(context.Context) (string, error) { return tr.AccessToken, nil }
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connexion PostgreSQL : %v", err)
	}
	defer conn.Close(ctx)
	var summary map[string]any
	if err := conn.QueryRow(ctx, `SELECT lattis.login_summary()`).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if summary["server_owner"] != true {
		t.Fatalf("résumé : %v", summary)
	}
	// Propriétaire du serveur : il administre en SQL sous sa propre identité.
	if _, err := conn.Exec(ctx, `SELECT lattis.set_config('display_name', '"Serveur Acme"')`); err != nil {
		t.Fatal(err)
	}
	t.Logf("connecté en %s, résumé %v", tr.PGRole, summary)

	// Console d'administration : lattisd ouvre lui-même une connexion
	// OAUTHBEARER sous l'identité du propriétaire.
	resp, body = read(c.Get(base + "/admin/status"))
	if strings.Contains(body, `type="password"`) {
		resp, body = read(c.PostForm(base+resp.Request.URL.RequestURI(), url.Values{"req": {field(body, "req")},
			"csrf": {field(body, "csrf")}, "email": {owner}, "password": {pw}}))
	}
	if resp.StatusCode != 200 || resp.Request.URL.Path != "/admin/status" || !strings.Contains(body, "Serveur Acme") {
		t.Fatalf("console : %d %s\n%s", resp.StatusCode, resp.Request.URL, body)
	}
	resp, body = read(c.PostForm(base+"/admin/do/invite_server", url.Values{"csrf": {field(body, "csrf")},
		"back": {"/admin/users"}, "email": {"invite.e2e@example.com"}}))
	if !strings.Contains(body, "Invitation enregistrée") {
		t.Fatalf("invitation depuis la console : %d", resp.StatusCode)
	}
	var actor string
	if err := conn.QueryRow(ctx, `SELECT actor FROM lattis.audit WHERE action = 'invite' ORDER BY id DESC LIMIT 1`).Scan(&actor); err != nil || actor != tr.PGRole {
		t.Fatalf("auteur de l'invitation : %q %v", actor, err)
	}
}
