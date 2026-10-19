// Package webapp porte l'édition web (spécification §14) : pour chaque
// utilisateur connecté, lattisd lance un moteur lattis-engine (le moteur de
// l'application, compilé sans Wails) et lui relaie /app/ (frontend,
// WebSocket, envois et téléchargements). Un moteur par utilisateur isole
// les sessions : le moteur garde des états globaux au processus.
//
// Le moteur est lancé par un Launcher : processus local, conteneur Docker
// (mini lanceur lattis-launcher) ou pod Kubernetes (LATTIS_WEB_LAUNCHER).
//
// Le moteur se connecte à PostgreSQL sous l'identité de l'utilisateur : il
// demande ses jetons à lattisd (GET /engine/token, secret propre au
// moteur), donc la RLS s'applique comme sur un poste de bureau.
package webapp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Be-Lattis/lattis-server/internal/config"
	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/tokens"
)

// ClientID est le client des jetons remis aux moteurs.
const ClientID = "lattis-web"

const (
	tokenTTL = 5 * time.Minute
	// startTimeout : démarrage d'un processus ; un conteneur ou un pod
	// (image à tirer, volume à monter) a droit à containerTimeout.
	startTimeout     = 30 * time.Second
	containerTimeout = 2 * time.Minute
)

// ErrBusy : nombre maximal de moteurs atteint.
var ErrBusy = errors.New("trop de sessions web ouvertes")

// Users lit un utilisateur (store.Store en production).
type Users interface {
	User(ctx context.Context, id string) (store.User, error)
}

// Manager lance, relaie et arrête les moteurs.
type Manager struct {
	Config *config.Config
	Users  Users
	Keys   *tokens.KeySet
	// Session renvoie l'utilisateur connecté (session de l'administration,
	// valable pour tout le site).
	Session func(*http.Request) (store.User, error)
	// TokenURL : adresse de /engine/token joignable par les moteurs.
	TokenURL string
	// Launcher : comment lancer les moteurs ; nil : processus locaux
	// (Config.WebEngine, Config.WebCacheDir, Config.WebEngineUIDs).
	Launcher Launcher
	Now      func() time.Time

	mu      sync.Mutex
	engines map[string]*engine // par utilisateur
	secrets map[string]string  // secret → utilisateur
}

type engine struct {
	userID string
	secret string
	inst   Instance
	proxy  *httputil.ReverseProxy
	// started se ferme une fois le démarrage fini (startErr dit comment).
	started  chan struct{}
	startErr error
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) launcher() Launcher {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Launcher == nil {
		m.Launcher = &ProcessLauncher{Bin: m.Config.WebEngine, CacheDir: m.Config.WebCacheDir, UIDs: m.Config.WebEngineUIDs}
	}
	return m.Launcher
}

// Routes enregistre /app/ et /engine/token. Sur l'écoute publique,
// /engine/token ne répond qu'en bouclage local (moteurs en processus) ; les
// moteurs en conteneur passent par TokenHandler, sur une écoute interne.
func (m *Manager) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/app/", m.app)
	mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/app/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /engine/token", func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			http.NotFound(w, r)
			return
		}
		m.token(w, r)
	})
}

// TokenHandler sert /engine/token aux moteurs en conteneur, sur une écoute
// interne (LATTIS_WEB_ENGINE_LISTEN) que seuls les moteurs joignent.
func (m *Manager) TokenHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /engine/token", m.token)
	return mux
}

// Init retire les moteurs laissés par une instance précédente de lattisd.
func (m *Manager) Init(ctx context.Context) {
	if c, ok := m.launcher().(Cleaner); ok {
		if err := c.Cleanup(ctx); err != nil {
			slog.Warn("moteurs web : nettoyage au démarrage", "err", err)
		}
	}
}

// Count renvoie le nombre de moteurs en cours (mesures, état).
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.engines)
}

