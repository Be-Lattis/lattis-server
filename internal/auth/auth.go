// Package auth fait de lattisd l'unique émetteur OIDC connu de PostgreSQL
// (§7) : flux code d'autorisation + PKCE pour les postes, délégation aux
// fournisseurs réels, comptes à mot de passe, inscription et jetons.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lalamefine/lattis/protocol"
	"golang.org/x/oauth2"

	"github.com/Be-Lattis/lattis-server/internal/config"
	"github.com/Be-Lattis/lattis-server/internal/metrics"
	"github.com/Be-Lattis/lattis-server/internal/password"
	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/tokens"
	"github.com/Be-Lattis/lattis-server/internal/upstream"
)

// DesktopClientID est le client public de l'application de bureau.
const DesktopClientID = protocol.DesktopClientID

// AdminClientID est le client interne de l'interface d'administration. Ses
// codes ne s'échangent pas sur /oauth/token : lattisd les consomme lui-même
// sur /admin/callback.
const AdminClientID = "lattis-admin"

// Scopes acceptés ; « openid lattis » est la valeur attendue par PostgreSQL.
var knownScopes = []string{"openid", "lattis", "offline_access", "email", "profile"}

const (
	DefaultScope = protocol.DefaultScope
	codeTTL      = 2 * time.Minute
	requestTTL   = 10 * time.Minute
	loginCookie  = "lattis_login"
	csrfCookie   = "lattis_csrf"
	// freshCookie : mot de passe choisi à l'instant sur /setup ; la connexion
	// suivante à l'administration ne le redemande pas.
	freshCookie = "lattis_fresh"
	audFresh    = "lattisd:fresh-login"
)

//go:embed web/*.html web/lattis.css
var webFS embed.FS

var pages = map[string]*template.Template{}

// SourceURL : code source proposé au pied des pages (AGPL-3.0, §13) ; fixé
// par lattisd au démarrage (LATTIS_SOURCE_URL).
var SourceURL = "https://github.com/Be-Lattis/lattis-server"

func init() {
	funcs := template.FuncMap{"sourceURL": func() string { return SourceURL }}
	for _, name := range []string{"choose", "password", "setup", "message"} {
		pages[name] = template.Must(template.New("").Funcs(funcs).ParseFS(webFS, "web/page.html", "web/"+name+".html"))
	}
}

// Service porte les points /oauth/*, /login/*, /callback/* et /setup.
type Service struct {
	Config   *config.Config
	Store    *store.Store
	Keys     *tokens.KeySet
	Upstream *upstream.Registry
	Metrics  *metrics.Metrics
	Now      func() time.Time
	// AdminURL : où envoyer un propriétaire qui vient de choisir son mot de
	// passe, déjà connecté (vide : pas d'interface d'administration).
	AdminURL string
}

// Routes enregistre les points d'accès.
func (s *Service) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /oauth/authorize", s.authorize)
	mux.HandleFunc("POST /oauth/token", s.token)
	mux.HandleFunc("POST /oauth/revoke", s.revoke)
	mux.HandleFunc("GET /login/{provider}", s.login)
	mux.HandleFunc("POST /login/{provider}", s.passwordLogin)
	mux.HandleFunc("GET /callback/{provider}", s.callback)
	mux.HandleFunc("GET /setup", s.setupForm)
	mux.HandleFunc("POST /setup", s.setupSubmit)
	mux.HandleFunc("GET /static/lattis.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		http.ServeFileFS(w, r, webFS, "web/lattis.css")
	})
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// --- Demande d'autorisation ------------------------------------------------------

// authRequest est la demande du client, transportée signée entre les pages.
type authRequest struct {
	ClientID      string `json:"client_id"`
	RedirectURI   string `json:"redirect_uri"`
	State         string `json:"state"`
	CodeChallenge string `json:"code_challenge"`
	Scope         string `json:"scope"`
	Nonce         string `json:"nonce,omitempty"`
	LoginHint     string `json:"login_hint,omitempty"`
}

