// Package admin sert l'interface web d'administration (spécification §15).
//
// L'administrateur se connecte par le même parcours que l'application de
// bureau (client interne lattis-admin), puis chaque page et chaque action
// s'exécutent sur une connexion PostgreSQL ouverte sous son identité : les
// fonctions lattis.* vérifient elles-mêmes ses droits et journalisent sous
// son nom. Aucune règle d'autorisation n'est dupliquée ici.
package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Be-Lattis/lattis-server/internal/auth"
	"github.com/Be-Lattis/lattis-server/internal/config"
	"github.com/Be-Lattis/lattis-server/internal/mail"
	"github.com/Be-Lattis/lattis-server/internal/monitor"
	"github.com/Be-Lattis/lattis-server/internal/pipeline"
	"github.com/Be-Lattis/lattis-server/internal/replicas"
	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/tokens"
	"github.com/Be-Lattis/lattis-server/internal/userdb"
)

const (
	sessionCookie = "lattis_admin"
	loginCookie   = "lattis_admin_login"
	csrfCookie    = "lattis_admin_csrf"
	flashCookie   = "lattis_admin_flash"
	sessionTTL    = 8 * time.Hour
	loginTTL      = 10 * time.Minute
	audSession    = "lattisd:admin"
	audLogin      = "lattisd:admin-login"
)

//go:embed web/*.html web/admin.css web/admin.js
var webFS embed.FS

var pages = map[string]*template.Template{}

// treeCtx : un nœud d'arbre et la page, pour les gabarits récursifs.
type treeCtx struct {
	Page view
	Node *treeNode
	All  any
}

// audienceCtx : audience d'une donnée ou d'un dossier, pour le gabarit « audience ».
type audienceCtx struct {
	Page            view
	Entries         any
	Dataset, Folder string
	Teams           any
	Inherit         bool
}

// providerField : un champ texte d'un fournisseur, pour le gabarit « pfield ».
type providerField struct {
	Label, Name, Value, Help string
	Required                 bool
}

// SourceURL : code source proposé au pied des pages (AGPL-3.0, §13) ; fixé
// par lattisd au démarrage (LATTIS_SOURCE_URL).
var SourceURL = "https://github.com/Be-Lattis/lattis-server"

var funcs = template.FuncMap{
	"sourceURL":       func() string { return SourceURL },
	"pipelineMode":    func(v any) string { return pipelineModes[fmt.Sprint(v)] },
	"pipelineTrigger": func(v any) string { return pipelineTriggers[fmt.Sprint(v)] },
	"pipelineStatus":  func(v any) string { return pipelineStatuses[fmt.Sprint(v)] },
	"pipelineSource":  pipelineSource,
	"pf": func(label, name, value, help string, required bool) providerField {
		return providerField{Label: label, Name: name, Value: value, Help: help, Required: required}
	},
	"join": func(v []string) string { return strings.Join(v, " ") },
	"node": func(page view, n *treeNode, all any) treeCtx { return treeCtx{Page: page, Node: n, All: all} },
	"leaf": func(item map[string]any) *treeNode { return &treeNode{Item: item} },
	"audienceCtx": func(page view, entries any, dataset, folder string, teams any, inherit bool) audienceCtx {
		return audienceCtx{Page: page, Entries: entries, Dataset: dataset, Folder: folder, Teams: teams, Inherit: inherit}
	},
	"tenantKind": func(v any) string {
		k, _ := v.(string)
		if l, ok := map[string]string{"domain": "Domaine", "tid": "Tenant Microsoft", "realm": "Realm", "email": "Adresse", "any": "Tous"}[k]; ok {
			return l
		}
		return k
	},
	"json": func(v any) string {
		if v == nil {
			return ""
		}
		b, _ := json.Marshal(v)
		return string(b)
	},
	"bytes": func(v any) string {
		var f float64
		switch n := v.(type) {
		case float64:
			f = n
		case int64:
			f = float64(n)
		case int:
			f = float64(n)
		}
		units := []string{"o", "Ko", "Mo", "Go", "To"}
		i := 0
		for f >= 1024 && i < len(units)-1 {
			f /= 1024
			i++
		}
		return trimFloat(f) + " " + units[i]
	},
	"date": func(v any) string {
		s, _ := v.(string)
		if s == "" {
			return ""
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return s
		}
		return t.Local().Format("02/01/2006 15:04")
	},
	"has": func(list any, s string) bool {
		switch l := list.(type) {
		case []any:
			for _, x := range l {
				if x == s {
					return true
				}
			}
		case []string:
			for _, x := range l {
				if x == s {
					return true
				}
			}
		}
		return false
	},
	"str": func(v any) string {
		switch x := v.(type) {
		case nil:
			return ""
		case string:
			return x
		case float64:
			return trimFloat(x)
		case bool:
			if x {
				return "true"
			}
			return "false"
		}
		b, _ := json.Marshal(v)
		return string(b)
	},
	"levelName":   levelName,
	"builtinName": builtinName,
}

