// Package events relaie aux postes les notifications de PostgreSQL
// (spécification §21, jalon 8) : chaque lattisd écoute PostgreSQL une seule
// fois (une connexion LISTEN, sur les canaux des classeurs suivis et
// lattis_data) et pousse les changements aux postes par un flux SSE
// (GET /api/events). Les postes n'ont plus de connexion permanente au
// parent : ils lisent les deltas sur un réplica, une fois celui-ci arrivé à
// la position WAL donnée avec chaque événement, et n'ouvrent sur le parent
// que des connexions courtes pour écrire.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/userdb"
)

// DataChannel : canal des données du Data Lake (toujours écouté).
const DataChannel = "lattis_data"

const (
	bufferSize = 64
	heartbeat  = 25 * time.Second
	// linger : un canal sans abonné reste écouté ce temps (réouvertures).
	linger      = 2 * time.Minute
	maxWorkbook = 200
)

// Event : un changement poussé à un poste.
type Event struct {
	Kind     string `json:"-"`                  // change, data, reset, revoked
	Workbook string `json:"workbook,omitempty"` // classeur (change, revoked)
	Payload  string `json:"payload,omitempty"`  // charge du NOTIFY (identifiant de tableau, *meta, *acl…)
	LSN      string `json:"lsn,omitempty"`      // position WAL du parent après le changement
}

type sub struct {
	user     store.User
	channels map[string]string // canal → classeur
	data     bool
	ch       chan Event
	closed   bool
}

// Hub écoute PostgreSQL et distribue les notifications.
type Hub struct {
	Pool  *pgxpool.Pool
	Users userdb.Runner

	mu        sync.Mutex
	subs      map[string]map[*sub]struct{} // canal → abonnés
	dataSubs  map[*sub]struct{}
	lastUse   map[string]time.Time // canal → dernier abonné parti
	listening map[string]bool
	changed   chan struct{}
	once      sync.Once
}

func (h *Hub) init() {
	h.once.Do(func() {
		h.subs = map[string]map[*sub]struct{}{}
		h.dataSubs = map[*sub]struct{}{}
		h.lastUse = map[string]time.Time{}
		h.listening = map[string]bool{}
		h.changed = make(chan struct{}, 1)
	})
}

// Subscribers : postes abonnés (mesures).
func (h *Hub) Subscribers() int {
	h.init()
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := map[*sub]bool{}
	for _, m := range h.subs {
		for s := range m {
			seen[s] = true
		}
	}
	for s := range h.dataSubs {
		seen[s] = true
	}
	return len(seen)
}

func (h *Hub) wanted() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []string{DataChannel}
	for c, m := range h.subs {
		if len(m) > 0 || time.Since(h.lastUse[c]) < linger {
			out = append(out, c)
		}
	}
	return out
}

func (h *Hub) poke() {
	select {
	case h.changed <- struct{}{}:
	default:
	}
}

// Run écoute jusqu'à la fin de ctx. Quand l'ensemble des canaux change, une
// nouvelle connexion LISTEN est ouverte avant de fermer l'ancienne (un
// changement peut alors arriver deux fois, jamais se perdre) ; après une
// coupure, les abonnés reçoivent « reset » et resynchronisent.
func (h *Hub) Run(ctx context.Context) {
	h.init()
	var cur *listener
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		want := h.wanted()
		if cur == nil || !cur.covers(want) || cur.broken() {
			next, err := h.listen(ctx, want)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Warn("relais des changements : écoute", "err", err)
				time.Sleep(2 * time.Second)
				continue
			}
			reset := cur != nil && cur.broken()
			if cur != nil {
				cur.stop()
			}
			cur = next
			if reset {
				h.broadcast(Event{Kind: "reset"})
			}
		}
		select {
		case <-ctx.Done():
			cur.stop()
			return
		case <-h.changed:
			time.Sleep(100 * time.Millisecond) // regroupe les abonnements simultanés
		case <-cur.done:
		case <-tick.C: // canaux restés sans abonné : écoute réduite
		}
	}
}