func (m *Manager) app(w http.ResponseWriter, r *http.Request) {
	u, err := m.Session(r)
	if err != nil {
		if r.Method != http.MethodGet || isWebSocket(r) {
			http.Error(w, "session expirée", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/admin/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	// WebSocket et envois : même origine seulement (le moteur vérifie aussi).
	if o := r.Header.Get("Origin"); o != "" && !m.sameOrigin(o) {
		http.Error(w, "origine refusée", http.StatusForbidden)
		return
	}
	e, err := m.engine(r.Context(), u)
	if errors.Is(err, ErrBusy) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "Le serveur a atteint son nombre maximal de sessions web. Réessayez plus tard.", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		slog.Error("moteur web", "user", u.ID, "err", err)
		http.Error(w, "Le moteur Lattis n'a pas pu démarrer.", http.StatusBadGateway)
		return
	}
	// WebSocket, envois et téléchargements durent plus que les délais du
	// serveur public.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	// Le cookie de session n'a rien à faire chez le moteur.
	r.Header.Del("Cookie")
	e.proxy.ServeHTTP(w, r)
}

func (m *Manager) sameOrigin(origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Scheme == m.Config.PublicURL.Scheme && strings.EqualFold(u.Host, m.Config.PublicURL.Host)
}

func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// engine renvoie le moteur de l'utilisateur, lancé au besoin.
func (m *Manager) engine(ctx context.Context, u store.User) (*engine, error) {
	l := m.launcher()
	m.mu.Lock()
	if e, ok := m.engines[u.ID]; ok {
		m.mu.Unlock()
		select {
		case <-e.started:
			if e.startErr != nil {
				return nil, e.startErr
			}
			return e, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if m.engines == nil {
		m.engines, m.secrets = map[string]*engine{}, map[string]string{}
	}
	if len(m.engines) >= m.Config.WebMaxEngines {
		m.mu.Unlock()
		return nil, ErrBusy
	}
	// Réservé pendant le démarrage : une seconde requête attend le même.
	e := &engine{userID: u.ID, secret: randomSecret(), started: make(chan struct{})}
	m.engines[u.ID] = e
	m.secrets[e.secret] = u.ID
	m.mu.Unlock()

	// Le démarrage continue même si la requête qui l'a lancé est abandonnée.
	go func() {
		e.startErr = m.start(l, e)
		if e.startErr != nil {
			m.forget(e)
		}
		close(e.started)
	}()
	select {
	case <-e.started:
		if e.startErr != nil {
			return nil, e.startErr
		}
		return e, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *Manager) start(l Launcher, e *engine) error {
	timeout := containerTimeout
	if _, ok := l.(*ProcessLauncher); ok {
		timeout = startTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	inst, err := l.Start(ctx, Spec{UserID: e.userID, Secret: e.secret, Server: m.Config.Issuer(),
		TokenURL: m.TokenURL, Idle: m.Config.WebIdle})
	if err != nil {
		return err
	}
	e.inst = inst
	go func() {
		<-inst.Done()
		m.forget(e)
		slog.Info("moteur web arrêté", "user", e.userID)
	}()
	target := &url.URL{Scheme: "http", Host: inst.Addr()}
	p := httputil.NewSingleHostReverseProxy(target)
	p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Warn("relais vers le moteur", "user", e.userID, "err", err)
		http.Error(w, "Le moteur Lattis ne répond pas.", http.StatusBadGateway)
	}
	p.ModifyResponse = func(resp *http.Response) error {
		for _, k := range []string{"Content-Security-Policy", "X-Frame-Options", "Strict-Transport-Security", "Set-Cookie"} {
			resp.Header.Del(k)
		}
		return nil
	}
	e.proxy = p
	slog.Info("moteur web démarré", "user", e.userID, "addr", inst.Addr())
	return nil
}

func (m *Manager) forget(e *engine) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.engines[e.userID] == e {
		delete(m.engines, e.userID)
	}
	delete(m.secrets, e.secret)
}

// Stop demande l'arrêt de tous les moteurs et attend.
func (m *Manager) Stop(ctx context.Context) {
	m.mu.Lock()
	var all []*engine
	for _, e := range m.engines {
		if e.inst != nil {
			all = append(all, e)
		}
	}
	m.mu.Unlock()
	for _, e := range all {
		e.inst.Stop()
	}
	for _, e := range all {
		select {
		case <-e.inst.Done():
		case <-ctx.Done():
			e.inst.Kill()
		}
	}
}

// token remet un jeton d'accès PostgreSQL au moteur d'un utilisateur.
func (m *Manager) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || secret == "" {
		http.Error(w, "secret manquant", http.StatusUnauthorized)
		return
	}
	m.mu.Lock()
	var userID string
	for s, id := range m.secrets {
		if subtle.ConstantTimeCompare([]byte(s), []byte(secret)) == 1 {
			userID = id
		}
	}
	m.mu.Unlock()
	if userID == "" {
		http.Error(w, "secret inconnu", http.StatusUnauthorized)
		return
	}
	u, err := m.Users.User(r.Context(), userID)
	if err != nil || u.Disabled {
		http.Error(w, "compte désactivé", http.StatusForbidden)
		return
	}
	now := m.now()
	access, err := m.Keys.Sign(tokens.TypeAccess, tokens.Claims{
		Issuer: m.Config.Issuer(), Subject: u.ID, Audience: tokens.Audience{tokens.AudiencePostgres},
		IssuedAt: now.Unix(), NotBefore: now.Unix(), ExpiresAt: now.Add(tokenTTL).Unix(), ID: tokens.NewID(),
		ClientID: ClientID, Scope: "openid lattis", PGRole: u.PGRole, Email: u.Email,
	})
	if err != nil {
		http.Error(w, "signature", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "token_type": "Bearer",
		"pg_role": u.PGRole, "expires_in": int(tokenTTL.Seconds())})
}

func randomSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
