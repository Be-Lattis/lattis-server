package webapp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"sync"
	"time"
)

// File d'attente (décidé par Louis le 26/10/2026) : quand une limite empêche
// d'ouvrir une session web ou une démo, la page d'attente donne un ticket
// (jeton aléatoire, cookie lattis_queue) associé à un numéro croissant, et
// interroge GET /queue toutes les quelques secondes pour afficher sa
// position ; pas de WebSocket gardé ouvert. Quand une place se libère, les
// premiers de la file passent avant les nouveaux venus ; un ticket qui
// n'est plus interrogé expire, pour ne pas bloquer la file.
//
// La file est propre à chaque lattisd : les limites y sont comptées, et
// l'affinité par cookie (lattis_route) ramène la personne au même.

const (
	queueCookie = "lattis_queue"
	queueApp    = "app"
	queueDemo   = "demo"
)

// queueTTL : un ticket non interrogé depuis ce délai est abandonné. Il
// couvre le ralentissement des minuteurs d'un onglet en arrière-plan (une
// interrogation par minute au pire dans Chrome).
var queueTTL = 90 * time.Second

// queuePoll : intervalle d'interrogation suggéré à la page : toutes les
// 10 s, puis toutes les 3 s dans les queueNear dernières places.
const queueNear = 10

func queuePoll(pos int) time.Duration {
	if pos > queueNear {
		return 10 * time.Second
	}
	return 3 * time.Second
}

var queueTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

type ticket struct {
	n    int64
	kind string
	seen time.Time
}

type waitQueue struct {
	mu      sync.Mutex
	next    int64
	tickets map[string]*ticket
}

// pruneLocked retire les tickets abandonnés.
func (q *waitQueue) pruneLocked(now time.Time) {
	for tok, t := range q.tickets {
		if now.Sub(t.seen) > queueTTL {
			delete(q.tickets, tok)
		}
	}
}

// ahead : tickets vivants de la file kind jusqu'à n inclus (n < 0 : tous).
func (q *waitQueue) aheadLocked(kind string, n int64) int {
	c := 0
	for _, t := range q.tickets {
		if t.kind == kind && (n < 0 || t.n <= n) {
			c++
		}
	}
	return c
}

// take : ticket du jeton tok dans la file kind, rafraîchi ; nil sinon.
func (q *waitQueue) takeLocked(kind, tok string, now time.Time) *ticket {
	if t := q.tickets[tok]; t != nil && t.kind == kind {
		t.seen = now
		return t
	}
	return nil
}

func (m *Manager) waitQueue() *waitQueue {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.queue == nil {
		m.queue = &waitQueue{tickets: map[string]*ticket{}}
	}
	return m.queue
}

// room : places libres pour une nouvelle session (kind app) ou démo, selon les mêmes limites que engine et
// demoEngine.
func (m *Manager) room(ctx context.Context, kind string) int {
	lim := m.limits(ctx)
	if m.checkMemory(ctx, lim) != nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	free := m.Config.WebMaxEngines - len(m.engines)
	demos := 0
	for k := range m.engines {
		if isDemo(k) {
			demos++
		}
	}
	if kind == queueDemo {
		demoMax := m.Config.WebDemoMax
		if lim.DemoSessions > 0 && (demoMax == 0 || lim.DemoSessions < demoMax) {
			demoMax = lim.DemoSessions
		}
		if demoMax > 0 {
			free = min(free, demoMax-demos)
		}
	} else if lim.Sessions > 0 {
		free = min(free, lim.Sessions-(len(m.engines)-demos))
	}
	return max(free, 0)
}

// admit : une nouvelle session peut-elle s'ouvrir sans passer devant la
// file ? Oui si le ticket tok (ou un nouveau venu, derrière tous) est parmi
// les places libres ; le ticket admis quitte la file.
func (m *Manager) admit(ctx context.Context, kind, tok string) error {
	q := m.waitQueue()
	q.mu.Lock()
	now := m.now()
	q.pruneLocked(now)
	waiting := q.aheadLocked(kind, -1)
	t := q.takeLocked(kind, tok, now)
	q.mu.Unlock()
	if waiting == 0 {
		return nil
	}
	pos := waiting + 1
	if t != nil {
		q.mu.Lock()
		pos = q.aheadLocked(kind, t.n)
		q.mu.Unlock()
	}
	room := m.room(ctx, kind)
	// Aucune place : la limite elle-même répond (la page la nomme).
	if room > 0 && pos > room {
		return &LimitError{Kind: LimitQueue, Max: int64(pos)}
	}
	if t != nil {
		q.mu.Lock()
		delete(q.tickets, tok)
		q.mu.Unlock()
	}
	return nil
}

