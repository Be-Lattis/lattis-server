package forms

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Be-Lattis/lattis-server/internal/tokens"
)

func samplePage() *Page {
	min, max := 0.0, 50.0
	return &Page{ID: "f1", Code: "abcdefghjk", State: "open", Access: "members", ServerName: "Lattis Acme",
		Definition: Definition{Lang: "fr",
			Appearance: Appearance{Title: "Relevé des nids 2026", Accent: "teal", Intro: "Une ligne **par nid**. [Guide](https://exemple.fr/guide)\n\n- Photo inutile\n- Position conseillée",
				Thanks: "Merci **beaucoup** !", Another: true, Privacy: "Données gardées 2 ans par l'association. Contact : [nous écrire](https://exemple.fr)."},
			Fields: []Field{
				{Kind: "section", Title: "Où ?", Description: "Le lieu du nid."},
				{Column: "c1", Label: "Commune", Required: true, Widget: "list", Param: "commune"},
				{Column: "c5", Label: "Position", Widget: "position", Help: "Touchez « Ma position » sur place."},
				{Kind: "section", Title: "Quoi ?"},
				{Column: "c2", Label: "Nombre de nids", Required: true, Widget: "number", Min: &min, Max: &max},
				{Column: "c3", Label: "Date d'observation", Default: &Default{Token: "today"}},
				{Column: "c4", Label: "Remarques", Widget: "textarea", Placeholder: "Facultatif"},
				{Column: "c6", Label: "J'accepte que mes données soient conservées", Required: true},
			}},
		Columns: map[string]Column{"c1": {"Commune", "text"}, "c2": {"Nombre", "integer"}, "c3": {"Date", "date"},
			"c4": {"Remarques", "text"}, "c5": {"Position", "text"}, "c6": {"Accord", "boolean"}},
		Lists: map[string][]any{"c1": {"Brest", "Quimper", "Morlaix"}},
	}
}

// Rendu de la page d'exemple ; avec LATTIS_FORM_PREVIEW, écrite dans ce
// dossier pour la regarder dans un navigateur.
func TestPreviewForm(t *testing.T) {
	pem, _ := tokens.GenerateKeyPEM()
	keys, _ := tokens.ParseKeys(pem)
	h := &Handler{Keys: keys}
	p := samplePage()
	r := httptest.NewRequest("GET", "/f/abcdefghjk?commune=Brest", nil)
	views := map[string]view{
		"form": h.form(p, r, nil, nil, nil),
		"erreurs": func() view {
			v := h.form(p, r, nil, nil, map[string]string{"c2": "Nombre attendu."})
			v.Message = "Corrigez les champs signalés."
			return v
		}(),
	}
	tv := views["form"]
	tv.Thanks, tv.Another = p.Definition.Appearance.Thanks, "/f/abcdefghjk"
	dir := os.Getenv("LATTIS_FORM_PREVIEW")
	for name, v := range views {
		var b bytes.Buffer
		if err := page.ExecuteTemplate(&b, "form", v); err != nil {
			t.Fatal(name, err)
		}
		if !strings.Contains(b.String(), `value="Brest"`) || !strings.Contains(b.String(), "<strong>par nid</strong>") {
			t.Fatalf("%s : pré-remplissage ou Markdown absent", name)
		}
		write(t, dir, name, b.String())
	}
	var b bytes.Buffer
	if err := page.ExecuteTemplate(&b, "thanks", tv); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "merci", b.String())
}

func write(t *testing.T, dir, name, html string) {
	if dir == "" {
		return
	}
	html = strings.ReplaceAll(html, "/form-static/", "")
	if err := os.WriteFile(filepath.Join(dir, name+".html"), []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"form.css", "form.js"} {
		b, _ := webFS.ReadFile("web/" + f)
		_ = os.WriteFile(filepath.Join(dir, f), b, 0o644)
	}
}
