package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Be-Lattis/lattis-server/internal/config"
)

func TestParseQuantity(t *testing.T) {
	for q, want := range map[string]int64{"": 0, "1024": 1024, "123456Ki": 123456 << 10, "12Mi": 12 << 20, "1.5Gi": 3 << 29, "2G": 2e9, "500m": 0} {
		got, err := ParseQuantity(q)
		if err != nil || got != want {
			t.Fatalf("%q : %d, %v (attendu %d)", q, got, err, want)
		}
	}
	if _, err := ParseQuantity("beaucoup"); err == nil {
		t.Fatal("quantité invalide acceptée")
	}
}

// Kubernetes : mémoire des pods des moteurs par metrics-server, et message
// clair quand l'API de métriques manque.
func TestKubeMemory(t *testing.T) {
	missing := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if missing {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/apis/metrics.k8s.io/v1beta1/namespaces/lattis/pods" || !strings.Contains(r.URL.Query().Get("labelSelector"), "lattis.dev/owner=lattisd-0") {
			http.Error(w, r.URL.String(), http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"containers":[{"usage":{"memory":"300Mi"}}]},{"containers":[{"usage":{"memory":"204800Ki"}}]}]}`))
	}))
	defer srv.Close()
	k := &KubeLauncher{API: srv.URL, HTTP: srv.Client(), Cfg: config.K8sEngines{Namespace: "lattis", OwnerPod: "lattisd-0"}}
	n, err := k.Memory(context.Background())
	if err != nil || n != 500<<20 {
		t.Fatalf("mémoire : %d, %v", n, err)
	}
	missing = true
	if _, err := k.Memory(context.Background()); !errors.Is(err, ErrNoMemory) || !strings.Contains(err.Error(), "metrics-server") {
		t.Fatalf("sans metrics-server : %v", err)
	}
}

// Manager.Usage : attribués et prêts de la réserve ; mémoire du lanceur.
func TestUsage(t *testing.T) {
	p := &Pool{Inner: &fakeLauncher{}, Spare: 0, Max: 4}
	m := &Manager{Launcher: p}
	if u := m.Usage(context.Background()); u.Assigned != 0 || u.Ready != 0 || u.Memory != 0 || u.MemoryErr != nil {
		t.Fatalf("sans moteur : %+v", u)
	}
	if _, err := os.Stat("/proc/self/status"); err == nil {
		n, err := RSS(os.Getpid())
		if err != nil || n <= 0 {
			t.Fatalf("RSS : %d, %v", n, err)
		}
	}
}

// Pod de la démo publique : options --public-demo, ni secret, ni volume
// persistant, ni serveur.
func TestKubeDemoPod(t *testing.T) {
	k := &KubeLauncher{Cfg: config.K8sEngines{Image: "moteur:1", Namespace: "lattis", CPU: "250m", Memory: "1Gi", CacheSize: "1Gi"}}
	b, _ := json.Marshal(k.PodSpec(Spec{UserID: "demo-ab", Demo: true, Idle: time.Minute}, "p"))
	for _, want := range []string{`"--public-demo"`, `"--base-path","/demo"`, `"emptyDir"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("pod de démo sans %s : %s", want, b)
		}
	}
	for _, bad := range []string{"LATTIS_ENGINE_SECRET", "--server", "persistentVolumeClaim"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("pod de démo avec %s : %s", bad, b)
		}
	}
}
