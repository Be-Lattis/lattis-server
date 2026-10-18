package e2e

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestComposeWeb vérifie l'édition web lancée par Docker Compose avec le vrai
// lattis-engine : connexion par la mire, moteur démarré pour l'utilisateur,
// frontend servi sous /app/, point des jetons fermé depuis l'extérieur.
//
// Variables : LATTIS_E2E_WEB_URL (lattisd en édition web) et LATTIS_E2E_OWNER,
// dont le mot de passe a été choisi par TestComposeLogin.
func TestComposeWeb(t *testing.T) {
	base, owner := os.Getenv("LATTIS_E2E_WEB_URL"), os.Getenv("LATTIS_E2E_OWNER")
	if base == "" || owner == "" {
		t.Skip("LATTIS_E2E_WEB_URL et LATTIS_E2E_OWNER requis")
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	read := func(resp *http.Response, err error) (*http.Response, string) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}
	field := func(body, name string) string {
		m := regexp.MustCompile(`name="` + name + `" value="([^"]*)"`).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("champ %s absent", name)
		}
		return m[1]
	}

	resp, body := read(c.Get(base + "/app/"))
	if !strings.Contains(body, `type="password"`) {
		t.Fatalf("mire de connexion attendue : %d %s", resp.StatusCode, resp.Request.URL)
	}
	resp, body = read(c.PostForm(base+resp.Request.URL.RequestURI(), url.Values{"req": {field(body, "req")},
		"csrf": {field(body, "csrf")}, "email": {owner}, "password": {"mot de passe de démonstration"}}))
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/app/" || !strings.Contains(body, "<script") {
		t.Fatalf("application web : %d %s\n%.400s", resp.StatusCode, resp.Request.URL, body)
	}
	t.Logf("application servie par le moteur (%d octets)", len(body))
	if resp, _ := read(c.Get(base + "/engine/token")); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("point des jetons joignable de l'extérieur : %d", resp.StatusCode)
	}
}
