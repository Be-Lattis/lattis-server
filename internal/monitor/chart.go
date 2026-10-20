package monitor

import (
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"
)

// Series : une courbe ; Values[i] correspond à At[i] (négatif : pas de
// valeur, la courbe s'interrompt).
type Series struct {
	Name, Class string
	Values      []float64
}

// Chart dessine un petit graphique SVG (sans script ni style en ligne,
// compatible avec la CSP de l'administration) : abscisse fixe de now-window
// à now, ordonnée de 0 au maximum des courbes. format écrit l'échelle.
func Chart(at []time.Time, series []Series, now time.Time, window time.Duration, format func(float64) string) template.HTML {
	const w, h, top, bottom, left = 320.0, 96.0, 14.0, 16.0, 2.0
	plot := h - top - bottom
	start := now.Add(-window)
	maxV := 0.0
	for _, s := range series {
		for i, v := range s.Values {
			if i < len(at) && !at[i].Before(start) {
				maxV = math.Max(maxV, v)
			}
		}
	}
	scale := niceCeil(maxV)
	x := func(t time.Time) float64 {
		return left + (w-2*left)*math.Max(0, float64(t.Sub(start)))/float64(window)
	}
	y := func(v float64) float64 { return top + plot - plot*v/scale }
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %g %g" role="img" aria-label="%s">`, w, h, template.HTMLEscapeString(names(series)))
	// Repères : base, mi-hauteur, sommet.
	for _, f := range []float64{0, 0.5, 1} {
		fmt.Fprintf(&b, `<line class="grid" x1="%g" x2="%g" y1="%.1f" y2="%.1f"/>`, left, w-left, y(scale*f), y(scale*f))
	}
	fmt.Fprintf(&b, `<text class="axis" x="%g" y="%g">%s</text>`, left, top-4, template.HTMLEscapeString(format(scale)))
	fmt.Fprintf(&b, `<text class="axis" x="%g" y="%g">−%s</text>`, left, h-3, template.HTMLEscapeString(shortDuration(window)))
	fmt.Fprintf(&b, `<text class="axis" x="%g" y="%g" text-anchor="end">maintenant</text>`, w-left, h-3)
	for _, s := range series {
		var pts []string
		flush := func() {
			if len(pts) == 1 {
				// Point isolé : un trait court pour rester visible.
				var px, py float64
				fmt.Sscanf(pts[0], "%g,%g", &px, &py)
				pts = append(pts, fmt.Sprintf("%.1f,%.1f", px+1.5, py))
			}
			if len(pts) > 1 {
				fmt.Fprintf(&b, `<polyline class="%s" points="%s"/>`, template.HTMLEscapeString(s.Class), strings.Join(pts, " "))
			}
			pts = pts[:0]
		}
		for i, v := range s.Values {
			if v < 0 || i >= len(at) || at[i].Before(start) {
				flush()
				continue
			}
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", x(at[i]), y(v)))
		}
		flush()
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

func names(series []Series) string {
	n := make([]string, len(series))
	for i, s := range series {
		n[i] = s.Name
	}
	return strings.Join(n, ", ")
}

// niceCeil : plus petite valeur « ronde » (1, 2, 5 × 10ⁿ) au moins égale à v.
func niceCeil(v float64) float64 {
	if v <= 1 {
		return 1
	}
	p := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 5, 10} {
		if m*p >= v {
			return m * p
		}
	}
	return 10 * p
}

func shortDuration(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%d h", d/time.Hour)
	}
	return fmt.Sprintf("%d min", d/time.Minute)
}
