package pipeline

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBlockedIP(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "192.168.0.1", "172.16.5.5", "169.254.169.254", "::1", "fd00::1", "100.64.0.1"} {
		if !blockedIP(net.ParseIP(s)) {
			t.Errorf("%s non bloquée", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "2606:4700::1111"} {
		if blockedIP(net.ParseIP(s)) {
			t.Errorf("%s bloquée", s)
		}
	}
}

func TestGuardedClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	r := &Runner{}
	if _, err := r.guardedClient(5 * time.Second).Get(srv.URL); err == nil || !errors.Is(err, ErrBlocked) && !strings.Contains(err.Error(), "réseau interne") {
		t.Fatalf("bouclage joint : %v", err)
	}
	r.AllowedHosts = []string{"127.0.0.1"}
	resp, err := r.guardedClient(5 * time.Second).Get(srv.URL)
	if err != nil {
		t.Fatalf("hôte autorisé refusé : %v", err)
	}
	resp.Body.Close()
}

func TestTeamHosts(t *testing.T) {
	list := []string{"api.exemple.fr", "*.partenaire.fr", "203.0.113.0/24"}
	for _, h := range []string{"api.exemple.fr", "API.exemple.fr.", "ftp.partenaire.fr", "203.0.113.7"} {
		if !matchHost(list, h) {
			t.Errorf("%s refusé", h)
		}
	}
	for _, h := range []string{"exemple.fr", "partenaire.fr", "evilpartenaire.fr", "10.0.0.1", ""} {
		if matchHost(list, h) {
			t.Errorf("%s accepté", h)
		}
	}
	// Pipeline d'un propriétaire : aucune restriction, sans même lire la liste.
	r := &Runner{}
	if err := r.teamGuardDSN(t.Context(), &Pipeline{}, "postgres://u@10.0.0.1/db"); err != nil {
		t.Fatal(err)
	}
}
