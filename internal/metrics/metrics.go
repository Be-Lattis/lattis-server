// Package metrics expose les mesures Prometheus de lattisd (§13).
//
// Le serveur n'embarque aucune règle de mise à l'échelle : il publie, et
// l'exploitant décide.
package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics regroupe les mesures publiées.
type Metrics struct {
	Registry    *prometheus.Registry
	Logins      *prometheus.CounterVec
	Enrollments *prometheus.CounterVec
	BuildInfo   *prometheus.GaugeVec
}

// New crée les mesures ; db alimente les jauges lues dans pg_stat_activity.
func New(server string, db *pgxpool.Pool) *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		Logins: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "lattis_auth_logins_total", Help: "Connexions par fournisseur et résultat.",
		}, []string{"provider", "result"}),
		Enrollments: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "lattis_enrollments_total", Help: "Comptes créés ou activés à la connexion.",
		}, []string{"provider", "mode"}),
		BuildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "lattis_build_info", Help: "Version de lattisd.",
		}, []string{"version", "edition", "pg_version"}),
	}
	m.Registry.MustRegister(m.Logins, m.Enrollments, m.BuildInfo,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		&pgCollector{server: server, db: db})
	return m
}

// Résultats de connexion (étiquette result).
const (
	ResultOK       = "ok"
	ResultTenant   = "denied_tenant"
	ResultInvite   = "not_invited"
	ResultDisabled = "disabled"
	ResultError    = "error"
)

var (
	descConnections = prometheus.NewDesc("lattis_connections", "Connexions PostgreSQL par état.",
		[]string{"server", "state"}, nil)
	descRunning = prometheus.NewDesc("lattis_queries_running", "Requêtes en cours (lourdes ou légères).",
		[]string{"server", "kind"}, nil)
	descWaiting = prometheus.NewDesc("lattis_queries_waiting", "Requêtes en attente d'un verrou.",
		[]string{"server"}, nil)
	descSessions = prometheus.NewDesc("lattis_sessions_active", "Utilisateurs distincts connectés, par type de client.",
		[]string{"client"}, nil)
)

// pgCollector lit pg_stat_activity à chaque relevé. Le classement lourd ou
// léger vient de application_name, posé par les clients (Lattis/<version>/<usage>).
type pgCollector struct {
	server string
	db     *pgxpool.Pool
}

func (c *pgCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descConnections
	ch <- descRunning
	ch <- descWaiting
	ch <- descSessions
}

func (c *pgCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := c.db.Query(ctx, `
		SELECT coalesce(state, 'unknown'),
		       CASE WHEN split_part(application_name, '/', 3) IN ('initial_load', 'pivot', 'sql', 'power_query')
		            THEN 'heavy' ELSE 'light' END,
		       wait_event_type = 'Lock',
		       count(*)
		FROM pg_stat_activity
		WHERE backend_type = 'client backend' AND datname = current_database() AND pid <> pg_backend_pid()
		GROUP BY 1, 2, 3`)
	if err != nil {
		slog.Warn("mesures pg_stat_activity", "err", err)
		return
	}
	defer rows.Close()
	conns := map[string]float64{"active": 0, "idle": 0, "idle_in_transaction": 0}
	running := map[string]float64{"heavy": 0, "light": 0}
	var waiting float64
	for rows.Next() {
		var state, kind string
		var locked *bool
		var n float64
		if err := rows.Scan(&state, &kind, &locked, &n); err != nil {
			return
		}
		switch state {
		case "idle in transaction", "idle in transaction (aborted)":
			state = "idle_in_transaction"
		}
		if _, ok := conns[state]; ok {
			conns[state] += n
		}
		if state == "active" {
			running[kind] += n
			if locked != nil && *locked {
				waiting += n
			}
		}
	}
	for s, n := range conns {
		ch <- prometheus.MustNewConstMetric(descConnections, prometheus.GaugeValue, n, c.server, s)
	}
	for k, n := range running {
		ch <- prometheus.MustNewConstMetric(descRunning, prometheus.GaugeValue, n, c.server, k)
	}
	ch <- prometheus.MustNewConstMetric(descWaiting, prometheus.GaugeValue, waiting, c.server)

	// Sessions : utilisateurs distincts (un poste ouvre plusieurs connexions).
	var desktop, web float64
	if err := c.db.QueryRow(ctx, `
		SELECT count(DISTINCT usename) FILTER (WHERE application_name LIKE 'Lattis/%'),
		       count(DISTINCT usename) FILTER (WHERE application_name LIKE 'lattisd/web%')
		FROM pg_stat_activity
		WHERE backend_type = 'client backend' AND datname = current_database()`).Scan(&desktop, &web); err != nil {
		slog.Warn("mesures des sessions", "err", err)
		return
	}
	ch <- prometheus.MustNewConstMetric(descSessions, prometheus.GaugeValue, desktop, "desktop")
	ch <- prometheus.MustNewConstMetric(descSessions, prometheus.GaugeValue, web, "web")
}
