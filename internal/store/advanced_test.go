package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

func TestAdvancedRights(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	us := openUsers(t, db, "proprio", "nord", "lecteur")
	owner, north, reader := us[0], us[1], us[2]
	as := func(u store.Enrollment, fn func(pgx.Tx) error) error { return db.AsUser(t, u.PGRole, fn) }
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	exec := func(u store.Enrollment, q string, args ...any) (int64, error) {
		var n int64
		err := as(u, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, q, args...)
			n = tag.RowsAffected()
			return err
		})
		return n, err
	}

	var wb, tid, schema, role string
	must(as(owner, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.create_workbook('Ventes', 'collaborative')::text`).Scan(&wb); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_table($1::uuid, 'Commandes', '[{"name":"Région","type":"text"},{"name":"Montant","type":"decimal"}]')::text`, wb).Scan(&tid); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid) ->> 'schema'`, wb).Scan(&schema); err != nil {
			return err
		}
		tbl := pgx.Identifier{schema, "Commandes"}.Sanitize()
		if _, err := tx.Exec(ctx, `INSERT INTO `+tbl+` (lattis_id, "Région", "Montant") VALUES
			('00000000-0000-7000-8000-000000000001', 'Nord', 500),
			('00000000-0000-7000-8000-000000000002', 'Nord', 5000),
			('00000000-0000-7000-8000-000000000003', 'Sud', 700)`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.finish_publish($1::uuid)`, wb).Scan(new(int)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT lattis.invite($1, $2::uuid, ARRAY['reader'])`, reader.Email, wb); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT lattis.create_role($1::uuid, 'Commerciaux Nord')::text`, wb).Scan(&role)
	}))
	tbl := pgx.Identifier{schema, "Commandes"}.Sanitize()
	count := func(u store.Enrollment) int {
		var n int
		must(as(u, func(tx pgx.Tx) error { return tx.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n) }))
		return n
	}

	// Un rôle sans niveau ne donne rien.
	_, err := exec(owner, `SELECT lattis.grant_role($1::uuid, $2::uuid)`, role, north.UserID)
	must(err)
	if err := as(north, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, `SELECT 1 FROM `+tbl); return err }); pgCode(err) != "42501" {
		t.Fatalf("rôle sans niveau : %v", err)
	}

	// Filtres : injections refusées, aucun effet de bord.
	for _, bad := range []string{
		`true; DROP TABLE ` + tbl,
		`true) OR (true`,
		`true) WITH CHECK (true`,
		`"Région" = 'Nord') OR ("Région" IS NOT NULL`,
		`"Région" = 'Nord' -- `,
		`pg_sleep(1) IS NULL /* x */`,
		`"Inconnue" = 1`,
		`'non fermée`,
		`$$x$$ = 'x'`,
	} {
		if _, err := exec(owner, `SELECT lattis.set_level($1::uuid, 'table', $2, 'editor', $3, NULL)`, role, tid, bad); err == nil {
			t.Fatalf("filtre accepté : %s", bad)
		}
	}
	if count(owner) != 3 {
		t.Fatal("le tableau a changé")
	}

	// Filtre de lecture : seules les lignes du Nord.
	_, err = exec(owner, `SELECT lattis.set_level($1::uuid, 'table', $2, 'editor', $3, NULL)`, role, tid, `("Région" IN ('Nord', 'l''Est'))`)
	must(err)
	if n := count(north); n != 2 {
		t.Fatalf("filtre de lecture : %d lignes", n)
	}
	if n, err := exec(north, `UPDATE `+tbl+` SET "Montant" = 1 WHERE "Région" = 'Sud'`); err != nil || n != 0 {
		t.Fatalf("modifie hors filtre : %d %v", n, err)
	}
	// Une ligne écrite doit rester visible (WITH CHECK).
	if _, err := exec(north, `UPDATE `+tbl+` SET "Région" = 'Sud' WHERE lattis_id = '00000000-0000-7000-8000-000000000001'`); pgCode(err) != "42501" {
		t.Fatalf("ligne sortie du filtre : %v", err)
	}
	var caps map[string]any
	must(as(north, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid)`, wb).Scan(&caps)
	}))
	tc := caps["tables"].(map[string]any)[tid].(map[string]any)
	if tc["level"] != "editor" || tc["filtered"] != true || caps["custom_roles"].([]any)[0] != "Commerciaux Nord" {
		t.Fatalf("capacités : %v", caps)
	}
	// Le lecteur fourni n'est pas concerné par le filtre.
	if count(reader) != 3 {
		t.Fatal("lecteur filtré à tort")
	}

	// Restriction : pas de modification au-delà de 1 000.
	var rid string
	must(as(owner, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.add_restriction($1::uuid, $2::uuid, 'update', ARRAY[$3::uuid], '"Montant" <= 1000', 'Gros montants')::text`,
			wb, tid, role).Scan(&rid)
	}))
	if n, err := exec(north, `UPDATE `+tbl+` SET "Montant" = "Montant" + 1`); err != nil || n != 1 {
		t.Fatalf("restriction : %d lignes, %v", n, err)
	}
	if _, err := exec(north, `UPDATE `+tbl+` SET "Montant" = 2000 WHERE lattis_id = '00000000-0000-7000-8000-000000000001'`); pgCode(err) != "42501" {
		t.Fatalf("restriction en écriture : %v", err)
	}
	must(as(north, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid)`, wb).Scan(&caps)
	}))
	if len(caps["restrictions"].([]any)) != 1 {
		t.Fatalf("restrictions visibles : %v", caps["restrictions"])
	}

	// « Voir en tant que… » : le propriétaire voit ce que voit le commercial.
	rows, err := db.Store.Preview(ctx, owner.PGRole, wb, north.UserID, tid)
	if err != nil || len(rows) != 2 {
		t.Fatalf("aperçu : %d lignes, %v", len(rows), err)
	}
	if _, err := db.Store.Preview(ctx, reader.PGRole, wb, north.UserID, tid); pgCode(err) != "42501" {
		t.Fatalf("aperçu par un lecteur : %v", err)
	}
	var audits int
	must(db.Super.QueryRow(ctx, `SELECT count(*) FROM lattis.audit WHERE action = 'preview_as'`).Scan(&audits))
	if audits != 1 {
		t.Fatalf("aperçu journalisé %d fois", audits)
	}

	// Écart : une règle ajoutée à la main est signalée.
	var drift []map[string]any
	checkDrift := func() {
		must(as(owner, func(tx pgx.Tx) error { return tx.QueryRow(ctx, `SELECT lattis.check_drift($1::uuid)`, wb).Scan(&drift) }))
	}
	checkDrift()
	if len(drift) != 0 {
		t.Fatalf("écart sans changement : %v", drift)
	}
	_, err = db.Super.Exec(ctx, `CREATE POLICY bricolage ON `+tbl+` FOR SELECT TO PUBLIC USING (true)`)
	must(err)
	checkDrift()
	if len(drift) != 1 || !strings.Contains(strings.Join(anyStrings(drift[0]["foreign_policies"]), ","), "bricolage") {
		t.Fatalf("écart non détecté : %v", drift)
	}
	// Recompiler ne l'absorbe pas : la règle étrangère reste signalée, comme un
	// droit donné à la main.
	_, err = db.Super.Exec(ctx, `GRANT SELECT ON `+tbl+` TO PUBLIC`)
	must(err)
	compile := func() {
		must(as(owner, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT lattis.compile_acl($1::uuid)`, wb)
			return err
		}))
	}
	compile()
	checkDrift()
	if len(drift) != 1 || !strings.Contains(strings.Join(anyStrings(drift[0]["foreign_policies"]), ","), "bricolage") ||
		strings.Join(anyStrings(drift[0]["foreign_grants"]), ",") != "PUBLIC" {
		t.Fatalf("écart absorbé par compile_acl : %v", drift)
	}
	_, err = db.Super.Exec(ctx, `DROP POLICY bricolage ON `+tbl+`; REVOKE SELECT ON `+tbl+` FROM PUBLIC`)
	must(err)
	checkDrift()
	if len(drift) != 0 {
		t.Fatalf("écart après retrait des ajouts : %v", drift)
	}
	// Une règle générée retirée à la main est signalée, et la recompilation la remet.
	_, err = db.Super.Exec(ctx, `DROP POLICY lattis_owner ON `+tbl)
	must(err)
	checkDrift()
	if len(drift) != 1 {
		t.Fatalf("règle générée retirée non détectée : %v", drift)
	}
	compile()
	checkDrift()
	if len(drift) != 0 {
		t.Fatalf("écart après recompilation : %v", drift)
	}
	// Un éditeur ne consulte pas les écarts.
	if _, err := exec(north, `SELECT lattis.check_drift($1::uuid)`, wb); pgCode(err) != "42501" {
		t.Fatalf("check_drift par un éditeur : %v", err)
	}

	// Restriction pour tous : le propriétaire n'est pas concerné.
	must(as(owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.add_restriction($1::uuid, $2::uuid, 'delete', '{}', 'false', 'Pas de suppression')`, wb, tid)
		return err
	}))
	if n, err := exec(north, `DELETE FROM `+tbl); err != nil || n != 0 {
		t.Fatalf("suppression malgré la restriction : %d %v", n, err)
	}
	if n, err := exec(owner, `DELETE FROM `+tbl+` WHERE "Région" = 'Sud'`); err != nil || n != 1 {
		t.Fatalf("propriétaire restreint : %d %v", n, err)
	}

	// Suppression de la restriction puis du rôle.
	_, err = exec(owner, `SELECT lattis.drop_restriction($1::uuid)`, rid)
	must(err)
	_, err = exec(owner, `SELECT lattis.drop_role($1::uuid)`, role)
	must(err)
	if err := as(north, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, `SELECT 1 FROM `+tbl); return err }); pgCode(err) != "42501" {
		t.Fatalf("après suppression du rôle : %v", err)
	}
	var acl map[string]any
	must(as(owner, func(tx pgx.Tx) error { return tx.QueryRow(ctx, `SELECT lattis.workbook_acl($1::uuid)`, wb).Scan(&acl) }))
	for _, r := range acl["roles"].([]any) {
		if r.(map[string]any)["name"] == "Commerciaux Nord" {
			t.Fatal("rôle toujours listé")
		}
	}
	if len(acl["restrictions"].([]any)) != 1 {
		t.Fatal("restriction mal listée")
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}
