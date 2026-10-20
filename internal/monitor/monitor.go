// Package monitor relève, à intervalle régulier, les utilisateurs connectés,
// les moteurs web de ce lattisd et la mémoire occupée, et garde la dernière
// heure en mémoire pour l'écran Surveillance de l'administration.
package monitor

import (
	"context"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Be-Lattis/lattis-server/internal/webapp"
)

// Sample : un relevé.
type Sample struct {
	At time.Time
	// Desktop, Web : utilisateurs distincts connectés à PostgreSQL, par
	// l'application de bureau ou par un moteur web (tous les lattisd).
	Desktop, Web int
	// Engines : moteurs attribués de ce lattisd ; Ready : moteurs prêts.
	Engines, Ready int
	// EngineMem : mémoire des moteurs (-1 : inconnue) ; ServerMem : mémoire
	// de lattisd.
	EngineMem, ServerMem int64
}

// Users : total des utilisateurs connectés.
func (s Sample) Users() int { return s.Desktop + s.Web }

// Monitor tient l'historique des relevés.
type Monitor struct {
	DB *pgxpool.Pool
	// Web : moteurs de ce lattisd ; nil hors édition web.
	Web func(context.Context) webapp.Usage
	// Period : intervalle des relevés (30 s) ; Window : durée gardée (1 h).
	Period, Window time.Duration

	mu      sync.Mutex
	samples []Sample
	memErr  string
}

func (m *Monitor) period() time.Duration {
	if m.Period > 0 {
		return m.Period
	}
	return 30 * time.Second
}

func (m *Monitor) window() time.Duration {
	if m.Window > 0 {
		return m.Window
	}
	return time.Hour
}

// Run relève jusqu'à la fin de ctx.
func (m *Monitor) Run(ctx context.Context) {
	t := time.NewTicker(m.period())
	defer t.Stop()
	for {
		m.Collect(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Collect fait un relevé et l'ajoute à l'historique.
func (m *Monitor) Collect(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	s := Sample{At: time.Now(), EngineMem: -1, ServerMem: serverMemory()}
	if m.DB != nil {
		if err := m.DB.QueryRow(ctx, `
			SELECT count(DISTINCT usename) FILTER (WHERE application_name LIKE 'Lattis/%'),
			       count(DISTINCT usename) FILTER (WHERE application_name LIKE 'lattisd/web%')
			FROM pg_stat_activity
			WHERE backend_type = 'client backend' AND datname = current_database()`).Scan(&s.Desktop, &s.Web); err != nil {
			slog.Warn("surveillance : utilisateurs connectés", "err", err)
		}
	}
	var memErr string
	if m.Web != nil {
		u := m.Web(ctx)
		s.Engines, s.Ready, s.EngineMem = u.Assigned, u.Ready, u.Memory
		if u.MemoryErr != nil {
			memErr = u.MemoryErr.Error()
		}
	}
	m.Record(s, memErr)
}

// Record ajoute un relevé ; memErr : raison d'une mémoire des moteurs inconnue.
func (m *Monitor) Record(s Sample, memErr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.memErr = memErr
	m.samples = append(m.samples, s)
	cut := s.At.Add(-m.window())
	kept := m.samples[:0]
	for _, x := range m.samples {
		if !x.At.Before(cut) {
			kept = append(kept, x)
		}
	}
	m.samples = kept
}

// Samples : relevés de la fenêtre, du plus ancien au plus récent, et la
// raison pour laquelle la mémoire des moteurs est inconnue (vide sinon).
func (m *Monitor) Samples() ([]Sample, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Sample(nil), m.samples...), m.memErr
}

// Window : durée gardée ; Period : intervalle des relevés.
func (m *Monitor) WindowDuration() time.Duration { return m.window() }
func (m *Monitor) PeriodDuration() time.Duration { return m.period() }

// serverMemory : mémoire résidente de lattisd (/proc), ou à défaut la
// mémoire obtenue du système par le runtime Go.
func serverMemory() int64 {
	if n, err := webapp.RSS(os.Getpid()); err == nil && n > 0 {
		return n
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.Sys)
}
