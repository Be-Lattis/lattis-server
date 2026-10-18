// Package tokens gère les clés de signature de lattisd et les jetons JWT
// (EdDSA / Ed25519) qu'il émet : jetons d'accès pour PostgreSQL, jetons
// d'identité pour les clients, et jetons internes (état de connexion).
package tokens

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
)

// Key est une clé Ed25519 ; Private est nil pour une clé retirée, encore
// acceptée en vérification le temps que ses jetons expirent.
type Key struct {
	ID      string
	Public  ed25519.PublicKey
	Private ed25519.PrivateKey
}

// KeySet : la première clé privée signe, toutes vérifient.
type KeySet struct {
	keys   []Key
	signer *Key
}

// KeyID dérive l'identifiant (kid) d'une clé publique.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return base64.RawURLEncoding.EncodeToString(sum[:12])
}

// LoadKeyFile lit un fichier PEM : blocs PRIVATE KEY (PKCS#8 Ed25519) et,
// pour les clés retirées, PUBLIC KEY.
func LoadKeyFile(path string) (*KeySet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseKeys(data)
}

// ParseKeys lit des blocs PEM.
func ParseKeys(data []byte) (*KeySet, error) {
	ks := &KeySet{}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		switch block.Type {
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("clé privée : %w", err)
			}
			priv, ok := k.(ed25519.PrivateKey)
			if !ok {
				return nil, errors.New("clé privée : Ed25519 attendue")
			}
			pub := priv.Public().(ed25519.PublicKey)
			ks.keys = append(ks.keys, Key{ID: KeyID(pub), Public: pub, Private: priv})
		case "PUBLIC KEY":
			k, err := x509.ParsePKIXPublicKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("clé publique : %w", err)
			}
			pub, ok := k.(ed25519.PublicKey)
			if !ok {
				return nil, errors.New("clé publique : Ed25519 attendue")
			}
			ks.keys = append(ks.keys, Key{ID: KeyID(pub), Public: pub})
		}
	}
	for i := range ks.keys {
		if ks.keys[i].Private != nil {
			ks.signer = &ks.keys[i]
			break
		}
	}
	if ks.signer == nil {
		return nil, errors.New("aucune clé privée de signature")
	}
	return ks, nil
}

// GenerateKeyPEM crée une clé Ed25519 au format PEM PKCS#8.
func GenerateKeyPEM() ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// Signer est la clé qui signe.
func (ks *KeySet) Signer() Key { return *ks.signer }

// Lookup cherche une clé par kid.
func (ks *KeySet) Lookup(kid string) (Key, bool) {
	for _, k := range ks.keys {
		if k.ID == kid {
			return k, true
		}
	}
	return Key{}, false
}

// PublicPEM renvoie toutes les clés publiques en PEM : c'est le fichier lu
// par le module lattis_oauth de PostgreSQL.
func (ks *KeySet) PublicPEM() ([]byte, error) {
	var out []byte
	for _, k := range ks.keys {
		der, err := x509.MarshalPKIXPublicKey(k.Public)
		if err != nil {
			return nil, err
		}
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})...)
	}
	return out, nil
}

// JWK est une clé publique au format JSON Web Key.
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

// JWKS renvoie le jeu de clés publié sur /jwks.json.
func (ks *KeySet) JWKS() map[string][]JWK {
	out := make([]JWK, 0, len(ks.keys))
	for _, k := range ks.keys {
		out = append(out, JWK{Kty: "OKP", Crv: "Ed25519", X: base64.RawURLEncoding.EncodeToString(k.Public),
			Kid: k.ID, Alg: "EdDSA", Use: "sig"})
	}
	return map[string][]JWK{"keys": out}
}
