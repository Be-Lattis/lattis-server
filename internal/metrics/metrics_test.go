package metrics_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Be-Lattis/lattis-server/internal/metrics"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

func TestCollector(t *testing.T) {
	db := testpg.New(t)
	cfg := db.Super.Config().Copy()
	cfg.ConnConfig.RuntimeParams["application_name"] = "Lattis/1.0/initial_load"
	client, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := metrics.New("pg", db.Super)
	n, err := testutil.GatherAndCount(m.Registry, "lattis_connections", "lattis_queries_running", "lattis_sessions_active")
	if err != nil || n != 3+2+2 {
		t.Fatalf("séries : %d %v", n, err)
	}
	if err := testutil.GatherAndCompare(m.Registry, strings.NewReader(`
# HELP lattis_sessions_active Utilisateurs distincts connectés, par type de client.
# TYPE lattis_sessions_active gauge
lattis_sessions_active{client="desktop"} 1
lattis_sessions_active{client="web"} 0
`), "lattis_sessions_active"); err != nil {
		t.Fatal(err)
	}
}
