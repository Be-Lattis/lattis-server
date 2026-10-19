package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Be-Lattis/lattis-server/internal/store"
)

// TestPreviewPages écrit quelques pages de l'administration, remplies de
// données d'exemple, dans LATTIS_ADMIN_PREVIEW (pour les regarder dans un
// navigateur pendant un travail sur la présentation). Sans la variable :
// rendu seulement, pour vérifier que les gabarits s'exécutent.
func TestPreviewPages(t *testing.T) {
	dir := os.Getenv("LATTIS_ADMIN_PREVIEW")
	web, _ := filepath.Abs("web")
	user := store.User{ID: "u1", Email: "louis@acme.fr"}
	base := view{Server: "Lattis Acme", User: user, CSRF: "x", Version: "0.9.0", Path: "/admin/x"}
	groups := make([]settingGroup, 0, len(settingGroups))
	for _, g := range settingGroups {
		g2 := g
		g2.Settings = append([]setting(nil), g.Settings...)
		for i := range g2.Settings {
			switch g2.Settings[i].Kind {
			case "select":
				g2.Settings[i].Value = g2.Settings[i].Options[0][0]
			case "bool":
				g2.Settings[i].Value = true
			case "duration", "number":
				g2.Settings[i].Value = 30
			default:
				g2.Settings[i].Value = "Lattis Acme"
			}
		}
		groups = append(groups, g2)
	}
	users := []map[string]any{
		{"id": "u1", "email": "louis@acme.fr", "display_name": "Louis Martin", "server_owner": true, "email_verified": true, "provider": "Google", "kind": "oidc", "last_login_at": "2026-10-19T09:12:00Z"},
		{"id": "u2", "email": "claire.dupont@acme.fr", "display_name": "Claire Dupont", "data_publisher": true, "email_verified": true, "provider": "Mot de passe", "kind": "password", "last_login_at": "2026-10-18T16:40:00Z"},
		{"id": "u3", "email": "marc@acme.fr", "email_verified": false, "kind": "password", "link_expires_at": "2026-10-26T10:00:00Z"},
		{"id": "u4", "email": "ancien@acme.fr", "disabled": true, "email_verified": true, "provider": "Microsoft", "kind": "oidc", "last_login_at": "2026-03-02T08:00:00Z"},
	}
	workbooks := []map[string]any{
		{"id": "w1", "title": "Suivi des commandes", "mode": "collaborative", "state": "published", "kind": "workbook", "owners": []any{"louis@acme.fr"}, "members": 12, "size": 4_800_000, "version": 3,
			"last_edit": map[string]any{"email": "claire.dupont@acme.fr", "at": "2026-10-19T08:30:00Z"}, "last_read": map[string]any{"email": "marc@acme.fr", "at": "2026-10-19T09:00:00Z"}},
		{"id": "w2", "title": "Budget 2027", "mode": "frozen", "state": "published", "kind": "workbook", "owners": []any{"claire.dupont@acme.fr"}, "members": 4, "size": 820_000, "version": 1},
		{"id": "w3", "title": "Source : Clients partagés", "mode": "collaborative", "state": "published", "kind": "source", "owners": []any{"claire.dupont@acme.fr"}, "members": 2, "size": 120_000, "version": 1},
	}
	pagesData := map[string]struct {
		title string
		data  any
	}{
		"server":    {"Serveur", groups},
		"users":     {"Utilisateurs", map[string]any{"Users": users, "Creators": true, "Me": "u1", "Q": "", "Invitations": []map[string]any{{"id": "i1", "email": "nouveau@acme.fr", "workbook": "Budget 2027", "roles": []any{"reader"}, "invited_by": "louis@acme.fr", "expires_at": "2026-11-02T00:00:00Z"}}}},
		"workbooks": {"Classeurs", map[string]any{"Workbooks": workbooks}},
	}
	for name, p := range pagesData {
		v := base
		v.Title, v.Page, v.Data = p.title, name, p.data
		if name == "users" {
			v.Flash = "Compte créé pour marc@acme.fr."
		}
		var b bytes.Buffer
		if err := pages[name].ExecuteTemplate(&b, "layout", v); err != nil {
			t.Fatalf("%s : %v", name, err)
		}
		if dir == "" {
			continue
		}
		html := strings.ReplaceAll(b.String(), "/admin/static/", "")
		if err := os.WriteFile(filepath.Join(dir, name+".html"), []byte(html), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if dir != "" {
		for _, f := range []string{"admin.css", "admin.js"} {
			b, _ := os.ReadFile(filepath.Join(web, f))
			_ = os.WriteFile(filepath.Join(dir, f), b, 0o644)
		}
	}
}
