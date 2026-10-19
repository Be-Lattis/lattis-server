package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
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
