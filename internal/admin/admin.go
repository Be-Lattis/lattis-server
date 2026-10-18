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
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Be-Lattis/lattis-server/internal/auth"
	"github.com/Be-Lattis/lattis-server/internal/config"
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

//go:embed web/*.html web/admin.css
var webFS embed.FS

var pages = map[string]*template.Template{}

// providerField : un champ texte d'un fournisseur, pour le gabarit « pfield ».
type providerField struct {
	Label, Name, Value, Help string
	Required                 bool
}

var funcs = template.FuncMap{
	"pf": func(label, name, value, help string, required bool) providerField {
		return providerField{Label: label, Name: name, Value: value, Help: help, Required: required}
	},
	"join": func(v []string) string { return strings.Join(v, " ") },
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
		f, _ := v.(float64)
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
	for _, name := range []string{"server", "providers", "users", "teams", "workbooks", "workbook", "audit", "status", "message"} {
		pages[name] = template.Must(template.New("").Funcs(funcs).ParseFS(webFS, "web/layout.html", "web/"+name+".html"))
	}
}

// Service porte les routes /admin/*.
type Service struct {
	Config *config.Config
	Store  *store.Store
	Keys   *tokens.KeySet
	Users  userdb.Runner
	// Replicas : réplicas surveillés par lattisd (écran État) ; nil possible.
	Replicas *replicas.Monitor
	Version  string
	Now      func() time.Time
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
	mux.HandleFunc("GET /admin/static/admin.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		b, _ := webFS.ReadFile("web/admin.css")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /admin/server", s.page("server", "Serveur", loadServer))
	mux.HandleFunc("GET /admin/providers", s.page("providers", "Connexion", loadProviders))
	mux.HandleFunc("GET /admin/users", s.page("users", "Utilisateurs", loadUsers))
	mux.HandleFunc("GET /admin/teams", s.page("teams", "Équipes", loadTeams))
	mux.HandleFunc("GET /admin/workbooks", s.page("workbooks", "Classeurs", loadWorkbooks))
	mux.HandleFunc("GET /admin/workbooks/{id}", s.page("workbook", "Droits du classeur", s.loadWorkbook))
	mux.HandleFunc("GET /admin/audit", s.page("audit", "Journal", loadAudit))
	mux.HandleFunc("GET /admin/status", s.page("status", "État", s.loadStatus))
	mux.HandleFunc("POST /admin/do/{action}", s.do)
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
	s.setCookie(w, sessionCookie, signed, "/admin", sessionTTL, http.SameSiteLaxMode)
	next := ld.Next
	if next == "" {
		next = "/admin/server"
	}
	http.Redirect(w, r, next, http.StatusFound)
}

func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.message(w, r, http.StatusForbidden, "Demande refusée", "Jeton de formulaire invalide : rechargez la page.")
		return
	}
	if u, err := s.session(r); err == nil {
		if f, ok := s.Users.(interface{ Forget(string) }); ok {
			f.Forget(u.ID)
		}
	}
	s.clearCookie(w, sessionCookie, "/admin")
	s.message(w, r, http.StatusOK, "Déconnecté", "Vous êtes déconnecté de l'administration.")
}

// session renvoie l'utilisateur connecté, encore actif.
func (s *Service) session(r *http.Request) (store.User, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return store.User{}, err
	}
	claims, err := s.Keys.Verify(c.Value, tokens.TypeState, s.Config.Issuer(), audSession, s.now())
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
	Title   string
	Page    string
	Server  string
	User    store.User
	CSRF    string
	Flash   string
	Error   string
	Path    string
	Data    any
	Version string
	Edition string
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
		v.Flash, v.Error = s.takeFlash(w, r)
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
	v := view{Title: title, Page: "message", Server: "Serveur Lattis", Error: msg}
	if status < 400 {
		v.Error, v.Flash = "", msg
	}
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
	switch pe.Code[:2] {
	case "42", "22", "23", "P0", "55", "2B":
		return pe.Message, true
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
}

func (s *Service) setFlash(w http.ResponseWriter, f flash) {
	b, _ := json.Marshal(f)
	s.setCookie(w, flashCookie, base64.RawURLEncoding.EncodeToString(b), "/admin", time.Minute, http.SameSiteStrictMode)
}

func (s *Service) takeFlash(w http.ResponseWriter, r *http.Request) (string, string) {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return "", ""
	}
	s.clearCookie(w, flashCookie, "/admin")
	b, err := base64.RawURLEncoding.DecodeString(c.Value)
	var f flash
	if err != nil || json.Unmarshal(b, &f) != nil {
		return "", ""
	}
	return f.OK, f.Err
}

// safeBack n'accepte qu'un chemin local de l'administration.
func safeBack(p string) string {
	if !strings.HasPrefix(p, "/admin/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "\\\r\n") {
		return ""
	}
	return p
}

func randomString() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
