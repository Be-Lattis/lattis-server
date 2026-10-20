package events_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/events"
	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
	"github.com/Be-Lattis/lattis-server/internal/userdb"
)

type ev struct {
	kind string
	data map[string]any
}

// stream lit les événements d'un flux SSE.
func stream(t *testing.T, url string) (<-chan ev, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan ev, 32)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		out <- ev{kind: "status", data: map[string]any{"code": resp.StatusCode}}
		close(out)
		return out, cancel
	}
	go func() {
		defer close(out)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var kind string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				kind = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				var d map[string]any
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &d)
				out <- ev{kind, d}
			}
		}
	}()
	return out, cancel
}

func next(t *testing.T, c <-chan ev, kind string) ev {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case e, ok := <-c:
			if !ok {
				t.Fatalf("flux fermé en attendant « %s »", kind)
			}
			if e.kind == kind {
				return e
			}
		case <-timeout:
			t.Fatalf("« %s » non reçu", kind)
		}
	}
}

// Un poste suit un classeur sans connexion à PostgreSQL : il reçoit le
// changement d'un collègue avec la position WAL, les notifications du Data
// Lake, et perd le flux du classeur quand on lui retire l'accès.
func TestRelay(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var google string
	must(db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT lattis.set_config('access_mode', '"open"')`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT lattis.add_provider('google', 'Google', NULL, 'cid', NULL, NULL)::text`).Scan(&google)
	}))
	enroll := func(n string) store.Enrollment {
		u, err := db.Store.Enroll(ctx, store.Identity{ProviderID: google, Subject: n, Email: testpg.Email(n), EmailVerified: true})
		must(err)
		return u
	}
	owner, reader, outsider := enroll("auteur"), enroll("lecteur"), enroll("dehors")
	as := func(u store.Enrollment, q string, dst any, args ...any) error {
		return db.AsUser(t, u.PGRole, func(tx pgx.Tx) error {
			if dst == nil {
				_, err := tx.Exec(ctx, q, args...)
				return err
			}
			return tx.QueryRow(ctx, q, args...).Scan(dst)
		})
	}
	var wb, tid, schema string
	must(as(owner, `SELECT lattis.create_workbook('Ventes', 'collaborative')::text`, &wb))
	must(as(owner, `SELECT lattis.create_table($1::uuid, 'Lignes', '[{"name":"Article","type":"text"}]')::text`, &tid, wb))
	must(as(owner, `SELECT lattis.my_capabilities($1::uuid) ->> 'schema'`, &schema, wb))
	must(as(owner, `SELECT lattis.finish_publish($1::uuid)`, new(int), wb))
	must(as(owner, `SELECT lattis.invite($1, $2::uuid, ARRAY['reader'])`, nil, reader.Email, wb))

	hub := &events.Hub{Pool: db.Store.DB, Users: userdb.SessionAuth{Pool: db.Super}}
	rctx, stop := context.WithCancel(ctx)
	defer stop()
	go hub.Run(rctx)
	who := map[string]store.Enrollment{"reader": reader, "outsider": outsider}
	srv := httptest.NewServer(hub.Handler(func(r *http.Request) (store.User, time.Time, error) {
		u, err := db.Store.User(r.Context(), who[r.URL.Query().Get("u")].UserID)
		var exp time.Time
		if r.URL.Query().Get("short") == "1" {
			exp = time.Now().Add(300 * time.Millisecond)
		}
		return u, exp, err
	}))
	defer srv.Close()

	// Jeton qui expire : le flux se ferme avec « expired » (le poste se
	// réabonne avec un jeton neuf).
	c, cancel := stream(t, srv.URL+"?u=reader&short=1&workbooks="+wb)
	next(t, c, "ready")
	next(t, c, "expired")
	cancel()

	// Classeur interdit : refusé avant tout flux.
	c, cancel = stream(t, srv.URL+"?u=outsider&workbooks="+wb)
	if e := <-c; e.kind != "status" || e.data["code"] != http.StatusForbidden {
		t.Fatalf("classeur interdit : %+v", e)
	}
	cancel()

	c, cancel = stream(t, srv.URL+"?u=reader&data=1&workbooks="+wb)
	defer cancel()
	next(t, c, "ready")
	// Présence : les membres qui suivent le classeur (ici le lecteur seul).
	if e := next(t, c, "presence"); e.data["workbook"] != wb || len(e.data["users"].([]any)) != 1 {
		t.Fatalf("présence : %+v", e.data)
	}
	must(as(owner, `INSERT INTO `+pgx.Identifier{schema, "Lignes"}.Sanitize()+` (lattis_id, "Article") VALUES (gen_random_uuid(), 'Vis')`, nil))
	e := next(t, c, "change")
	if e.data["workbook"] != wb || e.data["payload"] != tid || e.data["lsn"] == nil {
		t.Fatalf("changement : %+v", e.data)
	}
	_, err := db.Super.Exec(ctx, `SELECT pg_notify('lattis_data', '*catalog')`)
	must(err)
	if e := next(t, c, "data"); e.data["payload"] != "*catalog" {
		t.Fatalf("Data Lake : %+v", e.data)
	}
	// Accès retiré : le flux du classeur s'arrête pour ce poste.
	var role string
	must(db.Super.QueryRow(ctx, `SELECT id::text FROM lattis.roles WHERE workbook_id = $1 AND builtin = 'reader'`, wb).Scan(&role))
	must(as(owner, `SELECT lattis.revoke_role($1::uuid, $2::uuid)`, nil, role, reader.UserID))
	if e := next(t, c, "revoked"); e.data["workbook"] != wb {
		t.Fatalf("retrait : %+v", e.data)
	}
	if hub.Subscribers() != 1 {
		t.Fatalf("abonnés : %d", hub.Subscribers())
	}
}
