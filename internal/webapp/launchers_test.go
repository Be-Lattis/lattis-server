package webapp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Be-Lattis/lattis-server/internal/config"
	"github.com/Be-Lattis/lattis-server/internal/store"
)

// listener : un faux moteur qui accepte les connexions.
func listener(t *testing.T) net.Listener {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return ln
}

func waitDone(t *testing.T, inst Instance) {
	t.Helper()
	select {
	case <-inst.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("arrêt du moteur non vu")
	}
}

// Mode docker : démarrage par le lanceur, surveillance, arrêt.
func TestDockerLauncher(t *testing.T) {
	watchPeriod = 50 * time.Millisecond
	ln := listener(t)
	var mu sync.Mutex
	running, deleted := true, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer jeton" {
			http.Error(w, "jeton", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/engines":
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["user_id"] != "u1" || in["secret"] != "s3cr3t" {
				http.Error(w, "demande", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(LauncherEngine{ID: "c1", Addr: ln.Addr().String()})
		case r.Method == http.MethodGet && r.URL.Path == "/engines/c1":
			if !running {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"running": true})
		case r.Method == http.MethodDelete:
			deleted = r.URL.String()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	l := &DockerLauncher{URL: srv.URL, Token: "jeton"}
	inst, err := l.Start(context.Background(), Spec{UserID: "u1", Secret: "s3cr3t"})
	if err != nil || inst.Addr() != ln.Addr().String() {
		t.Fatalf("démarrage : %v", err)
	}
	inst.Stop()
	mu.Lock()
	if deleted != "/engines/c1" {
		t.Fatalf("arrêt : %q", deleted)
	}
	running = false // le conteneur est parti (inactivité)
	mu.Unlock()
	waitDone(t, inst)
	if _, err := (&DockerLauncher{URL: srv.URL, Token: "faux"}).Start(context.Background(), Spec{UserID: "u1", Secret: "s3cr3t"}); err == nil {
		t.Fatal("jeton refusé accepté")
	}
}

// Mode kubernetes : pod créé avec ses protections, attendu, surveillé, supprimé.
func TestKubeLauncher(t *testing.T) {
	watchPeriod = 50 * time.Millisecond
	ln := listener(t)
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	var mu sync.Mutex
	var pod map[string]any
	var calls []string
	gone := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pods"):
			_ = json.NewDecoder(r.Body).Decode(&pod)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("{}"))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/persistentvolumeclaims"):
			w.WriteHeader(http.StatusConflict) // déjà là : accepté
			_, _ = w.Write([]byte(`{"message":"exists"}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pods/"):
			if gone {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(`{"status":{"phase":"Running","podIP":"127.0.0.1"}}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	k := &KubeLauncher{API: srv.URL, HTTP: srv.Client(), Port: port,
		Cfg: config.K8sEngines{Image: "moteur:1", Namespace: "lattis", CPU: "250m", Memory: "1Gi", CacheSize: "1Gi",
			OwnerPod: "lattisd-0", OwnerUID: "uid-0"}}
	inst, err := k.Start(context.Background(), Spec{UserID: "u1", Secret: "s3cr3t", Server: "https://l", TokenURL: "http://t", Idle: time.Minute})
	if err != nil {
		t.Fatalf("démarrage : %v (%v)", err, calls)
	}
	mu.Lock()
	b, _ := json.Marshal(pod)
	mu.Unlock()
	for _, want := range []string{`"automountServiceAccountToken":false`, `"runAsNonRoot":true`, `"readOnlyRootFilesystem":true`,
		`"claimName":"lattis-web-u1"`, `"kind":"Pod","name":"lattisd-0","uid":"uid-0"`, `"lattis.dev/user":"u1"`, `"stdin":true`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("pod sans %s : %s", want, b)
		}
	}
	inst.Kill()
	mu.Lock()
	gone = true
	last := calls[len(calls)-1]
	mu.Unlock()
	if !strings.HasPrefix(last, "DELETE /api/v1/namespaces/lattis/pods/lattis-engine-") {
		t.Fatalf("suppression du pod : %v", calls)
	}
	waitDone(t, inst)
}

// Pod terminé tout seul (inactivité) : supprimé, pas laissé « Completed ».
func TestKubeDeletesFinishedPod(t *testing.T) {
	watchPeriod = 20 * time.Millisecond
	ln := listener(t)
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	var mu sync.Mutex
	phase, deleted := "Running", false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"status":{"phase":"` + phase + `","podIP":"127.0.0.1"}}`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/pods/"):
			deleted = true
			_, _ = w.Write([]byte("{}"))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		}
	}))
	defer srv.Close()
	k := &KubeLauncher{API: srv.URL, HTTP: srv.Client(), Port: port, Cfg: config.K8sEngines{Image: "m:1", Namespace: "l", CPU: "1", Memory: "1Gi", OwnerPod: "o"}}
	inst, err := k.Start(context.Background(), Spec{UserID: "pool-1", Idle: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	phase = "Succeeded"
	mu.Unlock()
	waitDone(t, inst)
	for i := 0; i < 200; i++ {
		mu.Lock()
		d := deleted
		mu.Unlock()
		if d {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("pod terminé non supprimé")
}

// Réserve : long délai d'inactivité pour un moteur prêt, et nouvel essai
// après une erreur du lanceur.
func TestPoolIdleAndRetry(t *testing.T) {
	old := poolRetry
	poolRetry = 20 * time.Millisecond
	defer func() { poolRetry = old }()
	f := &flakyLauncher{fails: 2}
	p := &Pool{Inner: f, Spare: 1, Max: 3, Template: Spec{Idle: 5 * time.Minute}}
	p.Warm()
	defer p.StopAll()
	for i := 0; i < 300 && p.Ready() < 1; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if p.Ready() != 1 {
		t.Fatal("réserve non refaite après une erreur")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.idle != poolIdle {
		t.Fatalf("inactivité d'un moteur prêt : %s", f.idle)
	}
}

type flakyLauncher struct {
	mu    sync.Mutex
	fails int
	idle  time.Duration
}

func (f *flakyLauncher) Start(_ context.Context, s Spec) (Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails > 0 {
		f.fails--
		return nil, errors.New("le pod du moteur n'a pas démarré à temps")
	}
	f.idle = s.Idle
	return &fakeInst{user: s.UserID, done: make(chan struct{})}, nil
}

// Moteur attribué par la réserve : arrêté par lattisd après LATTIS_WEB_IDLE
// sans onglet, gardé tant qu'un onglet est connecté.
func TestWatchTabs(t *testing.T) {
	oldTick := tabTick
	tabTick = 5 * time.Millisecond
	defer func() { tabTick = oldTick }()
	m := &Manager{Config: &config.Config{WebIdle: 50 * time.Millisecond}}
	inst := &fakeInst{user: "u", done: make(chan struct{})}
	e := &engine{userID: "u", inst: inst}
	e.tabs.Add(1)
	go m.watchTabs(e)
	time.Sleep(150 * time.Millisecond)
	select {
	case <-inst.done:
		t.Fatal("arrêté avec un onglet connecté")
	default:
	}
	e.lastTab.Store(time.Now().UnixNano())
	e.tabs.Add(-1)
	waitDone(t, inst)
}

// fakeLauncher : moteurs fictifs, arrêtés à la demande.
type fakeLauncher struct {
	mu    sync.Mutex
	insts []*fakeInst
}

type fakeInst struct {
	user string
	done chan struct{}
	once sync.Once
}

func (f *fakeInst) Addr() string          { return "127.0.0.1:1" }
func (f *fakeInst) Done() <-chan struct{} { return f.done }
func (f *fakeInst) Stop()                 { f.once.Do(func() { close(f.done) }) }
func (f *fakeInst) Kill()                 { f.Stop() }

func (f *fakeLauncher) Start(_ context.Context, s Spec) (Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := &fakeInst{user: s.UserID, done: make(chan struct{})}
	f.insts = append(f.insts, i)
	return i, nil
}

// Réserve : N moteurs prêts en plus des attribués, plafond, remplacement.
func TestPool(t *testing.T) {
	f := &fakeLauncher{}
	p := &Pool{Inner: f, Spare: 2, Max: 3}
	eventually := func(what string, cond func() bool) {
		t.Helper()
		for i := 0; i < 200 && !cond(); i++ {
			time.Sleep(10 * time.Millisecond)
		}
		if !cond() {
			t.Fatalf("%s (prêts : %d)", what, p.Ready())
		}
	}
	p.Warm()
	eventually("deux moteurs prêts", func() bool { return p.Ready() == 2 })
	a, err := p.Start(context.Background(), Spec{UserID: "a", Secret: "x"})
	if err != nil || !strings.HasPrefix(a.(*poolInstance).Instance.(*fakeInst).user, PoolPrefix) || a.(*poolInstance).Secret() == "x" {
		t.Fatalf("attribution : %v", err)
	}
	// 1 attribué + 2 prêts = plafond.
	eventually("réserve refaite", func() bool { return p.Ready() == 2 })
	if _, err := p.Start(context.Background(), Spec{UserID: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Start(context.Background(), Spec{UserID: "c"}); err != nil {
		t.Fatal(err)
	}
	eventually("plus de place pour un moteur prêt", func() bool { return p.Ready() == 0 })
	if _, err := p.Start(context.Background(), Spec{UserID: "d"}); err != ErrBusy {
		t.Fatalf("au-delà du plafond : %v", err)
	}
	a.Kill() // session finie : le moteur disparaît, un neuf le remplace dans la réserve
	eventually("moteur neuf après une fin de session", func() bool { return p.Ready() == 1 })
	f.mu.Lock()
	n := len(f.insts)
	f.mu.Unlock()
	if n != 4 { // 2 prêts, 1 refait après a, 1 après la fin de a
		t.Fatalf("moteurs démarrés : %d", n)
	}
	p.StopAll()
}

// addrLauncher : moteur fictif joignable à une adresse.
type addrLauncher struct{ addr string }

type addrInst struct {
	fakeInst
	addr string
}

func (a *addrInst) Addr() string { return a.addr }

func (l *addrLauncher) Start(_ context.Context, s Spec) (Instance, error) {
	return &addrInst{fakeInst: fakeInst{user: s.UserID, done: make(chan struct{})}, addr: l.addr}, nil
}

// Relais du WebSocket : la négociation de permessage-deflate va jusqu'au
// moteur et en revient (le relais ne termine pas la connexion).
func TestWebSocketExtensionsRelayed(t *testing.T) {
	var got string
	engineSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Sec-WebSocket-Extensions")
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: x\r\nSec-WebSocket-Extensions: permessage-deflate\r\n\r\n")
		_ = rw.Flush()
	}))
	defer engineSrv.Close()
	pub, _ := url.Parse("http://l.example")
	m := &Manager{Config: &config.Config{PublicURL: pub, WebMaxEngines: 2, WebIdle: time.Minute},
		Launcher: &addrLauncher{addr: strings.TrimPrefix(engineSrv.URL, "http://")},
		Session:  func(*http.Request) (store.User, error) { return store.User{ID: "u"}, nil }}
	srv := httptest.NewServer(http.HandlerFunc(m.app))
	defer srv.Close()
	c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("GET /app/ws HTTP/1.1\r\nHost: l.example\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: a\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Extensions: permessage-deflate; client_max_window_bits\r\n\r\n"))
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Sec-WebSocket-Extensions") != "permessage-deflate" || !strings.Contains(got, "permessage-deflate") {
		t.Fatalf("négociation : %d %q, reçu par le moteur %q", resp.StatusCode, resp.Header.Get("Sec-WebSocket-Extensions"), got)
	}
}