// validRedirect : bouclage local (RFC 8252 §7.3, port quelconque) ou URI
// déclarée dans LATTIS_EXTRA_REDIRECT_URIS.
func (s *Service) validRedirect(clientID, raw string) bool {
	if clientID == AdminClientID {
		return raw == s.Config.URL("/admin/callback")
	}
	if clientID != DesktopClientID {
		return false
	}
	if slices.Contains(s.Config.ExtraRedirects, raw) {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Fragment != "" {
		return false
	}
	switch u.Hostname() {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

func normalizeScope(raw string) (string, bool) {
	if raw == "" {
		return DefaultScope, true
	}
	parts := strings.Fields(raw)
	for _, p := range parts {
		if !slices.Contains(knownScopes, p) {
			return "", false
		}
	}
	if !slices.Contains(parts, "openid") || !slices.Contains(parts, "lattis") {
		return "", false
	}
	return strings.Join(parts, " "), true
}

func (s *Service) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req := authRequest{
		ClientID: q.Get("client_id"), RedirectURI: q.Get("redirect_uri"), State: q.Get("state"),
		CodeChallenge: q.Get("code_challenge"), Nonce: q.Get("nonce"), LoginHint: q.Get("login_hint"),
	}
	// Sans client ni redirection valides, on ne redirige pas (RFC 6749 §4.1.2.1).
	if !s.validRedirect(req.ClientID, req.RedirectURI) {
		s.render(w, r, http.StatusBadRequest, "message", view{Title: "Demande invalide",
			Error: "Client ou adresse de retour non reconnus."})
		return
	}
	fail := func(code, desc string) {
		redirectError(w, r, req.RedirectURI, req.State, code, desc, s.Config.Issuer())
	}
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "seul response_type=code est pris en charge")
		return
	}
	if q.Get("code_challenge_method") != "S256" || len(req.CodeChallenge) < 43 || len(req.CodeChallenge) > 128 {
		fail("invalid_request", "PKCE S256 obligatoire")
		return
	}
	scope, ok := normalizeScope(q.Get("scope"))
	if !ok {
		fail("invalid_scope", "scope attendu : openid lattis")
		return
	}
	req.Scope = scope
	signed, err := s.signRequest(req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if req.ClientID == AdminClientID && s.freshLogin(w, r, req) {
		return
	}
	providers, err := s.Store.Providers(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Fournisseur choisi par le client (identifiant ou type).
	if want := q.Get("provider"); want != "" {
		for _, p := range providers {
			if p.ID == want || p.Kind == want {
				http.Redirect(w, r, "/login/"+p.ID+"?req="+url.QueryEscape(signed), http.StatusFound)
				return
			}
		}
	}
	if len(providers) == 1 {
		http.Redirect(w, r, "/login/"+providers[0].ID+"?req="+url.QueryEscape(signed), http.StatusFound)
		return
	}
	v := view{Title: "Connexion"}
	for _, p := range providers {
		v.Providers = append(v.Providers, providerLink{Label: p.Label, URL: "/login/" + p.ID + "?req=" + url.QueryEscape(signed)})
	}
	s.render(w, r, http.StatusOK, "choose", v)
}

func (s *Service) signRequest(req authRequest) (string, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	now := s.now()
	return s.Keys.Sign(tokens.TypeState, tokens.Claims{
		Issuer: s.Config.Issuer(), Audience: tokens.Audience{"lattisd:authorize"},
		IssuedAt: now.Unix(), ExpiresAt: now.Add(requestTTL).Unix(), Data: data,
	})
}

func (s *Service) parseRequest(signed string) (authRequest, error) {
	var req authRequest
	c, err := s.Keys.Verify(signed, tokens.TypeState, s.Config.Issuer(), "lattisd:authorize", s.now())
	if err != nil {
		return req, err
	}
	err = json.Unmarshal(c.Data, &req)
	return req, err
}

// freshLogin termine la demande sans formulaire si le navigateur vient de
// choisir son mot de passe sur /setup (cookie à usage unique). Faux : rien
// fait, la connexion habituelle suit.
func (s *Service) freshLogin(w http.ResponseWriter, r *http.Request, req authRequest) bool {
	ck, err := r.Cookie(freshCookie)
	if err != nil {
		return false
	}
	s.clearCookie(w, freshCookie, "/oauth/authorize")
	claims, err := s.Keys.Verify(ck.Value, tokens.TypeState, s.Config.Issuer(), audFresh, s.now())
	if err != nil {
		return false
	}
	ctx := r.Context()
	u, err := s.Store.User(ctx, claims.Subject)
	if err != nil {
		return false
	}
	acct, err := s.Store.PasswordAccount(ctx, u.Email)
	if err != nil || acct.UserID != u.ID || acct.Hash == "" {
		return false
	}
	p, err := s.Store.Provider(ctx, acct.ProviderID)
	if err != nil || p.Kind != "password" {
		return false
	}
	s.complete(w, r, p, req, store.Identity{ProviderID: acct.ProviderID, Subject: acct.UserID, Email: u.Email, EmailVerified: true})
	return true
}

// --- Connexion auprès d'un fournisseur ----------------------------------------------

type loginState struct {
	SID      string `json:"sid"`
	Verifier string `json:"verifier"`
	Nonce    string `json:"nonce"`
	Provider string `json:"provider"`
	Req      string `json:"req"`
}

func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	signed := r.URL.Query().Get("req")
	req, err := s.parseRequest(signed)
	if err != nil {
		s.expired(w, r)
		return
	}
	p, err := s.Store.Provider(r.Context(), r.PathValue("provider"))
	if err != nil {
		s.render(w, r, http.StatusNotFound, "message", view{Title: "Connexion", Error: "Ce moyen de connexion n'est pas disponible."})
		return
	}
	if p.Kind == "password" {
		s.passwordForm(w, r, p, signed, req.LoginHint, "")
		return
	}
	c, err := s.Upstream.Get(r.Context(), p)
	if err != nil {
		slog.Error("fournisseur indisponible", "provider", p.Label, "err", err)
		s.render(w, r, http.StatusBadGateway, "message", view{Title: "Connexion",
			Error: "Le fournisseur " + p.Label + " ne répond pas. Réessayez plus tard ou choisissez un autre moyen de connexion."})
		return
	}
	st := loginState{SID: tokens.NewID(), Verifier: oauth2.GenerateVerifier(), Nonce: tokens.NewID(), Provider: p.ID, Req: signed}
	data, _ := json.Marshal(st)
	now := s.now()
	cookie, err := s.Keys.Sign(tokens.TypeState, tokens.Claims{
		Issuer: s.Config.Issuer(), Audience: tokens.Audience{"lattisd:login"},
		IssuedAt: now.Unix(), ExpiresAt: now.Add(requestTTL).Unix(), Data: data,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.setCookie(w, loginCookie, cookie, "/callback/", requestTTL)
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(st.Verifier), oauth2.SetAuthURLParam("nonce", st.Nonce)}
	if req.LoginHint != "" {
		opts = append(opts, oauth2.SetAuthURLParam("login_hint", req.LoginHint))
	}
	if p.Kind == "google_workspace" {
		// Indication seulement : le domaine est contrôlé au retour (hd).
		if ts, _ := s.Store.Tenants(r.Context(), p.ID); len(ts) == 1 && ts[0].Kind == "domain" {
			opts = append(opts, oauth2.SetAuthURLParam("hd", ts[0].Value))
		}
	}
	http.Redirect(w, r, c.OAuth2.AuthCodeURL(st.SID, opts...), http.StatusFound)
}

func (s *Service) callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ck, err := r.Cookie(loginCookie)
	if err != nil {
		s.expired(w, r)
		return
	}
	s.clearCookie(w, loginCookie, "/callback/")
	cl, err := s.Keys.Verify(ck.Value, tokens.TypeState, s.Config.Issuer(), "lattisd:login", s.now())
	if err != nil {
		s.expired(w, r)
		return
	}
	var st loginState
	if json.Unmarshal(cl.Data, &st) != nil || st.Provider != r.PathValue("provider") ||
		subtle.ConstantTimeCompare([]byte(st.SID), []byte(r.URL.Query().Get("state"))) != 1 {
		s.expired(w, r)
		return
	}
	req, err := s.parseRequest(st.Req)
	if err != nil {
		s.expired(w, r)
		return
	}
	p, err := s.Store.Provider(ctx, st.Provider)
	if err != nil {
		s.expired(w, r)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		s.Metrics.Logins.WithLabelValues(p.Kind, metrics.ResultError).Inc()
		redirectError(w, r, req.RedirectURI, req.State, "access_denied", "connexion refusée par "+p.Label, s.Config.Issuer())
		return
	}
	c, err := s.Upstream.Get(ctx, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	tok, err := c.OAuth2.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		s.loginError(w, r, p, err)
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	claims, err := c.Verify(ctx, rawID, st.Nonce)
	if err != nil {
		s.loginError(w, r, p, err)
		return
	}
	tenants, err := s.Store.Tenants(ctx, p.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	id, err := upstream.Admit(p, tenants, claims)
	if err != nil {
		s.Metrics.Logins.WithLabelValues(p.Kind, metrics.ResultTenant).Inc()
		slog.Info("connexion refusée", "provider", p.Label, "email", claims.Email, "reason", err)
		s.render(w, r, http.StatusForbidden, "message", view{Title: "Accès refusé",
			Error: "Votre compte " + claims.Email + " n'est pas autorisé sur ce serveur."})
		return
	}
	s.complete(w, r, p, req, id)
}

func (s *Service) loginError(w http.ResponseWriter, r *http.Request, p store.Provider, err error) {
	s.Metrics.Logins.WithLabelValues(p.Kind, metrics.ResultError).Inc()
	slog.Warn("échec de connexion", "provider", p.Label, "err", err)
	s.render(w, r, http.StatusBadGateway, "message", view{Title: "Connexion",
		Error: "La connexion avec " + p.Label + " a échoué. Réessayez depuis Lattis."})
}

// complete inscrit l'utilisateur et renvoie un code d'autorisation au client.
func (s *Service) complete(w http.ResponseWriter, r *http.Request, p store.Provider, req authRequest, id store.Identity) {
	e, err := s.Store.Enroll(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	switch e.Status {
	case "ok":
	case "disabled":
		s.Metrics.Logins.WithLabelValues(p.Kind, metrics.ResultDisabled).Inc()
		s.render(w, r, http.StatusForbidden, "message", view{Title: "Compte désactivé",
			Error: "Votre compte est désactivé. Contactez l'administrateur du serveur."})
		return
	default:
		s.Metrics.Logins.WithLabelValues(p.Kind, metrics.ResultInvite).Inc()
		s.render(w, r, http.StatusForbidden, "message", view{Title: "Accès sur invitation",
			Error: "Ce serveur est sur invitation et aucune invitation n'existe pour " + id.Email + "."})
		return
	}
	s.Metrics.Logins.WithLabelValues(p.Kind, metrics.ResultOK).Inc()
	if e.Mode != "existing" {
		s.Metrics.Enrollments.WithLabelValues(p.Kind, e.Mode).Inc()
	}
	code := tokens.NewID() + tokens.NewID()
	err = s.Store.SaveCode(r.Context(), code, store.Code{
		UserID: e.UserID, ClientID: req.ClientID, RedirectURI: req.RedirectURI,
		CodeChallenge: req.CodeChallenge, Scope: req.Scope, Nonce: req.Nonce,
	}, codeTTL)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	u, _ := url.Parse(req.RedirectURI)
	q := u.Query()
	q.Set("code", code)
	if req.State != "" {
		q.Set("state", req.State)
	}
	q.Set("iss", s.Config.Issuer()) // RFC 9207
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// --- Comptes à mot de passe ------------------------------------------------------------

func (s *Service) passwordForm(w http.ResponseWriter, r *http.Request, p store.Provider, signed, email, msg string) {
	if req, err := s.parseRequest(signed); err == nil {
		allowFormTarget(w, req.RedirectURI)
	}
	csrf := s.csrf(w, r)
	v := view{Title: "Connexion", Error: msg, Action: "/login/" + p.ID, Req: signed, CSRF: csrf, Email: email}
	if ps, err := s.Store.Providers(r.Context()); err == nil && len(ps) > 1 {
		if req, err := s.parseRequest(signed); err == nil {
			v.Back = "/oauth/authorize?" + url.Values{
				"client_id": {req.ClientID}, "redirect_uri": {req.RedirectURI}, "state": {req.State},
				"response_type": {"code"}, "code_challenge": {req.CodeChallenge}, "code_challenge_method": {"S256"},
				"scope": {req.Scope}, "nonce": {req.Nonce},
			}.Encode()
		}
	}
	status := http.StatusOK
	if msg != "" {
		status = http.StatusUnauthorized
	}
	s.render(w, r, status, "password", v)
}

func (s *Service) passwordLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.checkCSRF(r) {
		s.expired(w, r)
		return
	}
	signed := r.PostFormValue("req")
	req, err := s.parseRequest(signed)
	if err != nil {
		s.expired(w, r)
		return
	}
	p, err := s.Store.Provider(ctx, r.PathValue("provider"))
	if err != nil || p.Kind != "password" {
		s.expired(w, r)
		return
	}
	cfg, err := s.Store.Config(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	pw := r.PostFormValue("password")
	const bad = "Adresse ou mot de passe incorrect."
	acct, err := s.Store.PasswordAccount(ctx, email)
	if errors.Is(err, store.ErrNotFound) || (err == nil && acct.Hash == "") {
		_, _ = password.Verify(pw, password.Dummy)
		s.Metrics.Logins.WithLabelValues(p.Kind, metrics.ResultError).Inc()
		s.passwordForm(w, r, p, signed, email, bad)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if acct.LockedUntil != nil && acct.LockedUntil.After(s.now()) {
		s.Metrics.Logins.WithLabelValues(p.Kind, metrics.ResultError).Inc()
		s.passwordForm(w, r, p, signed, email, "Trop d'essais : compte verrouillé quelques minutes.")
		return
	}
	ok, err := password.Verify(pw, acct.Hash)
	if err != nil || !ok {
		if err := s.Store.PasswordFailed(ctx, acct.UserID, cfg.PasswordMaxFailures, cfg.PasswordLock); err != nil {
			slog.Error("compteur d'échecs", "err", err)
		}
		s.Metrics.Logins.WithLabelValues(p.Kind, metrics.ResultError).Inc()
		s.passwordForm(w, r, p, signed, email, bad)
		return
	}
	if err := s.Store.PasswordSucceeded(ctx, acct.UserID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.complete(w, r, p, req, store.Identity{ProviderID: acct.ProviderID, Subject: acct.UserID, Email: email, EmailVerified: true})
}

func (s *Service) setupForm(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	email, ok, err := s.Store.SetupTokenValid(r.Context(), token)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		s.render(w, r, http.StatusNotFound, "message", view{Title: "Lien expiré",
			Error: "Ce lien n'est plus valable. Demandez-en un nouveau à l'administrateur du serveur."})
		return
	}
	s.render(w, r, http.StatusOK, "setup", view{Title: "Choisir un mot de passe", Token: token, Email: email,
		CSRF: s.csrf(w, r), MinLength: password.MinLength})
}

func (s *Service) setupSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.expired(w, r)
		return
	}
	token := r.PostFormValue("token")
	email, ok, err := s.Store.SetupTokenValid(r.Context(), token)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		s.render(w, r, http.StatusNotFound, "message", view{Title: "Lien expiré",
			Error: "Ce lien n'est plus valable. Demandez-en un nouveau à l'administrateur du serveur."})
		return
	}
	// L'adresse prévue n'est qu'une suggestion : celle saisie la remplace.
	if e := strings.ToLower(strings.TrimSpace(r.PostFormValue("email"))); e != "" {
		email = e
	}
	pw := r.PostFormValue("password")
	retry := func(msg string) {
		s.render(w, r, http.StatusBadRequest, "setup", view{Title: "Choisir un mot de passe", Token: token, Email: email,
			CSRF: s.csrf(w, r), MinLength: password.MinLength, Error: msg})
	}
	if err := password.Check(pw); err != nil {
		retry(err.Error())
		return
	}
	if pw != r.PostFormValue("confirm") {
		retry("Les deux saisies sont différentes.")
		return
	}
	hash, err := password.Hash(pw)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	uid, err := s.Store.SetPasswordWithToken(r.Context(), token, hash, email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			retry("Ce lien n'est plus valable.")
			return
		}
		var pe *pgconn.PgError
		if errors.As(err, &pe) && (pe.Code == "22023" || pe.Code == "23505") {
			retry(strings.ToUpper(pe.Message[:1]) + pe.Message[1:] + ".")
			return
		}
		s.fail(w, r, err)
		return
	}
	// Un propriétaire arrive directement dans l'administration, connecté.
	if s.AdminURL != "" {
		owner, err := s.Store.ServerOwner(r.Context(), uid)
		if err != nil {
			slog.Error("propriétaire après choix du mot de passe", "err", err)
		}
		if owner {
			now := s.now()
			fresh, err := s.Keys.Sign(tokens.TypeState, tokens.Claims{Issuer: s.Config.Issuer(), Audience: tokens.Audience{audFresh},
				Subject: uid, IssuedAt: now.Unix(), ExpiresAt: now.Add(requestTTL).Unix()})
			if err == nil {
				s.setCookie(w, freshCookie, fresh, "/oauth/authorize", requestTTL)
				http.Redirect(w, r, s.AdminURL, http.StatusSeeOther)
				return
			}
			slog.Error("connexion après choix du mot de passe", "err", err)
		}
	}
	s.render(w, r, http.StatusOK, "message", view{Title: "Mot de passe enregistré",
		Message: "Vous pouvez maintenant vous connecter à ce serveur depuis Lattis."})
}

// --- Point de jeton --------------------------------------------------------------

// TokenResponse est la réponse de /oauth/token, partagée avec
// l'application (protocol). PGRole est le rôle PostgreSQL de la connexion.
type TokenResponse = protocol.TokenResponse

func (s *Service) token(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	clientID := r.PostFormValue("client_id")
	if clientID != DesktopClientID {
		tokenError(w, http.StatusUnauthorized, "invalid_client", "client inconnu")
		return
	}
	var (
		userID, scope, nonce, family string
		authTime                     int64
	)
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		c, err := s.Store.ConsumeCode(ctx, r.PostFormValue("code"))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				tokenError(w, http.StatusBadRequest, "invalid_grant", "code invalide, expiré ou déjà utilisé")
				return
			}
			s.tokenFail(w, err)
			return
		}
		if c.ClientID != clientID || c.RedirectURI != r.PostFormValue("redirect_uri") ||
			!pkceMatches(r.PostFormValue("code_verifier"), c.CodeChallenge) {
			tokenError(w, http.StatusBadRequest, "invalid_grant", "code, redirection ou vérificateur PKCE incorrects")
			return
		}
		userID, scope, nonce, authTime = c.UserID, c.Scope, c.Nonce, s.now().Unix()
	case "refresh_token":
		rt, err := s.Store.UseRefresh(ctx, r.PostFormValue("refresh_token"))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrReused) {
				if errors.Is(err, store.ErrReused) {
					slog.Warn("jeton de rafraîchissement réutilisé : famille révoquée")
				}
				tokenError(w, http.StatusBadRequest, "invalid_grant", "jeton de rafraîchissement invalide")
				return
			}
			s.tokenFail(w, err)
			return
		}
		if rt.ClientID != clientID {
			tokenError(w, http.StatusBadRequest, "invalid_grant", "jeton émis pour un autre client")
			return
		}
		userID, scope, family = rt.UserID, rt.Scope, rt.Family
	default:
		tokenError(w, http.StatusBadRequest, "unsupported_grant_type", "authorization_code ou refresh_token")
		return
	}
	u, err := s.Store.User(ctx, userID)
	if err != nil {
		s.tokenFail(w, err)
		return
	}
	if u.Disabled {
		tokenError(w, http.StatusBadRequest, "invalid_grant", "compte désactivé")
		return
	}
	cfg, err := s.Store.Config(ctx)
	if err != nil {
		s.tokenFail(w, err)
		return
	}
	resp, refresh, err := s.issue(u, clientID, scope, nonce, authTime, cfg.AccessTokenTTL)
	if err != nil {
		s.tokenFail(w, err)
		return
	}
	if err := s.Store.SaveRefresh(ctx, refresh, store.Refresh{Family: family, UserID: u.ID, ClientID: clientID, Scope: scope},
		cfg.RefreshTokenTTL); err != nil {
		s.tokenFail(w, err)
		return
	}
	resp.RefreshToken = refresh
	writeJSON(w, http.StatusOK, resp)
}

