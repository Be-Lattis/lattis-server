package tokens

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	pem, err := GenerateKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	ks, err := ParseKeys(pem)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tok, err := ks.Sign(TypeAccess, Claims{Issuer: "https://l", Audience: Audience{AudiencePostgres}, ExpiresAt: now.Add(time.Minute).Unix(), PGRole: "a@b.c"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := ks.Verify(tok, TypeAccess, "https://l", AudiencePostgres, now)
	if err != nil || c.PGRole != "a@b.c" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := ks.Verify(tok, TypeID, "https://l", AudiencePostgres, now); !errors.Is(err, ErrMalformed) {
		t.Fatalf("type : %v", err)
	}
	if _, err := ks.Verify(tok, TypeAccess, "https://autre", AudiencePostgres, now); !errors.Is(err, ErrClaims) {
		t.Fatalf("émetteur : %v", err)
	}
	if _, err := ks.Verify(tok, TypeAccess, "https://l", AudiencePostgres, now.Add(time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expiration : %v", err)
	}
	parts := strings.Split(tok, ".")
	forged := parts[0] + "." + b64.EncodeToString([]byte(`{"iss":"https://l","aud":"postgres","exp":9999999999,"pg_role":"postgres"}`)) + "." + parts[2]
	if _, err := ks.Verify(forged, TypeAccess, "https://l", AudiencePostgres, now); !errors.Is(err, ErrSignature) {
		t.Fatalf("falsification : %v", err)
	}
	// Clé retirée : encore vérifiable, plus utilisée pour signer.
	pem2, _ := GenerateKeyPEM()
	pub, _ := ks.PublicPEM()
	ks2, err := ParseKeys(append(pem2, pub...))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ks2.Verify(tok, TypeAccess, "https://l", AudiencePostgres, now); err != nil {
		t.Fatalf("clé retirée : %v", err)
	}
	if ks2.Signer().ID == ks.Signer().ID {
		t.Fatal("la nouvelle clé doit signer")
	}
}
