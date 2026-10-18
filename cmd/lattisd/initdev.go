package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/Be-Lattis/lattis-server/internal/tokens"
)

// initDev prépare un répertoire de secrets pour l'essai local (Docker
// Compose) : clé de signature, clés publiques pour lattis_oauth et
// certificat TLS autosigné pour PostgreSQL. Ne rien écraser.
//
//	DIR/keys/signing.pem   clé privée de lattisd
//	DIR/keys/public.pem    clés publiques lues par lattis_oauth
//	DIR/tls/server.crt|key certificat de PostgreSQL
func initDev(dir string, hosts []string) error {
	keys := filepath.Join(dir, "keys")
	tlsDir := filepath.Join(dir, "tls")
	for _, d := range []string{keys, tlsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	signing := filepath.Join(keys, "signing.pem")
	if _, err := os.Stat(signing); errors.Is(err, os.ErrNotExist) {
		if err := keygen(signing); err != nil {
			return err
		}
		fmt.Println("clé de signature créée :", signing)
	}
	// Init lancé en root (volume Docker neuf) : la clé revient à l'utilisateur de lattisd.
	if owner := os.Getenv("LATTIS_KEY_OWNER"); owner != "" {
		var uid, gid int
		if _, err := fmt.Sscanf(owner, "%d:%d", &uid, &gid); err != nil {
			return fmt.Errorf("LATTIS_KEY_OWNER : uid:gid attendu")
		}
		if err := os.Chown(signing, uid, gid); err != nil {
			return err
		}
	}
	ks, err := tokens.LoadKeyFile(signing)
	if err != nil {
		return err
	}
	pub, err := ks.PublicPEM()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(keys, "public.pem"), pub, 0o644); err != nil {
		return err
	}
	crt := filepath.Join(tlsDir, "server.crt")
	if _, err := os.Stat(crt); err == nil {
		return nil
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: hosts[0], Organization: []string{"Serveur Lattis (essai)"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &priv.PublicKey, priv)
	if err != nil {
		return err
	}
	kder, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	if err := os.WriteFile(crt, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	// Lisible par le conteneur PostgreSQL, qui le recopie en 0600 dans PGDATA.
	if err := os.WriteFile(filepath.Join(tlsDir, "server.key"),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}), 0o644); err != nil {
		return err
	}
	fmt.Println("certificat TLS autosigné créé :", crt)
	return nil
}
