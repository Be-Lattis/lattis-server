// Package launcher est le mini lanceur des moteurs de l'édition web en mode
// Docker (lattis-launcher). Il est seul à parler à Docker : le socket
// Docker donne root sur l'hôte, lattisd n'y a donc pas accès. lattisd lui
// demande, avec un jeton partagé, de démarrer le moteur d'un utilisateur,
// d'en donner l'état ou de l'arrêter ; tout le reste (image, réseau,
// options, limites) est fixé ici, à la configuration du lanceur.
package launcher

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// EnginePort : port d'écoute du moteur dans son conteneur.
const EnginePort = "8102"

// Config : réglages du lanceur (variables LAUNCHER_*).
type Config struct {
	Token string
	// Image du moteur (lattis-engine) et réseau Docker partagé avec lattisd.
	Image, Network string
	// Server : émetteur ; TokenURL : /engine/token vu des moteurs ; Idle :
	// inactivité avant arrêt (« 30m »).
	Server, TokenURL, Idle string
	// Limites par moteur : mémoire en octets, CPU en milliardièmes, processus.
	Memory, NanoCPUs, Pids int64
	// VolumePrefix : volume de cache par utilisateur (préfixe + identifiant).
	VolumePrefix string
	// Max : moteurs simultanés au plus.
	Max int
	// DemoIdle : inactivité avant arrêt d'un moteur de la démo publique (10m).
	DemoIdle string
}

// Server sert l'API du lanceur.
type Server struct {
	Config Config
	Docker *Docker
	mu     sync.Mutex // un démarrage à la fois (nombre maximal tenu)
}

var (
	userRe   = regexp.MustCompile(`^[0-9A-Za-z-]{1,64}$`)
	secretRe = regexp.MustCompile(`^[0-9A-Za-z_-]{20,128}$`)
)

// Label : étiquette des conteneurs de moteurs.
const Label = "lattis.engine"

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /engines", s.start)
	mux.HandleFunc("POST /engines/cleanup", s.cleanup)
	mux.HandleFunc("GET /engines/stats", s.stats)
	mux.HandleFunc("GET /engines/{id}", s.status)
	mux.HandleFunc("DELETE /engines/{id}", s.stop)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(tok), []byte(s.Config.Token)) != 1 || s.Config.Token == "" {
				http.Error(w, "jeton refusé", http.StatusUnauthorized)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	var in struct {
		UserID string `json:"user_id"`
		Secret string `json:"secret"`
		// Demo : démo publique, sans serveur ni secret (--public-demo).
		Demo bool `json:"demo"`
		// Memory : mémoire demandée par lattisd (octets), jamais au-delà de
		// LAUNCHER_MEMORY.
		Memory int64 `json:"memory"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil ||
		!userRe.MatchString(in.UserID) || (!in.Demo && !secretRe.MatchString(in.Secret)) ||
		(in.Demo && (in.Secret != "" || !strings.HasPrefix(in.UserID, "demo-"))) {
		http.Error(w, "demande invalide", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := r.Context()
	name := "lattis-engine-" + in.UserID
	// Un ancien conteneur du même utilisateur (lattisd redémarré) part.
	_ = s.Docker.Remove(ctx, name)
	n, err := s.Docker.Count(ctx, Label)
	if err != nil {
		s.fail(w, err)
		return
	}
	if n >= s.Config.Max {
		http.Error(w, "nombre maximal de moteurs atteint", http.StatusTooManyRequests)
		return
	}
	c := s.Config
	if in.Memory > 0 && (c.Memory == 0 || in.Memory < c.Memory) {
		c.Memory = max(in.Memory, 64<<20)
	}
	// Volume de cache par utilisateur ; un moteur de la réserve de lattisd
	// (pool-…) a un volume anonyme, supprimé avec son conteneur.
	mount := map[string]any{"Type": "volume", "Source": c.VolumePrefix + in.UserID, "Target": "/home/nonroot"}
	if strings.HasPrefix(in.UserID, "pool-") || in.Demo {
		delete(mount, "Source")
	}
	cmd := []string{"--addr", "0.0.0.0:" + EnginePort, "--dir", "/home/nonroot", "--base-path", "/app",
		"--server", c.Server, "--token-url", c.TokenURL, "--idle", c.Idle}
	env := []string{"LATTIS_ENGINE_SECRET=" + in.Secret}
	if in.Demo {
		// Démo publique : ni serveur, ni jeton, ni secret.
		cmd = []string{"--addr", "0.0.0.0:" + EnginePort, "--dir", "/home/nonroot", "--base-path", "/demo",
			"--public-demo", "--idle", c.DemoIdle}
		env = nil
	}
	spec := map[string]any{
		"Image": c.Image,
		"Cmd":   cmd,
		"Env":   env,
		// Entrée standard ouverte : le moteur s'arrête quand elle se ferme.
		"OpenStdin": true,
		"Labels":    map[string]string{Label: "1", "lattis.user": in.UserID},
		"HostConfig": map[string]any{
			"AutoRemove":     true,
			"NetworkMode":    c.Network,
			"Memory":         c.Memory,
			"NanoCpus":       c.NanoCPUs,
			"PidsLimit":      c.Pids,
			"ReadonlyRootfs": true,
			"CapDrop":        []string{"ALL"},
			"SecurityOpt":    []string{"no-new-privileges"},
			"Tmpfs":          map[string]string{"/tmp": "size=64m"},
			// Volume neuf monté sur le répertoire de nonroot : il en prend
			// le propriétaire (65532).
			"Mounts": []map[string]any{mount},
		},
	}
	id, err := s.Docker.Run(ctx, name, spec)
	if err != nil {
		s.fail(w, err)
		return
	}
	ip, err := s.Docker.IP(ctx, id, c.Network)
	if err != nil {
		_ = s.Docker.Remove(context.Background(), id)
		s.fail(w, err)
		return
	}
	slog.Info("moteur démarré", "user", in.UserID, "container", id[:12])
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "addr": ip + ":" + EnginePort})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	running, labeled, err := s.Docker.Running(r.Context(), r.PathValue("id"), Label)
	if errors.Is(err, ErrNotFound) || (err == nil && !labeled) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"running": running})
}

// stats : mémoire des moteurs en cours, en octets (écran Surveillance).
func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	ids, err := s.Docker.List(r.Context(), Label)
	if err != nil {
		s.fail(w, err)
		return
	}
	var total int64
	running := 0
	for _, id := range ids {
		n, err := s.Docker.Memory(r.Context(), id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			s.fail(w, err)
			return
		}
		if n > 0 {
			running++
		}
		total += n
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int64{"memory": total, "engines": int64(running)})
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, labeled, err := s.Docker.Running(r.Context(), id, Label); err != nil || !labeled {
		w.WriteHeader(http.StatusNoContent) // déjà parti
		return
	}
	var err error
	if r.URL.Query().Get("kill") != "" {
		err = s.Docker.Remove(r.Context(), id)
	} else {
		err = s.Docker.Stop(r.Context(), id)
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) cleanup(w http.ResponseWriter, r *http.Request) {
	ids, err := s.Docker.List(r.Context(), Label)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, id := range ids {
		_ = s.Docker.Remove(r.Context(), id)
	}
	slog.Info("moteurs retirés au démarrage de lattisd", "count", len(ids))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	slog.Error("lanceur", "err", err)
	http.Error(w, fmt.Sprintf("docker : %v", err), http.StatusBadGateway)
}
