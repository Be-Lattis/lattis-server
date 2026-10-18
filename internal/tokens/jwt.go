package tokens

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Types de jetons (en-tête typ).
const (
	TypeAccess = "at+jwt" // jeton d'accès (RFC 9068), présenté à PostgreSQL
	TypeID     = "JWT"    // jeton d'identité OIDC
	TypeState  = "lattis-state+jwt"
)

// AudiencePostgres est l'audience des jetons d'accès, vérifiée par lattis_oauth.
const AudiencePostgres = "postgres"

// Leeway est la tolérance d'horloge à la vérification.
const Leeway = 60 * time.Second

// Claims sont les revendications d'un jeton. Les champs inutilisés sont omis.
type Claims struct {
	Issuer    string   `json:"iss,omitempty"`
	Subject   string   `json:"sub,omitempty"`
	Audience  Audience `json:"aud,omitempty"`
	ExpiresAt int64    `json:"exp,omitempty"`
	NotBefore int64    `json:"nbf,omitempty"`
	IssuedAt  int64    `json:"iat,omitempty"`
	ID        string   `json:"jti,omitempty"`
	ClientID  string   `json:"client_id,omitempty"`
	Scope     string   `json:"scope,omitempty"`
	PGRole    string   `json:"pg_role,omitempty"`
	Email     string   `json:"email,omitempty"`
	Name      string   `json:"name,omitempty"`
	Nonce     string   `json:"nonce,omitempty"`
	AuthTime  int64    `json:"auth_time,omitempty"`
	// Data transporte les données d'un jeton interne.
	Data json.RawMessage `json:"data,omitempty"`
}

// Audience accepte une chaîne ou une liste (RFC 7519 §4.1.3).
type Audience []string

func (a Audience) MarshalJSON() ([]byte, error) {
	if len(a) == 1 {
		return json.Marshal(a[0])
	}
	return json.Marshal([]string(a))
}

func (a *Audience) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*a = Audience{s}
		return nil
	}
	var l []string
	if err := json.Unmarshal(b, &l); err != nil {
		return err
	}
	*a = l
	return nil
}

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

var b64 = base64.RawURLEncoding

// NewID renvoie un identifiant aléatoire de 128 bits en base64url.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return b64.EncodeToString(b[:])
}

// Sign signe des revendications avec la clé active.
func (ks *KeySet) Sign(typ string, c Claims) (string, error) {
	k := ks.Signer()
	h, err := json.Marshal(header{Alg: "EdDSA", Typ: typ, Kid: k.ID})
	if err != nil {
		return "", err
	}
	p, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	input := b64.EncodeToString(h) + "." + b64.EncodeToString(p)
	sig := ed25519.Sign(k.Private, []byte(input))
	return input + "." + b64.EncodeToString(sig), nil
}

// Erreurs de vérification.
var (
	ErrMalformed = errors.New("jeton mal formé")
	ErrSignature = errors.New("signature invalide")
	ErrExpired   = errors.New("jeton expiré")
	ErrClaims    = errors.New("revendications invalides")
)

// Verify contrôle la signature, le type, l'émetteur, l'audience et les dates.
func (ks *KeySet) Verify(token, typ, issuer, audience string, now time.Time) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrMalformed
	}
	hb, err := b64.DecodeString(parts[0])
	if err != nil {
		return nil, ErrMalformed
	}
	var h header
	if err := json.Unmarshal(hb, &h); err != nil {
		return nil, ErrMalformed
	}
	if h.Alg != "EdDSA" || h.Typ != typ {
		return nil, ErrMalformed
	}
	k, ok := ks.Lookup(h.Kid)
	if !ok {
		return nil, ErrSignature
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return nil, ErrMalformed
	}
	if !ed25519.Verify(k.Public, []byte(parts[0]+"."+parts[1]), sig) {
		return nil, ErrSignature
	}
	pb, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, ErrMalformed
	}
	var c Claims
	if err := json.Unmarshal(pb, &c); err != nil {
		return nil, ErrMalformed
	}
	if c.Issuer != issuer {
		return nil, fmt.Errorf("%w : émetteur", ErrClaims)
	}
	if audience != "" && !slices.Contains(c.Audience, audience) {
		return nil, fmt.Errorf("%w : audience", ErrClaims)
	}
	if c.ExpiresAt == 0 || now.After(time.Unix(c.ExpiresAt, 0).Add(Leeway)) {
		return nil, ErrExpired
	}
	if c.NotBefore != 0 && now.Add(Leeway).Before(time.Unix(c.NotBefore, 0)) {
		return nil, fmt.Errorf("%w : pas encore valide", ErrClaims)
	}
	return &c, nil
}
