package config

import "testing"

func TestParse(t *testing.T) {
	env := map[string]string{
		"LATTIS_PUBLIC_URL": "https://lattis.acme.fr/", "LATTIS_PG_RW_URL": "postgres://x",
		"LATTIS_SIGNING_KEY_FILE": "/k", "LATTIS_PG_PUBLIC_RW": "pg.acme.fr:5433",
		"LATTIS_PG_PUBLIC_RO": "ro1.acme.fr, [::1]:6000",
	}
	c, err := parse(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.Issuer() != "https://lattis.acme.fr" || c.PGPublicRW.Port != 5433 || len(c.PGPublicRO) != 2 ||
		c.PGPublicRO[0].Port != 5432 || c.PGPublicRO[1].Host != "::1" || c.Edition != EditionAdmin {
		t.Fatalf("%+v", c)
	}
	if _, err := parse(func(string) string { return "" }); err == nil {
		t.Fatal("configuration vide acceptée")
	}
}
