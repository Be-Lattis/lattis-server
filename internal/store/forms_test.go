package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

// Formulaires : création, droits du rôle interne, envoi comme le fait
// lattisd, suivi de la structure, suppression.
func TestForms(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	us := openUsers(t, db, "concepteur", "lecteur", "dehors")
	owner, reader, outsider := us[0], us[1], us[2]
	as := func(u string, fn func(pgx.Tx) error) error { return db.AsUser(t, u, fn) }
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	val := func(u, q string, dst any, args ...any) error {
		return as(u, func(tx pgx.Tx) error {
			if dst == nil {
				_, err := tx.Exec(ctx, q, args...)
				return err
			}
			return tx.QueryRow(ctx, q, args...).Scan(dst)
		})
	}

	var wb, tid, schema string
	must(val(owner.PGRole, `SELECT lattis.create_workbook('Nids', 'collaborative')::text`, &wb))
	must(val(owner.PGRole, `SELECT lattis.create_table($1::uuid, 'Relevés', '[{"name":"Commune","type":"text"},{"name":"Nombre","type":"integer"},{"name":"Reçu","type":"datetime"},{"name":"N","type":"integer"}]')::text`, &tid, wb))
	must(val(owner.PGRole, `SELECT lattis.my_capabilities($1::uuid) ->> 'schema'`, &schema, wb))
	must(val(owner.PGRole, `SELECT lattis.finish_publish($1::uuid)`, new(int), wb))
	must(val(owner.PGRole, `SELECT lattis.invite($1, $2::uuid, ARRAY['reader'])`, nil, reader.Email, wb))
	col := func(name string) string {
		var id string
		must(db.Super.QueryRow(ctx, `SELECT column_id::text FROM `+pgx.Identifier{schema, "lattis_meta_columns"}.Sanitize()+` WHERE name = $1`, name).Scan(&id))
		return id
	}
	def := `{"format": 1, "lang": "fr", "appearance": {"title": "Relevé des nids", "accent": "teal"},
		"fields": [{"kind": "section", "title": "Où ?"},
		           {"column": "` + col("Commune") + `", "label": "Commune", "widget": "list", "list": {"values": ["Brest", "Quimper"]}, "param": "commune"},
		           {"column": "` + col("Nombre") + `", "label": "Nombre de nids", "widget": "number", "required": true}],
		"server_values": [{"column": "` + col("Reçu") + `", "value": {"token": "received_at"}},
		                  {"column": "` + col("N") + `", "value": {"token": "next_number"}}]}`

	// Définition refusée avec la raison ; lien public refusé par le serveur.
	err := val(owner.PGRole, `SELECT lattis.create_form($1::uuid, $2::uuid, $3::jsonb)`, nil, wb, tid,
		`{"definition": {"appearance": {"title": "X"}, "fields": [{"column": "00000000-0000-0000-0000-000000000000"}]}}`)
	if err == nil || !strings.Contains(err.Error(), "colonne inconnue") {
		t.Fatalf("définition invalide : %v", err)
	}
	if err := val(owner.PGRole, `SELECT lattis.create_form($1::uuid, $2::uuid, $3::jsonb)`, nil, wb, tid,
		`{"access": "public", "definition": `+def+`}`); err == nil || !strings.Contains(err.Error(), "lien public") {
		t.Fatalf("lien public sans réglage : %v", err)
	}
	if err := val(reader.PGRole, `SELECT lattis.create_form($1::uuid, $2::uuid, $3::jsonb)`, nil, wb, tid, `{"definition": `+def+`}`); err == nil {
		t.Fatal("formulaire créé par un lecteur")
	}

	var created map[string]string
	must(val(owner.PGRole, `SELECT lattis.create_form($1::uuid, $2::uuid, $3::jsonb)`, &created, wb, tid, `{"definition": `+def+`}`))
	fid := created["id"]
	if len(created["code"]) != 10 || created["path"] != "/f/"+created["code"] {
		t.Fatalf("création : %v", created)
	}
	if err := val(owner.PGRole, `SELECT lattis.update_form($1::uuid, '{"max_per_respondent": 1}', 99)`, nil, fid); err == nil || !strings.Contains(err.Error(), "40001") {
		t.Fatalf("version périmée : %v", err)
	}
	must(val(owner.PGRole, `SELECT lattis.update_form($1::uuid, '{"max_per_respondent": 1}', 1)`, nil, fid))
	must(val(owner.PGRole, `SELECT lattis.set_form_state($1::uuid, 'open')`, nil, fid))

	// Qui peut répondre (connexion de service, comme lattisd).
	gate := func(user any) string {
		var why *string
		must(db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT lattis.form_gate($1::uuid, $2::uuid)`, fid, user).Scan(&why)
		}))
		if why == nil {
			return ""
		}
		return *why
	}
	if g := gate(reader.UserID); g != "" {
		t.Fatalf("lecteur du classeur : %q", g)
	}
	if g := gate(outsider.UserID); g != "forbidden" {
		t.Fatalf("hors du classeur : %q", g)
	}
	if g := gate(nil); g != "login" {
		t.Fatalf("anonyme : %q", g)
	}

	// Envoi comme lattisd : sous l'identité du formulaire.
	var identity string
	must(db.Super.QueryRow(ctx, `SELECT u.pg_role FROM lattis.forms f JOIN lattis.users u ON u.id = f.identity WHERE f.id = $1`, fid).Scan(&identity))
	tbl := pgx.Identifier{schema, "Relevés"}.Sanitize()
	must(val(owner.PGRole, `INSERT INTO `+tbl+` (lattis_id, lattis_pos, "Commune", "N") VALUES (gen_random_uuid(), 'a', 'Morlaix', 7)`, nil))
	submit := func(commune string, n int) error {
		return as(identity, func(tx pgx.Tx) error {
			var b struct {
				Numbers map[string]float64 `json:"numbers"`
			}
			if err := tx.QueryRow(ctx, `SELECT lattis.form_begin($1::uuid)`, fid).Scan(&b); err != nil {
				return err
			}
			if b.Numbers[col("N")] != 8 && n == 1 {
				t.Errorf("numéro : %v", b.Numbers)
			}
			var row string
			if err := tx.QueryRow(ctx, `INSERT INTO `+tbl+` (lattis_id, lattis_pos, "Commune", "Nombre", "Reçu", "N")
				VALUES (gen_random_uuid(), 'b', $1, $2, now(), $3) RETURNING lattis_id::text`, commune, n, b.Numbers[col("N")]).Scan(&row); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `SELECT lattis.form_record($1::uuid, $2::uuid, $3::uuid, NULL, NULL)`, fid, row, reader.UserID)
			return err
		})
	}
	must(submit("Brest", 1))
	if err := submit("Quimper", 2); err == nil || !strings.Contains(err.Error(), "déjà répondu") {
		t.Fatalf("une réponse par personne : %v", err)
	}
	var author string
	var seen int
	must(db.Super.QueryRow(ctx, `SELECT lattis_author FROM `+tbl+` WHERE "Commune" = 'Brest'`).Scan(&author))
	must(val(identity, `SELECT count(*) FROM `+tbl, &seen))
	if author != identity || seen != 1 {
		t.Fatalf("auteur %q, lignes vues par l'identité : %d", author, seen)
	}
	if err := val(identity, `DELETE FROM `+tbl, nil); err == nil {
		t.Fatal("l'identité supprime des lignes")
	}
	var drift string
	must(val(owner.PGRole, `SELECT lattis.check_drift($1::uuid)::text`, &drift, wb))
	if drift != "[]" {
		t.Fatalf("écart signalé : %s", drift)
	}
	var list string
	must(val(owner.PGRole, `SELECT lattis.list_forms($1::uuid)::text`, &list, wb))
	if !strings.Contains(list, `"responses": 1`) {
		t.Fatalf("liste : %s", list)
	}
	must(val(reader.PGRole, `SELECT lattis.list_forms($1::uuid)::text`, &list, wb))
	if !strings.Contains(list, fid) || strings.Contains(list, `"responses": 1`) {
		t.Fatalf("liste d'un lecteur : %s", list)
	}

	// Restriction « tous les rôles » : l'identité est visée aussi.
	var rid string
	must(val(owner.PGRole, `SELECT lattis.add_restriction($1::uuid, $2::uuid, 'insert', '{}', 'false', 'Ajout interdit')::text`, &rid, wb, tid))
	must(val(owner.PGRole, `SELECT lattis.update_form($1::uuid, '{"max_per_respondent": null}', 3)`, nil, fid))
	if err := submit("Quimper", 3); err == nil {
		t.Fatal("restriction d'ajout ignorée par le formulaire")
	}
	must(val(owner.PGRole, `SELECT lattis.drop_restriction($1::uuid)`, nil, rid))

	// Suppression d'une colonne obligatoire : champ retiré, formulaire en pause.
	must(val(owner.PGRole, `SELECT lattis.drop_column($1::uuid, $2::uuid, $3::uuid)`, nil, wb, tid, col("Nombre")))
	var state, reason string
	must(db.Super.QueryRow(ctx, `SELECT state, coalesce(pause_reason, '') FROM lattis.forms WHERE id = $1`, fid).Scan(&state, &reason))
	if state != "paused" || reason != "column_dropped" {
		t.Fatalf("après suppression de colonne : %s %s", state, reason)
	}

	// Format de l'application : position sur deux colonnes, audience en
	// identifiants, adresse complète.
	must(val(owner.PGRole, `SELECT lattis.add_column($1::uuid, $2::uuid, 'Lat', 'decimal')`, nil, wb, tid))
	must(val(owner.PGRole, `SELECT lattis.add_column($1::uuid, $2::uuid, 'Lon', 'decimal')`, nil, wb, tid))
	var second map[string]string
	must(val(owner.PGRole, `SELECT lattis.create_form($1::uuid, $2::uuid, $3::jsonb)`, &second, wb, tid,
		`{"access": "audience", "audience": ["`+outsider.UserID+`"], "edit": "7 days", "definition": {"appearance": {"title": "Position", "accent": "slate"},
		  "fields": [{"kind": "position", "latitude": "`+col("Lat")+`", "longitude": "`+col("Lon")+`"}]}}`))
	if !strings.HasSuffix(second["url"], "/f/"+second["code"]) {
		t.Fatalf("adresse : %v", second)
	}
	must(val(owner.PGRole, `SELECT lattis.list_forms($1::uuid)::text`, &list, wb))
	if !strings.Contains(list, `"kind": "user"`) || !strings.Contains(list, `"max_per_respondent"`) {
		t.Fatalf("liste (audience) : %s", list)
	}
	must(val(owner.PGRole, `SELECT lattis.drop_form($1::uuid)`, nil, second["id"]))

	// Suppression : rôle interne parti, identité désactivée, lignes gardées.
	must(val(owner.PGRole, `SELECT lattis.drop_form($1::uuid)`, nil, fid))
	var roles, rows int
	var disabled bool
	must(db.Super.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname LIKE 'wb\_%\_f\_%'`).Scan(&roles))
	must(db.Super.QueryRow(ctx, `SELECT disabled FROM lattis.users WHERE pg_role = $1`, identity).Scan(&disabled))
	must(db.Super.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&rows))
	if roles != 0 || !disabled || rows != 2 {
		t.Fatalf("après suppression : %d rôles, désactivée %v, %d lignes", roles, disabled, rows)
	}
	_ = store.Enrollment{}
}
