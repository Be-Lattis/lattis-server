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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Be-Lattis/lattis-server/internal/store"
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
	Kind     string `json:"-"`                  // change, data, reset, revoked, presence
	Workbook string `json:"workbook,omitempty"` // classeur (change, revoked, presence)
	Payload  string `json:"payload,omitempty"`  // charge du NOTIFY (identifiant de tableau, *meta, *acl…)
	LSN      string `json:"lsn,omitempty"`      // position WAL du parent après le changement
	// Users : membres qui suivent le classeur en ce moment (presence), un
	// par utilisateur, quel que soit son nombre de postes.
	Users []Present `json:"users,omitempty"`
}

// Present : un membre qui suit un classeur (événement presence).
type Present struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}

type sub struct {
	user     store.User
	channels map[string]string // canal → classeur
	data     bool
	// lsn : le poste veut la position WAL avec chaque changement (lsn=1),
	// pour lire ensuite sur un réplica.
	lsn    bool
	ch     chan Event
	closed bool
}

// Hub écoute PostgreSQL et distribue les notifications. Pool : connexion
// de service (écoute et vérification des droits).
type Hub struct {
	Pool *pgxpool.Pool

	mu        sync.Mutex
	subs      map[string]map[*sub]struct{} // canal → abonnés
	draining  chan struct{}
	drainOnce sync.Once
	dataSubs  map[*sub]struct{}
	lastUse   map[string]time.Time // canal → dernier abonné parti
	listening map[string]bool
	changed   chan struct{}
	once      sync.Once
}

// Drain ferme les flux en cours avec l'événement « restart » (le poste se
// réabonne, auprès d'un autre lattisd) et refuse les nouveaux.
func (h *Hub) Drain() {
	if h == nil {
		return
	}
	h.init()
	h.drainOnce.Do(func() { close(h.draining) })
}

