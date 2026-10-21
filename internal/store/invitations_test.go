package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

// Recherche de personnes et invitations : comptes actifs seulement ;
// invitation à un classeur (droits immédiats, ou préenregistrés et appliqués
// à l'inscription) ; invitation à une équipe ; liste et annulation.
func TestPeopleAndInvitations(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var google, team string
	must(db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.add_provider('google', 'Google', NULL, 'cid', NULL, NULL)::text`).Scan(&google)
	}))
	enroll := func(n string) store.Enrollment {
		t.Helper()
		email := testpg.Email(n)
		must(db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT lattis.invite($1, NULL, '{}')`, email)
			return err
		}))
		e, err := db.Store.Enroll(ctx, store.Identity{ProviderID: google, Subject: n, Email: email, EmailVerified: true, DisplayName: strings.ToUpper(n[:1]) + n[1:]})
		must(err)
		return e
	}
	owner, colleague := enroll("alice"), enroll("bruno")
	if colleague.UserID == "" || owner.UserID == "" {
		t.Fatalf("inscriptions : %+v %+v", owner, colleague)
	}
	_, err := db.Super.Exec(ctx, `UPDATE lattis.users SET display_name = 'Bruno Martin' WHERE id = $1`, colleague.UserID)
	must(err)
	as := func(u store.Enrollment, q string, dst any, args ...any) error {
		return db.AsUser(t, u.PGRole, func(tx pgx.Tx) error {
			if dst == nil {
				_, err := tx.Exec(ctx, q, args...)
				return err
			}
			return tx.QueryRow(ctx, q, args...).Scan(dst)
		})
	}
	var wb string
	must(db.Super.QueryRow(ctx, `SELECT 1`).Scan(new(int)))
	_, err = db.Super.Exec(ctx, `GRANT lattis_server_owners TO `+pgx.Identifier{owner.PGRole}.Sanitize())
	must(err)
	must(as(owner, `SELECT lattis.create_workbook('Ventes', 'collaborative')::text`, &wb))
	must(as(owner, `SELECT lattis.finish_publish($1::uuid)`, new(int), wb))

	// Recherche par nom ou adresse, comptes actifs seulement.
	var found []map[string]any
	must(as(colleague, `SELECT lattis.search_people('martin', 10)`, &found))
	if len(found) != 1 || found[0]["id"] != colleague.UserID {
		t.Fatalf("recherche par nom : %v", found)
	}
	must(as(colleague, `SELECT lattis.search_people($1, 10)`, &found, strings.Split(owner.Email, "@")[0]))
	if len(found) != 1 || found[0]["email"] != owner.Email {
		t.Fatalf("recherche par adresse : %v", found)
	}
	must(as(colleague, `SELECT lattis.search_people('a', 10)`, &found))
	if len(found) != 0 {
		t.Fatalf("recherche d'un caractère : %v", found)
	}

	// Compte existant : droits immédiats.
	var res map[string]any
	must(as(owner, `SELECT lattis.invite_person($1, $2::uuid, ARRAY['editor'])`, &res, colleague.Email, wb))
	if res["status"] != "granted" || res["mailed"] != false || res["link_path"] != "/w/"+wb {
		t.Fatalf("compte existant : %v", res)
	}
	// Inconnue : invitation en attente, droits préenregistrés, visible et
	// absente de la recherche.
	newcomer := testpg.Email("chloe")
	must(as(owner, `SELECT lattis.invite_person($1, $2::uuid, ARRAY['reader'])`, &res, newcomer, wb))
	if res["status"] != "invited" || res["mailed"] != false {
		t.Fatalf("invitation : %v", res)
	}
	var pending []map[string]any
	must(as(owner, `SELECT lattis.workbook_invitations($1::uuid)`, &pending, wb))
	if len(pending) != 1 || pending[0]["email"] != newcomer {
		t.Fatalf("invitations en attente : %v", pending)
	}
	must(as(colleague, `SELECT lattis.search_people('chloe', 10)`, &found))
	if len(found) != 0 {
		t.Fatalf("compte d'invitation trouvé : %v", found)
	}
	// Invitation à une équipe.
	must(as(owner, `SELECT lattis.create_team('Commerce', NULL)::text`, &team))
	var tres map[string]any
	must(as(owner, `SELECT lattis.invite_to_team($1, $2::uuid)`, &tres, newcomer, team))
	if tres["status"] != "invited" || tres["link_path"] != "/" {
		t.Fatalf("invitation à l'équipe : %v", tres)
	}
	// Inscription : droits du classeur et équipe appliqués.
	chloe, err := db.Store.Enroll(ctx, store.Identity{ProviderID: google, Subject: "chloe", Email: newcomer, EmailVerified: true})
	must(err)
	if chloe.Status != "ok" {
		t.Fatalf("inscription : %+v", chloe)
	}
	var roles, teams int
	must(db.Super.QueryRow(ctx, `SELECT count(*) FROM lattis.members m JOIN lattis.roles r ON r.id = m.role_id
		WHERE m.principal = $1 AND r.workbook_id = $2 AND r.builtin = 'reader'`, chloe.UserID, wb).Scan(&roles))
	must(db.Super.QueryRow(ctx, `SELECT count(*) FROM lattis.team_members WHERE user_id = $1 AND team_id = $2`, chloe.UserID, team).Scan(&teams))
	if roles != 1 || teams != 1 {
		t.Fatalf("droits préenregistrés : rôle %d, équipe %d", roles, teams)
	}
	// Annulation d'une invitation en attente.
	must(as(owner, `SELECT lattis.invite_person($1, $2::uuid, ARRAY['reader'])`, &res, testpg.Email("david"), wb))
	must(as(owner, `SELECT lattis.revoke_invitation($1::uuid)`, nil, res["invitation_id"]))
	must(as(owner, `SELECT lattis.workbook_invitations($1::uuid)`, &pending, wb))
	if len(pending) != 0 {
		t.Fatalf("invitation annulée encore listée : %v", pending)
	}
}
