// Package upstream fédère les fournisseurs d'identité réels (Google, Google
// Workspace, Microsoft, Keycloak, OIDC générique) derrière lattisd (§7.1) et
// contrôle les tenants autorisés (§7.2).
package upstream

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/Be-Lattis/lattis-server/internal/store"
)

const microsoftIssuerTemplate = "https://login.microsoftonline.com/{tenantid}/v2.0"

// Client est un fournisseur prêt à l'emploi.
type Client struct {
	Provider store.Provider
	OAuth2   oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// Registry garde les clients découverts, par fournisseur.
type Registry struct {
	SecretsDir string
	// CallbackURL construit l'URI de retour d'un fournisseur.
	CallbackURL func(providerID string) string

	mu      sync.Mutex
	clients map[string]*Client
}

// key change quand la configuration du fournisseur change.
func key(p store.Provider) string {
	return strings.Join([]string{p.ID, p.Kind, p.Issuer, p.ClientID, p.SecretRef, strings.Join(p.Scopes, " ")}, "\x00")
}

// Get renvoie (en le découvrant au besoin) le client d'un fournisseur.
func (r *Registry) Get(ctx context.Context, p store.Provider) (*Client, error) {
	if p.Kind == "password" {
		return nil, errors.New("fournisseur local")
	}
	r.mu.Lock()
	if c, ok := r.clients[p.ID]; ok && key(c.Provider) == key(p) {
		r.mu.Unlock()
		return c, nil
	}
	r.mu.Unlock()

	secret, err := r.secret(p.SecretRef)
	if err != nil {
		return nil, err
	}
	dctx := ctx
	issuer := p.Issuer
	multiTenant := p.Kind == "microsoft" &&
		(strings.Contains(issuer, "/common/") || strings.Contains(issuer, "/organizations/"))
	if multiTenant {
		dctx = oidc.InsecureIssuerURLContext(ctx, microsoftIssuerTemplate)
	}
	op, err := oidc.NewProvider(dctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("découverte de %s : %w", p.Label, err)
	}
	scopes := p.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "email", "profile"}
	}
	c := &Client{
		Provider: p,
		OAuth2: oauth2.Config{
			ClientID: p.ClientID, ClientSecret: secret, Endpoint: op.Endpoint(),
			RedirectURL: r.CallbackURL(p.ID), Scopes: scopes,
		},
		// Microsoft multi-tenant : l'émetteur dépend du tenant ; il est
		// contrôlé à la main contre tid (Claims.check).
		verifier: op.Verifier(&oidc.Config{ClientID: p.ClientID, SkipIssuerCheck: multiTenant}),
	}
	r.mu.Lock()
	if r.clients == nil {
		r.clients = map[string]*Client{}
	}
	r.clients[p.ID] = c
	r.mu.Unlock()
	return c, nil
}

func (r *Registry) secret(ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	if r.SecretsDir == "" {
		return "", fmt.Errorf("secret %q : LATTIS_SECRETS_DIR manquant", ref)
	}
	if strings.ContainsAny(ref, `/\`) || ref == ".." || ref == "." {
		return "", fmt.Errorf("référence de secret invalide : %q", ref)
	}
	b, err := os.ReadFile(filepath.Join(r.SecretsDir, ref))
	if err != nil {
		return "", fmt.Errorf("secret %q : %w", ref, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// Claims sont les revendications utiles d'un jeton d'identité.
type Claims struct {
	Issuer            string `json:"iss"`
	Subject           string `json:"sub"`
	Email             string `json:"email"`
	EmailVerified     any    `json:"email_verified"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	HostedDomain      string `json:"hd"`
	TenantID          string `json:"tid"`
	Nonce             string `json:"nonce"`

	all map[string]any
}

// Verify vérifie le jeton d'identité (signature, émetteur, audience,
// expiration, nonce) et en extrait les revendications.
func (c *Client) Verify(ctx context.Context, rawIDToken, nonce string) (*Claims, error) {
	tok, err := c.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, err
	}
	var cl Claims
	if err := tok.Claims(&cl); err != nil {
		return nil, err
	}
	if err := tok.Claims(&cl.all); err != nil {
		return nil, err
	}
	if cl.Nonce != nonce {
		return nil, errors.New("nonce différent")
	}
	if c.Provider.Kind == "microsoft" {
		if cl.TenantID == "" || cl.Issuer != strings.Replace(microsoftIssuerTemplate, "{tenantid}", cl.TenantID, 1) {
			return nil, errors.New("émetteur Microsoft incohérent avec tid")
		}
	}
	if cl.Email == "" && c.Provider.Kind == "microsoft" {
		cl.Email = cl.PreferredUsername
	}
	cl.Email = strings.ToLower(strings.TrimSpace(cl.Email))
	return &cl, nil
}

func (cl *Claims) emailVerified() bool {
	switch v := cl.EmailVerified.(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

func (cl *Claims) domain() string {
	_, d, _ := strings.Cut(cl.Email, "@")
	return d
}

// ErrTenant : l'utilisateur n'appartient à aucun tenant autorisé.
var ErrTenant = errors.New("tenant non autorisé")

// Admit applique les tenants autorisés du fournisseur. Elle renvoie l'identité
// à inscrire (dont le rôle par défaut du tenant retenu).
func Admit(p store.Provider, tenants []store.Tenant, cl *Claims) (store.Identity, error) {
	id := store.Identity{
		ProviderID: p.ID, Subject: cl.Subject, Email: cl.Email, DisplayName: cl.Name,
		EmailVerified: cl.emailVerified(),
	}
	if cl.Subject == "" || cl.Email == "" {
		return id, errors.New("jeton sans sub ou sans adresse")
	}
	// OIDC générique : filtre facultatif sur une revendication.
	if p.Kind == "oidc" && p.ClaimName != "" && fmt.Sprint(cl.all[p.ClaimName]) != p.ClaimValue {
		return id, ErrTenant
	}
	if p.Kind == "google" && !id.EmailVerified {
		return id, fmt.Errorf("%w : adresse Google non vérifiée", ErrTenant)
	}
	for _, t := range tenants {
		if match(p.Kind, t, cl) {
			id.TenantDefaultRole = t.DefaultRole
			// Microsoft : l'adresse est garantie par le tenant quand celui-ci
			// est désigné explicitement.
			if p.Kind == "microsoft" && t.Kind == "tid" {
				id.EmailVerified = true
			}
			return id, nil
		}
	}
	return id, ErrTenant
}

func match(kind string, t store.Tenant, cl *Claims) bool {
	switch t.Kind {
	case "any":
		return kind != "google_workspace" || cl.HostedDomain != ""
	case "email":
		return cl.emailVerified() && cl.Email == t.Value
	case "domain":
		if kind == "google_workspace" {
			return cl.HostedDomain != "" && strings.EqualFold(cl.HostedDomain, t.Value)
		}
		return cl.emailVerified() && strings.EqualFold(cl.domain(), t.Value)
	case "tid":
		return kind == "microsoft" && cl.TenantID != "" && strings.EqualFold(cl.TenantID, t.Value)
	case "realm":
		return (kind == "keycloak" || kind == "oidc") && strings.TrimSuffix(cl.Issuer, "/") == strings.TrimSuffix(t.Value, "/")
	}
	return false
}