// listener : une connexion LISTEN et sa boucle de lecture.
type listener struct {
	channels map[string]bool
	cancel   context.CancelFunc
	done     chan struct{}
	err      error
}

func (l *listener) covers(want []string) bool {
	if len(want) != len(l.channels) {
		return false
	}
	for _, c := range want {
		if !l.channels[c] {
			return false
		}
	}
	return true
}

func (l *listener) broken() bool {
	select {
	case <-l.done:
		return l.err != nil
	default:
		return false
	}
}

func (l *listener) stop() {
	l.cancel()
	<-l.done
}

func (h *Hub) listen(ctx context.Context, channels []string) (*listener, error) {
	cfg := h.Pool.Config().ConnConfig.Copy()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	l := &listener{channels: map[string]bool{}, done: make(chan struct{})}
	for _, c := range channels {
		if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{c}.Sanitize()); err != nil {
			conn.Close(context.Background())
			return nil, err
		}
		l.channels[c] = true
	}
	lctx, cancel := context.WithCancel(ctx)
	l.cancel = cancel
	h.mu.Lock()
	h.listening = l.channels
	h.mu.Unlock()
	go func() {
		defer close(l.done)
		defer conn.Close(context.Background())
		for {
			n, err := conn.WaitForNotification(lctx)
			if err != nil {
				if lctx.Err() == nil {
					l.err = err
				}
				return
			}
			h.dispatch(lctx, conn, n)
		}
	}()
	return l, nil
}

// dispatch envoie une notification à ses abonnés, avec la position WAL du
// parent (le changement est validé : la notification part au COMMIT).
func (h *Hub) dispatch(ctx context.Context, conn *pgx.Conn, n *pgconn.Notification) {
	var lsn string
	_ = conn.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&lsn)
	h.mu.Lock()
	var targets []*sub
	var ev Event
	if n.Channel == DataChannel {
		ev = Event{Kind: "data", Payload: n.Payload, LSN: lsn}
		for s := range h.dataSubs {
			targets = append(targets, s)
		}
	} else {
		for s := range h.subs[n.Channel] {
			targets = append(targets, s)
		}
	}
	h.mu.Unlock()
	for _, s := range targets {
		e := ev
		if n.Channel != DataChannel {
			e = Event{Kind: "change", Workbook: s.channels[n.Channel], Payload: n.Payload, LSN: lsn}
		}
		h.send(s, e)
		if n.Payload == "*acl" {
			go h.recheck(s, n.Channel)
		}
	}
}

func (h *Hub) send(s *sub, e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- e:
	default:
		// Poste trop lent : on vide sa file et il resynchronise tout.
		for len(s.ch) > 0 {
			<-s.ch
		}
		s.ch <- Event{Kind: "reset"}
	}
}

func (h *Hub) broadcast(e Event) {
	h.mu.Lock()
	all := map[*sub]bool{}
	for _, m := range h.subs {
		for s := range m {
			all[s] = true
		}
	}
	for s := range h.dataSubs {
		all[s] = true
	}
	h.mu.Unlock()
	for s := range all {
		h.send(s, e)
	}
}

// --- Abonnements -----------------------------------------------------------------------

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ErrForbidden : un classeur demandé est inconnu ou interdit.
var ErrForbidden = errors.New("classeur inconnu ou interdit")

// channelOf vérifie l'accès de l'utilisateur au classeur (sous son identité)
// et rend le canal du classeur.
func (h *Hub) channelOf(ctx context.Context, u store.User, workbook string) (string, error) {
	var ch string
	err := h.Users.As(ctx, u, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT lattis.my_capabilities($1::uuid)`, workbook); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT 'wb_' || slug FROM lattis.workbooks WHERE id = $1::uuid`, workbook).Scan(&ch)
	})
	var pe *pgconn.PgError
	if errors.As(err, &pe) && (pe.Code == "42501" || pe.Code == "P0002" || pe.Code == "22P02") || errors.Is(err, pgx.ErrNoRows) {
		return "", ErrForbidden
	}
	return ch, err
}

