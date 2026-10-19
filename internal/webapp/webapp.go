// Package webapp porte l'édition web (spécification §14) : pour chaque
// utilisateur connecté, lattisd lance un processus lattis-engine (le moteur
// de l'application, compilé sans Wails) et lui relaie /app/ (frontend,
// WebSocket, envois et téléchargements). Un processus par utilisateur isole
// les sessions : le moteur garde des états globaux au processus.
//
// Le moteur se connecte à PostgreSQL sous l'identité de l'utilisateur : il
// demande ses jetons à lattisd (GET /engine/token, secret propre au
// processus), donc la RLS s'applique comme sur un poste de bureau.
package webapp

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
	tokenTTL     = 5 * time.Minute
	startTimeout = 30 * time.Second
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
	// TokenURL : adresse de /engine/token joignable par les moteurs
	// (bouclage local vers l'écoute de lattisd).
	TokenURL string
	Now      func() time.Time

	mu      sync.Mutex
	engines map[string]*engine // par utilisateur
	secrets map[string]string  // secret → utilisateur
	uids    map[int]string     // UID système → utilisateur (LATTIS_WEB_ENGINE_UIDS)
}

type engine struct {
	userID string
	secret string
	uid    int // UID système du moteur ; -1 : celui de lattisd
	cmd    *exec.Cmd
	stdin  io.Closer
	proxy  *httputil.ReverseProxy
	done   chan struct{}
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

// Routes enregistre /app/ et /engine/token.
func (m *Manager) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/app/", m.app)
	mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/app/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /engine/token", m.token)
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
	e := &engine{userID: u.ID, secret: randomSecret(), uid: -1, done: make(chan struct{}), started: make(chan struct{})}
	if r := m.Config.WebEngineUIDs; r[1] > 0 {
		if m.uids == nil {
			m.uids = map[int]string{}
		}
		uid, ok := pickUID(r[0], r[1], m.uids, u.ID)
		if !ok {
			m.mu.Unlock()
			return nil, ErrBusy
		}
		e.uid = uid
		m.uids[uid] = u.ID
	}
	m.engines[u.ID] = e
	m.secrets[e.secret] = u.ID
	m.mu.Unlock()

	// Le démarrage continue même si la requête qui l'a lancé est abandonnée.
	go func() {
		e.startErr = m.start(e)
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

func (m *Manager) start(e *engine) error {
	dir := filepath.Join(m.Config.WebCacheDir, e.userID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if e.uid >= 0 {
		// Chaque moteur sous son propre UID : il n'atteint que son répertoire
		// (le parent reste traversable, pas lisible), ni les clés ni les
		// secrets de lattisd, ni les autres moteurs.
		if err := os.Chmod(m.Config.WebCacheDir, 0o711); err != nil {
			return err
		}
		if err := ownTree(dir, e.uid); err != nil {
			return fmt.Errorf("cache du moteur : %w", err)
		}
	}
	cmd := exec.Command(m.Config.WebEngine,
		"--addr", "127.0.0.1:0", "--dir", dir, "--base-path", "/app",
		"--server", m.Config.Issuer(), "--token-url", m.TokenURL, "--idle", m.Config.WebIdle.String())
	// Environnement minimal : rien des secrets de lattisd.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "XDG_CONFIG_HOME=" + filepath.Join(dir, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(dir, ".cache"), "XDG_DATA_HOME=" + filepath.Join(dir, ".local", "share"),
		"LATTIS_ENGINE_SECRET=" + e.secret}
	cmd.Dir = dir
	isolate(cmd, e.uid)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = &logWriter{user: e.userID}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("lancement de %s : %w", m.Config.WebEngine, err)
	}
	e.cmd, e.stdin = cmd, stdin
	go func() {
		_ = cmd.Wait()
		close(e.done)
		m.forget(e)
		slog.Info("moteur web arrêté", "user", e.userID)
	}()

	addr := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		found := false
		for sc.Scan() {
			line := sc.Text()
			if a, ok := strings.CutPrefix(line, "LISTENING "); ok && !found {
				found = true
				addr <- strings.TrimSpace(a)
				continue
			}
			slog.Info("moteur web", "user", e.userID, "out", line)
		}
	}()
	select {
	case a := <-addr:
		host, _, err := net.SplitHostPort(a)
		if err != nil || (host != "127.0.0.1" && host != "::1" && host != "localhost") {
			_ = cmd.Process.Kill()
			return fmt.Errorf("adresse du moteur inattendue : %q", a)
		}
		target := &url.URL{Scheme: "http", Host: a}
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
		slog.Info("moteur web démarré", "user", e.userID, "addr", a)
		return nil
	case <-e.done:
		return errors.New("le moteur s'est arrêté au démarrage")
	case <-time.After(startTimeout):
		_ = cmd.Process.Kill()
		return errors.New("le moteur n'a pas annoncé son adresse à temps")
	}
}

func (m *Manager) forget(e *engine) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.engines[e.userID] == e {
		delete(m.engines, e.userID)
	}
	delete(m.secrets, e.secret)
	if e.uid >= 0 && m.uids[e.uid] == e.userID {
		delete(m.uids, e.uid)
	}
}

// pickUID : UID de la plage pour un utilisateur, le même d'une fois sur
// l'autre tant qu'il est libre (son cache n'a alors pas à changer de
// propriétaire), sinon le suivant libre.
func pickUID(first, count int, used map[int]string, user string) (int, bool) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(user))
	start := int(h.Sum32() % uint32(count))
	for i := 0; i < count; i++ {
		uid := first + (start+i)%count
		if _, taken := used[uid]; !taken {
			return uid, true
		}
	}
	return 0, false
}

// Stop demande l'arrêt de tous les moteurs (fermeture de stdin) et attend.
func (m *Manager) Stop(ctx context.Context) {
	m.mu.Lock()
	var all []*engine
	for _, e := range m.engines {
		all = append(all, e)
	}
	m.mu.Unlock()
	for _, e := range all {
		if e.stdin != nil {
			_ = e.stdin.Close()
		}
	}
	for _, e := range all {
		select {
		case <-e.done:
		case <-ctx.Done():
			if e.cmd != nil && e.cmd.Process != nil {
				_ = e.cmd.Process.Kill()
			}
		}
	}
}

// token remet un jeton d'accès PostgreSQL au moteur d'un utilisateur.
func (m *Manager) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		http.NotFound(w, r)
		return
	}
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

// logWriter recopie la sortie d'erreur du moteur dans le journal.
type logWriter struct{ user string }

func (l *logWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line != "" {
			slog.Warn("moteur web", "user", l.user, "err", line)
		}
	}
	return len(p), nil
}