// issue signe le jeton d'accès (pour PostgreSQL) et le jeton d'identité.
func (s *Service) issue(u store.User, clientID, scope, nonce string, authTime int64, ttl time.Duration) (TokenResponse, string, error) {
	now := s.now()
	access, err := s.Keys.Sign(tokens.TypeAccess, tokens.Claims{
		Issuer: s.Config.Issuer(), Subject: u.ID, Audience: tokens.Audience{tokens.AudiencePostgres},
		IssuedAt: now.Unix(), NotBefore: now.Unix(), ExpiresAt: now.Add(ttl).Unix(), ID: tokens.NewID(),
		ClientID: clientID, Scope: scope, PGRole: u.PGRole, Email: u.Email,
	})
	if err != nil {
		return TokenResponse{}, "", err
	}
	id, err := s.Keys.Sign(tokens.TypeID, tokens.Claims{
		Issuer: s.Config.Issuer(), Subject: u.ID, Audience: tokens.Audience{clientID},
		IssuedAt: now.Unix(), ExpiresAt: now.Add(ttl).Unix(), Nonce: nonce, AuthTime: authTime,
		Email: u.Email, Name: u.DisplayName, PGRole: u.PGRole,
	})
	if err != nil {
		return TokenResponse{}, "", err
	}
	return TokenResponse{AccessToken: access, TokenType: "Bearer", ExpiresIn: int64(ttl.Seconds()),
		IDToken: id, Scope: scope, PGRole: u.PGRole}, randomToken(), nil
}