func (h *Hub) subscribe(ctx context.Context, u store.User, workbooks []string, data bool) (*sub, error) {
	h.init()
	s := &sub{user: u, channels: map[string]string{}, data: data, ch: make(chan Event, bufferSize)}
	for _, wb := range workbooks {
		ch, err := h.channelOf(ctx, u, wb)
		if err != nil {
			return nil, fmt.Errorf("%s : %w", wb, err)
		}
		s.channels[ch] = wb
	}
	h.mu.Lock()
	for ch := range s.channels {
		if h.subs[ch] == nil {
			h.subs[ch] = map[*sub]struct{}{}
		}
		h.subs[ch][s] = struct{}{}
	}
	if data {
		h.dataSubs[s] = struct{}{}
	}
	h.mu.Unlock()
	h.poke()
	return s, nil
}

func (h *Hub) unsubscribe(s *sub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s.closed = true
	for ch := range s.channels {
		delete(h.subs[ch], s)
		if len(h.subs[ch]) == 0 {
			h.lastUse[ch] = time.Now()
		}
	}
	delete(h.dataSubs, s)
}

// recheck : après un changement de droits, un poste qui a perdu l'accès
// au classeur cesse d'en recevoir les changements.
func (h *Hub) recheck(s *sub, ch string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wb := s.channels[ch]
	if _, err := h.channelOf(ctx, s.user, wb); !errors.Is(err, ErrForbidden) {
		return
	}
	h.mu.Lock()
	delete(h.subs[ch], s)
	h.mu.Unlock()
	h.send(s, Event{Kind: "revoked", Workbook: wb})
}

// covered : tous les canaux de l'abonné sont écoutés.
func (h *Hub) covered(s *sub) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.data && !h.listening[DataChannel] {
		return false
	}
	for ch := range s.channels {
		if !h.listening[ch] {
			return false
		}
	}
	return true
}

// --- Flux SSE ------------------------------------------------------------------------

// Handler sert GET /api/events?workbooks=<id>,<id>&data=1 (jeton d'accès en
// Bearer, le même que pour PostgreSQL).
func (h *Hub) Handler(bearer func(*http.Request) (store.User, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := bearer(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			http.Error(w, "jeton refusé", http.StatusUnauthorized)
			return
		}
		var wbs []string
		for _, p := range strings.Split(r.URL.Query().Get("workbooks"), ",") {
			if p = strings.TrimSpace(p); p == "" {
				continue
			}
			if !uuidRe.MatchString(p) {
				http.Error(w, "workbooks : identifiants de classeurs séparés par des virgules", http.StatusBadRequest)
				return
			}
			wbs = append(wbs, p)
		}
		data := r.URL.Query().Get("data") == "1"
		if len(wbs) > maxWorkbook || (len(wbs) == 0 && !data) {
			http.Error(w, fmt.Sprintf("de 1 à %d classeurs, ou data=1", maxWorkbook), http.StatusBadRequest)
			return
		}
		s, err := h.subscribe(r.Context(), u, wbs, data)
		if errors.Is(err, ErrForbidden) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		if err != nil {
			slog.Error("relais des changements : abonnement", "err", err)
			http.Error(w, "relais indisponible", http.StatusServiceUnavailable)
			return
		}
		defer h.unsubscribe(s)
		// « ready » seulement une fois les canaux écoutés : un changement
		// validé après ce point arrive forcément au poste.
		for i := 0; i < 100 && !h.covered(s); i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}

		rc := http.NewResponseController(w)
		_ = rc.SetWriteDeadline(time.Time{})
		_ = rc.SetReadDeadline(time.Time{})
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		// Point de départ : le poste lit ce qui a changé depuis sa dernière
		// synchronisation, puis suit les événements.
		write(w, "ready", map[string]any{"workbooks": wbs, "data": data})
		_ = rc.Flush()
		t := time.NewTicker(heartbeat)
		defer t.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-t.C:
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
					return
				}
			case e := <-s.ch:
				write(w, e.Kind, e)
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

func write(w http.ResponseWriter, kind string, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b)
}
