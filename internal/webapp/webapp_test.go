package webapp_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Be-Lattis/lattis-server/internal/config"
	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/tokens"
	"github.com/Be-Lattis/lattis-server/internal/webapp"
)

// Le binaire de test sert aussi de faux lattis-engine : lancé avec
// --token-url, il écoute, annonce son adresse et répond sous /app/.
func TestMain(m *testing.M) {
	for _, a := range os.Args {
		if a == "--token-url" {
			fakeEngine()
			return
		}
	}
	os.Exit(m.Run())
}

func fakeEngine() {
	fs := flag.NewFlagSet("engine", flag.ExitOnError)
	addr := fs.String("addr", "", "")
	tokenURL := fs.String("token-url", "", "")
	base := fs.String("base-path", "", "")
	fs.String("dir", "", "")
	fs.String("server", "", "")
	fs.String("idle", "", "")
	_ = fs.Parse(os.Args[1:])
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		os.Exit(2)
	}
	fmt.Println("démarrage du faux moteur")
	fmt.Println("LISTENING " + ln.Addr().String())
	mux := http.NewServeMux()
	mux.HandleFunc(*base+"/", func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequest("GET", *tokenURL, nil)
		req.Header.Set("Authorization", "Bearer "+os.Getenv("LATTIS_ENGINE_SECRET"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer resp.Body.Close()
		var tok map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&tok)
		w.Header().Set("Content-Security-Policy", "default-src *")
		_ = json.NewEncoder(w).Encode(map[string]any{"path": r.URL.Path, "pg_role": tok["pg_role"],
			"email": tok["email"], "user_id": tok["user_id"], "display_name": tok["display_name"],
			"cookie": r.Header.Get("Cookie"), "env_pg": os.Getenv("LATTIS_PG_RW_URL")})
	})
	go func() { _ = http.Serve(ln, mux) }()
	_, _ = io.Copy(io.Discard, os.Stdin) // stdin fermé : arrêt
}

type users map[string]store.User

func (u users) User(_ context.Context, id string) (store.User, error) {
	if x, ok := u[id]; ok {
		return x, nil
	}
	return store.User{}, errors.New("inconnu")
}

func TestManager(t *testing.T) {
	t.Setenv("LATTIS_PG_RW_URL", "postgres://secret") // ne doit pas parvenir au moteur
	pem, _ := tokens.GenerateKeyPEM()
	keys, _ := tokens.ParseKeys(pem)
	us := users{"a": {ID: "a", PGRole: "anne@acme.fr", Email: "anne@acme.fr", DisplayName: "Anne"}, "b": {ID: "b", PGRole: "bob@acme.fr"}}
	m := &webapp.Manager{Users: us, Keys: keys,
		Session: func(r *http.Request) (store.User, error) {
			if u, ok := us[r.Header.Get("X-Test-User")]; ok {
				return u, nil
			}
			return store.User{}, errors.New("pas de session")
		}}
	mux := http.NewServeMux()
	m.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	pub, _ := url.Parse(srv.URL)
	m.Config = &config.Config{PublicURL: pub, WebEngine: os.Args[0], WebCacheDir: t.TempDir(), WebMaxEngines: 1, WebIdle: time.Minute}
	m.TokenURL = srv.URL + "/engine/token"
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m.Stop(ctx)
	}()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(user, path string, h ...string) (*http.Response, string) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if user != "" {
			req.Header.Set("X-Test-User", user)
		}
		for i := 0; i+1 < len(h); i += 2 {
			req.Header.Set(h[i], h[i+1])
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}

	if resp, _ := get("", "/app/?workbook=x"); resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), "/admin/login?next=%2Fapp%2F") {
		t.Fatalf("sans session : %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, body := get("a", "/app/index.html", "Cookie", "lattis_admin=secret")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("moteur : %d %s", resp.StatusCode, body)
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(body), &got)
	// Identité de la session (compte affiché par l'interface web).
	if got["email"] != "anne@acme.fr" || got["user_id"] != "a" || got["display_name"] != "Anne" {
		t.Fatalf("identité remise au moteur : %v", got)
	}
	if got["pg_role"] != "anne@acme.fr" || got["path"] != "/app/index.html" || got["cookie"] != "" || got["env_pg"] != "" {
		t.Fatalf("réponse du moteur : %v", got)
	}
	if resp.Header.Get("Content-Security-Policy") != "" {
		t.Fatal("la politique du moteur doit être retirée (lattisd fixe la sienne)")
	}
	if m.Count() != 1 {
		t.Fatalf("moteurs : %d", m.Count())
	}
	// Même utilisateur : même moteur.
	if resp, _ := get("a", "/app/"); resp.StatusCode != http.StatusOK || m.Count() != 1 {
		t.Fatalf("second appel : %d, %d moteurs", resp.StatusCode, m.Count())
	}
	if resp, _ := get("b", "/app/"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("au-delà du maximum : %d", resp.StatusCode)
	}
	if resp, _ := get("a", "/app/", "Origin", "https://evil.example"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("autre origine : %d", resp.StatusCode)
	}
	if resp, _ := get("", "/engine/token", "Authorization", "Bearer faux"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("secret inconnu : %d", resp.StatusCode)
	}
}

