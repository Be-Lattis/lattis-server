package admin_test

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

// Gestionnaire d'équipe : administration réduite à ses équipes, aux
// habilitations de leurs membres et aux données dont il a les droits.
func TestTeamManagerConsole(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	ownerEmail := testpg.Email("admin")
	token, err := e.db.Store.Bootstrap(ctx, ownerEmail)
	if err != nil {
		t.Fatal(err)
	}
	e.account(token, pw)
	b := e.browser()
	_, body := b.login(ownerEmail, pw, "/admin/teams")
	csrf := map[*browser]string{b: field(body, "csrf")}
	post := func(br *browser, action string, v url.Values) string {
		t.Helper()
		v.Set("csrf", csrf[br])
		if v.Get("back") == "" {
			v.Set("back", "/admin/teams")
		}
		_, body := br.post("/admin/do/"+action, v)
		return body
	}
	newAccount := func(prefix string) string {
		t.Helper()
		email := testpg.Email(prefix)
		body := post(b, "add_user", url.Values{"back": {"/admin/users"}, "email": {email}})
		m := regexp.MustCompile(`/setup\?token=([A-Za-z0-9_-]+)`).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("lien de %s absent", prefix)
		}
		e.account(m[1], pw)
		return email
	}
	mgr, alice := newAccount("gestionnaire"), newAccount("alice")
	teamID := func(name string) string {
		t.Helper()
		var id string
		if err := e.db.Super.QueryRow(ctx, `SELECT id::text FROM lattis.teams WHERE name = $1`, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	post(b, "create_team", url.Values{"name": {"Ventes"}})
	post(b, "create_team", url.Values{"name": {"Achats"}})
	ventes, achats := teamID("Ventes"), teamID("Achats")
	post(b, "add_team_member", url.Values{"team_id": {ventes}, "email": {alice}})
	if body := post(b, "team_manager_on", url.Values{"team_id": {ventes}, "email": {mgr}}); !strings.Contains(body, "gère maintenant cette équipe") {
		t.Fatalf("nomination : %s", body)
	}
	if _, body := b.get("/admin/users"); !strings.Contains(body, "gestionnaire d'équipe</span>") {
		t.Fatal("rôle de gestionnaire absent de la page Utilisateurs")
	}

	// Le gestionnaire arrive sur ses équipes, sans les écrans du serveur.
	mb := e.browser()
	resp, body := mb.login(mgr, pw, "/admin/")
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/admin/teams" {
		t.Fatalf("arrivée du gestionnaire : %d %s", resp.StatusCode, resp.Request.URL)
	}
	csrf[mb] = field(body, "csrf")
	if !strings.Contains(body, "Ventes") || strings.Contains(body, "Achats") || strings.Contains(body, `href="/admin/server"`) ||
		strings.Contains(body, "Aucune (premier niveau)") {
		t.Fatalf("équipes du gestionnaire : %s", body)
	}
	for _, p := range []string{"/admin/server", "/admin/providers", "/admin/status", "/admin/monitor", "/admin/audit"} {
		if resp, body := mb.get(p); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "réservé aux propriétaires du serveur") {
			t.Fatalf("%s : %d", p, resp.StatusCode)
		}
	}
	for _, p := range []string{"/admin/users", "/admin/teams", "/admin/workbooks", "/admin/data", "/admin/forms"} {
		if resp, body := mb.get(p); resp.StatusCode != http.StatusOK || strings.Contains(body, `class="error`) {
			t.Fatalf("%s : %d %s", p, resp.StatusCode, body)
		}
	}
	_, body = mb.get("/admin/users")
	if !strings.Contains(body, alice) || strings.Contains(body, ownerEmail) || strings.Contains(body, "Ajouter un utilisateur") ||
		strings.Contains(body, "Invitations en attente") || strings.Contains(body, "Désactiver") {
		t.Fatalf("utilisateurs du gestionnaire : %s", body)
	}
	if body := post(mb, "data_publisher_on", url.Values{"back": {"/admin/users"}, "id": {userID(t, e, alice)}}); !strings.Contains(body, "peut maintenant partager") {
		t.Fatalf("habilitation : %s", body)
	}
	if body := post(mb, "data_publisher_on", url.Values{"back": {"/admin/users"}, "id": {userID(t, e, ownerEmail)}}); !strings.Contains(body, "gestionnaires des équipes de cette personne") {
		t.Fatalf("habilitation d'un propriétaire : %s", body)
	}

	// Actions du serveur refusées avant même d'appeler PostgreSQL.
	for _, a := range []string{"server_settings", "add_user", "disable_user", "reset_password", "forms_public", "add_provider"} {
		if body := post(mb, a, url.Values{"confirm": {"on"}}); !strings.Contains(body, "Réservé aux propriétaires du serveur.") {
			t.Fatalf("%s : %s", a, body)
		}
	}
	// Équipes : une sous-équipe oui, une équipe de premier niveau non.
	if body := post(mb, "create_team", url.Values{"name": {"Nord"}, "parent_id": {ventes}}); !strings.Contains(body, "Équipe créée.") || !strings.Contains(body, "Nord") {
		t.Fatalf("sous-équipe : %s", body)
	}
	if body := post(mb, "create_team", url.Values{"name": {"Libre"}}); !strings.Contains(body, "Réservé aux propriétaires du serveur") {
		t.Fatalf("équipe de premier niveau : %s", body)
	}
	if body := post(mb, "team_manager_on", url.Values{"team_id": {achats}, "email": {alice}}); !strings.Contains(body, "gestionnaires de cette équipe") {
		t.Fatalf("nomination hors périmètre : %s", body)
	}
	if body := post(mb, "add_team_member", url.Values{"team_id": {teamID("Nord")}, "email": {alice}}); !strings.Contains(body, "ajouté à l&#39;équipe") {
		t.Fatalf("membre d'une sous-équipe : %s", body)
	}
	if body := post(mb, "drop_team", url.Values{"id": {teamID("Nord")}, "confirm": {"on"}}); !strings.Contains(body, "Équipe supprimée.") {
		t.Fatalf("suppression d'une sous-équipe : %s", body)
	}

	// Retiré par le propriétaire : plus d'administration.
	post(b, "team_manager_off", url.Values{"team_id": {ventes}, "user_id": {userID(t, e, mgr)}})
	if resp, body := mb.get("/admin/teams"); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "gestionnaires d&#39;équipe") {
		t.Fatalf("après retrait : %d", resp.StatusCode)
	}
}