// trimFloat écrit un nombre à la française, une décimale au plus.
func trimFloat(f float64) string {
	return strings.Replace(strings.TrimSuffix(strconv.FormatFloat(f, 'f', 1, 64), ".0"), ".", ",", 1)
}

func init() {
	for _, name := range []string{"server", "providers", "users", "teams", "workbooks", "workbook", "audit", "status", "message", "link", "data", "pipelines", "forms", "monitor"} {
		pages[name] = template.Must(template.New("").Funcs(funcs).ParseFS(webFS, "web/layout.html", "web/"+name+".html"))
	}
}

// Service porte les routes /admin/*.
// Simulator simule un pipeline (pipeline.Runner).
type Simulator interface {
	Simulate(ctx context.Context, id, file string) (pipeline.Result, error)
}

type Service struct {
	Config *config.Config
	Store  *store.Store
	Keys   *tokens.KeySet
	Users  userdb.Runner
	// Replicas : réplicas surveillés par lattisd (écran État) ; nil possible.
	Replicas *replicas.Monitor
	Version  string
	Now      func() time.Time
	// Pipelines : simulation des pipelines ; nil : bouton absent.
	Pipelines Simulator
	// Mailer : envoi d'e-mails ; nil : non configuré (liens à copier).
	Mailer mail.Sender
	// Monitor : relevés de l'écran Surveillance ; nil : écran vide.
	Monitor *monitor.Monitor
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Routes enregistre les points d'accès.
func (s *Service) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/server", http.StatusFound)
	})
	mux.HandleFunc("GET /admin/login", s.login)
	mux.HandleFunc("GET /admin/callback", s.callback)
	mux.HandleFunc("POST /admin/logout", s.logout)
	mux.HandleFunc("POST /logout", s.siteLogout)
	mux.HandleFunc("GET /admin/static/admin.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		b, _ := webFS.ReadFile("web/admin.css")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /admin/static/admin.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		b, _ := webFS.ReadFile("web/admin.js")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /admin/server", s.page("server", "Serveur", loadServer))
	mux.HandleFunc("GET /admin/providers", s.page("providers", "Connexion", loadProviders))
	mux.HandleFunc("GET /admin/users", s.page("users", "Utilisateurs", loadUsers))
	mux.HandleFunc("GET /admin/teams", s.page("teams", "Équipes", loadTeams))
	mux.HandleFunc("GET /admin/workbooks", s.page("workbooks", "Classeurs", loadWorkbooks))
	mux.HandleFunc("GET /admin/workbooks/{id}", s.page("workbook", "Droits du classeur", s.loadWorkbook))
	mux.HandleFunc("GET /admin/data", s.page("data", "Data Lake Lattis", loadData))
	mux.HandleFunc("GET /admin/pipelines", s.page("pipelines", "Pipelines d'ingestion", s.loadPipelines))
	mux.HandleFunc("GET /admin/forms", s.page("forms", "Formulaires", s.loadForms))
	mux.HandleFunc("POST /admin/pipelines/{id}/upload", s.uploadPipeline)
	mux.HandleFunc("GET /admin/audit", s.page("audit", "Journal", loadAudit))
	mux.HandleFunc("GET /admin/status", s.page("status", "État", s.loadStatus))
	mux.HandleFunc("GET /admin/monitor", s.page("monitor", "Surveillance", s.loadMonitor))
	mux.HandleFunc("POST /admin/do/{action}", s.do)
	mux.HandleFunc("POST /admin/server/logo", s.uploadLogo)
	mux.HandleFunc("GET /w/{id}", s.workbookLink)
}

