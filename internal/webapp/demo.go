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
	if c, err := r.Cookie(demoCookie); err == nil && demoIDRe.MatchString(c.Value) && !r.URL.Query().Has("nouvelle") {
		id = c.Value
	}
	if id != "" && m.demoEnded(demoPrefix+id) {
		demoEndedResponse(w, r)
		return
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
			MaxAge: int(m.demoLifetime(m.limits(r.Context())).Seconds()) + 3600})
	}
	e, err := m.demoEngineFor(r.Context(), demoPrefix+id, m.clientIP(r), queueToken(r))
	var le *LimitError
	if errors.As(err, &le) {
		m.queueResponse(w, r, le, queueDemo)
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
	return m.demoEngineFor(ctx, key, ip, "")
}

// demoEngineFor : demoEngine, une nouvelle démo passant après la file
// d'attente sauf pour le ticket tok à son tour.
func (m *Manager) demoEngineFor(ctx context.Context, key, ip, tok string) (*engine, error) {
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
	if err := m.admit(ctx, queueDemo, tok); err != nil {
		return nil, err
	}
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
	life := m.demoLifetime(lim)
	e := &engine{userID: key, demo: true, ip: ip, memory: lim.EngineMemory, deadline: time.Now().Add(life), warn: demoWarn(life), started: make(chan struct{})}
	m.engines[key] = e
	m.mu.Unlock()
	go func() {
		e.startErr = m.start(l, e)
		if e.startErr != nil {
			m.forget(e)
		} else {
			// Échéance : la personne a été prévenue par le moteur ; arrêt
			// propre peu après, puis forcé.
			go func() {
				select {
				case <-e.inst.Done():
				case <-time.After(time.Until(e.deadline) + demoGrace):
					slog.Info("démo publique : durée écoulée, démo arrêtée", "engine", key)
					m.markEnded(key)
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

// demoGrace : délai entre l'échéance d'une démo (annoncée au moteur) et son
// arrêt, pour un dernier téléchargement.
var demoGrace = 15 * time.Second

// demoLifetime : durée d'une démo (réglage de l'administration, sinon
// LATTIS_WEB_DEMO_LIFETIME).
func (m *Manager) demoLifetime(lim Limits) time.Duration {
	if lim.DemoLifetime > 0 {
		return lim.DemoLifetime
	}
	return m.Config.WebDemoLimit
}

// demoWarn : avertissement 2 minutes avant la fin, ou à mi-parcours pour
// une démo de moins de 4 minutes.
func demoWarn(life time.Duration) time.Duration {
	if life < 4*time.Minute {
		return life / 2
	}
	return 2 * time.Minute
}

// markEnded retient une démo arrêtée à son échéance (page « Démo
// terminée » pour son navigateur, pendant une heure).
func (m *Manager) markEnded(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ended == nil {
		m.ended = map[string]time.Time{}
	}
	now := time.Now()
	for k, at := range m.ended {
		if now.Sub(at) > time.Hour {
			delete(m.ended, k)
		}
	}
	m.ended[key] = now
}

func (m *Manager) demoEnded(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.ended[key]
	return ok && time.Since(at) <= time.Hour
}

// demoEndedResponse : la démo de ce navigateur est terminée (410).
func demoEndedResponse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet || isWebSocket(r) {
		http.Error(w, "démo terminée", http.StatusGone)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusGone)
	_, _ = w.Write([]byte(`<!doctype html><html lang="fr"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Démo terminée</title></head><body style="font-family:system-ui,sans-serif;max-width:38rem;margin:4rem auto;padding:0 1rem;line-height:1.5">
<h1>Démo terminée</h1>
<p>Le temps de cette démonstration de Lattis est écoulé, et son classeur n'est pas gardé sur le serveur.</p>
<p><a href="` + DemoBase + `/?nouvelle=1">Commencer une nouvelle démo</a></p>
<p style="color:#5f6b76">Pour garder votre travail, installez Lattis sur votre poste : un classeur téléchargé pendant la démo s'y ouvre.</p>
</body></html>`))
}
