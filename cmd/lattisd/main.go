// Commande lattisd : service du Serveur Lattis.
//
//	lattisd [--migrate]        démarre le service (et applique les migrations)
//	lattisd migrate            applique les migrations puis s'arrête
//	lattisd keygen FICHIER     crée une clé de signature Ed25519
//	lattisd public-keys        écrit les clés publiques (PEM) pour lattis_oauth
//	lattisd init-dev DIR [HÔTE…] prépare les secrets d'un essai local
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Be-Lattis/lattis-server/internal/auth"
	"github.com/Be-Lattis/lattis-server/internal/config"
	"github.com/Be-Lattis/lattis-server/internal/httpserver"
	"github.com/Be-Lattis/lattis-server/internal/metrics"
	"github.com/Be-Lattis/lattis-server/internal/migrate"
	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/tokens"
	"github.com/Be-Lattis/lattis-server/internal/upstream"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(os.Args[1:]); err != nil {
		slog.Error("lattisd", "err", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "keygen":
			if len(args) != 2 {
				return errors.New("usage : lattisd keygen FICHIER")
			}
			return keygen(args[1])
		case "init-dev":
			if len(args) < 2 {
				return errors.New("usage : lattisd init-dev DIR [HÔTE…]")
			}
			hosts := args[2:]
			if len(hosts) == 0 {
				hosts = []string{"localhost", "postgres", "127.0.0.1"}
			}
			return initDev(args[1], hosts)
		case "public-keys":
			return publicKeys()
		case "migrate":
			return migrateOnly()
		case "version":
			fmt.Println(httpserver.Version)
			return nil
		}
	}
	fs := flag.NewFlagSet("lattisd", flag.ContinueOnError)
	doMigrate := fs.Bool("migrate", false, "appliquer les migrations au démarrage")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return serve(*doMigrate)
}

func keygen(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s existe déjà", path)
	}
	pem, err := tokens.GenerateKeyPEM()
	if err != nil {
		return err
	}
	return os.WriteFile(path, pem, 0o600)
}

func publicKeys() error {
	path := os.Getenv("LATTIS_SIGNING_KEY_FILE")
	if path == "" {
		return errors.New("LATTIS_SIGNING_KEY_FILE manquant")
	}
	ks, err := tokens.LoadKeyFile(path)
	if err != nil {
		return err
	}
	pem, err := ks.PublicPEM()
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(pem)
	return err
}

func migrateOnly() error {
	url := os.Getenv("LATTIS_PG_RW_URL")
	if url == "" {
		return errors.New("LATTIS_PG_RW_URL manquant")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url)
	if err != nil {
		return err
	}
	defer st.Close()
	n, err := migrate.Up(ctx, st.DB)
	if err != nil {
		return err
	}
	slog.Info("migrations appliquées", "count", n, "version", migrate.Latest())
	return nil
}

func serve(doMigrate bool) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	keys, err := tokens.LoadKeyFile(cfg.SigningKeyFile)
	if err != nil {
		return fmt.Errorf("clé de signature : %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.PGRWURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if doMigrate {
		n, err := migrate.Up(ctx, st.DB)
		if err != nil {
			return err
		}
		slog.Info("migrations appliquées", "count", n, "version", migrate.Latest())
	}
	if cfg.BootstrapOwner != "" {
		if token, err := st.Bootstrap(ctx, cfg.BootstrapOwner); err != nil {
			slog.Error("amorçage du premier propriétaire", "err", err)
		} else if token != "" {
			slog.Warn("premier propriétaire invité : ouvrez ce lien pour choisir son mot de passe",
				"email", cfg.BootstrapOwner, "url", cfg.URL("/setup?token="+token))
		}
	}

	var pgVersion string
	_ = st.DB.QueryRow(ctx, `SHOW server_version`).Scan(&pgVersion)
	m := metrics.New(cfg.PGPublicRW.Host, st.DB)
	m.BuildInfo.WithLabelValues(httpserver.Version, string(cfg.Edition), pgVersion).Set(1)

	authSvc := &auth.Service{
		Config: cfg, Store: st, Keys: keys, Metrics: m,
		Upstream: &upstream.Registry{SecretsDir: cfg.SecretsDir,
			CallbackURL: func(id string) string { return cfg.URL("/callback/" + id) }},
	}
	srv := &httpserver.Server{Config: cfg, Store: st, Keys: keys, Auth: authSvc}

	public := &http.Server{Addr: cfg.ListenAddr, Handler: srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	internal := &http.Server{Addr: cfg.MetricsAddr, Handler: metricsMux, ReadHeaderTimeout: 10 * time.Second}

	go purgeLoop(ctx, st)
	errc := make(chan error, 2)
	go func() {
		slog.Info("lattisd à l'écoute", "addr", cfg.ListenAddr, "issuer", cfg.Issuer(), "edition", cfg.Edition)
		if cfg.TLSCertFile != "" {
			errc <- public.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			errc <- public.ListenAndServe()
		}
	}()
	go func() { errc <- internal.ListenAndServe() }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	_ = internal.Shutdown(sctx)
	return public.Shutdown(sctx)
}

func purgeLoop(ctx context.Context, st *store.Store) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := st.Purge(ctx); err != nil {
				slog.Warn("purge des jetons", "err", err)
			}
		}
	}
}