// --- Connexion -------------------------------------------------------------------

type loginData struct {
	Verifier string `json:"v"`
	State    string `json:"s"`
	Next     string `json:"n"`
}

func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	ld := loginData{Verifier: randomString(), State: randomString(), Next: safeBack(r.URL.Query().Get("next"))}
	data, _ := json.Marshal(ld)
	now := s.now()
	signed, err := s.Keys.Sign(tokens.TypeState, tokens.Claims{Issuer: s.Config.Issuer(), Audience: tokens.Audience{audLogin},
		IssuedAt: now.Unix(), ExpiresAt: now.Add(loginTTL).Unix(), Data: data})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.setCookie(w, loginCookie, signed, "/admin/callback", loginTTL, http.SameSiteLaxMode)
	sum := sha256.Sum256([]byte(ld.Verifier))
	q := url.Values{
		"client_id": {auth.AdminClientID}, "redirect_uri": {s.Config.URL("/admin/callback")},
		"response_type": {"code"}, "scope": {auth.DefaultScope}, "state": {ld.State},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
	}
	http.Redirect(w, r, "/oauth/authorize?"+q.Encode(), http.StatusFound)
}

func (s *Service) callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := r.Cookie(loginCookie)
	if err != nil {
		http.Redirect(w, r, "/admin/login", http.StatusFound)
		return
	}
	s.clearCookie(w, loginCookie, "/admin/callback")
	claims, err := s.Keys.Verify(c.Value, tokens.TypeState, s.Config.Issuer(), audLogin, s.now())
	var ld loginData
	if err != nil || json.Unmarshal(claims.Data, &ld) != nil {
		http.Redirect(w, r, "/admin/login", http.StatusFound)
		return
	}
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(ld.State)) != 1 || q.Get("iss") != s.Config.Issuer() {
		s.message(w, r, http.StatusBadRequest, "Connexion refusée", "La réponse de connexion ne correspond pas à la demande.")
		return
	}
	if e := q.Get("error"); e != "" {
		s.message(w, r, http.StatusForbidden, "Connexion refusée", q.Get("error_description"))
		return
	}
	code, err := s.Store.ConsumeCode(ctx, q.Get("code"))
	if err != nil {
		s.message(w, r, http.StatusBadRequest, "Connexion refusée", "Code de connexion invalide ou expiré.")
		return
	}
	sum := sha256.Sum256([]byte(ld.Verifier))
	if code.ClientID != auth.AdminClientID || code.RedirectURI != s.Config.URL("/admin/callback") ||
		subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(code.CodeChallenge)) != 1 {
		s.message(w, r, http.StatusBadRequest, "Connexion refusée", "Code de connexion invalide ou expiré.")
		return
	}
	u, err := s.Store.User(ctx, code.UserID)
	if err != nil || u.Disabled {
		s.message(w, r, http.StatusForbidden, "Compte désactivé", "Votre compte est désactivé.")
		return
	}
	now := s.now()
	signed, err := s.Keys.Sign(tokens.TypeState, tokens.Claims{Issuer: s.Config.Issuer(), Audience: tokens.Audience{audSession},
		Subject: u.ID, PGRole: u.PGRole, ID: tokens.NewID(), IssuedAt: now.Unix(), ExpiresAt: now.Add(sessionTTL).Unix()})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.setCookie(w, sessionCookie, signed, "/", sessionTTL, http.SameSiteLaxMode)
	s.clearCookie(w, sessionCookie, "/admin") // session d'avant le 18/10, sous /admin
	next := ld.Next
	if next == "" {
		next = "/admin/server"
	}
	http.Redirect(w, r, next, http.StatusFound)
}

