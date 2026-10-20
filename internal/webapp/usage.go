package webapp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// Usage : moteurs de ce lattisd à un instant (écran Surveillance).
type Usage struct {
	// Assigned : moteurs attribués à un utilisateur ; Ready : moteurs prêts
	// de la réserve.
	Assigned, Ready int
	// Memory : mémoire occupée par l'ensemble des moteurs, en octets ;
	// MemoryErr dit pourquoi elle est inconnue (Memory vaut alors -1).
	Memory    int64
	MemoryErr error
}

// MemoryMeter : lanceur qui sait mesurer la mémoire de ses moteurs.
type MemoryMeter interface {
	Memory(ctx context.Context) (int64, error)
}

// ErrNoMemory : mesure non disponible avec ce lanceur.
var ErrNoMemory = errors.New("mesure non disponible")

// Usage relève les moteurs et leur mémoire.
func (m *Manager) Usage(ctx context.Context) Usage {
	l := m.launcher()
	u := Usage{Assigned: m.Count(), Memory: -1}
	if p, ok := l.(interface{ Ready() int }); ok {
		u.Ready = p.Ready()
	}
	if u.Assigned+u.Ready == 0 {
		u.Memory = 0
		return u
	}
	mm, ok := l.(MemoryMeter)
	if !ok {
		u.MemoryErr = ErrNoMemory
		return u
	}
	u.Memory, u.MemoryErr = mm.Memory(ctx)
	if u.MemoryErr != nil {
		u.Memory = -1
	}
	return u
}

// Memory : mémoire des moteurs du lanceur sous-jacent.
func (p *Pool) Memory(ctx context.Context) (int64, error) {
	if mm, ok := p.Inner.(MemoryMeter); ok {
		return mm.Memory(ctx)
	}
	return 0, ErrNoMemory
}

// Memory : mémoire résidente des processus des moteurs (Linux, /proc).
func (l *ProcessLauncher) Memory(context.Context) (int64, error) {
	l.mu.Lock()
	pids := make([]int, 0, len(l.pids))
	for pid := range l.pids {
		pids = append(pids, pid)
	}
	l.mu.Unlock()
	var total int64
	for _, pid := range pids {
		n, err := RSS(pid)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func (l *ProcessLauncher) track(pid int, on bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pids == nil {
		l.pids = map[int]bool{}
	}
	if on {
		l.pids[pid] = true
	} else {
		delete(l.pids, pid)
	}
}

// RSS : mémoire résidente d'un processus (VmRSS de /proc/<pid>/status).
func RSS(pid int) (int64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, e := os.Stat("/proc/self/status"); e != nil {
				return 0, fmt.Errorf("%w (pas de /proc sur ce système)", ErrNoMemory)
			}
			return 0, nil // processus parti entre-temps
		}
		return 0, err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "VmRSS:"); ok {
			f := strings.Fields(v)
			n, err := strconv.ParseInt(f[0], 10, 64)
			if err != nil {
				return 0, err
			}
			return n * 1024, nil // en kB
		}
	}
	return 0, nil
}

// Memory : mémoire des conteneurs des moteurs, relevée par le lanceur.
func (l *DockerLauncher) Memory(ctx context.Context) (int64, error) {
	var st struct {
		Memory int64 `json:"memory"`
	}
	code, err := l.call(ctx, http.MethodGet, "/engines/stats", nil, &st)
	if err != nil {
		return 0, err
	}
	if code == http.StatusNotFound {
		return 0, fmt.Errorf("%w : lanceur trop ancien (mettre à jour lattis-launcher)", ErrNoMemory)
	}
	return st.Memory, nil
}

// Memory : mémoire des pods des moteurs, par l'API de métriques du cluster
// (metrics-server).
func (k *KubeLauncher) Memory(ctx context.Context) (int64, error) {
	var out struct {
		Items []struct {
			Containers []struct {
				Usage struct {
					Memory string `json:"memory"`
				} `json:"usage"`
			} `json:"containers"`
		} `json:"items"`
	}
	path := "/apis/metrics.k8s.io/v1beta1/namespaces/" + k.Cfg.Namespace + "/pods?labelSelector=" + k.selector("")
	code, err := k.call(ctx, http.MethodGet, path, nil, &out)
	if code == http.StatusNotFound || code == http.StatusServiceUnavailable {
		return 0, fmt.Errorf("%w : metrics-server absent du cluster", ErrNoMemory)
	}
	if code == http.StatusForbidden {
		return 0, fmt.Errorf("%w : droit de lecture de pods.metrics.k8s.io manquant", ErrNoMemory)
	}
	if err != nil {
		return 0, err
	}
	var total int64
	for _, p := range out.Items {
		for _, c := range p.Containers {
			n, err := ParseQuantity(c.Usage.Memory)
			if err != nil {
				return 0, err
			}
			total += n
		}
	}
	return total, nil
}

// ParseQuantity lit une quantité Kubernetes en octets (« 123456Ki », « 12Mi », « 1G »).
func ParseQuantity(q string) (int64, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return 0, nil
	}
	mult := map[string]float64{"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40,
		"k": 1e3, "M": 1e6, "G": 1e9, "T": 1e12, "m": 1e-3}
	for _, suf := range []string{"Ki", "Mi", "Gi", "Ti", "k", "M", "G", "T", "m"} {
		if num, ok := strings.CutSuffix(q, suf); ok {
			f, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, fmt.Errorf("quantité %q : %w", q, err)
			}
			return int64(f * mult[suf]), nil
		}
	}
	f, err := strconv.ParseFloat(q, 64)
	if err != nil {
		return 0, fmt.Errorf("quantité %q : %w", q, err)
	}
	return int64(f), nil
}
