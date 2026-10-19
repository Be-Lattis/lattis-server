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