func (s *Service) revoke(w http.ResponseWriter, r *http.Request) {
	// RFC 7009 : réponse 200 même pour un jeton inconnu.
	if t := r.PostFormValue("token"); t != "" {
		if err := s.Store.RevokeRefresh(r.Context(), t); err != nil {
			slog.Error("révocation", "err", err)
		}
	}
	w.WriteHeader(http.StatusOK)
}

func pkceMatches(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// --- Utilitaires HTTP ----------------------------------------------------------------

type providerLink struct{ Label, URL string }

type view struct {
	Title, Server, Logo, Error, Message string
	Providers                           []providerLink
	Action, Req, CSRF, Email, Back      string
	Token                               string
	MinLength                           int
}

func (s *Service) render(w http.ResponseWriter, r *http.Request, status int, page string, v view) {
	if cfg, err := s.Store.Config(r.Context()); err == nil {
		v.Server, v.Logo = cfg.DisplayName, cfg.LogoURL
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := pages[page].ExecuteTemplate(w, "page", v); err != nil {
		slog.Error("page", "page", page, "err", err)
	}
}

func (s *Service) expired(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusBadRequest, "message", view{Title: "Session expirée",
		Error: "La demande de connexion a expiré. Relancez la connexion depuis Lattis."})
}

func (s *Service) fail(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("erreur interne", "path", r.URL.Path, "err", err)
	s.render(w, r, http.StatusInternalServerError, "message", view{Title: "Erreur", Error: "Erreur interne du serveur."})
}

func (s *Service) tokenFail(w http.ResponseWriter, err error) {
	slog.Error("point de jeton", "err", err)
	tokenError(w, http.StatusInternalServerError, "server_error", "erreur interne")
}

func tokenError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func redirectError(w http.ResponseWriter, r *http.Request, redirect, state, code, desc, issuer string) {
	u, _ := url.Parse(redirect)
	q := u.Query()
	q.Set("error", code)
	q.Set("error_description", desc)
	if state != "" {
		q.Set("state", state)
	}
	q.Set("iss", issuer)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (s *Service) secureCookies() bool { return s.Config.PublicURL.Scheme == "https" }

func (s *Service) setCookie(w http.ResponseWriter, name, value, path string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: path, MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode})
}

