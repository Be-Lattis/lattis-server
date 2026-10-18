// Package config lit la configuration d'amorçage de lattisd (§16).
//
// Seul l'amorçage est hors base ; le reste est dans lattis.server_config.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Edition choisit la version déployée (§4).
type Edition string

const (
	EditionAdmin Edition = "admin"
	EditionWeb   Edition = "web"
)

// Endpoint est un point de connexion PostgreSQL annoncé aux clients.
type Endpoint struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// String renvoie « hôte:port ».
func (e Endpoint) String() string { return net.JoinHostPort(e.Host, strconv.Itoa(e.Port)) }

// Config est la configuration d'amorçage.
type Config struct {
	Edition        Edition
	PublicURL      *url.URL // émetteur OIDC
	ListenAddr     string
	MetricsAddr    string
	TLSCertFile    string
	TLSKeyFile     string
	PGRWURL        string   // connexion de service (lattis_service)
	PGROURLs       []string // connexions de service vers les réplicas, dans l'ordre de PGPublicRO
	PGPublicRW     Endpoint
	PGPublicRO     []Endpoint
	PGDatabase     string
	PGSSLMode      string
	SigningKeyFile string
	SecretsDir     string
	BootstrapOwner string
	MinClient      string
	WebCacheDir    string
	WebMaxEngines  int
	// TrustProxy : faire confiance à X-Forwarded-For pour la limitation de débit.
	TrustProxy bool
	// ExtraRedirects : URI de redirection supplémentaires acceptées pour le
	// client public lattis-desktop (en plus du bouclage local).
	ExtraRedirects []string
	// DownloadURL : page de téléchargement de l'application, proposée par
	// les liens vers un classeur (vide : rien).
	DownloadURL   string
	ShutdownGrace time.Duration
	// RateLimit : requêtes par minute et par adresse sur /oauth/*, /login/*, /setup.
	RateLimit int
	// ReplicaMaxLag : retard au-delà duquel un réplica est retiré de la
	// découverte (§12).
	ReplicaMaxLag time.Duration
}

// FromEnv lit les variables LATTIS_*.
func FromEnv() (*Config, error) {
	return parse(os.Getenv)
}

