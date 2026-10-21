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
	// PipelineRuns : exécutions des pipelines d'ingestion par statut ;
	// PipelineDuration : leur durée ; PipelineRows : lignes par effet.
	PipelineRuns     *prometheus.CounterVec
	PipelineDuration *prometheus.HistogramVec
	PipelineRows     *prometheus.CounterVec
}

// Observe compte la durée et les lignes d'une exécution de pipeline.
func (m *Metrics) Observe(status string, seconds float64, rows map[string]int) {
	m.PipelineDuration.WithLabelValues(status).Observe(seconds)
	for k, n := range rows {
		m.PipelineRows.WithLabelValues(k).Add(float64(n))
	}
}

// Inc compte une exécution de pipeline (pipeline.Counter).
func (m *Metrics) Inc(status string) { m.PipelineRuns.WithLabelValues(status).Inc() }

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
		PipelineRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "lattis_pipeline_runs_total", Help: "Exécutions des pipelines d'ingestion par statut (ok, warning, error).",
		}, []string{"status"}),
		PipelineDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "lattis_pipeline_duration_seconds", Help: "Durée des exécutions de pipelines.",
			Buckets: []float64{1, 5, 15, 60, 300, 900, 1800},
		}, []string{"status"}),
		PipelineRows: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "lattis_pipeline_rows_total", Help: "Lignes traitées par les pipelines (read, inserted, updated, deleted, rejected).",
		}, []string{"kind"}),
	}
	m.Registry.MustRegister(m.Logins, m.Enrollments, m.BuildInfo, m.PipelineRuns, m.PipelineDuration, m.PipelineRows,
		&lattisCollector{db: db},
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
	// ResultEmailTaken : adresse déjà prise par un compte d'un autre
	// fournisseur, que celui-ci ne peut pas relier.
	ResultEmailTaken = "email_taken"
	ResultError      = "error"
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

var (
	descOldestRun = prometheus.NewDesc("lattis_pipeline_oldest_running_seconds",
		"Âge de la plus ancienne exécution de pipeline en cours (0 : aucune).", nil, nil)
	descUnavailable = prometheus.NewDesc("lattis_datasets_unavailable",
		"Données du Data Lake dont la source est indisponible (classeur supprimé ou tableau introuvable).", nil, nil)
)

// lattisCollector lit l'état des pipelines et du Data Lake à chaque relevé.
type lattisCollector struct{ db *pgxpool.Pool }

func (c *lattisCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descOldestRun
	ch <- descUnavailable
}

func (c *lattisCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var oldest, unavailable float64
	if err := c.db.QueryRow(ctx, `SELECT coalesce(extract(epoch FROM now() - min(started_at)), 0)::float8
		FROM lattis.pipeline_runs WHERE status = 'running'`).Scan(&oldest); err == nil {
		ch <- prometheus.MustNewConstMetric(descOldestRun, prometheus.GaugeValue, oldest)
	}
	if err := c.db.QueryRow(ctx, `SELECT count(*)::float8 FROM lattis.datasets WHERE workbook_id IS NULL OR table_id IS NULL`).
		Scan(&unavailable); err == nil {
		ch <- prometheus.MustNewConstMetric(descUnavailable, prometheus.GaugeValue, unavailable)
	}
}
