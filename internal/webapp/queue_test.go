package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Be-Lattis/lattis-server/internal/config"
	"github.com/Be-Lattis/lattis-server/internal/store"
)

// File d'attente : position affichée et interrogée, place libérée donnée au
// premier de la file avant un nouveau venu, ticket abandonné expiré.
func TestQueue(t *testing.T) {
	l := &memLauncher{}
	clock := time.Now()
	pub, _ := url.Parse("https://l.example")
	m := &Manager{Config: &config.Config{PublicURL: pub, WebMaxEngines: 10, WebIdle: time.Minute}, Launcher: l,
		Limits: func(context.Context) Limits { return Limits{Sessions: 1} }, Now: func() time.Time { return clock }}
	ctx := context.Background()
	if _, err := m.engine(ctx, store.User{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	// Plein : b puis c prennent un ticket.
	wait := func(user string) (string, int) {
		t.Helper()
		_, err := m.engineFor(ctx, store.User{ID: user}, "")
		var le *LimitError
		if !errors.As(err, &le) {
			t.Fatalf("%s : limite attendue, %v", user, err)
		}
		rec := httptest.NewRecorder()
		m.queueResponse(rec, httptest.NewRequest("GET", "/app/", nil), le, queueApp)
		var tok string
		for _, c := range rec.Result().Cookies() {
			if c.Name == queueCookie {
				tok = c.Value
			}
		}
		if rec.Code != 503 || tok == "" || !strings.Contains(rec.Body.String(), `id="pos"`) {
			t.Fatalf("page d'attente : %d %q", rec.Code, tok)
		}
		var st struct {
			Position int  `json:"position"`
			Ready    bool `json:"ready"`
		}
		req := httptest.NewRequest("GET", "/queue", nil)
		req.AddCookie(&http.Cookie{Name: queueCookie, Value: tok})
		rec = httptest.NewRecorder()
		m.queueStatus(rec, req)
		_ = json.Unmarshal(rec.Body.Bytes(), &st)
		return tok, st.Position
	}
	tb, pb := wait("b")
	tc, pc := wait("c")
	if pb != 1 || pc != 2 {
		t.Fatalf("positions : %d %d", pb, pc)
	}
	// La place de a se libère : c (deuxième) et un nouveau venu attendent, b passe.
	m.forget(m.engines["a"])
	if _, err := m.engineFor(ctx, store.User{ID: "c"}, tc); err == nil {
		t.Fatal("c passe devant b")
	}
	if _, err := m.engineFor(ctx, store.User{ID: "d"}, ""); err == nil {
		t.Fatal("nouveau venu passe devant la file")
	}
	if _, err := m.engineFor(ctx, store.User{ID: "b"}, tb); err != nil {
		t.Fatalf("b à son tour : %v", err)
	}
	// b parti, puis la place se libère : c, abandonné, a expiré ; d passe.
	m.forget(m.engines["b"])
	clock = clock.Add(queueTTL + time.Second)
	req := httptest.NewRequest("GET", "/queue", nil)
	req.AddCookie(&http.Cookie{Name: queueCookie, Value: tc})
	rec := httptest.NewRecorder()
	m.queueStatus(rec, req)
	if !strings.Contains(rec.Body.String(), `"expired":true`) {
		t.Fatalf("ticket abandonné : %s", rec.Body.String())
	}
	if _, err := m.engineFor(ctx, store.User{ID: "d"}, ""); err != nil {
		t.Fatalf("file vide : %v", err)
	}
}
