package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/auth"
	"github.com/Be-Lattis/lattis-server/internal/config"
	"github.com/Be-Lattis/lattis-server/internal/httpserver"
	"github.com/Be-Lattis/lattis-server/internal/metrics"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
	"github.com/Be-Lattis/lattis-server/internal/tokens"
	"github.com/Be-Lattis/lattis-server/internal/upstream"
)

const redirect = "http://127.0.0.1:53682/callback"

type env struct {
	t    *testing.T
	db   *testpg.DB
	keys *tokens.KeySet
	srv  *httptest.Server
	c    *http.Client
}

func newKeys(t *testing.T) *tokens.KeySet {
	pem, err := tokens.GenerateKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	ks, err := tokens.ParseKeys(pem)
	if err != nil {
		t.Fatal(err)
	}
	return ks
}

func setup(t *testing.T) *env {
	db := testpg.New(t)
	keys := newKeys(t)
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	pub, _ := url.Parse(srv.URL)
	cfg := &config.Config{Edition: config.EditionAdmin, PublicURL: pub, PGDatabase: "lattis", PGSSLMode: "verify-full",
		PGPublicRW: config.Endpoint{Host: "pg.example.test", Port: 5432}, RateLimit: 10000}
	m := metrics.New("test", db.Store.DB)
	a := &auth.Service{Config: cfg, Store: db.Store, Keys: keys, Metrics: m,
		Upstream: &upstream.Registry{CallbackURL: func(id string) string { return cfg.URL("/callback/" + id) }}}
	h = (&httpserver.Server{Config: cfg, Store: db.Store, Keys: keys, Auth: a}).Handler()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Host == "127.0.0.1:53682" {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	return &env{t: t, db: db, keys: keys, srv: srv, c: c}
}

var fieldRe = regexp.MustCompile(`name="(req|csrf|token)" value="([^"]*)"`)

func fields(body string) map[string]string {
	out := map[string]string{}
	for _, m := range fieldRe.FindAllStringSubmatch(body, -1) {
		out[m[1]] = strings.ReplaceAll(m[2], "&#43;", "+")
	}
	return out
}

func (e *env) get(u string) (*http.Response, string) {
	e.t.Helper()
	resp, err := e.c.Get(u)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func (e *env) post(u string, form url.Values) (*http.Response, string) {
	e.t.Helper()
	resp, err := e.c.PostForm(u, form)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func pkce() (verifier, challenge string) {
	verifier = tokens.NewID() + tokens.NewID() + "abc"
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func (e *env) authorizeURL(challenge, extra string) string {
	return e.srv.URL + "/oauth/authorize?" + url.Values{
		"client_id": {auth.DesktopClientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"etat-client"},
		"scope": {"openid lattis"}, "nonce": {"n-1"},
	}.Encode() + extra
}

func (e *env) exchange(form url.Values) (int, auth.TokenResponse, map[string]string) {
	e.t.Helper()
	form.Set("client_id", auth.DesktopClientID)
	resp, body := e.post(e.srv.URL+"/oauth/token", form)
	var tr auth.TokenResponse
	var er map[string]string
	_ = json.Unmarshal([]byte(body), &tr)
	_ = json.Unmarshal([]byte(body), &er)
	return resp.StatusCode, tr, er
}

func codeFrom(t *testing.T, resp *http.Response) string {
	t.Helper()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("attendu une redirection vers le client, obtenu %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(loc.String(), redirect) || loc.Query().Get("state") != "etat-client" {
		t.Fatalf("redirection : %s", loc)
	}
	return loc.Query().Get("code")
}

func TestPasswordFlow(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	email := testpg.Email("owner")
	setupToken, err := e.db.Store.Bootstrap(ctx, email)
	if err != nil {
		t.Fatal(err)
	}

	// Choix du mot de passe.
	_, body := e.get(e.srv.URL + "/setup?token=" + setupToken)
	f := fields(body)
	if resp, body := e.post(e.srv.URL+"/setup", url.Values{"token": {f["token"]}, "csrf": {f["csrf"]},
		"password": {"court"}, "confirm": {"court"}}); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "12 caractères") {
		t.Fatalf("mot de passe court accepté : %d", resp.StatusCode)
	}
	pw := "un mot de passe solide"
	if resp, _ := e.post(e.srv.URL+"/setup", url.Values{"token": {f["token"]}, "csrf": {f["csrf"]},
		"password": {pw}, "confirm": {pw}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("setup : %d", resp.StatusCode)
	}

	// Connexion : un seul fournisseur, le formulaire s'affiche directement.
	verifier, challenge := pkce()
	resp, body := e.get(e.authorizeURL(challenge, ""))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `type="password"`) {
		t.Fatalf("formulaire attendu : %d", resp.StatusCode)
	}
	action := resp.Request.URL.Path
	f = fields(body)
	if resp, _ := e.post(e.srv.URL+action, url.Values{"req": {f["req"]}, "csrf": {f["csrf"]}, "email": {email}, "password": {"mauvais mot de passe"}}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("mauvais mot de passe : %d", resp.StatusCode)
	}
	if resp, _ := e.post(e.srv.URL+action, url.Values{"req": {f["req"]}, "csrf": {"faux"}, "email": {email}, "password": {pw}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("CSRF : %d", resp.StatusCode)
	}
	resp, _ = e.post(e.srv.URL+action, url.Values{"req": {f["req"]}, "csrf": {f["csrf"]}, "email": {email}, "password": {pw}})
	code := codeFrom(t, resp)

	// Échange du code : PKCE obligatoire.
	if st, _, er := e.exchange(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}, "code_verifier": {verifier + "x"}}); st != 400 || er["error"] != "invalid_grant" {
		t.Fatalf("PKCE faux : %d %v", st, er)
	}
	// Le code est consommé même après un échec : on recommence.
	resp, body = e.get(e.authorizeURL(challenge, ""))
	f = fields(body)
	resp, _ = e.post(e.srv.URL+resp.Request.URL.Path, url.Values{"req": {f["req"]}, "csrf": {f["csrf"]}, "email": {email}, "password": {pw}})
	code = codeFrom(t, resp)
	st, tr, er := e.exchange(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}, "code_verifier": {verifier}})
	if st != 200 {
		t.Fatalf("échange : %d %v", st, er)
	}
	if tr.PGRole != email || tr.RefreshToken == "" || tr.TokenType != "Bearer" {
		t.Fatalf("réponse : %+v", tr)
	}
	claims, err := e.keys.Verify(tr.AccessToken, tokens.TypeAccess, e.srv.URL, tokens.AudiencePostgres, time.Now())
	if err != nil || claims.PGRole != email || claims.Scope != "openid lattis" {
		t.Fatalf("jeton d'accès : %+v %v", claims, err)
	}
	idc, err := e.keys.Verify(tr.IDToken, tokens.TypeID, e.srv.URL, auth.DesktopClientID, time.Now())
	if err != nil || idc.Nonce != "n-1" || idc.Email != email {
		t.Fatalf("jeton d'identité : %+v %v", idc, err)
	}

	// Rafraîchissement avec rotation, puis réutilisation détectée.
	st, tr2, _ := e.exchange(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tr.RefreshToken}})
	if st != 200 || tr2.RefreshToken == tr.RefreshToken {
		t.Fatalf("rafraîchissement : %d", st)
	}
	if st, _, _ := e.exchange(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tr.RefreshToken}}); st != 400 {
		t.Fatalf("réutilisation : %d", st)
	}
	if st, _, _ := e.exchange(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tr2.RefreshToken}}); st != 400 {
		t.Fatalf("famille non révoquée : %d", st)
	}

	// Verrouillage après échecs répétés.
	for i := 0; i < 5; i++ {
		resp, body = e.get(e.authorizeURL(challenge, ""))
		f = fields(body)
		e.post(e.srv.URL+resp.Request.URL.Path, url.Values{"req": {f["req"]}, "csrf": {f["csrf"]}, "email": {email}, "password": {"encore faux !!"}})
	}
	resp, body = e.get(e.authorizeURL(challenge, ""))
	f = fields(body)
	_, body = e.post(e.srv.URL+resp.Request.URL.Path, url.Values{"req": {f["req"]}, "csrf": {f["csrf"]}, "email": {email}, "password": {pw}})
	if !strings.Contains(body, "verrouillé") {
		t.Fatal("compte non verrouillé")
	}
}

