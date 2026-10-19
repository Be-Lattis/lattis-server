package admin_test

import (
	"context"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/admin"
	"github.com/Be-Lattis/lattis-server/internal/auth"
	"github.com/Be-Lattis/lattis-server/internal/config"
	"github.com/Be-Lattis/lattis-server/internal/httpserver"
	"github.com/Be-Lattis/lattis-server/internal/metrics"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
	"github.com/Be-Lattis/lattis-server/internal/tokens"
	"github.com/Be-Lattis/lattis-server/internal/upstream"
	"github.com/Be-Lattis/lattis-server/internal/userdb"
)

type env struct {
	t   *testing.T
	db  *testpg.DB
	srv *httptest.Server
}

func setup(t *testing.T) *env {
	db := testpg.New(t)
	pem, err := tokens.GenerateKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := tokens.ParseKeys(pem)
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	pub, _ := url.Parse(srv.URL)
	cfg := &config.Config{Edition: config.EditionAdmin, PublicURL: pub, PGDatabase: "lattis", PGSSLMode: "verify-full",
		PGPublicRW: config.Endpoint{Host: "pg.example.test", Port: 5432}, RateLimit: 10000}
	a := &auth.Service{Config: cfg, Store: db.Store, Keys: keys, Metrics: metrics.New("test", db.Store.DB),
		Upstream: &upstream.Registry{CallbackURL: func(id string) string { return cfg.URL("/callback/" + id) }},
		AdminURL: "/admin/"}
	ad := &admin.Service{Config: cfg, Store: db.Store, Keys: keys, Users: userdb.SessionAuth{Pool: db.Super}, Version: "test"}
	h = (&httpserver.Server{Config: cfg, Store: db.Store, Keys: keys, Auth: a, Admin: ad}).Handler()
	return &env{t: t, db: db, srv: srv}
}

// browser est un navigateur avec ses propres cookies.
type browser struct {
	e *env
	c *http.Client
}

func (e *env) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{e: e, c: &http.Client{Jar: jar}}
}

