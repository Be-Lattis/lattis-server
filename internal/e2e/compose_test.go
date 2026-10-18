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
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/config"
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
	connect := func(e config.Endpoint) *pgx.Conn {
		t.Helper()
		cfg, err := pgx.ParseConfig(fmt.Sprintf("postgres://%s:%d/%s?sslmode=%s",
			e.Host, e.Port, d.Postgres.Database, d.Postgres.SSLMode))
		if err != nil {
			t.Fatal(err)
		}
		cfg.User = tr.PGRole
		cfg.OAuthTokenProvider = func(context.Context) (string, error) { return tr.AccessToken, nil }
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("connexion PostgreSQL %s : %v", e, err)
		}
		t.Cleanup(func() { conn.Close(ctx) })
		return conn
	}
	conn := connect(d.Postgres.RW)
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
	resp, body = read(c.PostForm(base+"/admin/do/add_user", url.Values{"csrf": {field(body, "csrf")},
		"back": {"/admin/users"}, "email": {"invite.e2e@example.com"}, "server_owner": {"on"}}))
	if !strings.Contains(body, "Compte créé pour") {
		t.Fatalf("invitation depuis la console : %d", resp.StatusCode)
	}
	var actor string
	if err := conn.QueryRow(ctx, `SELECT actor FROM lattis.audit WHERE action = 'invite' ORDER BY id DESC LIMIT 1`).Scan(&actor); err != nil || actor != tr.PGRole {
		t.Fatalf("auteur de l'invitation : %q %v", actor, err)
	}

	// Réplicas annoncés : même jeton, lecture seule, deltas lus sur le réplica.
	if len(d.Postgres.RO) == 0 {
		t.Log("aucun réplica annoncé")
		return
	}
	var wb, tid, schema string
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT lattis.create_workbook('Réplica e2e', 'collaborative')::text`).Scan(&wb); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT lattis.create_table($1::uuid, 'Lignes', '[{"name":"Titre","type":"text"}]')::text`, wb).Scan(&tid); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid) ->> 'schema'`, wb).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT lattis.finish_publish($1::uuid)`, wb).Scan(new(int)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tbl := pgx.Identifier{schema, "Lignes"}.Sanitize()
	for _, e := range d.Postgres.RO {
		ro := connect(e)
		var since string
		deadline := time.Now().Add(15 * time.Second)
		// Le classeur arrive par le WAL : attendre qu'il soit rejoué. Une
		// session ouverte pendant ce rejeu peut garder des appartenances aux
		// rôles périmées (cache de PostgreSQL) : sur un refus, le poste ouvre
		// une nouvelle connexion (contrat client).
		for reconnects := 0; ; {
			_, err := ro.Exec(ctx, `SELECT count(*) FROM lattis.changes_since($1::uuid, $2::uuid, '0')`, wb, tid)
			if err == nil {
				if err := ro.QueryRow(ctx, `SELECT lattis.sync_point()::text`).Scan(&since); err != nil {
					t.Fatalf("réplica %s : sync_point : %v", e, err)
				}
				if reconnects > 0 {
					t.Logf("réplica %s : %d reconnexion(s) après refus", e, reconnects)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("réplica %s : classeur jamais rejoué : %v", e, err)
			}
			time.Sleep(200 * time.Millisecond)
			if strings.Contains(err.Error(), "42501") {
				ro.Close(ctx)
				ro = connect(e)
				reconnects++
			}
		}
		if _, err := ro.Exec(ctx, `INSERT INTO `+tbl+` (lattis_id, lattis_pos, "Titre") VALUES (uuidv7(), 'a0', 'refusé')`); err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("réplica %s : écriture acceptée : %v", e, err)
		}
		title := "ligne " + e.String()
		if _, err := conn.Exec(ctx, `INSERT INTO `+tbl+` (lattis_id, lattis_pos, "Titre") VALUES (uuidv7(), 'a0', $1)`, title); err != nil {
			t.Fatal(err)
		}
		for {
			var n int
			if err := ro.QueryRow(ctx, `SELECT count(*) FROM lattis.changes_since($1::uuid, $2::uuid, $3::xid8) WHERE row_data ->> 'Titre' = $4`,
				wb, tid, since, title).Scan(&n); err != nil {
				t.Fatalf("réplica %s : deltas : %v", e, err)
			}
			if n == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("réplica %s : delta jamais vu depuis %s", e, since)
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Logf("réplica %s : connexion OAUTHBEARER et deltas depuis %s", e, since)
	}
}