func parse(get func(string) string) (*Config, error) {
	var errs []error
	def := func(k, d string) string {
		if v := strings.TrimSpace(get(k)); v != "" {
			return v
		}
		return d
	}
	c := &Config{
		Edition:        Edition(def("LATTIS_EDITION", string(EditionAdmin))),
		ListenAddr:     def("LATTIS_LISTEN_ADDR", ":8080"),
		MetricsAddr:    def("LATTIS_METRICS_ADDR", ":9464"),
		TLSCertFile:    def("LATTIS_TLS_CERT_FILE", ""),
		TLSKeyFile:     def("LATTIS_TLS_KEY_FILE", ""),
		PGRWURL:        def("LATTIS_PG_RW_URL", ""),
		PGDatabase:     def("LATTIS_PG_DATABASE", "lattis"),
		PGSSLMode:      def("LATTIS_PG_SSLMODE", "verify-full"),
		SigningKeyFile: def("LATTIS_SIGNING_KEY_FILE", ""),
		SecretsDir:     def("LATTIS_SECRETS_DIR", ""),
		BootstrapOwner: strings.ToLower(def("LATTIS_BOOTSTRAP_OWNER", "")),
		MinClient:      def("LATTIS_MIN_CLIENT_VERSION", "0.0.0"),
		WebCacheDir:    def("LATTIS_WEB_CACHE_DIR", ""),
		ExtraRedirects: splitList(def("LATTIS_EXTRA_REDIRECT_URIS", "")),
		DownloadURL:    def("LATTIS_DOWNLOAD_URL", ""),
		ShutdownGrace:  15 * time.Second,
	}
	if c.Edition != EditionAdmin && c.Edition != EditionWeb {
		errs = append(errs, fmt.Errorf("LATTIS_EDITION : admin ou web, pas %q", c.Edition))
	}
	if c.Edition == EditionWeb {
		errs = append(errs, errors.New("LATTIS_EDITION=web : l'application web n'est pas encore disponible"))
	}
	pub := def("LATTIS_PUBLIC_URL", "")
	if pub == "" {
		errs = append(errs, errors.New("LATTIS_PUBLIC_URL manquant"))
	} else if u, err := url.Parse(pub); err != nil || u.Scheme == "" || u.Host == "" {
		errs = append(errs, fmt.Errorf("LATTIS_PUBLIC_URL invalide : %q", pub))
	} else {
		u.Path = strings.TrimSuffix(u.Path, "/")
		c.PublicURL = u
	}
	if c.PGRWURL == "" {
		errs = append(errs, errors.New("LATTIS_PG_RW_URL manquant"))
	}
	c.PGROURLs = splitList(def("LATTIS_PG_RO_URLS", ""))
	if c.SigningKeyFile == "" {
		errs = append(errs, errors.New("LATTIS_SIGNING_KEY_FILE manquant"))
	}
	if rw := def("LATTIS_PG_PUBLIC_RW", ""); rw != "" {
		ep, err := parseEndpoint(rw)
		if err != nil {
			errs = append(errs, fmt.Errorf("LATTIS_PG_PUBLIC_RW : %w", err))
		}
		c.PGPublicRW = ep
	} else {
		errs = append(errs, errors.New("LATTIS_PG_PUBLIC_RW manquant (hôte:port annoncé aux postes)"))
	}
	for _, s := range splitList(def("LATTIS_PG_PUBLIC_RO", "")) {
		ep, err := parseEndpoint(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("LATTIS_PG_PUBLIC_RO : %w", err))
			continue
		}
		c.PGPublicRO = append(c.PGPublicRO, ep)
	}
	// Chaque réplica surveillé correspond, dans l'ordre, à un point annoncé.
	// Sans connexion de service, les points ro sont annoncés tels quels (par
	// exemple un service unique qui répartit et écarte lui-même les retards).
	if len(c.PGROURLs) > 0 && len(c.PGROURLs) != len(c.PGPublicRO) {
		errs = append(errs, fmt.Errorf("LATTIS_PG_RO_URLS (%d) et LATTIS_PG_PUBLIC_RO (%d) doivent avoir le même nombre d'éléments",
			len(c.PGROURLs), len(c.PGPublicRO)))
	}
	if d, err := time.ParseDuration(def("LATTIS_REPLICA_MAX_LAG", "10s")); err == nil && d > 0 {
		c.ReplicaMaxLag = d
	} else {
		errs = append(errs, errors.New("LATTIS_REPLICA_MAX_LAG : durée positive attendue (10s)"))
	}
	if n, err := strconv.Atoi(def("LATTIS_WEB_MAX_ENGINES", "0")); err == nil {
		c.WebMaxEngines = n
	}
	c.TrustProxy = def("LATTIS_TRUST_PROXY", "false") == "true"
	if n, err := strconv.Atoi(def("LATTIS_RATE_LIMIT", "120")); err == nil && n > 0 {
		c.RateLimit = n
	} else {
		errs = append(errs, errors.New("LATTIS_RATE_LIMIT : entier positif attendu"))
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		errs = append(errs, errors.New("LATTIS_TLS_CERT_FILE et LATTIS_TLS_KEY_FILE vont ensemble"))
	}
	return c, errors.Join(errs...)
}

// Issuer est l'émetteur OIDC, sans barre finale.
func (c *Config) Issuer() string { return c.PublicURL.String() }

// URL construit une URL publique.
func (c *Config) URL(path string) string { return c.Issuer() + path }

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseEndpoint(s string) (Endpoint, error) {
	host, port, ok := strings.Cut(s, ":")
	if i := strings.LastIndex(s, ":"); strings.HasPrefix(s, "[") && i > 0 {
		host, port, ok = strings.Trim(s[:i], "[]"), s[i+1:], true
	}
	if !ok {
		return Endpoint{Host: s, Port: 5432}, nil
	}
	p, err := strconv.Atoi(port)
	if err != nil || p <= 0 || p > 65535 {
		return Endpoint{}, fmt.Errorf("port invalide dans %q", s)
	}
	return Endpoint{Host: host, Port: p}, nil
}
