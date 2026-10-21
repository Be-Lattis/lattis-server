package webapp

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Be-Lattis/lattis-server/internal/config"
	"github.com/Be-Lattis/lattis-server/internal/store"
)

// memLauncher : moteurs fictifs, mémoire mesurée fixée par le test.
type memLauncher struct {
	mu    sync.Mutex
	specs []Spec
	mem   int64
}

func (l *memLauncher) Start(_ context.Context, s Spec) (Instance, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.specs = append(l.specs, s)
	return &fakeInst{user: s.UserID, done: make(chan struct{})}, nil
}

func (l *memLauncher) Memory(context.Context) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.mem, nil
}

// Limites de l'administration : sessions web, mémoire totale (page qui la
// nomme), mémoire allouée à chaque moteur.
func TestLimits(t *testing.T) {
	memTTL = -1 // mesure à chaque demande
	l := &memLauncher{}
	lim := Limits{Sessions: 2, Memory: 1 << 30, EngineMemory: 512 << 20}
	pub, _ := url.Parse("https://l.example")
	m := &Manager{Config: &config.Config{PublicURL: pub, WebMaxEngines: 10, WebIdle: time.Minute}, Launcher: l,
		Limits: func(context.Context) Limits { return lim }}
	ctx := context.Background()
	if _, err := m.engine(ctx, store.User{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	if l.specs[0].Memory != 512<<20 {
		t.Fatalf("mémoire du moteur : %d", l.specs[0].Memory)
	}
	// Mémoire des moteurs au-delà de la limite : refus nommé.
	l.mem = 2 << 30
	_, err := m.engine(ctx, store.User{ID: "b"})
	var le *LimitError
	if !errors.As(err, &le) || le.Kind != LimitMemory || le.Used != 2<<30 {
		t.Fatalf("limite de mémoire : %v", err)
	}
	rec := httptest.NewRecorder()
	limitResponse(rec, httptest.NewRequest("GET", "/app/", nil), le)
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "Mémoire maximale de l&#39;application web atteinte") ||
		!strings.Contains(rec.Body.String(), "2048 Mo") || !strings.Contains(rec.Body.String(), "Limites") {
		t.Fatalf("page de la limite : %d %s", rec.Code, rec.Body.String())
	}
	// Session déjà ouverte : jamais refusée.
	if _, err := m.engine(ctx, store.User{ID: "a"}); err != nil {
		t.Fatalf("session existante : %v", err)
	}
	l.mem = 0
	if _, err := m.engine(ctx, store.User{ID: "b"}); err != nil {
		t.Fatal(err)
	}
	// Deux sessions au plus.
	if _, err := m.engine(ctx, store.User{ID: "c"}); !errors.As(err, &le) || le.Kind != LimitSessions || le.Max != 2 {
		t.Fatalf("limite de sessions : %v", err)
	}
	rec = httptest.NewRecorder()
	limitResponse(rec, httptest.NewRequest("GET", "/app/", nil), le)
	if !strings.Contains(rec.Body.String(), "au plus 2 sessions") {
		t.Fatalf("page des sessions : %s", rec.Body.String())
	}
}

// Réserve : un moteur prêt démarré avec une autre mémoire est remplacé.
func TestPoolMemory(t *testing.T) {
	l := &memLauncher{}
	p := &Pool{Inner: l, Spare: 1, Max: 4, Template: Spec{Idle: time.Minute}}
	p.Warm()
	deadline := time.Now().Add(5 * time.Second)
	for p.Ready() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := p.Start(context.Background(), Spec{UserID: "a", Memory: 256 << 20}); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	last := l.specs[len(l.specs)-1]
	if l.specs[0].Memory != 0 || last.Memory != 256<<20 {
		t.Fatalf("mémoire des moteurs : %+v", l.specs)
	}
	p.StopAll()
}
