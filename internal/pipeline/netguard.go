package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// Garde réseau des pipelines (sources HTTP et SFTP, webhook) : pas de
// bouclage, d'adresses privées, link-local ni de métadonnées du nuage, sauf
// hôtes autorisés par l'exploitant (LATTIS_PIPELINE_ALLOWED_HOSTS). Le
// contrôle porte sur l'adresse réellement jointe, après résolution DNS.

// ErrBlocked : destination refusée par la garde réseau.
var ErrBlocked = errors.New("adresse refusée : réseau interne (à autoriser dans LATTIS_PIPELINE_ALLOWED_HOSTS)")

// blockedIP dit si une adresse est interne.
func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(net.IPv4(100, 100, 100, 200)) ||
		(ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 64) // 100.64.0.0/10 (CGNAT)
}

// allowedHost dit si l'exploitant a autorisé cet hôte (nom ou adresse).
func (r *Runner) allowedHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, a := range r.AllowedHosts {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if a == host || (strings.HasPrefix(a, "*.") && strings.HasSuffix(host, a[1:])) {
			return true
		}
		if _, n, err := net.ParseCIDR(a); err == nil {
			if ip := net.ParseIP(host); ip != nil && n.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// dialer renvoie un Dialer qui refuse les adresses internes non autorisées.
func (r *Runner) dialer(host string) *net.Dialer {
	allowed := r.allowedHost(host)
	return &net.Dialer{Timeout: 30 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		h, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(h)
		if ip == nil {
			return ErrBlocked
		}
		if blockedIP(ip) && !allowed && !r.allowedHost(h) {
			return fmt.Errorf("%w (%s)", ErrBlocked, h)
		}
		return nil
	}}
}

// guardedClient : client HTTP soumis à la garde réseau ; une redirection
// ne peut ni changer d'hôte ni passer en http, et ne garde aucun en-tête
// d'authentification.
func (r *Runner) guardedClient(timeout time.Duration) *http.Client {
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			return r.dialer(host).DialContext(ctx, network, addr)
		},
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
	}
	return &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("trop de redirections")
		}
		first := via[0].URL
		if req.URL.Host != first.Host || (first.Scheme == "https" && req.URL.Scheme != "https") {
			return fmt.Errorf("redirection vers %s refusée", req.URL.Redacted())
		}
		return nil
	}}
}

// --- Pipelines des gestionnaires d'équipe -------------------------------------

// ErrTeamHost : hôte hors de la liste autorisée aux gestionnaires d'équipe.
var ErrTeamHost = errors.New("adresse non autorisée aux gestionnaires d'équipe (à ajouter sur l'écran Serveur par un propriétaire)")

// teamHosts lit la liste des hôtes autorisés aux gestionnaires d'équipe.
func (r *Runner) teamHosts(ctx context.Context) ([]string, error) {
	var hosts []string
	err := r.Store.DB.QueryRow(ctx, `SELECT coalesce((SELECT value FROM lattis.server_config WHERE key = 'pipeline_team_hosts'), '[]')`).Scan(&hosts)
	return hosts, err
}

// matchHost : nom exact, *.domaine ou bloc d'adresses.
func matchHost(list []string, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, a := range list {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" || host == "" {
			continue
		}
		if a == host || (strings.HasPrefix(a, "*.") && strings.HasSuffix(host, a[1:])) {
			return true
		}
		if _, n, err := net.ParseCIDR(a); err == nil {
			if ip := net.ParseIP(host); ip != nil && n.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// teamGuard : un pipeline restreint ne joint que les hôtes autorisés aux
// gestionnaires d'équipe.
func (r *Runner) teamGuard(ctx context.Context, p *Pipeline, hosts ...string) error {
	if !p.Restricted {
		return nil
	}
	list, err := r.teamHosts(ctx)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		return ErrTeamHost
	}
	for _, h := range hosts {
		if !matchHost(list, h) {
			return fmt.Errorf("%w : %s", ErrTeamHost, h)
		}
	}
	return nil
}

func (r *Runner) teamGuardURL(ctx context.Context, p *Pipeline, raw string) error {
	if !p.Restricted {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	return r.teamGuard(ctx, p, u.Hostname())
}

// teamGuardDSN : hôtes d'une chaîne de connexion PostgreSQL (repli compris).
func (r *Runner) teamGuardDSN(ctx context.Context, p *Pipeline, dsn string) error {
	if !p.Restricted {
		return nil
	}
	cfg, err := pgx.ParseConfig(strings.TrimSpace(dsn))
	if err != nil {
		return err
	}
	hosts := []string{cfg.Host}
	for _, f := range cfg.Fallbacks {
		hosts = append(hosts, f.Host)
	}
	for _, h := range hosts {
		if strings.HasPrefix(h, "/") {
			return fmt.Errorf("%w : socket %s", ErrTeamHost, h)
		}
	}
	return r.teamGuard(ctx, p, hosts...)
}
