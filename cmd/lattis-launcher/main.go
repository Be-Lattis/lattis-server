// Commande lattis-launcher : mini lanceur des moteurs de l'édition web en
// mode Docker (LATTIS_WEB_LAUNCHER=docker). Seul service à recevoir le
// socket Docker ; lattisd lui parle avec un jeton partagé.
//
// Variables :
//
//	LAUNCHER_ADDR           écoute (:8090), joignable par lattisd seulement
//	LAUNCHER_TOKEN_FILE     jeton partagé avec lattisd (obligatoire)
//	LAUNCHER_DOCKER_SOCKET  socket Docker (/var/run/docker.sock)
//	LAUNCHER_IMAGE          image de lattis-engine (obligatoire)
//	LAUNCHER_NETWORK        réseau Docker partagé avec lattisd (obligatoire)
//	LAUNCHER_SERVER         émetteur, LATTIS_PUBLIC_URL de lattisd (obligatoire)
//	LAUNCHER_TOKEN_URL      /engine/token vu des moteurs (obligatoire)
//	LAUNCHER_IDLE           inactivité avant arrêt d'un moteur (30m)
//	LAUNCHER_MEMORY         mémoire par moteur (1g)
//	LAUNCHER_CPUS           processeurs par moteur (1)
//	LAUNCHER_PIDS           processus par moteur (256)
//	LAUNCHER_VOLUME_PREFIX  volumes de cache par utilisateur (lattis-web-)
//	LAUNCHER_MAX            moteurs simultanés au plus (50)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Be-Lattis/lattis-server/internal/launcher"
)

func main() {
	if err := run(); err != nil {
		slog.Error("lattis-launcher", "err", err)
		os.Exit(1)
	}
}

func run() error {
	def := func(k, d string) string {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
		return d
	}
	var errs []error
	need := func(k string) string {
		v := def(k, "")
		if v == "" {
			errs = append(errs, fmt.Errorf("%s manquant", k))
		}
		return v
	}
	cfg := launcher.Config{Image: need("LAUNCHER_IMAGE"), Network: need("LAUNCHER_NETWORK"),
		Server: need("LAUNCHER_SERVER"), TokenURL: need("LAUNCHER_TOKEN_URL"), Idle: def("LAUNCHER_IDLE", "30m"),
		VolumePrefix: def("LAUNCHER_VOLUME_PREFIX", "lattis-web-")}
	if f := need("LAUNCHER_TOKEN_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			errs = append(errs, err)
		}
		if cfg.Token = strings.TrimSpace(string(b)); len(cfg.Token) < 32 && err == nil {
			errs = append(errs, errors.New("LAUNCHER_TOKEN_FILE : jeton d'au moins 32 caractères attendu"))
		}
	}
	var err error
	if cfg.Memory, err = parseBytes(def("LAUNCHER_MEMORY", "1g")); err != nil {
		errs = append(errs, fmt.Errorf("LAUNCHER_MEMORY : %w", err))
	}
	if cpus, err := strconv.ParseFloat(def("LAUNCHER_CPUS", "1"), 64); err != nil || cpus <= 0 {
		errs = append(errs, errors.New("LAUNCHER_CPUS : nombre positif attendu"))
	} else {
		cfg.NanoCPUs = int64(cpus * 1e9)
	}
	if cfg.Pids, err = strconv.ParseInt(def("LAUNCHER_PIDS", "256"), 10, 64); err != nil || cfg.Pids <= 0 {
		errs = append(errs, errors.New("LAUNCHER_PIDS : entier positif attendu"))
	}
	if cfg.Max, err = strconv.Atoi(def("LAUNCHER_MAX", "50")); err != nil || cfg.Max <= 0 {
		errs = append(errs, errors.New("LAUNCHER_MAX : entier positif attendu"))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	srv := &launcher.Server{Config: cfg, Docker: launcher.NewDocker(def("LAUNCHER_DOCKER_SOCKET", "/var/run/docker.sock"))}
	hs := &http.Server{Addr: def("LAUNCHER_ADDR", ":8090"), Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	slog.Info("lattis-launcher à l'écoute", "addr", hs.Addr, "image", cfg.Image, "network", cfg.Network)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// parseBytes : « 512m », « 1g », « 1073741824 ».
func parseBytes(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "g"):
		mult, s = 1<<30, strings.TrimSuffix(s, "g")
	case strings.HasSuffix(s, "m"):
		mult, s = 1<<20, strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "k"):
		mult, s = 1<<10, strings.TrimSuffix(s, "k")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, errors.New("taille attendue (512m, 1g)")
	}
	return n * mult, nil
}
