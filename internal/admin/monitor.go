package admin

import (
	"context"
	"html/template"
	"math"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/monitor"
	"github.com/Be-Lattis/lattis-server/internal/store"
)

// monitorView : écran Surveillance.
type monitorView struct {
	Running        bool
	Web            bool
	Last           monitor.Sample
	Engines        int // attribués + prêts
	PerEngine      int64
	MemErr         string
	Usage, Memory  template.HTML
	Window, Period string
	Since          time.Time
}

const mib = 1 << 20

func (s *Service) loadMonitor(ctx context.Context, tx pgx.Tx, r *http.Request, u store.User) (any, error) {
	v := monitorView{Web: s.Monitor != nil && s.Monitor.Web != nil}
	if s.Monitor == nil {
		return v, nil
	}
	samples, memErr := s.Monitor.Samples()
	v.Window, v.Period = "les dernières "+shortDur(s.Monitor.WindowDuration()), shortDur(s.Monitor.PeriodDuration())
	if s.Monitor.WindowDuration() == time.Hour {
		v.Window = "la dernière heure"
	}
	if len(samples) == 0 {
		return v, nil
	}
	v.Running, v.MemErr, v.Last, v.Since = true, memErr, samples[len(samples)-1], samples[0].At
	v.Engines = v.Last.Engines + v.Last.Ready
	if v.Engines > 0 && v.Last.EngineMem > 0 {
		v.PerEngine = v.Last.EngineMem / int64(v.Engines)
	}
	n := len(samples)
	at := make([]time.Time, n)
	users, engines, engMem, srvMem := make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)
	for i, x := range samples {
		at[i] = x.At
		users[i], engines[i] = float64(x.Users()), float64(x.Engines+x.Ready)
		engMem[i], srvMem[i] = -1, float64(x.ServerMem)/mib
		if x.EngineMem >= 0 {
			engMem[i] = float64(x.EngineMem) / mib
		}
	}
	now, win := s.now(), s.Monitor.WindowDuration()
	count := func(f float64) string { return trimFloat(f) }
	// Mémoire en Mo, ou en Go au-delà de 1 Go (échelle ronde dans l'unité).
	unit, mo := " Mo", 1.0
	for i := range n {
		if math.Max(engMem[i], srvMem[i]) >= 1024 {
			unit, mo = " Go", 1024
			break
		}
	}
	for i := range n {
		srvMem[i] /= mo
		if engMem[i] >= 0 {
			engMem[i] /= mo
		}
	}
	memFmt := func(f float64) string { return trimFloat(f) + unit }
	us := []monitor.Series{{Name: "Utilisateurs connectés", Class: "s1", Values: users}}
	mem := []monitor.Series{{Name: "lattisd", Class: "s3", Values: srvMem}}
	if v.Web {
		us = append(us, monitor.Series{Name: "Moteurs web", Class: "s2", Values: engines})
		mem = append([]monitor.Series{{Name: "Moteurs web", Class: "s2", Values: engMem}}, mem...)
	}
	v.Usage = monitor.Chart(at, us, now, win, count)
	v.Memory = monitor.Chart(at, mem, now, win, memFmt)
	return v, nil
}

func shortDur(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return trimFloat(d.Hours()) + " h"
	case d >= time.Minute && d%time.Minute == 0:
		return trimFloat(d.Minutes()) + " min"
	}
	return trimFloat(d.Seconds()) + " s"
}