func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.message(w, r, http.StatusForbidden, "Demande refusée", "Cette page était restée ouverte trop longtemps : revenez-y et recommencez.")
		return
	}
	if u, err := s.session(r); err == nil {
		if f, ok := s.Users.(interface{ Forget(string) }); ok {
			f.Forget(u.ID)
		}
	}
	s.clearCookie(w, sessionCookie, "/")
	s.clearCookie(w, sessionCookie, "/admin") // ancien chemin
	s.message(w, r, http.StatusOK, "Déconnecté", "Vous êtes déconnecté de l'administration.")
}

// Session renvoie l'utilisateur connecté au site (édition web, liens).
func (s *Service) Session(r *http.Request) (store.User, error) { return s.session(r) }

// session renvoie l'utilisateur connecté, encore actif.
//
// Le navigateur peut envoyer plusieurs cookies de ce nom : avant le 18/10 la
// session vivait sous /admin, elle vit maintenant sous /. Le plus ancien,
// envoyé en premier car son chemin est plus précis, peut être périmé (autre
// clé de signature après une réinstallation) : on prend le premier valable,
// sinon la connexion tournerait en rond.
func (s *Service) session(r *http.Request) (store.User, error) {
	var claims *tokens.Claims
	err := http.ErrNoCookie
	for _, c := range r.Cookies() {
		if c.Name != sessionCookie {
			continue
		}
		if claims, err = s.Keys.Verify(c.Value, tokens.TypeState, s.Config.Issuer(), audSession, s.now()); err == nil {
			break
		}
	}
	if err != nil {
		return store.User{}, err
	}
	u, err := s.Store.User(r.Context(), claims.Subject)
	if err != nil {
		return store.User{}, err
	}
	if u.Disabled || u.PGRole != claims.PGRole {
		return store.User{}, errors.New("compte désactivé")
	}
	return u, nil
}

// --- Pages -------------------------------------------------------------------------

type loader func(ctx context.Context, tx pgx.Tx, r *http.Request, u store.User) (any, error)

type view struct {
	Title  string
	Page   string
	Server string
	User   store.User
	CSRF   string
	Flash  string
	// FlashLink : lien de choix du mot de passe extrait du message, montré
	// une seule fois avec un bouton « Copier ».
	FlashLink string
	Error     string
	Path      string
	Data      any
	Version   string
	Edition   string
	// Back : lien de retour d'une page de message.
	Back string
	// Refill : saisie à remettre dans le formulaire refusé (JSON).
	Refill string
}

