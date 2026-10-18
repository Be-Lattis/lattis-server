package upstream

import (
	"errors"
	"testing"

	"github.com/Be-Lattis/lattis-server/internal/store"
)

func TestAdmit(t *testing.T) {
	google := store.Provider{ID: "g", Kind: "google"}
	workspace := store.Provider{ID: "w", Kind: "google_workspace"}
	ms := store.Provider{ID: "m", Kind: "microsoft"}
	oidcP := store.Provider{ID: "o", Kind: "oidc", ClaimName: "groups_ok", ClaimValue: "true"}
	cases := []struct {
		name     string
		p        store.Provider
		tenants  []store.Tenant
		claims   Claims
		ok       bool
		verified bool
		role     string
	}{
		{"google adresse listée", google, []store.Tenant{{Kind: "email", Value: "a@gmail.com", DefaultRole: "editor"}},
			Claims{Subject: "1", Email: "a@gmail.com", EmailVerified: true}, true, true, "editor"},
		{"google non vérifiée", google, []store.Tenant{{Kind: "any"}},
			Claims{Subject: "1", Email: "a@gmail.com", EmailVerified: false}, false, false, ""},
		{"google any", google, []store.Tenant{{Kind: "any"}},
			Claims{Subject: "1", Email: "b@gmail.com", EmailVerified: "true"}, true, true, ""},
		{"workspace bon domaine", workspace, []store.Tenant{{Kind: "domain", Value: "acme.fr"}},
			Claims{Subject: "1", Email: "a@acme.fr", EmailVerified: true, HostedDomain: "acme.fr"}, true, true, ""},
		{"workspace sans hd", workspace, []store.Tenant{{Kind: "domain", Value: "acme.fr"}},
			Claims{Subject: "1", Email: "a@acme.fr", EmailVerified: true}, false, true, ""},
		{"workspace any sans hd", workspace, []store.Tenant{{Kind: "any"}},
			Claims{Subject: "1", Email: "a@gmail.com", EmailVerified: true}, false, true, ""},
		{"microsoft bon tid", ms, []store.Tenant{{Kind: "tid", Value: "T-1"}},
			Claims{Subject: "1", Email: "a@acme.fr", TenantID: "t-1"}, true, true, ""},
		{"microsoft autre tid", ms, []store.Tenant{{Kind: "tid", Value: "T-1"}},
			Claims{Subject: "1", Email: "a@acme.fr", TenantID: "t-2"}, false, false, ""},
		{"aucun tenant", google, nil, Claims{Subject: "1", Email: "a@gmail.com", EmailVerified: true}, false, true, ""},
		{"oidc filtre ok", oidcP, []store.Tenant{{Kind: "any"}},
			Claims{Subject: "1", Email: "a@x.fr", all: map[string]any{"groups_ok": true}}, true, false, ""},
		{"oidc filtre refusé", oidcP, []store.Tenant{{Kind: "any"}},
			Claims{Subject: "1", Email: "a@x.fr", all: map[string]any{"groups_ok": false}}, false, false, ""},
	}
	for _, c := range cases {
		id, err := Admit(c.p, c.tenants, &c.claims)
		if (err == nil) != c.ok {
			t.Errorf("%s : err = %v", c.name, err)
			continue
		}
		if err != nil && !errors.Is(err, ErrTenant) {
			t.Errorf("%s : erreur inattendue %v", c.name, err)
		}
		if c.ok && (id.EmailVerified != c.verified || id.TenantDefaultRole != c.role) {
			t.Errorf("%s : %+v", c.name, id)
		}
	}
}
