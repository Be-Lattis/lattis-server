// Package httpserver assemble l'API HTTP de lattisd (§11).
package httpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Be-Lattis/lattis-server/internal/admin"
	"github.com/Be-Lattis/lattis-server/internal/auth"
	"github.com/Be-Lattis/lattis-server/internal/config"
	"github.com/Be-Lattis/lattis-server/internal/migrate"
	"github.com/Be-Lattis/lattis-server/internal/replicas"
	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/tokens"
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
}

// Handler renvoie le routeur avec ses intergiciels.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/lattis.json", s.discovery)
	mux.HandleFunc("GET /.well-known/openid-configuration", s.openidConfiguration)
	mux.HandleFunc("GET /jwks.json", s.jwks)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/.well-known/lattis.json", http.StatusFound)
	})
	mux.HandleFunc("POST /api/preview", s.preview)
	s.Auth.Routes(mux)
	if s.Admin != nil {
		s.Admin.Routes(mux)
	}

	limiter := newLimiter(max(s.Config.RateLimit, 1), time.Minute)
	return securityHeaders(s.Config.PublicURL.Scheme == "https", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/oauth/") || strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/admin/do/") || strings.HasPrefix(p, "/login/") || strings.HasPrefix(p, "/setup") {
			if !limiter.allow(auth.ClientIP(r, s.Config.TrustProxy)) {
				w.Header().Set("Retry-After", "60")
				http.Error(w, "trop de requêtes", http.StatusTooManyRequests)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		}
		mux.ServeHTTP(w, r)
	})
}

// Discovery est le document /.well-known/lattis.json lu par l'application.
type Discovery struct {
	Name             string              `json:"name"`
	Logo             string              `json:"logo,omitempty"`
	Version          string              `json:"version"`
	MinClientVersion string              `json:"min_client_version"`
	Edition          string              `json:"edition"`
	AccessMode       string              `json:"access_mode"`
	Issuer           string              `json:"issuer"`
	ClientID         string              `json:"client_id"`
	Scope            string              `json:"scope"`
	AuthorizationURL string              `json:"authorization_endpoint"`
	TokenURL         string              `json:"token_endpoint"`
	RevocationURL    string              `json:"revocation_endpoint"`
	PreviewURL       string              `json:"preview_endpoint"`
	Providers        []DiscoveryProvider `json:"providers"`
	Postgres         DiscoveryPostgres   `json:"postgres"`
}

// DiscoveryProvider est un fournisseur affiché sur la page de connexion de Lattis.
type DiscoveryProvider struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
	Icon  string `json:"icon"`
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
		Name: cfg.DisplayName, Logo: cfg.LogoURL, Version: Version, MinClientVersion: s.Config.MinClient,
		Edition: string(s.Config.Edition), AccessMode: cfg.AccessMode, Issuer: s.Config.Issuer(),
		ClientID: auth.DesktopClientID, Scope: auth.DefaultScope,
		AuthorizationURL: s.Config.URL("/oauth/authorize"), TokenURL: s.Config.URL("/oauth/token"),
		RevocationURL: s.Config.URL("/oauth/revoke"), PreviewURL: s.Config.URL("/api/preview"),
		Providers: []DiscoveryProvider{},
		Postgres: DiscoveryPostgres{Database: s.Config.PGDatabase, SSLMode: s.Config.PGSSLMode,
			RW: s.Config.PGPublicRW, RO: s.readEndpoints()},
	}
	for _, p := range providers {
		d.Providers = append(d.Providers, DiscoveryProvider{ID: p.ID, Label: p.Label, Kind: p.Kind, Icon: p.Kind})
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
	v, err := migrate.Current(ctx, s.Store.DB)
	if err != nil {
		http.Error(w, "PostgreSQL injoignable", http.StatusServiceUnavailable)
		return
	}
	if v != migrate.Latest() {
		http.Error(w, "migrations en retard", http.StatusServiceUnavailable)
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
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' https: data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next(w, r)
	})
}
