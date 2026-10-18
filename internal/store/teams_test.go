package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

// Équipes Lattis : membres d'un rôle par une équipe, filtre « mon équipe »,
// notification « *acl » quand l'équipe change.
func TestTeams(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	us := openUsers(t, db, "proprio", "alice", "bob", "carole")
	owner, alice, bob, carole := us[0], us[1], us[2], us[3]
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	service := func(fn func(pgx.Tx) error) error { return db.Store.AsActor(ctx, "test", fn) }
	count := func(who string, tbl string) (int, error) {
		var n int
		err := db.AsUser(t, who, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n)
		})
		return n, err
	}

	var nord, sud string
	must(service(func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.create_team('Nord')::text`).Scan(&nord); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_team('Sud')::text`).Scan(&sud); err != nil {
			return err
		}
		for _, m := range [][2]string{{nord, alice.UserID}, {nord, bob.UserID}, {sud, carole.UserID}} {
			if _, err := tx.Exec(ctx, `SELECT lattis.add_team_member($1::uuid, $2::uuid)`, m[0], m[1]); err != nil {
				return err
			}
		}
		return nil
	}))
	if err := service(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.create_team('nord')`)
		return err
	}); pgCode(err) != "23505" {
		t.Fatalf("nom d'équipe en double : %v", err)
	}
	if err := db.AsUser(t, owner.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.create_team('Est')`)
		return err
	}); pgCode(err) != "42501" {
		t.Fatalf("équipe créée par un non-propriétaire du serveur : %v", err)
	}

	var wb, tid, schema, role, channel string
	must(db.AsUser(t, owner.PGRole, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.create_workbook('Ventes par équipe', 'collaborative')::text`).Scan(&wb); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_table($1::uuid, 'Ventes', '[{"name":"Équipe","type":"text"},{"name":"Montant","type":"integer"}]')::text`, wb).Scan(&tid); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid) ->> 'schema'`, wb).Scan(&schema); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "Ventes"}.Sanitize()+` (lattis_id, "Équipe", "Montant") VALUES
			('00000000-0000-7000-8000-000000000001', 'Nord', 1),
			('00000000-0000-7000-8000-000000000002', 'Sud', 2),
			('00000000-0000-7000-8000-000000000003', 'Nord', 3)`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.finish_publish($1::uuid)`, wb).Scan(new(int)); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_role($1::uuid, 'Commerciaux')::text`, wb).Scan(&role); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT lattis.set_level($1::uuid, 'table', $2, 'reader', '"Équipe" IN (SELECT lattis.my_teams())', NULL)`, role, tid); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT lattis.grant_role($1::uuid, $2::uuid)`, role, nord); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid) ->> 'channel'`, wb).Scan(&channel)
	}))
	tbl := pgx.Identifier{schema, "Ventes"}.Sanitize()

	// Alice, de Nord, lit les lignes de Nord ; Carole, de Sud, n'a pas le rôle.
	var teams []string
	must(db.AsUser(t, alice.PGRole, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT coalesce(array_agg(t), '{}') FROM lattis.my_teams() t`).Scan(&teams); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid)`, wb).Scan(new(map[string]any))
	}))
	if !slices.Equal(teams, []string{"Nord"}) {
		t.Fatalf("my_teams : %v", teams)
	}
	if n, err := count(alice.PGRole, tbl); err != nil || n != 2 {
		t.Fatalf("Alice lit %d lignes, %v", n, err)
	}
	if _, err := count(carole.PGRole, tbl); pgCode(err) != "42501" {
		t.Fatalf("Carole sans rôle : %v", err)
	}

	var acl struct {
		Roles []struct {
			ID    string `json:"id"`
			Teams []struct {
				Name string `json:"name"`
			} `json:"teams"`
		} `json:"roles"`
	}
	var members []struct {
		Email string   `json:"email"`
		Roles []string `json:"roles"`
		Teams []string `json:"teams"`
	}
	must(db.AsUser(t, owner.PGRole, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.workbook_acl($1::uuid)`, wb).Scan(&acl); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT lattis.workbook_members($1::uuid)`, wb).Scan(&members)
	}))
	found := false
	for _, r := range acl.Roles {
		if r.ID == role {
			found = len(r.Teams) == 1 && r.Teams[0].Name == "Nord"
		}
	}
	if !found {
		t.Fatalf("équipes du rôle : %+v", acl)
	}
	byEmail := map[string][]string{}
	for _, m := range members {
		byEmail[m.Email] = m.Teams
	}
	if !slices.Equal(byEmail[alice.Email], []string{"Nord"}) || !slices.Equal(byEmail[bob.Email], []string{"Nord"}) {
		t.Fatalf("membres par équipe : %+v", members)
	}
	if _, ok := byEmail[carole.Email]; ok {
		t.Fatalf("Carole parmi les membres : %+v", members)
	}

	// « Voir en tant que… » : le filtre suit les équipes de la personne vue.
	if rows, err := db.Store.Preview(ctx, owner.PGRole, wb, alice.UserID, tid); err != nil || len(rows) != 2 {
		t.Fatalf("aperçu d'Alice : %d lignes, %v", len(rows), err)
	}

	// Carole rejoint Nord : le classeur est prévenu, elle lit Nord et Sud.
	listen, err := db.Super.Acquire(ctx)
	must(err)
	defer listen.Release()
	_, err = listen.Exec(ctx, `LISTEN `+pgx.Identifier{channel}.Sanitize())
	must(err)
	must(service(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.add_team_member($1::uuid, $2::uuid)`, nord, carole.UserID)
		return err
	}))
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	note, err := listen.Conn().WaitForNotification(wctx)
	if err != nil || note.Payload != "*acl" {
		t.Fatalf("notification : %+v, %v", note, err)
	}
	if n, err := count(carole.PGRole, tbl); err != nil || n != 3 {
		t.Fatalf("Carole dans Nord et Sud lit %d lignes, %v", n, err)
	}

	// Bob quitte Nord ; le rôle retiré à l'équipe, Alice perd l'accès.
	must(service(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.remove_team_member($1::uuid, $2::uuid)`, nord, bob.UserID)
		return err
	}))
	if _, err := count(bob.PGRole, tbl); pgCode(err) != "42501" {
		t.Fatalf("Bob hors de l'équipe : %v", err)
	}
	must(db.AsUser(t, owner.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.revoke_role($1::uuid, $2::uuid)`, role, nord)
		return err
	}))
	if _, err := count(alice.PGRole, tbl); pgCode(err) != "42501" {
		t.Fatalf("Alice après retrait du rôle à l'équipe : %v", err)
	}

	// Liste : noms pour tous, membres pour les propriétaires du serveur.
	var list []struct {
		Name    string `json:"name"`
		Members []any  `json:"members"`
		Count   int    `json:"member_count"`
	}
	must(db.AsUser(t, alice.PGRole, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.list_teams()`).Scan(&list)
	}))
	if len(list) != 2 || list[0].Name != "Nord" || list[0].Members != nil || list[0].Count != 2 {
		t.Fatalf("liste des équipes : %+v", list)
	}

	// Suppression : le rôle PostgreSQL de l'équipe disparaît.
	must(db.AsUser(t, owner.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.grant_role($1::uuid, $2::uuid)`, role, sud)
		return err
	}))
	var pgRole string
	must(db.Super.QueryRow(ctx, `SELECT pg_role FROM lattis.teams WHERE id = $1`, sud).Scan(&pgRole))
	must(service(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.drop_team($1::uuid)`, sud)
		return err
	}))
	var exists bool
	must(db.Super.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, pgRole).Scan(&exists))
	if exists {
		t.Fatal("rôle de l'équipe supprimée encore là")
	}
}
