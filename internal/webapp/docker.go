package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DockerLauncher lance chaque moteur dans un conteneur, par le mini lanceur
// lattis-launcher : lui seul parle à Docker (le socket Docker donne root
// sur l'hôte), et il ne sait faire que démarrer l'image du moteur, avec
// des options fixées de son côté, la surveiller et l'arrêter.
type DockerLauncher struct {
	// URL du lanceur (http://launcher:8090) et jeton partagé avec lui.
	URL   string
	Token string
	HTTP  *http.Client
}

// LauncherEngine : réponse du lanceur à un démarrage.
type LauncherEngine struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
}

func (l *DockerLauncher) client() *http.Client {
	if l.HTTP != nil {
		return l.HTTP
	}
	return &http.Client{Timeout: 90 * time.Second}
}

func (l *DockerLauncher) call(ctx context.Context, method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, l.URL+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+l.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.client().Do(req)
	if err != nil {
		return 0, fmt.Errorf("lanceur : %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return resp.StatusCode, ErrBusy
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		if resp.StatusCode == http.StatusNotFound {
			return resp.StatusCode, nil
		}
		return resp.StatusCode, fmt.Errorf("lanceur : %d %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode, nil
}

func (l *DockerLauncher) Start(ctx context.Context, s Spec) (Instance, error) {
	var e LauncherEngine
	if _, err := l.call(ctx, http.MethodPost, "/engines", map[string]string{"user_id": s.UserID, "secret": s.Secret}, &e); err != nil {
		return nil, err
	}
	if e.ID == "" || e.Addr == "" {
		return nil, errors.New("lanceur : réponse incomplète")
	}
	inst := &remoteInstance{addr: e.Addr, done: make(chan struct{}), stop: make(chan struct{}),
		halt: func(kill bool) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			q := ""
			if kill {
				q = "?kill=1"
			}
			_, _ = l.call(ctx, http.MethodDelete, "/engines/"+url.PathEscape(e.ID)+q, nil, nil)
		}}
	if err := waitReady(ctx, e.Addr, inst.done); err != nil {
		inst.Kill()
		return nil, err
	}
	go watch(watchPeriod, func(ctx context.Context) (bool, error) {
		var st struct {
			Running bool `json:"running"`
		}
		code, err := l.call(ctx, http.MethodGet, "/engines/"+url.PathEscape(e.ID), nil, &st)
		if code == http.StatusNotFound {
			return false, nil
		}
		return st.Running, err
	}, inst.done, inst.stop)
	return inst, nil
}

// Cleanup retire les conteneurs de moteurs laissés par un lattisd précédent.
func (l *DockerLauncher) Cleanup(ctx context.Context) error {
	_, err := l.call(ctx, http.MethodPost, "/engines/cleanup", nil, nil)
	return err
}

// remoteInstance : moteur en conteneur ou en pod, surveillé à distance.
type remoteInstance struct {
	addr string
	done chan struct{}
	stop chan struct{}
	halt func(kill bool)
	once onceFlag
}

func (r *remoteInstance) Addr() string          { return r.addr }
func (r *remoteInstance) Done() <-chan struct{} { return r.done }
func (r *remoteInstance) Stop()                 { r.halt(false) }

func (r *remoteInstance) Kill() {
	r.halt(true)
	if r.once.set() {
		close(r.stop)
	}
}
