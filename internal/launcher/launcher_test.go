package launcher

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeDocker : l'API Docker réduite à ce que le lanceur appelle.
type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]map[string]any // id → spec
	created    map[string]any
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/v1.43")
	switch {
	case r.Method == http.MethodPost && p == "/containers/create":
		var spec map[string]any
		_ = json.NewDecoder(r.Body).Decode(&spec)
		f.created = spec
		id := "c" + r.URL.Query().Get("name")
		f.containers[id] = spec
		_ = json.NewEncoder(w).Encode(map[string]string{"Id": id})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/start"):
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && p == "/containers/json":
		var out []map[string]string
		for id := range f.containers {
			out = append(out, map[string]string{"Id": id})
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/stats"):
		_ = json.NewEncoder(w).Encode(map[string]any{"memory_stats": map[string]any{
			"usage": 100 << 20, "stats": map[string]any{"inactive_file": 10 << 20}}})
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/json"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")
		spec, ok := f.containers[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"State": map[string]bool{"Running": true},
			"Config":          map[string]any{"Labels": spec["Labels"]},
			"NetworkSettings": map[string]any{"Networks": map[string]any{"lattis_default": map[string]string{"IPAddress": "172.18.0.9"}}}})
	case r.Method == http.MethodDelete:
		id := strings.TrimPrefix(p, "/containers/")
		if _, ok := f.containers[id]; !ok {
			// Suppression par nom (ancien conteneur du même utilisateur).
			if _, ok := f.containers["c"+id]; !ok {
				http.NotFound(w, r)
				return
			}
			id = "c" + id
		}
		delete(f.containers, id)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/stop"):
		delete(f.containers, strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/stop"))
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func TestServer(t *testing.T) {
	fd := &fakeDocker{containers: map[string]map[string]any{}}
	ds := httptest.NewServer(fd)
	defer ds.Close()
	s := &Server{Docker: &Docker{Base: ds.URL + "/v1.43", HTTP: ds.Client()}, Config: Config{
		Token: strings.Repeat("j", 32), Image: "moteur:1", Network: "lattis_default", Server: "https://l",
		TokenURL: "http://lattisd:8081/engine/token", Idle: "30m", Memory: 1 << 30, NanoCPUs: 1e9, Pids: 256,
		VolumePrefix: "lattis-web-", Max: 1}}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	call := func(method, path, token, body string) (*http.Response, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp, out
	}
	tok := strings.Repeat("j", 32)
	secret := strings.Repeat("s", 43)
	if resp, _ := call("POST", "/engines", "faux", `{}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("jeton faux : %d", resp.StatusCode)
	}
	if resp, _ := call("POST", "/engines", tok, `{"user_id":"../x","secret":"`+secret+`"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("utilisateur invalide : %d", resp.StatusCode)
	}
	resp, out := call("POST", "/engines", tok, `{"user_id":"u1","secret":"`+secret+`"}`)
	if resp.StatusCode != http.StatusCreated || out["addr"] != "172.18.0.9:8102" || out["id"] != "clattis-engine-u1" {
		t.Fatalf("démarrage : %d %v", resp.StatusCode, out)
	}
	b, _ := json.Marshal(fd.created)
	for _, want := range []string{`"Image":"moteur:1"`, `"ReadonlyRootfs":true`, `"CapDrop":["ALL"]`, `"no-new-privileges"`,
		`"Source":"lattis-web-u1"`, `"LATTIS_ENGINE_SECRET=` + secret + `"`, `"OpenStdin":true`, `"--token-url","http://lattisd:8081/engine/token"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("conteneur sans %s : %s", want, b)
		}
	}
	if resp, _ := call("POST", "/engines", tok, `{"user_id":"u2","secret":"`+secret+`"}`); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("au-delà du maximum : %d", resp.StatusCode)
	}
	if resp, out := call("GET", "/engines/clattis-engine-u1", tok, ""); resp.StatusCode != http.StatusOK || out["running"] != true {
		t.Fatalf("état : %d %v", resp.StatusCode, out)
	}
	if resp, out := call("GET", "/engines/stats", tok, ""); resp.StatusCode != http.StatusOK || out["memory"] != float64(90<<20) || out["engines"] != float64(1) {
		t.Fatalf("mémoire : %d %v", resp.StatusCode, out)
	}
	if resp, _ := call("DELETE", "/engines/clattis-engine-u1", tok, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("arrêt : %d", resp.StatusCode)
	}
	if resp, _ := call("GET", "/engines/clattis-engine-u1", tok, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("après arrêt : %d", resp.StatusCode)
	}
	call("POST", "/engines", tok, `{"user_id":"u3","secret":"`+secret+`"}`)
	if resp, _ := call("POST", "/engines/cleanup", tok, ""); resp.StatusCode != http.StatusNoContent || len(fd.containers) != 0 {
		t.Fatalf("nettoyage : %d, %d conteneurs", resp.StatusCode, len(fd.containers))
	}
}
