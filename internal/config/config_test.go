package config

import (
	"runtime"
	"testing"
)

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
	env["LATTIS_WEB_ENGINE_UIDS"] = "200000-200009"
	if _, err := parse(func(k string) string { return env[k] }); err == nil {
		t.Fatal("10 UID pour 50 moteurs acceptés")
	}
	env["LATTIS_WEB_ENGINE_UIDS"] = "200000-200099"
	if c, err := parse(func(k string) string { return env[k] }); runtime.GOOS == "linux" && (err != nil || c.WebEngineUIDs != [2]int{200000, 100}) {
		t.Fatalf("plage d'UID : %v %v", c, err)
	}
	if _, err := parse(func(string) string { return "" }); err == nil {
		t.Fatal("configuration vide acceptée")
	}
}

func TestTrustedProxies(t *testing.T) {
	env := map[string]string{
		"LATTIS_PUBLIC_URL": "https://lattis.acme.fr/", "LATTIS_PG_RW_URL": "postgres://x",
		"LATTIS_SIGNING_KEY_FILE": "/k", "LATTIS_PG_PUBLIC_RW": "pg.acme.fr:5432", "LATTIS_TRUSTED_PROXIES": " 10.42.0.0/16, 192.168.1.10 ,fd00::/8",
	}
	c, err := parse(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if len(c.TrustedProxies) != 3 || c.TrustedProxies[1].String() != "192.168.1.10/32" {
		t.Fatalf("%v", c.TrustedProxies)
	}
	env["LATTIS_TRUSTED_PROXIES"] = "npm.local"
	if _, err := parse(func(k string) string { return env[k] }); err == nil {
		t.Fatal("nom d'hôte accepté")
	}
}
