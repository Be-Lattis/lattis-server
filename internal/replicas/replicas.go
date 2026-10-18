// Package replicas surveille les réplicas PostgreSQL (spécification §12) :
// retard en secondes et en octets, mesures Prometheus, et liste des points
// de lecture annoncés par la découverte. Un réplica injoignable, sorti de
// la récupération (promu) ou en retard au-delà du seuil est retiré de la
// liste jusqu'à son retour.
package replicas

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/Be-Lattis/lattis-server/internal/config"
)

// Interval est la période des relevés.
const Interval = 5 * time.Second

// State est l'état d'un réplica au dernier relevé.
type State struct {
	Name       string          `json:"name"`
	Endpoint   config.Endpoint `json:"endpoint"`
	Reachable  bool            `json:"reachable"`
	InRecovery bool            `json:"in_recovery"`
	LagSeconds float64         `json:"lag_seconds"`
	LagBytes   float64         `json:"lag_bytes"`
	Available  bool            `json:"available"`
	Error      string          `json:"error,omitempty"`
	CheckedAt  time.Time       `json:"checked_at"`
}

type replica struct {
	name     string
	endpoint config.Endpoint
	pool     *pgxpool.Pool
}

// Monitor relève l'état des réplicas.
type Monitor struct {
	primary *pgxpool.Pool
	maxLag  time.Duration
	// static : points annoncés sans surveillance (aucune connexion de service).
	static   []config.Endpoint
	replicas []replica

	mu     sync.RWMutex
	states []State
	ready  bool
}

// New prépare la surveillance. urls et public vont par paires (même ordre) ;
// sans urls, public est annoncé tel quel.
func New(ctx context.Context, primary *pgxpool.Pool, urls []string, public []config.Endpoint, maxLag time.Duration) (*Monitor, error) {
	m := &Monitor{primary: primary, maxLag: maxLag}
	if len(urls) == 0 {
		m.static = append([]config.Endpoint{}, public...)
		return m, nil
	}
	for i, u := range urls {
		cfg, err := pgxpool.ParseConfig(u)
		if err != nil {
			m.Close()
			return nil, err
		}
		cfg.MaxConns = 1
		cfg.ConnConfig.ConnectTimeout = 3 * time.Second
		cfg.ConnConfig.RuntimeParams["application_name"] = "lattisd/replicas"
		p, err := pgxpool.NewWithConfig(ctx, cfg) // connexion paresseuse
		if err != nil {
			m.Close()
			return nil, err
		}
		m.replicas = append(m.replicas, replica{name: public[i].String(), endpoint: public[i], pool: p})
	}
	return m, nil
}

// Close ferme les connexions aux réplicas.
func (m *Monitor) Close() {
	for _, r := range m.replicas {
		r.pool.Close()
	}
}

// Run relève l'état toutes les Interval jusqu'à l'annulation de ctx.
func (m *Monitor) Run(ctx context.Context) {
	if len(m.replicas) == 0 {
		return
	}
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		m.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Check fait un relevé.
func (m *Monitor) Check(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var primaryLSN string
	if err := m.primary.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&primaryLSN); err != nil {
		slog.Warn("réplicas : position WAL du parent illisible", "err", err)
	}
	states := make([]State, len(m.replicas))
	var wg sync.WaitGroup
	for i, r := range m.replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			states[i] = m.check(ctx, r, primaryLSN)
		}()
	}
	wg.Wait()
	m.mu.Lock()
	prev := m.states
	m.states, m.ready = states, true
	m.mu.Unlock()
	for i, s := range states {
		if i < len(prev) && prev[i].Available != s.Available {
			if s.Available {
				slog.Info("réplica de nouveau annoncé", "replica", s.Name, "lag_seconds", s.LagSeconds)
			} else {
				slog.Warn("réplica retiré de la découverte", "replica", s.Name, "lag_seconds", s.LagSeconds, "err", s.Error)
			}
		}
	}
}

func (m *Monitor) check(ctx context.Context, r replica, primaryLSN string) State {
	s := State{Name: r.name, Endpoint: r.endpoint, CheckedAt: time.Now()}
	// Retard en temps : nul quand tout le WAL reçu est rejoué (un parent sans
	// écriture ne doit pas faire croire à un réplica en retard).
	var lagBytes *float64
	err := r.pool.QueryRow(ctx, `
		SELECT pg_is_in_recovery(),
		       CASE WHEN pg_last_wal_receive_lsn() IS NULL OR pg_last_wal_receive_lsn() = pg_last_wal_replay_lsn() THEN 0
		            ELSE coalesce(extract(epoch FROM now() - pg_last_xact_replay_timestamp()), 0) END,
		       CASE WHEN $1 = '' THEN NULL ELSE pg_wal_lsn_diff($1::pg_lsn, pg_last_wal_replay_lsn()) END`,
		primaryLSN).Scan(&s.InRecovery, &s.LagSeconds, &lagBytes)
	if err != nil {
		s.Error = err.Error()
		return s
	}
	s.Reachable = true
	if lagBytes != nil && *lagBytes > 0 {
		s.LagBytes = *lagBytes
	}
	switch {
	case !s.InRecovery:
		s.Error = "n'est plus en récupération (promu ?)"
	case s.LagSeconds > m.maxLag.Seconds():
		s.Error = "retard au-delà du seuil"
	default:
		s.Available = true
	}
	return s
}

// Available renvoie les points de lecture à annoncer. Avant le premier
// relevé, tous les réplicas surveillés sont annoncés.
func (m *Monitor) Available() []config.Endpoint {
	if len(m.replicas) == 0 {
		return append([]config.Endpoint{}, m.static...)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []config.Endpoint{}
	if !m.ready {
		for _, r := range m.replicas {
			out = append(out, r.endpoint)
		}
		return out
	}
	for _, s := range m.states {
		if s.Available {
			out = append(out, s.Endpoint)
		}
	}
	return out
}

// States renvoie le dernier relevé (vide sans surveillance).
func (m *Monitor) States() []State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]State{}, m.states...)
}

var (
	descLagSeconds = prometheus.NewDesc("lattis_replica_lag_seconds", "Retard de rejeu du réplica.", []string{"replica"}, nil)
	descLagBytes   = prometheus.NewDesc("lattis_replica_lag_bytes", "WAL du parent pas encore rejoué par le réplica.", []string{"replica"}, nil)
	descAvailable  = prometheus.NewDesc("lattis_replicas_available", "Réplicas annoncés par la découverte.", nil, nil)
)

// Describe et Collect publient les mesures du dernier relevé.
func (m *Monitor) Describe(ch chan<- *prometheus.Desc) {
	ch <- descLagSeconds
	ch <- descLagBytes
	ch <- descAvailable
}

func (m *Monitor) Collect(ch chan<- prometheus.Metric) {
	for _, s := range m.States() {
		if s.Reachable {
			ch <- prometheus.MustNewConstMetric(descLagSeconds, prometheus.GaugeValue, s.LagSeconds, s.Name)
			ch <- prometheus.MustNewConstMetric(descLagBytes, prometheus.GaugeValue, s.LagBytes, s.Name)
		}
	}
	ch <- prometheus.MustNewConstMetric(descAvailable, prometheus.GaugeValue, float64(len(m.Available())))
}
