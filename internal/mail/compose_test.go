package mail

import (
	"strings"
	"testing"
)

func TestCompose(t *testing.T) {
	b := string(compose("Lattis <lattis@acme.fr>", "lattis@acme.fr", Message{To: "a@b.fr", Subject: "Invitation « Été »", Body: "Bonjour,\nligne é"}))
	for _, want := range []string{"From: Lattis <lattis@acme.fr>\r\n", "To: a@b.fr\r\n", "Subject: =?utf-8?q?", "Content-Transfer-Encoding: quoted-printable", "@acme.fr>\r\n", "ligne =C3=A9"} {
		if !strings.Contains(b, want) {
			t.Fatalf("message sans %q :\n%s", want, b)
		}
	}
}