// Réserve devant des processus : l'utilisateur reçoit un moteur déjà
// démarré, qui obtient bien ses jetons à lui ; au plafond, la page
// « Plus de ressources disponibles ».
func TestManagerPool(t *testing.T) {
	pem, _ := tokens.GenerateKeyPEM()
	keys, _ := tokens.ParseKeys(pem)
	us := users{"a": {ID: "a", PGRole: "anne@acme.fr"}, "b": {ID: "b", PGRole: "bob@acme.fr"}, "c": {ID: "c", PGRole: "chloe@acme.fr"}}
	m := &webapp.Manager{Users: us, Keys: keys, Session: func(r *http.Request) (store.User, error) {
		if u, ok := us[r.Header.Get("X-Test-User")]; ok {
			return u, nil
		}
		return store.User{}, errors.New("pas de session")
	}}
	mux := http.NewServeMux()
	m.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	pub, _ := url.Parse(srv.URL)
	cache := t.TempDir()
	m.Config = &config.Config{PublicURL: pub, WebEngine: os.Args[0], WebCacheDir: cache, WebMaxEngines: 2, WebIdle: time.Minute}
	m.TokenURL = srv.URL + "/engine/token"
	pool := &webapp.Pool{Inner: &webapp.ProcessLauncher{Bin: os.Args[0], CacheDir: cache}, Spare: 1, Max: 2,
		Template: webapp.Spec{Server: srv.URL, TokenURL: m.TokenURL, Idle: time.Minute}}
	m.Launcher = pool
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m.Stop(ctx)
	}()
	m.Init(context.Background())
	waitReady := func(n int) {
		t.Helper()
		for i := 0; i < 300 && pool.Ready() != n; i++ {
			time.Sleep(20 * time.Millisecond)
		}
		if pool.Ready() != n {
			t.Fatalf("moteurs prêts : %d, attendu %d", pool.Ready(), n)
		}
	}
	waitReady(1)
	get := func(user string) (int, string) {
		req, _ := http.NewRequest("GET", srv.URL+"/app/", nil)
		req.Header.Set("X-Test-User", user)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	code, body := get("a")
	var got map[string]any
	_ = json.Unmarshal([]byte(body), &got)
	if code != http.StatusOK || got["pg_role"] != "anne@acme.fr" {
		t.Fatalf("moteur de la réserve : %d %s", code, body)
	}
	waitReady(1) // 1 attribué + 1 prêt = plafond
	if code, body := get("b"); code != http.StatusOK || !strings.Contains(body, "bob@acme.fr") {
		t.Fatalf("second utilisateur : %d %s", code, body)
	}
	if code, body := get("c"); code != http.StatusServiceUnavailable || !strings.Contains(body, "Plus de ressources disponibles") {
		t.Fatalf("au plafond : %d %s", code, body)
	}
}
