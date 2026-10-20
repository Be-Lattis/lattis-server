// Commande lattisd : service du Serveur Lattis.
//
//	lattisd [--migrate]        démarre le service (et applique les migrations)
//	lattisd migrate            applique les migrations puis s'arrête
//	lattisd migrate --status   état des migrations (rien n'est changé)
//	lattisd migrate --dry-run  essaie les migrations en attente puis annule
//	lattisd keygen FICHIER     crée une clé de signature Ed25519
//	lattisd public-keys        écrit les clés publiques (PEM) pour lattis_oauth
//	lattisd init-dev DIR [HÔTE…] prépare les secrets d'un essai local
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/jackc/pgx/v5"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Be-Lattis/lattis-server/internal/admin"
	"github.com/Be-Lattis/lattis-server/internal/auth"
	"github.com/Be-Lattis/lattis-server/internal/config"
	"github.com/Be-Lattis/lattis-server/internal/events"
	"github.com/Be-Lattis/lattis-server/internal/forms"
	"github.com/Be-Lattis/lattis-server/internal/httpserver"
	"github.com/Be-Lattis/lattis-server/internal/metrics"
	"github.com/Be-Lattis/lattis-server/internal/migrate"
	"github.com/Be-Lattis/lattis-server/internal/pipeline"
	"github.com/Be-Lattis/lattis-server/internal/replicas"
	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/tokens"
	"github.com/Be-Lattis/lattis-server/internal/upstream"
	"github.com/Be-Lattis/lattis-server/internal/userdb"
	"github.com/Be-Lattis/lattis-server/internal/webapp"
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
			if len(args) > 2 {
				return errors.New("usage : lattisd public-keys [FICHIER]")
			}
			return publicKeys(args[1:])
		case "migrate":
			return migrateOnly(args[1:])
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

// publicKeys écrit les clés publiques (PEM) sur la sortie standard ou dans
// FICHIER (image sans shell : conteneur d'initialisation du chart Helm).
func publicKeys(args []string) error {
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
	if len(args) == 1 {
		return os.WriteFile(args[0], pem, 0o644)
	}
	_, err = os.Stdout.Write(pem)
	return err
}

// migrateOnly : lattisd migrate [--status | --dry-run].
func migrateOnly(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	status := fs.Bool("status", false, "afficher l'état des migrations sans rien changer")
	dry := fs.Bool("dry-run", false, "essayer les migrations en attente puis tout annuler")
	if err := fs.Parse(args); err != nil {
		return err
	}
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
	migrate.Version = httpserver.Version
	s, err := migrate.Check(ctx, st.DB)
	if err != nil {
		return err
	}
	switch {
	case *status:
		fmt.Printf("lattisd %s : base en version %d, migrations embarquées jusqu'à %d\n", httpserver.Version, s.Current, s.Latest)
		if s.Newer() {
			fmt.Println("La base est plus récente que ce lattisd : installez un lattisd au moins aussi récent.")
		}
		for _, m := range s.Pending {
			fmt.Println("à appliquer :", m)
		}
		for _, m := range s.Modified {
			fmt.Println("modifiée depuis son application (non rejouée) :", m)
		}
		if len(s.Pending) == 0 && !s.Newer() {
			fmt.Println("À jour.")
		}
		return nil
	case *dry:
		n, err := migrate.DryRun(ctx, st.DB)
		if err != nil {
			return fmt.Errorf("essai à blanc : %w", err)
		}
		fmt.Printf("Essai à blanc réussi : %d migration(s) passeraient ; rien n'a été changé.\n", n)
		return nil
	}
	n, err := migrate.Up(ctx, st.DB)
	if err != nil {
		return err
	}
	slog.Info("migrations appliquées", "count", n, "version", migrate.Latest())
	return nil
}

