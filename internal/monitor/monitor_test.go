package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Be-Lattis/lattis-server/internal/webapp"
)

// Relevés : moteurs et mémoire lus du Manager, fenêtre glissante.
func TestMonitor(t *testing.T) {
	m := &Monitor{Window: time.Hour, Web: func(context.Context) webapp.Usage {
		return webapp.Usage{Assigned: 3, Ready: 2, Memory: -1, MemoryErr: errors.New("metrics-server absent du cluster")}
	}}
	m.Collect(context.Background())
	s, memErr := m.Samples()
	if len(s) != 1 || s[0].Engines != 3 || s[0].Ready != 2 || s[0].EngineMem != -1 || s[0].ServerMem <= 0 || !strings.Contains(memErr, "metrics-server") {
		t.Fatalf("relevé : %+v %q", s, memErr)
	}
	now := time.Now()
	m.Record(Sample{At: now.Add(-2 * time.Hour)}, "")
	m.Record(Sample{At: now, Desktop: 4, Web: 1}, "")
	s, memErr = m.Samples()
	if len(s) != 2 || s[1].Users() != 5 || memErr != "" {
		t.Fatalf("fenêtre : %+v", s)
	}
}

func TestChart(t *testing.T) {
	now := time.Now()
	at := []time.Time{now.Add(-2 * time.Hour), now.Add(-30 * time.Minute), now.Add(-29 * time.Minute), now.Add(-time.Minute), now}
	svg := string(Chart(at, []Series{
		{Name: "Utilisateurs", Class: "s1", Values: []float64{90, 3, 4, 7, 6}},
		{Name: "Mémoire <x>", Class: "s2", Values: []float64{1, 1, -1, 2, 2}},
	}, now, time.Hour, func(f float64) string { return fmt.Sprintf("max %g", f) }))
	for _, want := range []string{`<svg class="chart"`, `aria-label="Utilisateurs, Mémoire &lt;x&gt;"`, `>max 10<`, `>−1 h<`, `class="s1"`, `class="s2"`} {
		if !strings.Contains(svg, want) {
			t.Fatalf("graphique sans %s : %s", want, svg)
		}
	}
	// Relevé hors fenêtre ignoré ; valeur négative : la courbe s'interrompt.
	if n := strings.Count(svg, "<polyline"); n != 3 {
		t.Fatalf("%d tracés : %s", n, svg)
	}
	if strings.Contains(svg, "style=") || strings.Contains(svg, "<script") {
		t.Fatal("style ou script en ligne (CSP)")
	}
	for v, want := range map[float64]float64{0: 1, 1: 1, 3: 5, 7: 10, 12: 20, 480: 500, 1001: 2000} {
		if got := niceCeil(v); got != want {
			t.Fatalf("niceCeil(%g) = %g, attendu %g", v, got, want)
		}
	}
}
