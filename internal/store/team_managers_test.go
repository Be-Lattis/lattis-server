package store_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

// Gestionnaire d'équipe : ce qu'il peut faire dans son périmètre (son équipe
// et ses sous-équipes, leurs membres), rien au-dessus ni à côté.
func TestTeamManagers(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	us := openUsers(t, db, "gestionnaire", "alice", "carole", "proprio", "pair", "ordinaire")
	mgr, alice, carole, owner, peer, plain := us[0], us[1], us[2], us[3], us[4], us[5]
	if _, err := db.Super.Exec(ctx, `GRANT lattis_server_owners TO `+pgx.Identifier{owner.PGRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}

	// as exécute une requête sous un rôle ; scan facultatif.
	as := func(role, q string, args ...any) error {
		t.Helper()
		return db.AsUser(t, role, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q, args...)
			return err
		})
	}
	scan := func(role string, dst any, q string, args ...any) {
		t.Helper()
		if err := db.AsUser(t, role, func(tx pgx.Tx) error { return tx.QueryRow(ctx, q, args...).Scan(dst) }); err != nil {
			t.Fatalf("%s : %v", q, err)
		}
	}
	ok := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	denied := func(err error) {
		t.Helper()
		if pgCode(err) != "42501" {
			t.Fatalf("refus attendu : %v", err)
		}
	}

	// Équipes : Ventes › Nord, et Achats à côté.
	var ventes, nord, achats string
	scan(owner.PGRole, &ventes, `SELECT lattis.create_team('Ventes')::text`)
	scan(owner.PGRole, &nord, `SELECT lattis.create_team('Nord', $1::uuid)::text`, ventes)
	scan(owner.PGRole, &achats, `SELECT lattis.create_team('Achats')::text`)
	ok(as(owner.PGRole, `SELECT lattis.add_team_member($1::uuid, $2::uuid)`, nord, alice.UserID))
	ok(as(owner.PGRole, `SELECT lattis.add_team_member($1::uuid, $2::uuid)`, achats, carole.UserID))
	ok(as(owner.PGRole, `SELECT lattis.add_team_member($1::uuid, $2::uuid)`, ventes, owner.UserID))

	var access *string
	scan(mgr.PGRole, &access, `SELECT lattis.admin_access()`)
	if access != nil {
		t.Fatalf("accès avant nomination : %v", *access)
	}
	// Seul qui gère l'équipe nomme ses gestionnaires.
	denied(as(mgr.PGRole, `SELECT lattis.set_team_manager($1::uuid, $2::uuid, true)`, ventes, mgr.UserID))
	ok(as(owner.PGRole, `SELECT lattis.set_team_manager($1::uuid, $2::uuid, true)`, ventes, mgr.UserID))
	scan(mgr.PGRole, &access, `SELECT lattis.admin_access()`)
	if access == nil || *access != "team_manager" {
		t.Fatalf("accès du gestionnaire : %v", access)
	}

	// Équipes : sous-équipes oui, premier niveau non ; rien sur Achats.
	denied(as(mgr.PGRole, `SELECT lattis.create_team('Libre')`))
	denied(as(mgr.PGRole, `SELECT lattis.create_team('Libre', NULL::uuid)`))
	var sud string
	scan(mgr.PGRole, &sud, `SELECT lattis.create_team('Sud', $1::uuid)::text`, ventes)
	ok(as(mgr.PGRole, `SELECT lattis.rename_team($1::uuid, 'Nord-Est')`, nord))
	denied(as(mgr.PGRole, `SELECT lattis.rename_team($1::uuid, 'Achats 2')`, achats))
	denied(as(mgr.PGRole, `SELECT lattis.add_team_member($1::uuid, $2::uuid)`, achats, alice.UserID))
	ok(as(mgr.PGRole, `SELECT lattis.add_team_member($1::uuid, $2::uuid)`, sud, carole.UserID))
	ok(as(mgr.PGRole, `SELECT lattis.remove_team_member($1::uuid, $2::uuid)`, sud, carole.UserID))
	// Déplacer : dans le périmètre seulement, jamais l'équipe confiée.
	ok(as(mgr.PGRole, `SELECT lattis.set_team_parent($1::uuid, $2::uuid)`, sud, nord))
	denied(as(mgr.PGRole, `SELECT lattis.set_team_parent($1::uuid, $2::uuid)`, sud, achats))
	denied(as(mgr.PGRole, `SELECT lattis.set_team_parent($1::uuid, NULL)`, sud))
	denied(as(mgr.PGRole, `SELECT lattis.set_team_parent($1::uuid, $2::uuid)`, ventes, achats))
	denied(as(mgr.PGRole, `SELECT lattis.drop_team($1::uuid)`, ventes))
	denied(as(mgr.PGRole, `SELECT lattis.drop_team($1::uuid)`, achats))
	ok(as(mgr.PGRole, `SELECT lattis.drop_team($1::uuid)`, sud))

	// Liste : membres et gestionnaires de ses équipes seulement.
	var teams []map[string]any
	scan(mgr.PGRole, &teams, `SELECT lattis.list_teams()`)
	for _, tm := range teams {
		want := tm["id"] == ventes || tm["id"] == nord
		if tm["manageable"] != want || (tm["members"] != nil) != want || (tm["managers"] != nil) != want {
			t.Fatalf("équipe %v : %v", tm["name"], tm)
		}
		if tm["movable"] != (tm["id"] == nord) {
			t.Fatalf("déplaçable %v : %v", tm["name"], tm["movable"])
		}
	}

	// Habilitations : membres du périmètre, sauf lui-même et les propriétaires.
	ok(as(mgr.PGRole, `SELECT lattis.set_workbook_creator($1::uuid, true)`, alice.UserID))
	ok(as(mgr.PGRole, `SELECT lattis.set_data_publisher($1::uuid, true)`, alice.UserID))
	ok(as(mgr.PGRole, `SELECT lattis.set_data_publisher($1::uuid, false)`, alice.UserID))
	denied(as(mgr.PGRole, `SELECT lattis.set_workbook_creator($1::uuid, true)`, carole.UserID))
	denied(as(mgr.PGRole, `SELECT lattis.set_workbook_creator($1::uuid, true)`, mgr.UserID))
	denied(as(mgr.PGRole, `SELECT lattis.set_data_publisher($1::uuid, false)`, owner.UserID))
	denied(as(mgr.PGRole, `SELECT lattis.invite($1, NULL, '{server_owner}')`, alice.Email))
	denied(as(mgr.PGRole, `SELECT lattis.disable_user($1::uuid)`, alice.UserID))
	denied(as(mgr.PGRole, `SELECT lattis.reset_password($1::uuid)`, alice.UserID))
	denied(as(mgr.PGRole, `SELECT lattis.set_config('display_name', '"Pris"')`))

	// Gestionnaires : jusqu'à son propre rôle ; sur l'équipe confiée, il ne
	// retire que lui-même.
	ok(as(mgr.PGRole, `SELECT lattis.set_team_manager($1::uuid, $2::uuid, true)`, nord, alice.UserID))
	ok(as(mgr.PGRole, `SELECT lattis.set_team_manager($1::uuid, $2::uuid, true)`, ventes, peer.UserID))
	denied(as(mgr.PGRole, `SELECT lattis.set_team_manager($1::uuid, $2::uuid, false)`, ventes, peer.UserID))
	denied(as(mgr.PGRole, `SELECT lattis.set_team_manager($1::uuid, $2::uuid, true)`, achats, alice.UserID))
	denied(as(alice.PGRole, `SELECT lattis.set_team_manager($1::uuid, $2::uuid, true)`, ventes, alice.UserID))
	ok(as(mgr.PGRole, `SELECT lattis.set_team_manager($1::uuid, $2::uuid, false)`, nord, alice.UserID))

	// Écran Utilisateurs : le périmètre seulement, sans invitations.
	var adm struct {
		Users []struct {
			Email       string   `json:"email"`
			Manageable  bool     `json:"manageable"`
			TeamManager []string `json:"team_manager"`
		} `json:"users"`
		Invitations []any `json:"invitations"`
	}
	ok(as(owner.PGRole, `SELECT lattis.invite($1, NULL, '{server_owner}')`, testpg.Email("invite")))
	scan(mgr.PGRole, &adm, `SELECT lattis.admin_users('')`)
	var emails []string
	for _, u := range adm.Users {
		emails = append(emails, u.Email)
		if u.Manageable != (u.Email == alice.Email || u.Email == peer.Email) {
			t.Fatalf("%s gérable : %v", u.Email, u.Manageable)
		}
		if u.Email == mgr.Email && !slices.Equal(u.TeamManager, []string{"Ventes"}) {
			t.Fatalf("%s gestionnaire de %v", u.Email, u.TeamManager)
		}
	}
	slices.Sort(emails)
	want := []string{alice.Email, mgr.Email, owner.Email, peer.Email}
	slices.Sort(want)
	if !slices.Equal(emails, want) || len(adm.Invitations) != 0 {
		t.Fatalf("utilisateurs vus : %v, invitations %v", emails, adm.Invitations)
	}
	scan(owner.PGRole, &adm, `SELECT lattis.admin_users('')`)
	if len(adm.Users) < 6 || len(adm.Invitations) != 1 {
		t.Fatalf("propriétaire : %d utilisateurs, %d invitations", len(adm.Users), len(adm.Invitations))
	}

	// Classeurs : ceux dont il est propriétaire.
	ok(as(owner.PGRole, `SELECT lattis.set_config('workbook_creators', '"everyone"')`))
	var mine, theirs string
	tables := map[string]string{}
	newWorkbook := func(role, title string) string {
		var wb, tid string
		scan(role, &wb, `SELECT lattis.create_workbook($1, 'collaborative')::text`, title)
		scan(role, &tid, `SELECT lattis.create_table($1::uuid, 'Lignes', '[{"name":"Code","type":"text"}]')::text`, wb)
		ok(as(role, `SELECT lattis.finish_publish($1::uuid)`, wb))
		tables[wb] = tid
		return wb
	}
	mine, theirs = newWorkbook(mgr.PGRole, "À moi"), newWorkbook(owner.PGRole, "Pas à moi")
	var wbs []map[string]any
	scan(mgr.PGRole, &wbs, `SELECT lattis.admin_workbooks()`)
	if len(wbs) != 1 || wbs[0]["id"] != mine {
		t.Fatalf("classeurs du gestionnaire : %v", wbs)
	}
	denied(as(mgr.PGRole, `SELECT lattis.workbook_acl($1::uuid)`, theirs))
	scan(owner.PGRole, &wbs, `SELECT lattis.admin_workbooks()`)
	if len(wbs) != 2 {
		t.Fatalf("classeurs du propriétaire : %d", len(wbs))
	}
	var forms json.RawMessage
	scan(mgr.PGRole, &forms, `SELECT lattis.admin_forms()`)

	// Pipelines : ceux de ses classeurs ; ni fichier du dossier des secrets,
	// ni motif qui sort du dossier de dépôt.
	pipeline := func(wb string, extra map[string]any) string {
		p := map[string]any{"name": "Import " + wb[:4], "workbook_id": wb, "table_id": tables[wb], "source_kind": "file",
			"source": map[string]any{"pattern": "*.csv"}, "format": "csv", "mode": "append"}
		for k, v := range extra {
			p[k] = v
		}
		b, _ := json.Marshal(p)
		return string(b)
	}
	var mineP, theirsP string
	scan(mgr.PGRole, &mineP, `SELECT lattis.create_pipeline($1::jsonb)::text`, pipeline(mine, nil))
	scan(owner.PGRole, &theirsP, `SELECT lattis.create_pipeline($1::jsonb)::text`, pipeline(theirs, nil))
	denied(as(mgr.PGRole, `SELECT lattis.create_pipeline($1::jsonb)`, pipeline(theirs, nil)))
	denied(as(mgr.PGRole, `SELECT lattis.create_pipeline($1::jsonb)`, pipeline(mine, map[string]any{"secret_ref": "cle-serveur"})))
	if err := as(mgr.PGRole, `SELECT lattis.create_pipeline($1::jsonb)`, pipeline(mine, map[string]any{"source": map[string]any{"pattern": "../*"}})); pgCode(err) != "22023" {
		t.Fatalf("motif hors du dépôt : %v", err)
	}
	denied(as(mgr.PGRole, `SELECT lattis.update_pipeline($1::uuid, '{"secret_ref": "cle-serveur"}')`, mineP))
	ok(as(mgr.PGRole, `SELECT lattis.update_pipeline($1::uuid, '{"enabled": false}')`, mineP))
	ok(as(mgr.PGRole, `SELECT lattis.run_pipeline($1::uuid, NULL)`, mineP))
	ok(as(mgr.PGRole, `SELECT lattis.set_pipeline_alert_email($1::uuid, $2)`, mineP, mgr.Email))
	for _, q := range []string{`SELECT lattis.run_pipeline($1::uuid, NULL)`, `SELECT lattis.drop_pipeline($1::uuid)`,
		`SELECT lattis.update_pipeline($1::uuid, '{"enabled": false}')`, `SELECT lattis.set_pipeline_alert_email($1::uuid, 'x@example.test')`} {
		denied(as(mgr.PGRole, q, theirsP))
	}
	var pls, targets []map[string]any
	scan(mgr.PGRole, &pls, `SELECT lattis.admin_pipelines()`)
	scan(mgr.PGRole, &targets, `SELECT lattis.pipeline_targets()`)
	if len(pls) != 1 || pls[0]["id"] != mineP || len(targets) != 1 || targets[0]["workbook_id"] != mine {
		t.Fatalf("pipelines du gestionnaire : %v, cibles %v", pls, targets)
	}
	var can bool
	scan(mgr.PGRole, &can, `SELECT lattis.can_manage_pipeline($1::uuid)`, theirsP)
	if can {
		t.Fatal("pipeline d'autrui administrable")
	}
	ok(as(mgr.PGRole, `SELECT lattis.drop_pipeline($1::uuid)`, mineP))

	// Sources réseau : seulement vers les adresses autorisées par un
	// propriétaire ; le pipeline reste alors restreint.
	httpSrc := map[string]any{"source_kind": "http", "source": map[string]any{"url": "https://api.exemple.fr/export.csv"}}
	denied(as(mgr.PGRole, `SELECT lattis.create_pipeline($1::jsonb)`, pipeline(mine, httpSrc)))
	if err := as(owner.PGRole, `SELECT lattis.set_config('pipeline_team_hosts', '["http://x"]')`); pgCode(err) != "22023" {
		t.Fatalf("adresse mal écrite acceptée : %v", err)
	}
	ok(as(owner.PGRole, `SELECT lattis.set_config('pipeline_team_hosts', '["api.exemple.fr", "*.partenaire.fr"]')`))
	denied(as(mgr.PGRole, `SELECT lattis.create_pipeline($1::jsonb)`, pipeline(mine, map[string]any{"source_kind": "http",
		"source": map[string]any{"url": "http://10.0.0.5/x.csv"}})))
	denied(as(mgr.PGRole, `SELECT lattis.create_pipeline($1::jsonb)`, pipeline(mine, map[string]any{"source_kind": "sftp",
		"source": map[string]any{"host": "interne.local", "user": "u", "path": "/x"}})))
	var netP string
	scan(mgr.PGRole, &netP, `SELECT lattis.create_pipeline($1::jsonb)::text`, pipeline(mine, httpSrc))
	ok(as(mgr.PGRole, `SELECT lattis.update_pipeline($1::uuid, '{"source": {"url": "https://ftp.partenaire.fr/a.csv"}}')`, netP))
	denied(as(mgr.PGRole, `SELECT lattis.update_pipeline($1::uuid, '{"webhook_url": "https://hooks.ailleurs.com/x"}')`, netP))
	ok(as(owner.PGRole, `SELECT lattis.update_pipeline($1::uuid, '{"name": "Repris"}')`, netP))
	var restricted map[string]bool
	ok(db.Super.QueryRow(ctx, `SELECT jsonb_object_agg(id, restricted) FROM lattis.pipelines`).Scan(&restricted))
	if !restricted[netP] || restricted[theirsP] {
		t.Fatalf("pipelines restreints : %v", restricted)
	}
	ok(as(owner.PGRole, `SELECT lattis.update_pipeline($1::uuid, '{"secret_ref": "cle-serveur"}')`, theirsP))

	// Adresse → compte : administrateurs seulement.
	var found map[string]any
	scan(mgr.PGRole, &found, `SELECT lattis.find_user($1)`, carole.Email)
	if found["id"] != carole.UserID || found["verified"] != true {
		t.Fatalf("compte trouvé : %v", found)
	}

	// Personne ordinaire : aucun accès.
	for _, q := range []string{`SELECT lattis.admin_users('')`, `SELECT lattis.find_user('x@example.test')`,
		`SELECT lattis.admin_workbooks()`, `SELECT lattis.admin_forms()`} {
		denied(as(plain.PGRole, q))
	}
	// Retiré, puis compte désactivé : plus rien.
	ok(as(mgr.PGRole, `SELECT lattis.set_team_manager($1::uuid, $2::uuid, false)`, ventes, mgr.UserID))
	scan(mgr.PGRole, &access, `SELECT lattis.admin_access()`)
	if access != nil {
		t.Fatalf("accès après retrait : %v", *access)
	}
	ok(as(owner.PGRole, `SELECT lattis.disable_user($1::uuid)`, peer.UserID))
	scan(peer.PGRole, &access, `SELECT lattis.admin_access()`)
	if access != nil {
		t.Fatalf("accès d'un compte désactivé : %v", *access)
	}
}