// checkSchema : au démarrage, migrations appliquées si --migrate, sinon
// refus de servir une base en retard ; toujours refus d'une base plus
// récente que ce lattisd.
func checkSchema(ctx context.Context, st *store.Store, doMigrate bool) error {
	migrate.Version = httpserver.Version
	s, err := migrate.Check(ctx, st.DB)
	if err != nil {
		return fmt.Errorf("état des migrations : %w", err)
	}
	if s.Newer() {
		return fmt.Errorf("base en version %d, plus récente que ce lattisd (migrations jusqu'à %d) : installez un lattisd au moins aussi récent que celui qui a migré la base", s.Current, s.Latest)
	}
	for _, m := range s.Modified {
		slog.Warn("migration modifiée depuis son application (non rejouée)", "migration", m)
	}
	if len(s.Pending) == 0 {
		slog.Info("schéma à jour", "version", s.Current)
		return nil
	}
	if !doMigrate {
		return fmt.Errorf("%d migration(s) en attente (base en version %d, lattisd en %d) : démarrez avec --migrate, ou lancez « lattisd migrate » (« lattisd migrate --dry-run » pour essayer d'abord)",
			len(s.Pending), s.Current, s.Latest)
	}
	slog.Info("montée de version du schéma", "from", s.Current, "to", s.Latest, "pending", len(s.Pending))
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
	if err := checkSchema(ctx, st, doMigrate); err != nil {
		return err
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
	users, err := userdb.NewOAuth(cfg.PGRWURL, keys, cfg.Issuer())
	if err != nil {
		return err
	}
	defer users.Close()
	reps, err := replicas.New(ctx, st.DB, cfg.PGROURLs, cfg.PGPublicRO, cfg.ReplicaMaxLag)
	if err != nil {
		return fmt.Errorf("réplicas : %w", err)
	}
	defer reps.Close()
	m.Registry.MustRegister(reps)
	go reps.Run(ctx)
	adminSvc := &admin.Service{Config: cfg, Store: st, Keys: keys, Users: users, Replicas: reps, Version: httpserver.Version}
	authSvc.AdminURL = "/admin/"
	// Horaires des pipelines, dates importées et affichage : fuseau du serveur.
	time.Local = cfg.Timezone
	auth.SourceURL, admin.SourceURL = cfg.SourceURL, cfg.SourceURL
	srv := &httpserver.Server{Config: cfg, Store: st, Keys: keys, Auth: authSvc, Admin: adminSvc, Replicas: reps}
	if cfg.Edition == config.EditionWeb {
		web, err := webManager(cfg, st, keys, adminSvc.Session)
		if err != nil {
			return err
		}
		srv.Web = web
		web.Init(ctx)
		if cfg.WebEngineListen != "" {
			// Moteurs en conteneur ou en pod : /engine/token sur une écoute
			// interne, jamais exposée au public.
			el := &http.Server{Addr: cfg.WebEngineListen, Handler: web.TokenHandler(), ReadHeaderTimeout: 10 * time.Second}
			go func() {
				if err := el.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
					slog.Error("écoute des moteurs", "addr", cfg.WebEngineListen, "err", err)
				}
			}()
			defer el.Close()
		}
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
			defer cancel()
			srv.Web.Stop(sctx)
		}()
	}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				users.Sweep()
			}
		}
	}()

	public := &http.Server{Addr: cfg.ListenAddr, Handler: srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	internal := &http.Server{Addr: cfg.MetricsAddr, Handler: metricsMux, ReadHeaderTimeout: 10 * time.Second}

	go purgeLoop(ctx, st)
	runner := &pipeline.Runner{Store: st, Users: users, Dir: cfg.PipelineDir, SecretsDir: cfg.PipelineSecretsDir,
		AllowedHosts: cfg.PipelineAllowedHosts, AdminURL: cfg.URL("/admin/pipelines"), Metrics: m}
	adminSvc.Pipelines = runner
	srv.Users, srv.Pipelines = users, runner
	srv.Forms = &forms.Handler{Store: st, Users: users, Keys: keys, Session: adminSvc.Session}
	// Relais des changements : une écoute PostgreSQL pour tous les postes.
	srv.Events = &events.Hub{Pool: st.DB, Users: users}
	go srv.Events.Run(ctx)
	go runner.Run(ctx)
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
			if err := st.AsActor(ctx, "lattisd (formulaires)", func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `SELECT lattis.forms_housekeeping()`)
				return err
			}); err != nil {
				slog.Warn("entretien des formulaires", "err", err)
			}
		}
	}
}

// webManager : moteurs de l'édition web, lancés selon LATTIS_WEB_LAUNCHER.
func webManager(cfg *config.Config, st *store.Store, keys *tokens.KeySet, session func(*http.Request) (store.User, error)) (*webapp.Manager, error) {
	m := &webapp.Manager{Config: cfg, Users: st, Keys: keys, Session: session, TokenURL: cfg.WebTokenURL}
	switch cfg.WebLauncher {
	case "docker":
		tok, err := os.ReadFile(cfg.LauncherTokenFile)
		if err != nil {
			return nil, fmt.Errorf("LATTIS_WEB_LAUNCHER_TOKEN_FILE : %w", err)
		}
		m.Launcher = &webapp.DockerLauncher{URL: cfg.LauncherURL, Token: strings.TrimSpace(string(tok))}
	case "kubernetes":
		k, err := webapp.NewKubeLauncher(cfg.K8s)
		if err != nil {
			return nil, err
		}
		m.Launcher = k
	default:
		uids := cfg.WebEngineUIDs
		// En root sous Linux : un UID par moteur, sans réglage.
		if uids[1] == 0 && runtime.GOOS == "linux" && os.Geteuid() == 0 {
			uids = webapp.DefaultUIDs(cfg.WebMaxEngines)
			slog.Info("moteurs web : un UID système par moteur", "first", uids[0], "count", uids[1])
		}
		m.Launcher = &webapp.ProcessLauncher{Bin: cfg.WebEngine, CacheDir: cfg.WebCacheDir, UIDs: uids}
		if m.TokenURL == "" {
			u, err := webTokenURL(cfg)
			if err != nil {
				return nil, err
			}
			m.TokenURL = u
		}
	}
	if cfg.WebSpare > 0 {
		m.Launcher = &webapp.Pool{Inner: m.Launcher, Spare: cfg.WebSpare, Max: cfg.WebMaxEngines,
			Template: webapp.Spec{Server: cfg.Issuer(), TokenURL: m.TokenURL, Idle: cfg.WebIdle}}
	}
	slog.Info("moteurs web", "launcher", cfg.WebLauncher, "spare", cfg.WebSpare, "max", cfg.WebMaxEngines)
	return m, nil
}

// webTokenURL : adresse de /engine/token pour les moteurs en processus, en
// bouclage local sur l'écoute de lattisd (LATTIS_WEB_TOKEN_URL la remplace).
func webTokenURL(cfg *config.Config) (string, error) {
	if cfg.TLSCertFile != "" {
		return "", errors.New("édition web avec TLS dans lattisd : préciser LATTIS_WEB_TOKEN_URL")
	}
	_, port, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		return "", fmt.Errorf("LATTIS_LISTEN_ADDR : %w", err)
	}
	return "http://127.0.0.1:" + port + "/engine/token", nil
}
