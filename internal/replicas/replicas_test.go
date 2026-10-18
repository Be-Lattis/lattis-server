package replicas_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Be-Lattis/lattis-server/internal/config"
	"github.com/Be-Lattis/lattis-server/internal/replicas"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

func TestStatic(t *testing.T) {
	pub := []config.Endpoint{{Host: "ro1", Port: 5432}}
	m, err := replicas.New(context.Background(), nil, nil, pub, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Available(); len(got) != 1 || got[0] != pub[0] {
		t.Fatalf("annoncés : %v", got)
	}
	if n := testutil.CollectAndCount(m, "lattis_replicas_available"); n != 1 {
		t.Fatalf("mesures : %d", n)
	}
}

// Un serveur hors récupération (promu) et un réplica injoignable sont
// retirés de la découverte après le premier relevé.
func TestCheck(t *testing.T) {
	db := testpg.New(t)
	ctx := context.Background()
	pub := []config.Endpoint{{Host: "promu", Port: 5432}, {Host: "absent", Port: 5432}}
	m, err := replicas.New(ctx, db.Super, []string{db.SuperURL, "postgres://x@127.0.0.1:1/x?connect_timeout=1"}, pub, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if got := m.Available(); len(got) != 2 {
		t.Fatalf("avant relevé : %v", got)
	}
	m.Check(ctx)
	if got := m.Available(); len(got) != 0 {
		t.Fatalf("après relevé : %v", got)
	}
	st := m.States()
	if !st[0].Reachable || st[0].InRecovery || !strings.Contains(st[0].Error, "promu") {
		t.Fatalf("promu : %+v", st[0])
	}
	if st[1].Reachable || st[1].Error == "" {
		t.Fatalf("absent : %+v", st[1])
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(m)
	if err := testutil.GatherAndCompare(reg, strings.NewReader(`
# HELP lattis_replicas_available Réplicas annoncés par la découverte.
# TYPE lattis_replicas_available gauge
lattis_replicas_available 0
`), "lattis_replicas_available"); err != nil {
		t.Fatal(err)
	}
	if n := testutil.CollectAndCount(m, "lattis_replica_lag_seconds"); n != 1 {
		t.Fatalf("retard mesuré pour %d serveurs", n)
	}
}