// enqueue : ticket du jeton tok (prolongé) ou nouveau, en fin de file.
func (m *Manager) enqueue(kind, tok string) (string, int) {
	q := m.waitQueue()
	q.mu.Lock()
	defer q.mu.Unlock()
	now := m.now()
	q.pruneLocked(now)
	t := q.takeLocked(kind, tok, now)
	if t == nil {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		tok = base64.RawURLEncoding.EncodeToString(b)
		q.next++
		t = &ticket{n: q.next, kind: kind, seen: now}
		q.tickets[tok] = t
	}
	return tok, q.aheadLocked(kind, t.n)
}

func queueToken(r *http.Request) string {
	if c, err := r.Cookie(queueCookie); err == nil && queueTokenRe.MatchString(c.Value) {
		return c.Value
	}
	return ""
}

// queueStatus : GET /queue, interrogé par la page d'attente.
func (m *Manager) queueStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	tok := queueToken(r)
	q := m.waitQueue()
	q.mu.Lock()
	now := m.now()
	q.pruneLocked(now)
	t := q.tickets[tok]
	pos := 0
	kind := ""
	if t != nil {
		t.seen = now
		kind = t.kind
		pos = q.aheadLocked(t.kind, t.n)
	}
	q.mu.Unlock()
	if t == nil {
		// Ticket expiré ou inconnu : la page se recharge et en reprend un.
		_ = json.NewEncoder(w).Encode(map[string]any{"expired": true})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"position": pos, "ready": pos <= m.room(r.Context(), kind),
		"poll_ms": queuePoll(pos).Milliseconds()})
}

// queueResponse : page d'attente avec le ticket et la position (requêtes
// de page) ; réponse courte sinon.
func (m *Manager) queueResponse(w http.ResponseWriter, r *http.Request, e *LimitError, kind string) {
	if r.Method != http.MethodGet || isWebSocket(r) {
		limitResponse(w, r, e)
		return
	}
	tok, pos := m.enqueue(kind, queueToken(r))
	http.SetCookie(w, &http.Cookie{Name: queueCookie, Value: tok, Path: "/", HttpOnly: true,
		Secure: m.Config.PublicURL != nil && m.Config.PublicURL.Scheme == "https", SameSite: http.SameSiteLaxMode,
		MaxAge: int(queueTTL.Seconds()) * 2})
	title, body, admin := limitText(e)
	if e.Kind == LimitQueue {
		title, body = "Vous êtes dans la file d'attente", "Toutes les places de ce serveur sont prises ou déjà promises aux personnes qui attendent avant vous."
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintf(w, `<!doctype html><html lang="fr"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>File d'attente</title></head><body style="font-family:system-ui,sans-serif;max-width:38rem;margin:4rem auto;padding:0 1rem;line-height:1.5">
<h1>%[1]s</h1>
<p style="font-size:1.4rem">Votre position : <strong id="pos">%[4]d</strong></p>
<p id="state">Gardez cette page ouverte : Lattis s'ouvrira dès que votre tour arrive.</p>
<p>%[2]s</p>
<p style="color:#5f6b76">%[3]s</p>
<script>
(function () {
  var delay = %[5]d, busy = false, timer;
  function later(d) { clearTimeout(timer); timer = setTimeout(poll, d); }
  function poll() {
    if (busy) return;
    busy = true;
    clearTimeout(timer);
    fetch("/queue", { credentials: "same-origin", cache: "no-store" }).then(function (r) { return r.json(); }).then(function (s) {
      busy = false;
      if (s.expired || s.ready) { location.reload(); return; }
      document.getElementById("pos").textContent = s.position;
      if (s.poll_ms) delay = s.poll_ms;
      later(delay);
    }).catch(function () { busy = false; later(delay * 2); });
  }
  later(delay);
  document.addEventListener("visibilitychange", function () { if (!document.hidden) poll(); });
})();
</script>
</body></html>`, html.EscapeString(title), html.EscapeString(body), html.EscapeString(admin), pos, queuePoll(pos).Milliseconds())
}
