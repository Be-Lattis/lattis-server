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
	"runtime"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // base des fuseaux dans le binaire (image distroless)
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
// Mail : envoi d'e-mails (LATTIS_SMTP_*) ; Host vide : pas d'envoi.
type Mail struct {
	Host, User, PasswordFile, From string
	Port                           int
	// TLS : starttls (défaut), tls (TLS dès la connexion, port 465) ou none.
	TLS string
	// PerHour : e-mails par heure pour tout le serveur ; PerAddress : par
	// destinataire et par heure (quotas du fournisseur, abus).
	PerHour, PerAddress int
}

// K8sEngines : pods des moteurs (LATTIS_WEB_K8S_*).
type K8sEngines struct {
	Image, Namespace, PullSecret string
	CPU, Memory                  string // requêtes ; Memory est aussi la limite
	// CacheSize : volume persistant par utilisateur (« 1Gi ») ; vide : emptyDir.
	CacheSize, StorageClass string
	// OwnerPod, OwnerUID : pod de lattisd (API descendante), propriétaire des
	// pods des moteurs (supprimés avec lui).
	OwnerPod, OwnerUID string
}

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
	// WebEngine : binaire lattis-engine lancé pour chaque session web ;
	// WebIdle : inactivité avant son arrêt.
	WebEngine string
	WebIdle   time.Duration
	// WebEngineUIDs : plage d'UID (et de GID) système des moteurs, un par
	// moteur en cours ; [0, 0] : même utilisateur que lattisd.
	WebEngineUIDs [2]int
	// WebSpare : moteurs gardés prêts d'avance (réserve), 0 : aucun.
	WebSpare int
	// WebLauncher : process (processus de lattisd), docker (conteneurs par
	// le mini lanceur lattis-launcher) ou kubernetes (pods).
	WebLauncher string
	// WebEngineListen : écoute interne de /engine/token pour les moteurs en
	// conteneur ou en pod (vide en mode process).
	WebEngineListen string
	// WebTokenURL : adresse de /engine/token vue des moteurs (vide en mode
	// process : bouclage local sur l'écoute publique).
	WebTokenURL string
	// Mode docker : adresse du mini lanceur et fichier de son jeton.
	LauncherURL       string
	LauncherTokenFile string
	// Mode kubernetes : image du moteur et réglages des pods.
	K8s K8sEngines
	// Mail : envoi d'e-mails ; Mail.Host vide : désactivé.
	Mail Mail
	// TrustProxy : faire confiance à X-Forwarded-For pour la limitation de débit.
	TrustProxy bool
	// ExtraRedirects : URI de redirection supplémentaires acceptées pour le
	// client public lattis-desktop (en plus du bouclage local).
	ExtraRedirects []string
	// DownloadURL : page de téléchargement de l'application, proposée par
	// les liens vers un classeur (vide : rien).
	DownloadURL string
	// SourceURL : code source de ce serveur, proposé à ses utilisateurs
	// (AGPL-3.0, §13). À changer pour une version modifiée.
	SourceURL string
	// PipelineDir : fichiers des pipelines d'ingestion (envois, dépôts).
	PipelineDir string
	// PipelineSecretsDir : secrets des pipelines, distinct de celui des
	// clients OAuth ; PipelineAllowedHosts : hôtes internes joignables.
	PipelineSecretsDir string
	// Timezone : fuseau des horaires de pipelines, des dates importées et de
	// l'affichage (LATTIS_TIMEZONE, Europe/Paris par défaut).
	Timezone             *time.Location
	PipelineAllowedHosts []string
	ShutdownGrace        time.Duration
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
		Edition:              Edition(def("LATTIS_EDITION", string(EditionAdmin))),
		ListenAddr:           def("LATTIS_LISTEN_ADDR", ":8080"),
		MetricsAddr:          def("LATTIS_METRICS_ADDR", ":9464"),
		TLSCertFile:          def("LATTIS_TLS_CERT_FILE", ""),
		TLSKeyFile:           def("LATTIS_TLS_KEY_FILE", ""),
		PGRWURL:              def("LATTIS_PG_RW_URL", ""),
		PGDatabase:           def("LATTIS_PG_DATABASE", "lattis"),
		PGSSLMode:            def("LATTIS_PG_SSLMODE", "verify-full"),
		SigningKeyFile:       def("LATTIS_SIGNING_KEY_FILE", ""),
		SecretsDir:           def("LATTIS_SECRETS_DIR", ""),
		BootstrapOwner:       strings.ToLower(def("LATTIS_BOOTSTRAP_OWNER", "")),
		MinClient:            def("LATTIS_MIN_CLIENT_VERSION", "0.0.0"),
		WebCacheDir:          def("LATTIS_WEB_CACHE_DIR", "/var/lib/lattis/web"),
		WebEngine:            def("LATTIS_WEB_ENGINE", ""),
		ExtraRedirects:       splitList(def("LATTIS_EXTRA_REDIRECT_URIS", "")),
		DownloadURL:          def("LATTIS_DOWNLOAD_URL", ""),
		SourceURL:            def("LATTIS_SOURCE_URL", "https://github.com/Be-Lattis/lattis-server"),
		PipelineDir:          def("LATTIS_PIPELINE_DIR", "/var/lib/lattis/pipelines"),
		PipelineSecretsDir:   def("LATTIS_PIPELINE_SECRETS_DIR", ""),
		Timezone:             time.UTC,
		PipelineAllowedHosts: splitList(def("LATTIS_PIPELINE_ALLOWED_HOSTS", "")),
		ShutdownGrace:        15 * time.Second,
	}
	if c.Edition != EditionAdmin && c.Edition != EditionWeb {
		errs = append(errs, fmt.Errorf("LATTIS_EDITION : admin ou web, pas %q", c.Edition))
	}
	c.WebLauncher = def("LATTIS_WEB_LAUNCHER", "process")
	c.WebTokenURL = def("LATTIS_WEB_TOKEN_URL", "")
	c.LauncherURL = strings.TrimRight(def("LATTIS_WEB_LAUNCHER_URL", ""), "/")
	c.LauncherTokenFile = def("LATTIS_WEB_LAUNCHER_TOKEN_FILE", "")
	c.K8s = K8sEngines{Image: def("LATTIS_WEB_K8S_IMAGE", ""), Namespace: def("LATTIS_WEB_K8S_NAMESPACE", ""),
		PullSecret: def("LATTIS_WEB_K8S_PULL_SECRET", ""), CPU: def("LATTIS_WEB_K8S_CPU", "250m"),
		Memory: def("LATTIS_WEB_K8S_MEMORY", "1Gi"), CacheSize: def("LATTIS_WEB_K8S_CACHE_SIZE", ""),
		StorageClass: def("LATTIS_WEB_K8S_STORAGE_CLASS", ""), OwnerPod: def("LATTIS_WEB_K8S_OWNER_POD", ""),
		OwnerUID: def("LATTIS_WEB_K8S_OWNER_UID", "")}
	if c.Edition == EditionWeb {
		switch c.WebLauncher {
		case "process":
			if c.WebEngine == "" {
				errs = append(errs, errors.New("LATTIS_EDITION=web : LATTIS_WEB_ENGINE (chemin de lattis-engine) manquant"))
			}
		case "docker", "kubernetes":
			c.WebEngineListen = def("LATTIS_WEB_ENGINE_LISTEN", ":8081")
			if c.WebTokenURL == "" {
				errs = append(errs, fmt.Errorf("LATTIS_WEB_LAUNCHER=%s : LATTIS_WEB_TOKEN_URL (adresse de /engine/token vue des moteurs, écoute LATTIS_WEB_ENGINE_LISTEN) manquant", c.WebLauncher))
			}
			if c.WebLauncher == "docker" && (c.LauncherURL == "" || c.LauncherTokenFile == "") {
				errs = append(errs, errors.New("LATTIS_WEB_LAUNCHER=docker : LATTIS_WEB_LAUNCHER_URL et LATTIS_WEB_LAUNCHER_TOKEN_FILE manquants"))
			}
			if c.WebLauncher == "kubernetes" && c.K8s.Image == "" {
				errs = append(errs, errors.New("LATTIS_WEB_LAUNCHER=kubernetes : LATTIS_WEB_K8S_IMAGE (image de lattis-engine) manquant"))
			}
		default:
			errs = append(errs, fmt.Errorf("LATTIS_WEB_LAUNCHER : process, docker ou kubernetes, pas %q", c.WebLauncher))
		}
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
	if n, err := strconv.Atoi(def("LATTIS_WEB_MAX_ENGINES", "50")); err == nil && n > 0 {
		c.WebMaxEngines = n
	} else {
		errs = append(errs, errors.New("LATTIS_WEB_MAX_ENGINES : entier positif attendu"))
	}
	if d, err := time.ParseDuration(def("LATTIS_WEB_IDLE", "30m")); err == nil && d > 0 {
		c.WebIdle = d
	} else {
		errs = append(errs, errors.New("LATTIS_WEB_IDLE : durée positive attendue (30m)"))
	}
	if v := def("LATTIS_WEB_ENGINE_UIDS", ""); v != "" {
		lo, hi, _ := strings.Cut(v, "-")
		first, err1 := strconv.Atoi(strings.TrimSpace(lo))
		last, err2 := strconv.Atoi(strings.TrimSpace(hi))
		switch {
		case err1 != nil || err2 != nil || first < 1000 || last < first:
			errs = append(errs, errors.New("LATTIS_WEB_ENGINE_UIDS : plage d'UID attendue (200000-200999, à partir de 1000)"))
		case last-first+1 < c.WebMaxEngines:
			errs = append(errs, fmt.Errorf("LATTIS_WEB_ENGINE_UIDS : %d UID pour %d moteurs (LATTIS_WEB_MAX_ENGINES)", last-first+1, c.WebMaxEngines))
		case runtime.GOOS != "linux":
			errs = append(errs, errors.New("LATTIS_WEB_ENGINE_UIDS : Linux seulement"))
		default:
			c.WebEngineUIDs = [2]int{first, last - first + 1}
		}
	}
	if n, err := strconv.Atoi(def("LATTIS_WEB_POOL_SPARE", "0")); err == nil && n >= 0 && n < c.WebMaxEngines {
		c.WebSpare = n
	} else {
		errs = append(errs, fmt.Errorf("LATTIS_WEB_POOL_SPARE : entier de 0 à %d (moins que LATTIS_WEB_MAX_ENGINES)", c.WebMaxEngines-1))
	}
	c.Mail = Mail{Host: def("LATTIS_SMTP_HOST", ""), User: def("LATTIS_SMTP_USER", ""),
		PasswordFile: def("LATTIS_SMTP_PASSWORD_FILE", ""), From: def("LATTIS_SMTP_FROM", ""), TLS: def("LATTIS_SMTP_TLS", "starttls")}
	if c.Mail.Host != "" {
		defPort := map[string]string{"starttls": "587", "tls": "465", "none": "25"}[c.Mail.TLS]
		port, err := strconv.Atoi(def("LATTIS_SMTP_PORT", defPort))
		switch {
		case err != nil || port <= 0 || port > 65535:
			errs = append(errs, errors.New("LATTIS_SMTP_PORT : port attendu"))
		case c.Mail.TLS != "starttls" && c.Mail.TLS != "tls" && c.Mail.TLS != "none":
			errs = append(errs, errors.New("LATTIS_SMTP_TLS : starttls, tls ou none"))
		case !strings.Contains(c.Mail.From, "@"):
			errs = append(errs, errors.New("LATTIS_SMTP_FROM : adresse d'expédition attendue (« Lattis <lattis@acme.fr> »)"))
		}
		c.Mail.Port = port
		for k, dst := range map[string]*int{"LATTIS_MAIL_PER_HOUR": &c.Mail.PerHour, "LATTIS_MAIL_PER_ADDRESS": &c.Mail.PerAddress} {
			d := map[string]string{"LATTIS_MAIL_PER_HOUR": "200", "LATTIS_MAIL_PER_ADDRESS": "5"}[k]
			if n, err := strconv.Atoi(def(k, d)); err == nil && n > 0 {
				*dst = n
			} else {
				errs = append(errs, fmt.Errorf("%s : entier positif attendu", k))
			}
		}
	}
	if loc, err := time.LoadLocation(def("LATTIS_TIMEZONE", "Europe/Paris")); err == nil {
		c.Timezone = loc
	} else {
		errs = append(errs, fmt.Errorf("LATTIS_TIMEZONE : fuseau inconnu (%v)", err))
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