func (s *Service) page(name, title string, load loader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.session(r)
		if err != nil {
			http.Redirect(w, r, "/admin/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		v := view{Title: title, Page: name, User: u, CSRF: s.csrf(w, r), Path: r.URL.RequestURI(),
			Version: s.Version, Edition: string(s.Config.Edition)}
		fl := s.takeFlash(w, r)
		v.Flash, v.Error = fl.OK, fl.Err
		if fl.Form != nil {
			b, _ := json.Marshal(fl.Form)
			v.Refill = string(b)
		}
		v.Flash, v.FlashLink = splitSetupLink(v.Flash)
		var forbidden bool
		err = s.Users.As(r.Context(), u, func(tx pgx.Tx) error {
			var owner bool
			if err := tx.QueryRow(r.Context(), `SELECT (s ->> 'server_owner')::boolean, s #>> '{server,display_name}' FROM lattis.login_summary() s`).
				Scan(&owner, &v.Server); err != nil {
				return err
			}
			if !owner {
				forbidden = true
				return nil
			}
			v.Data, err = load(r.Context(), tx, r, u)
			return err
		})
		if forbidden {
			s.message(w, r, http.StatusForbidden, "Accès réservé",
				"L'administration est réservée aux propriétaires du serveur ("+u.Email+").")
			return
		}
		if err != nil {
			if errors.Is(err, store.ErrNotFound) || errors.Is(err, pgx.ErrNoRows) {
				s.message(w, r, http.StatusNotFound, "Introuvable", "Cet élément n'existe pas ou plus.")
				return
			}
			if msg, ok := userMessage(err); ok {
				v.Error = msg
				v.Data = nil
				s.render(w, http.StatusBadRequest, "message", v)
				return
			}
			s.fail(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		s.render(w, http.StatusOK, name, v)
	}
}

func (s *Service) render(w http.ResponseWriter, status int, name string, v view) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := pages[name].ExecuteTemplate(w, "layout", v); err != nil {
		slog.Error("gabarit d'administration", "page", name, "err", err)
	}
}

func (s *Service) message(w http.ResponseWriter, r *http.Request, status int, title, msg string) {
	v := view{Title: title, Page: "message", Server: "Serveur Lattis", Error: msg, Version: s.Version}
	if status < 400 {
		v.Error, v.Flash = "", msg
	}
	// Connecté : le menu reste là ; sinon, un lien pour se (re)connecter.
	if s.Store != nil && s.Keys != nil {
		if u, err := s.session(r); err == nil {
			v.User = u
			v.CSRF = s.csrf(w, r)
		}
	}
	v.Back = safeBack(r.PostFormValue("back"))
	s.render(w, status, "message", v)
}

func (s *Service) fail(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("administration", "path", r.URL.Path, "err", err)
	s.message(w, r, http.StatusInternalServerError, "Erreur", "Une erreur interne est survenue ; elle a été journalisée.")
}

// userMessage renvoie le message d'une erreur levée volontairement par une
// fonction lattis.* (droits, validation), affichable tel quel.
func userMessage(err error) (string, bool) {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return "", false
	}
	switch pe.Code {
	case "23514":
		return "Valeur refusée : un champ obligatoire manque ou ne convient pas (" + pe.ConstraintName + ").", true
	case "22P02":
		return "Valeur refusée : un choix ou un nombre attendu est vide ou mal écrit.", true
	case "42601":
		return "Filtre mal écrit : vérifiez les guillemets, les parenthèses et les opérateurs.", true
	case "23505":
		if strings.Contains(pe.Message, "duplicate key") {
			return "Cet élément existe déjà.", true
		}
	}
	switch pe.Code[:2] {
	case "42", "22", "23", "P0", "55", "2B":
		return upperFirst(pe.Message), true
	}
	return "", false
}

// --- Cookies, CSRF, messages ---------------------------------------------------------

func (s *Service) secure() bool { return s.Config.PublicURL.Scheme == "https" }

func (s *Service) setCookie(w http.ResponseWriter, name, value, path string, ttl time.Duration, same http.SameSite) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: path, MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: s.secure(), SameSite: same})
}

func (s *Service) clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: path, MaxAge: -1,
		HttpOnly: true, Secure: s.secure(), SameSite: http.SameSiteLaxMode})
}

func (s *Service) csrf(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) >= 22 {
		return c.Value
	}
	t := randomString()
	s.setCookie(w, csrfCookie, t, "/admin", sessionTTL, http.SameSiteStrictMode)
	return t
}

func (s *Service) checkCSRF(r *http.Request) bool {
	c, err := r.Cookie(csrfCookie)
	if err != nil || c.Value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(r.PostFormValue("csrf"))) == 1
}

type flash struct {
	OK  string `json:"ok,omitempty"`
	Err string `json:"err,omitempty"`
	// Form : saisie du formulaire refusé, remise en place par admin.js.
	Form *refill `json:"form,omitempty"`
}

