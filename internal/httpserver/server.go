// Package httpserver assemble l'API HTTP de lattisd (§11).
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lalamefine/lattis/protocol"

	"github.com/Be-Lattis/lattis-server/internal/admin"
	"github.com/Be-Lattis/lattis-server/internal/auth"
	"github.com/Be-Lattis/lattis-server/internal/config"
	"github.com/Be-Lattis/lattis-server/internal/events"
	"github.com/Be-Lattis/lattis-server/internal/forms"
	"github.com/Be-Lattis/lattis-server/internal/migrate"
	"github.com/Be-Lattis/lattis-server/internal/replicas"
	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/tokens"
	"github.com/Be-Lattis/lattis-server/internal/userdb"
	"github.com/Be-Lattis/lattis-server/internal/webapp"
)

// Version de lattisd, fixée à la compilation (-ldflags -X).
var Version = "dev"

// Server porte les routes publiques.
type Server struct {
	Config *config.Config
	Store  *store.Store
	Keys   *tokens.KeySet
	Auth   *auth.Service
	Admin  *admin.Service // nil : pas d'interface d'administration
	// Replicas choisit les points ro annoncés ; nil : ceux de la configuration.
	Replicas *replicas.Monitor
	// Web : moteurs de l'édition web ; nil hors édition web.
	Web *webapp.Manager
	// Users : connexions sous l'identité d'un utilisateur ; Pipelines :
	// réveil des pipelines (sources poussées, POST /api/push).
	Users     userdb.Runner
	Pipelines Kicker
	// Forms : formulaires (/f/<code>) ; nil : absents.
	Forms *forms.Handler
	// Events : relais des changements aux postes (GET /api/events).
	Events *events.Hub

	draining atomic.Bool
}

// Drain sort lattisd du service : /readyz répond 503, le reste continue.
func (s *Server) Drain() { s.draining.Store(true) }