func (h *Hub) init() {
	h.once.Do(func() {
		h.draining = make(chan struct{})
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

// Run écoute jusqu'à la fin de ctx. Quand l'ensemble des canaux change, la
// même connexion écoute les nouveaux canaux (LISTEN) puis cesse d'écouter
// ceux qui ne servent plus (UNLISTEN) : aucun changement perdu, et pas de
// reconnexion qui réécoute tout ; après une coupure, les abonnés reçoivent
// « reset » et resynchronisent.
func (h *Hub) Run(ctx context.Context) {
	h.init()
	var cur *listener
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		want := h.wanted()
		if cur != nil && !cur.broken() && !cur.covers(want) {
			cur.update(want)
		}
		if cur == nil || cur.broken() {
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
	cancel context.CancelFunc
	done   chan struct{}
	err    error

	mu       sync.Mutex
	channels map[string]bool // canaux écoutés
	pending  []string        // canaux voulus, à appliquer par la boucle
	wake     context.CancelFunc
}

func (l *listener) covers(want []string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending != nil {
		return sameSet(want, l.pending)
	}
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

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]bool{}
	for _, c := range b {
		m[c] = true
	}
	for _, c := range a {
		if !m[c] {
			return false
		}
	}
	return true
}

// update demande à la boucle d'écouter want : l'attente d'une notification
// est interrompue (sans fermer la connexion) pour envoyer LISTEN et UNLISTEN.
func (l *listener) update(want []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending = want
	if l.wake != nil {
		l.wake()
	}
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
	// Repérable dans pg_stat_activity.
	cfg.RuntimeParams["application_name"] = "lattisd-relais"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	l := &listener{channels: map[string]bool{}, done: make(chan struct{})}
	// Tous les LISTEN en un seul aller-retour (protocole simple, plusieurs
	// instructions) : avec des milliers de classeurs suivis, un aller-retour
	// par canal faisait durer chaque abonnement des secondes.
	var sql strings.Builder
	for _, c := range channels {
		sql.WriteString("LISTEN " + pgx.Identifier{c}.Sanitize() + ";")
		l.channels[c] = true
	}
	if _, err := conn.PgConn().Exec(ctx, sql.String()).ReadAll(); err != nil {
		conn.Close(context.Background())
		return nil, err
	}
	lctx, cancel := context.WithCancel(ctx)
	l.cancel = cancel
	h.setListening(l.channels)
	go func() {
		defer close(l.done)
		defer conn.Close(context.Background())
		for {
			l.mu.Lock()
			if want := l.pending; want != nil {
				l.pending = nil
				l.mu.Unlock()
				if err := h.relisten(lctx, conn, l, want); err != nil {
					if lctx.Err() == nil {
						l.err = err
					}
					return
				}
				continue
			}
			wctx, wake := context.WithCancel(lctx)
			l.wake = wake
			l.mu.Unlock()
			n, err := conn.WaitForNotification(wctx)
			l.mu.Lock()
			l.wake = nil
			l.mu.Unlock()
			wake()
			if err != nil {
				if lctx.Err() != nil {
					return
				}
				if wctx.Err() != nil {
					continue // réveillée par update : la connexion reste ouverte
				}
				l.err = err
				return
			}
			h.dispatch(lctx, conn, n)
		}
	}()
	return l, nil
}

// setListening publie les canaux écoutés (covered).
func (h *Hub) setListening(channels map[string]bool) {
	cp := make(map[string]bool, len(channels))
	for c := range channels {
		cp[c] = true
	}
	h.mu.Lock()
	h.listening = cp
	h.mu.Unlock()
}

// relisten passe la connexion de l'écoute actuelle à want, en un
// aller-retour : LISTEN des nouveaux canaux d'abord, puis UNLISTEN des
// anciens. Les notifications reçues pendant ce temps sont gardées par pgx et
// lues ensuite.
func (h *Hub) relisten(ctx context.Context, conn *pgx.Conn, l *listener, want []string) error {
	l.mu.Lock()
	next := map[string]bool{}
	var sql strings.Builder
	for _, c := range want {
		next[c] = true
		if !l.channels[c] {
			sql.WriteString("LISTEN " + pgx.Identifier{c}.Sanitize() + ";")
		}
	}
	for c := range l.channels {
		if !next[c] {
			sql.WriteString("UNLISTEN " + pgx.Identifier{c}.Sanitize() + ";")
		}
	}
	l.mu.Unlock()
	if sql.Len() > 0 {
		if _, err := conn.PgConn().Exec(ctx, sql.String()).ReadAll(); err != nil {
			return err
		}
	}
	l.mu.Lock()
	l.channels = next
	l.mu.Unlock()
	h.setListening(next)
	return nil
}

// dispatch envoie une notification à ses abonnés. La position WAL du
// parent (le changement est validé : la notification part au COMMIT) n'est
// lue que si un abonné la demande (lsn=1) : un aller-retour de moins par
// notification sinon.
func (h *Hub) dispatch(ctx context.Context, conn *pgx.Conn, n *pgconn.Notification) {
	h.mu.Lock()
	var targets []*sub
	wantLSN := false
	if n.Channel == DataChannel {
		for s := range h.dataSubs {
			targets = append(targets, s)
			wantLSN = wantLSN || s.lsn
		}
	} else {
		for s := range h.subs[n.Channel] {
			targets = append(targets, s)
			wantLSN = wantLSN || s.lsn
		}
	}
	h.mu.Unlock()
	var lsn string
	if wantLSN {
		_ = conn.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&lsn)
	}
	for _, s := range targets {
		e := Event{Kind: "data", Payload: n.Payload}
		if n.Channel != DataChannel {
			e = Event{Kind: "change", Workbook: s.channels[n.Channel], Payload: n.Payload}
		}
		if s.lsn {
			e.LSN = lsn
		}
		h.send(s, e)
	}
	if n.Payload == "*acl" && len(targets) > 0 {
		go h.recheckAll(n.Channel, targets)
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

// channelOf vérifie l'accès de l'utilisateur au classeur et rend le canal
// du classeur. La vérification (lattis.can_follow, même règle que
// lattis.my_capabilities) passe par la connexion de service : pas de
// connexion PostgreSQL par utilisateur pour suivre un classeur.
func (h *Hub) channelOf(ctx context.Context, u store.User, workbook string) (string, error) {
	var ch *string
	err := h.Pool.QueryRow(ctx, `SELECT lattis.can_follow($1::uuid, $2::uuid)`, u.ID, workbook).Scan(&ch)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "22P02" || err == nil && ch == nil {
		return "", ErrForbidden
	}
	if err != nil {
		return "", err
	}
	return *ch, nil
}

func (h *Hub) subscribe(ctx context.Context, u store.User, workbooks []string, data, lsn bool) (*sub, error) {
	h.init()
	s := &sub{user: u, channels: map[string]string{}, data: data, lsn: lsn, ch: make(chan Event, bufferSize)}
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
	for ch := range s.channels {
		h.presence(ch)
	}
	return s, nil
}

// presence envoie à chaque poste qui suit le classeur du canal ch la liste
// des membres qui le suivent (arrivée ou départ d'un poste). Un abonné qui
// n'a pas encore reçu « ready » la reçoit à sa suite.
func (h *Hub) presence(ch string) {
	h.mu.Lock()
	var targets []*sub
	seen := map[string]bool{}
	var users []Present
	wb := ""
	for s := range h.subs[ch] {
		targets = append(targets, s)
		wb = s.channels[ch]
		if !seen[s.user.ID] {
			seen[s.user.ID] = true
			name := s.user.DisplayName
			if name == "" {
				name = s.user.Email
			}
			users = append(users, Present{ID: s.user.ID, Name: name, Email: s.user.Email})
		}
	}
	h.mu.Unlock()
	slices.SortFunc(users, func(a, b Present) int { return strings.Compare(a.Name, b.Name) })
	for _, s := range targets {
		h.send(s, Event{Kind: "presence", Workbook: wb, Users: users})
	}
}

func (h *Hub) unsubscribe(s *sub) {
	h.mu.Lock()
	s.closed = true
	var left []string
	for ch := range s.channels {
		delete(h.subs[ch], s)
		if len(h.subs[ch]) == 0 {
			h.lastUse[ch] = time.Now()
		} else {
			left = append(left, ch)
		}
	}
	delete(h.dataSubs, s)
	h.mu.Unlock()
	for _, ch := range left {
		h.presence(ch)
	}
}

// recheckWorkers : vérifications de droits simultanées après un « *acl ».
const recheckWorkers = 4

// recheckAll : après un changement de droits du classeur (canal ch), les
// postes qui ont perdu l'accès cessent d'en recevoir les changements. Une
// vérification par utilisateur (pas par poste), au plus recheckWorkers à
// la fois : 500 postes sur un classeur ne lancent plus 500 transactions
// d'un coup.
func (h *Hub) recheckAll(ch string, subs []*sub) {
	byUser := map[string][]*sub{}
	var users []string
	for _, s := range subs {
		if _, ok := byUser[s.user.ID]; !ok {
			users = append(users, s.user.ID)
		}
		byUser[s.user.ID] = append(byUser[s.user.ID], s)
	}
	work := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < min(recheckWorkers, len(users)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range work {
				h.recheck(byUser[id], ch)
			}
		}()
	}
	for _, id := range users {
		work <- id
	}
	close(work)
	wg.Wait()
}

// recheck vérifie l'accès d'un utilisateur (ses postes abonnés subs) au
// classeur du canal ch ; perdu, ses postes reçoivent « revoked ».
func (h *Hub) recheck(subs []*sub, ch string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wb := subs[0].channels[ch]
	if _, err := h.channelOf(ctx, subs[0].user, wb); !errors.Is(err, ErrForbidden) {
		return
	}
	h.mu.Lock()
	for _, s := range subs {
		delete(h.subs[ch], s)
	}
	h.mu.Unlock()
	for _, s := range subs {
		h.send(s, Event{Kind: "revoked", Workbook: wb})
	}
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

// Handler sert GET /api/events?workbooks=<id>,<id>&data=1&lsn=1 (jeton d'accès en
// Bearer, le même que pour PostgreSQL). bearer rend aussi l'expiration du
// jeton : le flux se ferme alors (événement « expired »), le poste se
// réabonne avec un jeton neuf ; un compte désactivé entre-temps est refusé.
func (h *Hub) Handler(bearer func(*http.Request) (store.User, time.Time, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, exp, err := bearer(r)
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
		select {
		case <-h.draining:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "lattisd en cours d'arrêt", http.StatusServiceUnavailable)
			return
		default:
		}
		s, err := h.subscribe(r.Context(), u, wbs, data, r.URL.Query().Get("lsn") == "1")
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
		if !h.covered(s) {
			// Canaux pas écoutés (PostgreSQL refuse une connexion, écoute
			// en échec) : pas de « ready » trompeur, le poste réessaie.
			w.Header().Set("Retry-After", "5")
			http.Error(w, "relais indisponible : canaux pas encore écoutés", http.StatusServiceUnavailable)
			return
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
		var expired <-chan time.Time
		if !exp.IsZero() {
			et := time.NewTimer(time.Until(exp))
			defer et.Stop()
			expired = et.C
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case <-h.draining:
				write(w, "restart", map[string]any{})
				_ = rc.Flush()
				return
			case <-expired:
				write(w, "expired", map[string]any{})
				_ = rc.Flush()
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
