package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrNotFound : conteneur inconnu de Docker.
var ErrNotFound = errors.New("conteneur introuvable")

// Docker : client minimal de l'API Docker Engine sur son socket Unix.
type Docker struct {
	HTTP *http.Client
	// Base : préfixe des chemins (version de l'API).
	Base string
}

// NewDocker : client du socket (par exemple /var/run/docker.sock).
func NewDocker(socket string) *Docker {
	return &Docker{Base: "http://docker/v1.43", HTTP: &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}}}
}

func (d *Docker) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.Base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		var m struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&m)
		return fmt.Errorf("%s %s : %d %s", method, strings.SplitN(path, "?", 2)[0], resp.StatusCode, m.Message)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Run crée et démarre un conteneur ; renvoie son identifiant.
func (d *Docker) Run(ctx context.Context, name string, spec map[string]any) (string, error) {
	var created struct {
		ID string `json:"Id"`
	}
	if err := d.do(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), spec, &created); err != nil {
		return "", err
	}
	if err := d.do(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil, nil); err != nil {
		_ = d.Remove(context.Background(), created.ID)
		return "", err
	}
	return created.ID, nil
}

type inspect struct {
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// IP : adresse du conteneur sur le réseau donné.
func (d *Docker) IP(ctx context.Context, id, network string) (string, error) {
	var in inspect
	if err := d.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, &in); err != nil {
		return "", err
	}
	n, ok := in.NetworkSettings.Networks[network]
	if !ok || n.IPAddress == "" {
		return "", fmt.Errorf("conteneur sans adresse sur le réseau %q", network)
	}
	return n.IPAddress, nil
}

// Running : le conteneur tourne-t-il, et porte-t-il l'étiquette ?
func (d *Docker) Running(ctx context.Context, id, label string) (running, labeled bool, err error) {
	var in inspect
	if err := d.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, &in); err != nil {
		return false, false, err
	}
	return in.State.Running, in.Config.Labels[label] != "", nil
}

// Stop arrête proprement (10 s) ; le conteneur se supprime ensuite (AutoRemove).
func (d *Docker) Stop(ctx context.Context, id string) error {
	return d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop?t=10", nil, nil)
}

// Remove arrête et supprime tout de suite.
func (d *Docker) Remove(ctx context.Context, id string) error {
	err := d.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id)+"?force=1&v=1", nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// List : conteneurs (même arrêtés) qui portent l'étiquette.
func (d *Docker) List(ctx context.Context, label string) ([]string, error) {
	f, _ := json.Marshal(map[string][]string{"label": {label}})
	var out []struct {
		ID string `json:"Id"`
	}
	if err := d.do(ctx, http.MethodGet, "/containers/json?all=1&filters="+url.QueryEscape(string(f)), nil, &out); err != nil {
		return nil, err
	}
	ids := make([]string, len(out))
	for i, c := range out {
		ids[i] = c.ID
	}
	return ids, nil
}

// Memory : mémoire d'un conteneur, cache de fichiers inactif déduit (comme
// docker stats) ; 0 pour un conteneur arrêté.
func (d *Docker) Memory(ctx context.Context, id string) (int64, error) {
	var st struct {
		MemoryStats struct {
			Usage int64            `json:"usage"`
			Stats map[string]int64 `json:"stats"`
		} `json:"memory_stats"`
	}
	if err := d.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/stats?stream=false&one-shot=true", nil, &st); err != nil {
		return 0, err
	}
	m := st.MemoryStats
	cache := m.Stats["inactive_file"] // cgroup v2
	if v, ok := m.Stats["total_inactive_file"]; ok {
		cache = v // cgroup v1
	}
	if cache > m.Usage {
		return m.Usage, nil
	}
	return m.Usage - cache, nil
}

// Count : conteneurs en cours qui portent l'étiquette.
func (d *Docker) Count(ctx context.Context, label string) (int, error) {
	f, _ := json.Marshal(map[string][]string{"label": {label}})
	var out []json.RawMessage
	if err := d.do(ctx, http.MethodGet, "/containers/json?filters="+url.QueryEscape(string(f)), nil, &out); err != nil {
		return 0, err
	}
	return len(out), nil
}