// Handler renvoie le routeur avec ses intergiciels.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/lattis.json", s.discovery)
	mux.HandleFunc("GET /.well-known/openid-configuration", s.openidConfiguration)
	mux.HandleFunc("GET /jwks.json", s.jwks)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("POST /api/preview", s.preview)
	mux.HandleFunc("POST /api/push/{id}", s.push)
	mux.HandleFunc("GET /api/push/{id}", s.pushStatus)
	if s.Events != nil {
		mux.HandleFunc("GET /api/events", s.Events.Handler(s.bearerExp))
	}
	if s.Forms != nil {
		s.Forms.Bearer = s.bearer
		s.Forms.ClientIP = func(r *http.Request) string { return auth.ClientIP(r, s.Config.TrustProxy) }
		s.Forms.Routes(mux)
	}
	s.Auth.Routes(mux)
	if s.Admin != nil {
		s.Admin.Routes(mux)
	}
	if s.Web != nil {
		s.Web.Routes(mux)
	} else {
		// Édition « gestion des rôles » : l'application web est refusée, avec
		// une page qui le dit (plutôt qu'une page introuvable sans explication).
		mux.HandleFunc("/app/", webDisabled)
		mux.HandleFunc("/app", webDisabled)
	}

	limiter := newLimiter(max(s.Config.RateLimit, 1), time.Minute)
	return securityHeaders(s.Config.PublicURL.Scheme == "https", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/oauth/") || (strings.HasPrefix(p, "/api/") && p != "/api/events") || strings.HasPrefix(p, "/admin/do/") || strings.HasPrefix(p, "/login/") || strings.HasPrefix(p, "/setup") || strings.HasPrefix(p, "/password/") {
			if !limiter.allow(auth.ClientIP(r, s.Config.TrustProxy)) {
				w.Header().Set("Retry-After", "60")
				http.Error(w, "trop de requêtes", http.StatusTooManyRequests)
				return
			}
			// Un tableau poussé a sa propre limite (pipeline.MaxBytes).
			if !strings.HasPrefix(p, "/api/push/") && !strings.HasPrefix(p, "/api/forms/") {
				r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// Discovery est le document /.well-known/lattis.json lu par l'application :
// le type partagé avec elle (protocol), points PostgreSQL au format objet
// {"host", "port"} du contrat (§1).
type Discovery struct {
	protocol.Discovery
	Postgres DiscoveryPostgres `json:"postgres"`
}

// DiscoveryPostgres décrit les points de connexion PostgreSQL.
type DiscoveryPostgres struct {
	Database string            `json:"database"`
	SSLMode  string            `json:"sslmode"`
	RW       config.Endpoint   `json:"rw"`
	RO       []config.Endpoint `json:"ro"`
}

func (s *Server) discovery(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg, err := s.Store.Config(ctx)
	if err != nil {
		s.unavailable(w, err)
		return
	}
	providers, err := s.Store.Providers(ctx)
	if err != nil {
		s.unavailable(w, err)
		return
	}
	d := Discovery{
		Discovery: protocol.Discovery{
			Name: cfg.DisplayName, Logo: cfg.LogoURL, Version: Version, MinClientVersion: s.Config.MinClient,
			Edition: string(s.Config.Edition), AccessMode: cfg.AccessMode, Issuer: s.Config.Issuer(),
			ClientID: protocol.DesktopClientID, Scope: protocol.DefaultScope,
			AuthorizationEndpoint: s.Config.URL(protocol.PathAuthorize), TokenEndpoint: s.Config.URL(protocol.PathToken),
			RevocationEndpoint: s.Config.URL(protocol.PathRevoke), PreviewEndpoint: s.Config.URL("/api/preview"),
			SourceURL: s.Config.SourceURL,
			Providers: []protocol.Provider{},
		},
		Postgres: DiscoveryPostgres{Database: s.Config.PGDatabase, SSLMode: s.Config.PGSSLMode,
			RW: s.Config.PGPublicRW, RO: s.readEndpoints()},
	}
	for _, p := range providers {
		d.Providers = append(d.Providers, protocol.Provider{ID: p.ID, Label: p.Label, Kind: p.Kind, Icon: p.Kind})
	}
	w.Header().Set("Cache-Control", "public, max-age=10") // court : la liste ro suit le retard des réplicas
	writeJSON(w, d)
}

// readEndpoints : réplicas annoncés, sans ceux en retard ou injoignables.
func (s *Server) readEndpoints() []config.Endpoint {
	if s.Replicas != nil {
		return s.Replicas.Available()
	}
	return append([]config.Endpoint{}, s.Config.PGPublicRO...)
}

func (s *Server) openidConfiguration(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=3600")
	writeJSON(w, map[string]any{
		"issuer":                                         s.Config.Issuer(),
		"authorization_endpoint":                         s.Config.URL("/oauth/authorize"),
		"token_endpoint":                                 s.Config.URL("/oauth/token"),
		"revocation_endpoint":                            s.Config.URL("/oauth/revoke"),
		"jwks_uri":                                       s.Config.URL("/jwks.json"),
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"subject_types_supported":                        []string{"public"},
		"id_token_signing_alg_values_supported":          []string{"EdDSA"},
		"scopes_supported":                               []string{"openid", "lattis", "offline_access", "email", "profile"},
		"claims_supported":                               []string{"sub", "email", "name", "pg_role"},
		"authorization_response_iss_parameter_supported": true,
	})
}

func (s *Server) jwks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, s.Keys.JWKS())
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if s.draining.Load() {
		http.Error(w, "arrêt en cours", http.StatusServiceUnavailable)
		return
	}
	// Base à jour, ou en avance sans migration incompatible : pendant un
	// déploiement progressif, l'ancienne version reste dans le service.
	if err := migrate.Ready(ctx, s.Store.DB); err != nil {
		if !errors.Is(err, migrate.ErrPending) && !errors.Is(err, migrate.ErrNewer) {
			http.Error(w, "PostgreSQL injoignable", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) unavailable(w http.ResponseWriter, err error) {
	slog.Error("base indisponible", "err", err)
	http.Error(w, "service indisponible", http.StatusServiceUnavailable)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func securityHeaders(https bool, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		if https {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		if strings.HasPrefix(r.URL.Path, "/app/") || strings.HasPrefix(r.URL.Path, "/demo/") {
			// Application web (SvelteKit) : script d'amorçage en ligne,
			// styles de composants, WebSocket vers la même origine.
			h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' https: data: blob:; font-src 'self' data:; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'self'")
		} else {
			h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' https: data:; font-src 'self' data:; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		}
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next(w, r)
	})
}

// webDisabled : réponse de /app/ quand l'édition web n'est pas activée.
func webDisabled(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`<!doctype html><html lang="fr"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Édition web non activée</title></head><body>
<h1>L'application web n'est pas activée sur ce serveur</h1>
<p>Ce Serveur Lattis est installé en édition « gestion des rôles » : ouvrez vos classeurs avec l'application Lattis sur votre poste.</p>
<p>Administrateur : l'édition web s'active avec <code>LATTIS_EDITION=web</code> (voir la documentation d'installation, §5.1).</p>
<p><a href="/">Accueil du serveur</a></p>
</body></html>
`))
}