func TestAuthorizeRejectsBadClients(t *testing.T) {
	e := setup(t)
	_, challenge := pkce()
	for name, u := range map[string]string{
		"redirection distante": strings.Replace(e.authorizeURL(challenge, ""), url.QueryEscape(redirect), url.QueryEscape("https://evil.example/cb"), 1),
		"client inconnu":       strings.Replace(e.authorizeURL(challenge, ""), "lattis-desktop", "autre", 1),
	} {
		resp, _ := e.get(u)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s : %d", name, resp.StatusCode)
		}
	}
	// Sans PKCE : erreur renvoyée au client.
	resp, _ := e.get(strings.Replace(e.authorizeURL(challenge, ""), "S256", "plain", 1))
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Query().Get("error") != "invalid_request" {
		t.Fatalf("PKCE plain : %v", loc)
	}
}

func TestDiscovery(t *testing.T) {
	e := setup(t)
	_, body := e.get(e.srv.URL + "/.well-known/lattis.json")
	var d httpserver.Discovery
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatal(err)
	}
	if d.Issuer != e.srv.URL || d.Postgres.RW.Host != "pg.example.test" || len(d.Providers) != 1 || d.Providers[0].Kind != "password" || d.Edition != "admin" {
		t.Fatalf("découverte : %+v", d)
	}
	_, body = e.get(e.srv.URL + "/jwks.json")
	if !strings.Contains(body, `"crv":"Ed25519"`) {
		t.Fatalf("jwks : %s", body)
	}
	resp, _ := e.get(e.srv.URL + "/readyz")
	if resp.StatusCode != 200 {
		t.Fatalf("readyz : %d", resp.StatusCode)
	}
}

