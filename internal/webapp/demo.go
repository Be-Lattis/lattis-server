package webapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Démo publique (réglage public_demo) : /demo/ ouvre Lattis dans le
// navigateur sans compte. Chaque navigateur a son moteur (cookie
// lattis_demo), lancé avec --public-demo : ni serveur, ni jeton, ni secret,
// aucun accès à PostgreSQL ; le classeur n'est jamais enregistré sur le
// serveur (Ctrl+S le télécharge). Plafonds : celui de l'installation
// (LATTIS_WEB_MAX_ENGINES), la limite « Sessions de démonstration » de
// l'administration ou LATTIS_WEB_DEMO_MAX s'ils sont posés (sinon aucun) ;
// pas de plafond par adresse (plusieurs personnes peuvent partager une
// adresse derrière un proxy) ; arrêt après LATTIS_WEB_DEMO_IDLE sans onglet ou LATTIS_WEB_DEMO_LIFETIME en tout.

// DemoBase : préfixe des adresses de la démo publique.
const DemoBase = "/demo"

const (
	demoCookie = "lattis_demo"
	demoPrefix = "demo-"
)

var demoIDRe = regexp.MustCompile(`^[0-9a-f]{24}$`)

func isDemo(user string) bool { return strings.HasPrefix(user, demoPrefix) }

func (m *Manager) demoRoutes(mux *http.ServeMux) {
	mux.HandleFunc(DemoBase+"/", m.demo)
	mux.HandleFunc("GET "+DemoBase, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, DemoBase+"/", http.StatusMovedPermanently)
	})
}

// demoEnabled lit le réglage (nil : désactivée).
func (m *Manager) demoEnabled(ctx context.Context) bool {
	if m.DemoEnabled == nil {
		return false
	}
	return m.DemoEnabled(ctx)
}

func (m *Manager) demo(w http.ResponseWriter, r *http.Request) {
	if !m.demoEnabled(r.Context()) {
		http.NotFound(w, r)
		return
	}
	if o := r.Header.Get("Origin"); o != "" && !m.sameOrigin(o) {
		http.Error(w, "origine refusée", http.StatusForbidden)
		return
	}
	id := ""
	if c, err := r.Cookie(demoCookie); err == nil && demoIDRe.MatchString(c.Value) {
		id = c.Value
	}
	if id == "" {
		// Nouvelle démo : seulement à l'ouverture d'une page.
		if r.Method != http.MethodGet || isWebSocket(r) {
			http.Error(w, "démo expirée : rechargez la page", http.StatusUnauthorized)
			return
		}
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		id = hex.EncodeToString(b)
		http.SetCookie(w, &http.Cookie{Name: demoCookie, Value: id, Path: DemoBase + "/", HttpOnly: true,
			Secure: m.Config.PublicURL != nil && m.Config.PublicURL.Scheme == "https", SameSite: http.SameSiteLaxMode,
			MaxAge: int(m.Config.WebDemoLimit.Seconds())})
	}
	e, err := m.demoEngine(r.Context(), demoPrefix+id, m.clientIP(r))
	var le *LimitError
	if errors.As(err, &le) {
		limitResponse(w, r, le)
		return
	}
	if err != nil {
		slog.Error("démo publique", "err", err)
		http.Error(w, "Le moteur Lattis n'a pas pu démarrer.", http.StatusBadGateway)
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	r.Header.Del("Cookie")
	e.proxy.ServeHTTP(w, r)
}

func (m *Manager) clientIP(r *http.Request) string {
	if m.ClientIP != nil {
		return m.ClientIP(r)
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// demoEngine : moteur de démo de la clé key, démarré au besoin.
func (m *Manager) demoEngine(ctx context.Context, key, ip string) (*engine, error) {
	l := m.launcher()
	// La réserve garde des moteurs du mode serveur : la démo passe à côté.
	if p, ok := l.(*Pool); ok {
		l = p.Inner
	}
	m.mu.Lock()
	if e, ok := m.engines[key]; ok {
		m.mu.Unlock()
		return waitStarted(ctx, e)
	}
	if m.engines == nil {
		m.engines, m.secrets = map[string]*engine{}, map[string]string{}
	}
	m.mu.Unlock()
	lim := m.limits(ctx)
	m.mu.Lock()
	demos := 0
	for k := range m.engines {
		if isDemo(k) {
			demos++
		}
	}
	demoMax := m.Config.WebDemoMax
	if lim.DemoSessions > 0 && (demoMax == 0 || lim.DemoSessions < demoMax) {
		demoMax = lim.DemoSessions
	}
	switch {
	case demoMax > 0 && demos >= demoMax:
		m.mu.Unlock()
		slog.Warn("démo publique refusée : sessions de démonstration au maximum", "max", demoMax, "ip", ip)
		return nil, &LimitError{Kind: LimitDemo, Max: int64(demoMax)}
	case len(m.engines) >= m.Config.WebMaxEngines:
		m.mu.Unlock()
		slog.Warn("démo publique refusée : plafond de l'installation", "max", m.Config.WebMaxEngines, "ip", ip)
		return nil, &LimitError{Kind: LimitInstall, Max: int64(m.Config.WebMaxEngines)}
	}
	m.mu.Unlock()
	if err := m.checkMemory(ctx, lim); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if e, ok := m.engines[key]; ok {
		m.mu.Unlock()
		return waitStarted(ctx, e)
	}
	e := &engine{userID: key, demo: true, ip: ip, memory: lim.EngineMemory, started: make(chan struct{})}
	m.engines[key] = e
	m.mu.Unlock()
	go func() {
		e.startErr = m.start(l, e)
		if e.startErr != nil {
			m.forget(e)
		} else {
			// Durée maximale : arrêt propre, puis forcé.
			go func() {
				select {
				case <-e.inst.Done():
				case <-time.After(m.Config.WebDemoLimit):
					slog.Info("démo publique : durée maximale atteinte", "engine", key)
					e.inst.Stop()
					select {
					case <-e.inst.Done():
					case <-time.After(10 * time.Second):
						e.inst.Kill()
					}
				}
			}()
		}
		close(e.started)
	}()
	return waitStarted(ctx, e)
}

func waitStarted(ctx context.Context, e *engine) (*engine, error) {
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
