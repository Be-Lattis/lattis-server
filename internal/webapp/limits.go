package webapp

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"time"
)

// Limits : limites de l'édition web réglées dans l'administration
// (0 : sans limite).
type Limits struct {
	// Sessions : sessions web (moteurs attribués, hors démo) au plus, sous
	// LATTIS_WEB_MAX_ENGINES.
	Sessions int
	// DemoSessions : sessions de la démo publique au plus.
	DemoSessions int
	// Memory : mémoire totale des moteurs au-delà de laquelle aucune session
	// ne s'ouvre ; EngineMemory : mémoire allouée à chaque moteur (octets).
	Memory, EngineMemory int64
}

// Genres de limite atteinte.
const (
	LimitSessions = "sessions"
	LimitMemory   = "memory"
	LimitDemo     = "demo"
	// LimitInstall : plafond de l'installation (LATTIS_WEB_MAX_ENGINES).
	LimitInstall = "install"
)

// LimitError : une limite de l'administration empêche d'ouvrir une session.
type LimitError struct {
	Kind string
	// Max : la limite ; Used : la mesure qui la dépasse (mémoire).
	Max, Used int64
}

func (e *LimitError) Error() string {
	switch e.Kind {
	case LimitMemory:
		return fmt.Sprintf("mémoire de l'application web au maximum (%d Mo sur %d Mo)", e.Used>>20, e.Max>>20)
	case LimitDemo:
		return fmt.Sprintf("sessions de démonstration au maximum (%d)", e.Max)
	case LimitInstall:
		return fmt.Sprintf("moteurs au maximum de l'installation (%d, LATTIS_WEB_MAX_ENGINES)", e.Max)
	}
	return fmt.Sprintf("sessions web au maximum (%d)", e.Max)
}

func (m *Manager) limits(ctx context.Context) Limits {
	if m.Limits == nil {
		return Limits{}
	}
	return m.Limits(ctx)
}

// sessionsLocked : moteurs attribués à des personnes (sous m.mu).
func (m *Manager) sessionsLocked() int {
	n := 0
	for k := range m.engines {
		if !isDemo(k) {
			n++
		}
	}
	return n
}

// memTTL : durée de validité d'une mesure de la mémoire des moteurs.
var memTTL = 15 * time.Second

// checkMemory refuse une nouvelle session quand la mémoire des moteurs
// atteint la limite. Sans mesure possible (lanceur qui ne sait pas la
// relever), la limite est sans effet : signalé dans le journal.
func (m *Manager) checkMemory(ctx context.Context, lim Limits) error {
	if lim.Memory <= 0 {
		return nil
	}
	m.memMu.Lock()
	defer m.memMu.Unlock()
	if time.Since(m.memAt) > memTTL {
		u := m.Usage(ctx)
		if u.MemoryErr != nil {
			slog.Warn("limite de mémoire de l'application web sans effet : mémoire des moteurs inconnue", "err", u.MemoryErr)
			m.memLast = -1
		} else {
			m.memLast = u.Memory
		}
		m.memAt = time.Now()
	}
	if m.memLast >= lim.Memory {
		return &LimitError{Kind: LimitMemory, Max: lim.Memory, Used: m.memLast}
	}
	return nil
}

// limitResponse : page (ou réponse courte) qui dit quelle limite est atteinte.
func limitResponse(w http.ResponseWriter, r *http.Request, e *LimitError) {
	w.Header().Set("Retry-After", "60")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet || isWebSocket(r) {
		http.Error(w, e.Error(), http.StatusServiceUnavailable)
		return
	}
	title, body := "Nombre maximal de sessions web atteint",
		fmt.Sprintf("Ce serveur accepte au plus %d sessions de l'application web en même temps, et elles sont toutes ouvertes.", e.Max)
	admin := "Administrateur : cette limite se règle dans Administration › Serveur › Limites ; l'écran Surveillance montre l'occupation."
	switch e.Kind {
	case LimitMemory:
		title = "Mémoire maximale de l'application web atteinte"
		body = fmt.Sprintf("Les sessions web ouvertes occupent %d Mo, pour une limite de %d Mo fixée par l'administrateur. Aucune nouvelle session ne s'ouvre tant que de la mémoire n'est pas libérée.", e.Used>>20, e.Max>>20)
	case LimitDemo:
		title = "La démo est complète"
		body = fmt.Sprintf("Ce serveur accepte au plus %d sessions de démonstration en même temps, et elles sont toutes ouvertes.", e.Max)
	case LimitInstall:
		title = "Plus de ressources disponibles"
		body = fmt.Sprintf("Ce serveur fait tourner au plus %d moteurs de l'application web à la fois (sessions et démonstrations), et ils sont tous pris.", e.Max)
		admin = "Administrateur : ce plafond est fixé à l'installation (LATTIS_WEB_MAX_ENGINES, chart : web.maxEngines) ; l'écran Surveillance montre l'occupation."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintf(w, `<!doctype html><html lang="fr"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>%[1]s</title></head><body style="font-family:system-ui,sans-serif;max-width:38rem;margin:4rem auto;padding:0 1rem;line-height:1.5">
<h1>%[1]s</h1>
<p>%[2]s</p>
<p>Réessayez dans quelques minutes : une session se libère après une période d'inactivité. Vous pouvez aussi ouvrir vos classeurs avec l'application Lattis sur votre poste.</p>
<p style="color:#5f6b76">%[3]s</p>
</body></html>`, html.EscapeString(title), html.EscapeString(body), html.EscapeString(admin))
}