// fakeIdP est un fournisseur OIDC minimal (découverte, autorisation, jeton).
type fakeIdP struct {
	srv    *httptest.Server
	keys   *tokens.KeySet
	mu     sync.Mutex
	nonce  string
	claims tokens.Claims
	extra  map[string]any
}

func newFakeIdP(t *testing.T) *fakeIdP {
	f := &fakeIdP{keys: newKeys(t)}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize", "token_endpoint": f.srv.URL + "/token",
			"jwks_uri": f.srv.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"EdDSA"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(f.keys.JWKS()) })
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.nonce = r.URL.Query().Get("nonce")
		f.mu.Unlock()
		u, _ := url.Parse(r.URL.Query().Get("redirect_uri"))
		u.RawQuery = url.Values{"code": {"idp-code"}, "state": {r.URL.Query().Get("state")}}.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		c := f.claims
		c.Issuer, c.Audience, c.Nonce = f.srv.URL, tokens.Audience{"idp-client"}, f.nonce
		c.IssuedAt, c.ExpiresAt = time.Now().Unix(), time.Now().Add(time.Hour).Unix()
		f.mu.Unlock()
		raw, _ := f.keys.Sign(tokens.TypeID, c)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "x", "token_type": "Bearer", "id_token": raw, "expires_in": 3600})
	})
	return f
}

func TestUpstreamOIDCFlow(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	idp := newFakeIdP(t)
	var pid string
	if err := e.db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.add_provider('keycloak', 'Keycloak Acme', $1, 'idp-client', NULL, NULL)::text`, idp.srv.URL).Scan(&pid); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT lattis.allow_tenant($1::uuid, 'realm', $2, 'reader')`, pid, idp.srv.URL); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT lattis.set_config('access_mode', '"open"')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	email := testpg.Email("kc")
	idp.claims = tokens.Claims{Subject: "kc-42", Email: email, Name: "Agent Keycloak"}

	verifier, challenge := pkce()
	resp, _ := e.get(e.authorizeURL(challenge, "&provider=keycloak"))
	code := codeFrom(t, resp)
	st, tr, er := e.exchange(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}, "code_verifier": {verifier}})
	if st != 200 || tr.PGRole != email {
		t.Fatalf("échange : %d %v %+v", st, er, tr)
	}
	var mode string
	_ = e.db.Super.QueryRow(ctx, `SELECT after->>'arrival_role' FROM lattis.audit WHERE action = 'enroll'`).Scan(&mode)
	if mode != "reader" {
		t.Fatalf("rôle par défaut du tenant : %q", mode)
	}

	// Sur invitation, un nouvel arrivant est refusé.
	if err := e.db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.set_config('access_mode', '"invitation"')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	idp.claims = tokens.Claims{Subject: "kc-43", Email: testpg.Email("kc2")}
	resp, body := e.get(e.authorizeURL(challenge, "&provider="+pid))
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "invitation") {
		t.Fatalf("non invité : %d", resp.StatusCode)
	}
}