func (s *Service) clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: path, MaxAge: -1,
		HttpOnly: true, Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode})
}

// csrf renvoie le jeton anti-CSRF (double soumission : cookie + champ).
func (s *Service) csrf(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) >= 22 {
		return c.Value
	}
	t := tokens.NewID()
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: t, Path: "/", MaxAge: int(requestTTL.Seconds()) * 3,
		HttpOnly: true, Secure: s.secureCookies(), SameSite: http.SameSiteStrictMode})
	return t
}

func (s *Service) checkCSRF(r *http.Request) bool {
	c, err := r.Cookie(csrfCookie)
	if err != nil || c.Value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(r.PostFormValue("csrf"))) == 1
}

// ClientIP renvoie l'adresse du client (pour la limitation de débit).
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		// Dernière adresse de la chaîne : celle que le mandataire de confiance
		// a ajoutée. Les premières sont fournies par le client et ne prouvent rien.
		if f := r.Header.Values("X-Forwarded-For"); len(f) > 0 {
			parts := strings.Split(f[len(f)-1], ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// allowFormTarget ajoute l'origine de retour du client à « form-action » :
// le formulaire de mot de passe se termine par une redirection vers
// l'application (http://127.0.0.1:<port>/callback), que les navigateurs
// bloquent sinon (CSP form-action s'applique aussi aux redirections), et le
// clic sur « Se connecter » semble ne rien faire.
func allowFormTarget(w http.ResponseWriter, redirectURI string) {
	u, err := url.Parse(redirectURI)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return
	}
	origin := u.Scheme + "://" + u.Host
	h := w.Header()
	if csp := h.Get("Content-Security-Policy"); csp != "" && !strings.Contains(csp, "form-action 'self' "+origin) {
		h.Set("Content-Security-Policy", strings.Replace(csp, "form-action 'self'", "form-action 'self' "+origin, 1))
	}
}
