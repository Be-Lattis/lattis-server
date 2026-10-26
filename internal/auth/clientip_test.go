package auth_test

import (
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/Be-Lattis/lattis-server/internal/auth"
)

// Adresse du client derrière un ou plusieurs mandataires (audit de sécurité
// du 26/10, M7) : X-Forwarded-For lu depuis la fin, mandataires de
// confiance sautés.
func TestClientIP(t *testing.T) {
	npm := []netip.Prefix{netip.MustParsePrefix("10.42.0.0/16"), netip.MustParsePrefix("192.168.1.10/32")}
	for _, c := range []struct {
		name    string
		xff     []string
		trust   bool
		trusted []netip.Prefix
		want    string
	}{
		{"sans confiance", []string{"1.2.3.4"}, false, nil, "192.0.2.1"},
		{"un mandataire", []string{"6.6.6.6, 1.2.3.4"}, true, nil, "1.2.3.4"},
		{"deux mandataires sans liste", []string{"1.2.3.4, 192.168.1.10"}, true, nil, "192.168.1.10"},
		{"deux mandataires", []string{"6.6.6.6, 1.2.3.4, 192.168.1.10"}, true, npm, "1.2.3.4"},
		{"en-têtes séparés", []string{"6.6.6.6", "1.2.3.4", "10.42.3.4"}, true, npm, "1.2.3.4"},
		{"tout de confiance", []string{"10.42.0.9, 192.168.1.10"}, true, npm, "10.42.0.9"},
		{"IPv6", []string{"2001:db8::1, 10.42.0.9"}, true, npm, "2001:db8::1"},
		{"sans en-tête", nil, true, npm, "192.0.2.1"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		for _, v := range c.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := auth.ClientIP(r, c.trust, c.trusted...); got != c.want {
			t.Errorf("%s : %q, attendu %q", c.name, got, c.want)
		}
	}
}