func (b *browser) get(path string) (*http.Response, string) {
	b.e.t.Helper()
	resp, err := b.c.Get(b.e.srv.URL + path)
	if err != nil {
		b.e.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// LATTIS_ADMIN_DUMP=<dossier> : garde les pages pour les relire à l'œil.
	if dir := os.Getenv("LATTIS_ADMIN_DUMP"); dir != "" && resp.StatusCode == http.StatusOK {
		name := strings.NewReplacer("/", "_", "?", "_", "&", "_", "=", "_").Replace(strings.TrimPrefix(resp.Request.URL.Path, "/"))
		_ = os.WriteFile(filepath.Join(dir, name+".html"), body, 0o644)
	}
	return resp, string(body)
}

func (b *browser) post(path string, form url.Values) (*http.Response, string) {
	b.e.t.Helper()
	resp, err := b.c.PostForm(b.e.srv.URL+path, form)
	if err != nil {
		b.e.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

var fieldRe = regexp.MustCompile(`name="(req|csrf|token)" value="([^"]*)"`)

func field(body, name string) string {
	for _, m := range fieldRe.FindAllStringSubmatch(body, -1) {
		if m[1] == name {
			return html.UnescapeString(m[2])
		}
	}
	return ""
}

// account crée un compte à mot de passe prêt à servir.
func (e *env) account(token, pw string) {
	b := e.browser()
	_, body := b.get("/setup?token=" + token)
	if resp, _ := b.post("/setup", url.Values{"token": {field(body, "token")}, "csrf": {field(body, "csrf")},
		"password": {pw}, "confirm": {pw}}); resp.StatusCode != http.StatusOK {
		e.t.Fatalf("setup : %d", resp.StatusCode)
	}
}

// login ouvre l'administration : redirection vers la connexion, formulaire,
// puis retour sur la page demandée.
func (b *browser) login(email, pw, path string) (*http.Response, string) {
	b.e.t.Helper()
	resp, body := b.get(path)
	if !strings.Contains(body, `type="password"`) {
		b.e.t.Fatalf("formulaire de connexion attendu : %d", resp.StatusCode)
	}
	return b.post(resp.Request.URL.RequestURI(), url.Values{"req": {field(body, "req")}, "csrf": {field(body, "csrf")},
		"email": {email}, "password": {pw}})
}

const pw = "un mot de passe solide"

// Le propriétaire qui vient de choisir son mot de passe arrive connecté dans
// l'administration, sans repasser par le formulaire de connexion.
func TestSetupSignsOwnerIn(t *testing.T) {
	e := setup(t)
	token, err := e.db.Store.Bootstrap(context.Background(), testpg.Email("first"))
	if err != nil {
		t.Fatal(err)
	}
	b := e.browser()
	// Session d'une installation précédente, sous l'ancien chemin /admin et
	// signée d'une autre clé : elle ne doit pas faire tourner en rond.
	srvURL, _ := url.Parse(e.srv.URL + "/admin/")
	b.c.Jar.SetCookies(srvURL, []*http.Cookie{{Name: "lattis_admin", Value: "eyJwZXJpbWUiOnRydWV9.perime.perime", Path: "/admin"}})
	_, body := b.get("/setup?token=" + token)
	// L'adresse prévue n'est qu'une suggestion : on en choisit une autre.
	chosen := testpg.Email("chosen")
	resp, body := b.post("/setup", url.Values{"token": {field(body, "token")}, "csrf": {field(body, "csrf")},
		"email": {chosen}, "password": {pw}, "confirm": {pw}})
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/admin/server" || !strings.Contains(body, "Nom du serveur") {
		t.Fatalf("après le choix du mot de passe : %d %s", resp.StatusCode, resp.Request.URL)
	}
	if !strings.Contains(body, chosen) {
		t.Fatalf("connecté sous l'adresse choisie attendu : %s", chosen)
	}
	// Une nouvelle connexion (mot de passe) mène aussi à la console.
	b.c.Jar.SetCookies(resp.Request.URL, []*http.Cookie{{Name: "lattis_admin", Value: "", MaxAge: -1, Path: "/"}})
	b.c.Jar.SetCookies(srvURL, []*http.Cookie{{Name: "lattis_admin", Value: "eyJwZXJpbWUiOnRydWV9.perime.perime", Path: "/admin"}})
	if resp, body := b.login(chosen, pw, "/admin/server"); resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/admin/server" || !strings.Contains(body, "Nom du serveur") {
		t.Fatalf("connexion avec une ancienne session : %d %s", resp.StatusCode, resp.Request.URL)
	}
	// Accueil à la racine, puis déconnexion depuis une page du site.
	if _, body := b.get("/"); !strings.Contains(body, "Adresse du serveur") {
		t.Fatal("page d'accueil attendue à la racine")
	}
	req, _ := http.NewRequest("POST", e.srv.URL+"/logout", strings.NewReader("next=%2Fadmin%2Fserver"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.srv.URL)
	if resp, err := b.c.Do(req); err != nil || !strings.Contains(resp.Request.URL.String(), "/login/") {
		t.Fatalf("déconnexion : %v %v", resp.Request.URL, err)
	}
	b.login(chosen, pw, "/admin/server")
	// Le passe-droit ne sert qu'une fois : une nouvelle connexion redemande le mot de passe.
	b.c.Jar.SetCookies(resp.Request.URL, []*http.Cookie{{Name: "lattis_admin", Value: "", MaxAge: -1, Path: "/"}})
	if _, body := b.get("/admin/server"); !strings.Contains(body, `type="password"`) {
		t.Fatal("formulaire de connexion attendu une fois la session fermée")
	}
}

func TestAdminConsole(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	ownerEmail := testpg.Email("admin")
	token, err := e.db.Store.Bootstrap(ctx, ownerEmail)
	if err != nil {
		t.Fatal(err)
	}
	e.account(token, pw)

	b := e.browser()
	resp, body := b.login(ownerEmail, pw, "/admin/server")
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/admin/server" || !strings.Contains(body, "Nom du serveur") {
		t.Fatalf("console : %d %s %s", resp.StatusCode, resp.Request.URL, body)
	}
	csrf := field(body, "csrf")
	settings := url.Values{"csrf": {csrf}, "back": {"/admin/server"}, "display_name": {"Serveur de test"},
		"access_mode": {"open"}, "default_role": {"reader"}, "workbook_creators": {"everyone"}, "logo_url": {""},
		// Durées dans l'unité affichée (minutes, jours), enregistrées en secondes.
		"access_token_ttl": {"10"}, "refresh_token_ttl": {"30"}, "invitation_ttl": {"14"},
		"password_max_failures": {"5"}, "password_lock_seconds": {"15"}, "tombstone_retention": {"30"},
		"republish_grace": {"10"}}

	// Sans jeton de formulaire : refusé.
	noCSRF := url.Values{}
	for k, v := range settings {
		if k != "csrf" {
			noCSRF[k] = v
		}
	}
	if resp, _ := b.post("/admin/do/server_settings", noCSRF); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("sans CSRF : %d", resp.StatusCode)
	}
	resp, body = b.post("/admin/do/server_settings", settings)
	if !strings.Contains(body, "Réglages enregistrés.") || !strings.Contains(body, "Serveur de test") {
		t.Fatalf("réglages : %d", resp.StatusCode)
	}
	var ttl float64
	if err := e.db.Store.DB.QueryRow(ctx, `SELECT (value #>> '{}')::float8 FROM lattis.server_config WHERE key = 'access_token_ttl'`).Scan(&ttl); err != nil || ttl != 600 {
		t.Fatalf("jetons d'accès : %v s, %v", ttl, err)
	}
	// Valeur refusée par la base : message affiché, rien d'enregistré.
	bad := url.Values{}
	for k, v := range settings {
		bad[k] = v
	}
	bad.Set("logo_url", "javascript:alert(1)")
	if _, body = b.post("/admin/do/server_settings", bad); !strings.Contains(body, "logo_url : adresse https://") {
		t.Fatal("logo invalide accepté")
	}
	// Journal : l'action est au nom de l'administrateur.
	var actor string
	if err := e.db.Super.QueryRow(ctx, `SELECT actor FROM lattis.audit WHERE action = 'set_config' AND target = 'display_name'`).Scan(&actor); err != nil || actor != ownerEmail {
		t.Fatalf("auteur : %q %v", actor, err)
	}

	// Compte à mot de passe créé depuis la console, puis connexion refusée à
	// l'administration (pas propriétaire du serveur).
	other := testpg.Email("autre")
	_, body = b.post("/admin/do/add_user", url.Values{"csrf": {csrf}, "back": {"/admin/users"}, "email": {other}})
	m := regexp.MustCompile(`/setup\?token=([A-Za-z0-9_-]+)`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("lien de création absent")
	}
	e.account(m[1], pw)
	ob := e.browser()
	if resp, body := ob.login(other, pw, "/admin/users"); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "réservée aux propriétaires") {
		t.Fatalf("non-propriétaire : %d", resp.StatusCode)
	}
	if resp, _ := ob.post("/admin/do/disable_user", url.Values{"csrf": {csrf}, "id": {"00000000-0000-0000-0000-000000000000"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("action d'un non-propriétaire sans son jeton : %d", resp.StatusCode)
	}

	// Ajout d'un propriétaire du serveur par lien : il entre dans l'administration.
	second := testpg.Email("second")
	_, body = b.post("/admin/do/add_user", url.Values{"csrf": {csrf}, "back": {"/admin/users"}, "email": {second}, "server_owner": {"on"}})
	m = regexp.MustCompile(`/setup\?token=([A-Za-z0-9_-]+)`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("lien du second propriétaire absent")
	}
	e.account(m[1], pw)
	if resp, _ := e.browser().login(second, pw, "/admin/users"); resp.StatusCode != http.StatusOK {
		t.Fatalf("second propriétaire : %d", resp.StatusCode)
	}

	// Désactivation : confirmation exigée.
	otherUser, _ := e.db.Store.User(ctx, userID(t, e, other))
	if _, body = b.post("/admin/do/disable_user", url.Values{"csrf": {csrf}, "back": {"/admin/users"}, "id": {otherUser.ID}}); !strings.Contains(body, "Confirmer") {
		t.Fatal("confirmation non exigée")
	}
	if _, body = b.post("/admin/do/disable_user", url.Values{"csrf": {csrf}, "back": {"/admin/users"}, "id": {otherUser.ID}, "confirm": {"on"}}); !strings.Contains(body, "Compte désactivé") {
		t.Fatal("désactivation")
	}

	// Droits d'un classeur.
	var wb, tid string
	if err := e.db.AsUser(t, ownerEmail, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.create_workbook('Ventes console', 'collaborative')::text`).Scan(&wb); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_table($1::uuid, 'Commandes', '[{"name":"Région","type":"text"}]')::text`, wb).Scan(&tid); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT lattis.finish_publish($1::uuid)`, wb).Scan(new(int))
	}); err != nil {
		t.Fatal(err)
	}
	page := "/admin/workbooks/" + wb

	// Création d'un classeur depuis la console, avec un premier tableau.
	if _, body := b.post("/admin/do/create_workbook", url.Values{"csrf": {csrf}, "back": {"/admin/workbooks"}, "title": {"Suivi console"},
		"mode": {"collaborative"}, "table": {"Commandes"}, "columns": {"Client : texte\r\nMontant : décimal\r\nLivrée : booléen"}}); !strings.Contains(body, "Suivi console") || !strings.Contains(body, "créé") {
		t.Fatalf("création de classeur : %s", body)
	}
	var cols int
	if err := e.db.Super.QueryRow(ctx, `SELECT count(*) FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN lattis.workbooks w ON w.schema_name = n.nspname WHERE w.title = 'Suivi console' AND c.relname = 'Commandes' AND a.attname IN ('Client', 'Montant', 'Livrée')`).Scan(&cols); err != nil || cols != 3 {
		t.Fatalf("tableau créé : %d colonnes, %v", cols, err)
	}
	if _, body := b.post("/admin/do/create_workbook", url.Values{"csrf": {csrf}, "back": {"/admin/workbooks"}, "title": {"Mauvais"},
		"mode": {"collaborative"}, "table": {"T"}, "columns": {"A : couleur"}}); !strings.Contains(body, "inconnu") {
		t.Fatal("type de colonne inconnu accepté")
	}

	// Lien à partager : connexion exigée, accès vérifié, choix retenu.
	if resp, body := b.get("/w/" + wb); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Ventes console") || !strings.Contains(body, "Ouvrir dans Lattis") {
		t.Fatalf("lien : %d %s", resp.StatusCode, body)
	}
	if _, body := b.get("/w/" + wb + "?open=desktop"); !strings.Contains(body, `url=lattis://open?server=`) {
		t.Fatal("ouverture dans l'application attendue")
	}
	if _, body := b.get("/w/" + wb); !strings.Contains(body, `url=lattis://open?`) {
		t.Fatal("choix non retenu")
	}
	if resp, body := b.get("/w/00000000-0000-0000-0000-000000000000"); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "pas accès") {
		t.Fatalf("classeur inconnu : %d", resp.StatusCode)
	}
	if _, body := e.browser().get("/w/" + wb); !strings.Contains(body, `type="password"`) {
		t.Fatal("connexion attendue avant le lien")
	}
	if _, body := b.get(page); !strings.Contains(body, "/w/"+wb) {
		t.Fatal("lien absent de la page du classeur")
	}
	if resp, body := b.get("/admin/workbooks"); resp.StatusCode != http.StatusOK || !strings.Contains(body, page) {
		t.Fatalf("liste des classeurs : %d", resp.StatusCode)
	}
	if _, body = b.post("/admin/do/create_role", url.Values{"csrf": {csrf}, "back": {page}, "workbook_id": {wb}, "name": {"Commerciaux"}}); !strings.Contains(body, "Rôle créé") {
		t.Fatal("création de rôle")
	}
	roleID := regexp.MustCompile(`name="role_id" value="([0-9a-f-]{36})"`).FindAllStringSubmatch(body, -1)
	var custom string
	if err := e.db.Super.QueryRow(ctx, `SELECT id::text FROM lattis.roles WHERE workbook_id = $1 AND builtin IS NULL`, wb).Scan(&custom); err != nil || len(roleID) == 0 {
		t.Fatalf("rôle : %v", err)
	}
	if _, body = b.post("/admin/do/set_level", url.Values{"csrf": {csrf}, "back": {page}, "role_id": {custom},
		"node": {"table:" + tid}, "level": {"reader"}, "read_filter": {`true) OR (true`}}); !strings.Contains(body, "filtre refusé") {
		t.Fatal("filtre dangereux accepté")
	}
	if _, body = b.post("/admin/do/set_level", url.Values{"csrf": {csrf}, "back": {page}, "role_id": {custom},
		"node": {"table:" + tid}, "level": {"reader"}, "read_filter": {`"Région" = 'Nord'`}}); !strings.Contains(body, "Niveau enregistré.") {
		t.Fatal("niveau")
	}
	if _, body = b.post("/admin/do/grant_role", url.Values{"csrf": {csrf}, "back": {page}, "role_id": {custom}, "email": {other}}); !strings.Contains(body, "ajouté au rôle") {
		t.Fatal("ajout au rôle")
	}
	if _, body = b.get(page + "?preview_user=" + url.QueryEscape(other) + "&preview_table=" + tid); !strings.Contains(body, "0 ligne(s) visibles") {
		t.Fatal("aperçu")
	}

	// Data Lake Lattis : dossier, donnée partagée, audience, retrait.
	if _, body = b.post("/admin/do/create_data_folder", url.Values{"csrf": {csrf}, "back": {"/admin/data"}, "name": {"Commercial"}}); !strings.Contains(body, "Dossier créé.") {
		t.Fatal("création de dossier")
	}
	var folder, ds string
	if err := e.db.Super.QueryRow(ctx, `SELECT id::text FROM lattis.data_folders WHERE name = 'Commercial'`).Scan(&folder); err != nil {
		t.Fatal(err)
	}
	if err := e.db.AsUser(t, ownerEmail, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.create_dataset($1::uuid, $2::uuid, $3::uuid, 'Commandes', 'Toutes les commandes', NULL, NULL)::text`, wb, tid, folder).Scan(&ds)
	}); err != nil {
		t.Fatal(err)
	}
	if _, body = b.get("/admin/data"); !strings.Contains(body, "donnees.commandes") || !strings.Contains(body, "Commercial") {
		t.Fatal("catalogue")
	}
	if _, body = b.post("/admin/do/add_data_audience", url.Values{"csrf": {csrf}, "back": {"/admin/data"}, "dataset_id": {ds},
		"kind": {"user"}, "email": {ownerEmail}, "predicate": {`"Région" = 'Nord'`}}); !strings.Contains(body, "Ajouté à l&#39;audience.") {
		t.Fatalf("audience : %s", body)
	}
	if _, body = b.post("/admin/do/drop_dataset", url.Values{"csrf": {csrf}, "back": {"/admin/data"}, "id": {ds}, "confirm": {"on"}}); !strings.Contains(body, "Partage retiré.") {
		t.Fatal("retrait du partage")
	}

	// Pipelines : création depuis le formulaire, puis liste.
	if _, body = b.post("/admin/do/create_pipeline", url.Values{"csrf": {csrf}, "back": {"/admin/pipelines"}, "name": {"Import"},
		"target": {wb + "|" + tid}, "role": {"editor"}, "source_kind": {"upload"}, "format": {"csv"}, "mode": {"upsert"},
		"key_columns": {"Région"}, "schedule_kind": {"manual"}}); !strings.Contains(body, "Pipeline créé.") {
		t.Fatalf("création de pipeline : %s", body)
	}
	if _, body = b.get("/admin/pipelines"); !strings.Contains(body, "Exécuter avec ce fichier") || !strings.Contains(body, "Ventes console") {
		t.Fatal("liste des pipelines")
	}

	// Équipes : création, membre, rôle donné à l'équipe.
	if _, body = b.post("/admin/do/create_team", url.Values{"csrf": {csrf}, "back": {"/admin/teams"}, "name": {"Nord"}}); !strings.Contains(body, "Équipe créée.") {
		t.Fatal("création d'équipe")
	}
	var team string
	if err := e.db.Super.QueryRow(ctx, `SELECT id::text FROM lattis.teams WHERE name = 'Nord'`).Scan(&team); err != nil {
		t.Fatal(err)
	}
	if _, body = b.post("/admin/do/add_team_member", url.Values{"csrf": {csrf}, "back": {"/admin/teams"}, "team_id": {team}, "email": {ownerEmail}}); !strings.Contains(body, "ajouté à l&#39;équipe") || !strings.Contains(body, ownerEmail) {
		t.Fatal("membre d'équipe")
	}
	if _, body = b.post("/admin/do/grant_team", url.Values{"csrf": {csrf}, "back": {page}, "role_id": {custom}, "team_id": {team}}); !strings.Contains(body, "Équipe ajoutée au rôle.") || !strings.Contains(body, "Équipe Nord") {
		t.Fatal("équipe ajoutée au rôle")
	}
	if _, body = b.post("/admin/do/drop_team", url.Values{"csrf": {csrf}, "back": {"/admin/teams"}, "id": {team}, "confirm": {"on"}}); !strings.Contains(body, "Équipe supprimée.") || !strings.Contains(body, "Aucune équipe.") {
		t.Fatal("suppression d'équipe")
	}

	for _, p := range []string{"/admin/providers", "/admin/users?q=autre", "/admin/audit?action=set_level", "/admin/status", "/admin/teams"} {
		if resp, body := b.get(p); resp.StatusCode != http.StatusOK || strings.Contains(body, `class="error"`) {
			t.Fatalf("%s : %d", p, resp.StatusCode)
		}
	}

	// Déconnexion.
	b.post("/admin/logout", url.Values{"csrf": {csrf}})
	if resp, body := b.get("/admin/status"); !strings.Contains(body, `type="password"`) {
		t.Fatalf("après déconnexion : %d", resp.StatusCode)
	}
}

func userID(t *testing.T, e *env, email string) string {
	var id string
	if err := e.db.Super.QueryRow(context.Background(), `SELECT id::text FROM lattis.users WHERE email = $1`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
