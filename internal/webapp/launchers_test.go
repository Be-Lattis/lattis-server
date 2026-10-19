package webapp

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Be-Lattis/lattis-server/internal/config"
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