// refill : formulaire (chemin d'action et champ id) et valeurs saisies.
type refill struct {
	Action string              `json:"action"`
	ID     string              `json:"id,omitempty"`
	Fields map[string][]string `json:"fields"`
}

// maxRefill borne la saisie gardée (le cookie du message fait au plus 4 ko).
const maxRefill = 2000

// keepInput : saisie d'un formulaire refusé, sans jeton, confirmations ni
// secrets ; nil si rien à garder ou trop long.
func keepInput(r *http.Request) *refill {
	f := &refill{Action: r.URL.Path, ID: r.PostFormValue("id"), Fields: map[string][]string{}}
	for k, v := range r.PostForm {
		l := strings.ToLower(k)
		if k == "csrf" || k == "back" || k == "id" || strings.HasPrefix(l, "confirm") ||
			strings.Contains(l, "password") || strings.Contains(l, "secret") {
			continue
		}
		f.Fields[k] = v
	}
	if len(f.Fields) == 0 {
		return nil
	}
	if b, _ := json.Marshal(f); len(b) > maxRefill {
		return nil
	}
	return f
}

func (s *Service) setFlash(w http.ResponseWriter, f flash) {
	b, _ := json.Marshal(f)
	s.setCookie(w, flashCookie, base64.RawURLEncoding.EncodeToString(b), "/admin", time.Minute, http.SameSiteStrictMode)
}

func (s *Service) takeFlash(w http.ResponseWriter, r *http.Request) flash {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return flash{}
	}
	s.clearCookie(w, flashCookie, "/admin")
	b, err := base64.RawURLEncoding.DecodeString(c.Value)
	var f flash
	if err != nil || json.Unmarshal(b, &f) != nil {
		return flash{}
	}
	return f
}

// safeBack n'accepte qu'un chemin local de l'administration.
func safeBack(p string) string {
	if !(strings.HasPrefix(p, "/admin/") || strings.HasPrefix(p, "/w/") || strings.HasPrefix(p, "/app/")) || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "\\\r\n") {
		return ""
	}
	return p
}

func randomString() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

var setupLinkRe = regexp.MustCompile(`https?://\S+/setup\?token=[A-Za-z0-9_-]+`)

// splitSetupLink sépare d'un message le lien de choix du mot de passe.
func splitSetupLink(msg string) (string, string) {
	loc := setupLinkRe.FindStringIndex(msg)
	if loc == nil {
		return msg, ""
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(msg[:loc[0]]), ":")), msg[loc[0]:loc[1]]
}

// upperFirst met une majuscule au début d'un message de PostgreSQL.
func upperFirst(s string) string {
	for i, r := range s {
		return strings.ToUpper(string(r)) + s[i+len(string(r)):]
	}
	return s
}

// siteLogout : déconnexion depuis les pages hors console (lien vers un
// classeur, accès refusé, édition web). Pas de jeton de formulaire ici :
// la requête doit venir du site lui-même (Origin ou Referer).
func (s *Service) siteLogout(w http.ResponseWriter, r *http.Request) {
	if !s.sameSite(r) {
		s.message(w, r, http.StatusForbidden, "Demande refusée", "Déconnexion refusée : elle doit venir d'une page de ce serveur.")
		return
	}
	if u, err := s.session(r); err == nil {
		if f, ok := s.Users.(interface{ Forget(string) }); ok {
			f.Forget(u.ID)
		}
	}
	s.clearCookie(w, sessionCookie, "/")
	s.clearCookie(w, sessionCookie, "/admin")
	next := safeBack(r.PostFormValue("next"))
	if next == "" {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// sameSite : la requête vient d'une page de ce serveur.
func (s *Service) sameSite(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	u, err := url.Parse(origin)
	return err == nil && u.Scheme == s.Config.PublicURL.Scheme && strings.EqualFold(u.Host, s.Config.PublicURL.Host)
}
