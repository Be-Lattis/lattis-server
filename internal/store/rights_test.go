package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

// Retours de l'application sur les droits : niveau par feuille, invitation
// à un rôle personnalisé, membres lus par un concepteur, filtres détaillés.
func TestRightsFeedback(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	us := openUsers(t, db, "proprio", "concepteur", "compta")
	owner, designer, compta := us[0], us[1], us[2]
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var wb, tid, other, role string
	must(db.AsUser(t, owner.PGRole, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.create_workbook('Droits fins', 'collaborative')::text`).Scan(&wb); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_table($1::uuid, 'Factures', '[{"name":"Montant","type":"integer"}]', 'feuille-1')::text`, wb).Scan(&tid); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_table($1::uuid, 'Notes', '[{"name":"Texte","type":"text"}]')::text`, wb).Scan(&other); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.finish_publish($1::uuid)`, wb).Scan(new(int)); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_role($1::uuid, 'Comptables')::text`, wb).Scan(&role); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT lattis.set_level($1::uuid, 'sheet', 'feuille-1', 'editor', NULL, NULL)`, role); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT lattis.add_restriction($1::uuid, $2::uuid, 'select', ARRAY[$3::uuid], '"Montant" < 1000', 'Petits montants')`, wb, tid, role); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT lattis.invite($1, $2::uuid, '{comptables}')`, compta.Email, wb); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT lattis.invite($1, $2::uuid, '{designer}')`, designer.Email, wb)
		return err
	}))

	var caps struct {
		Tables map[string]struct {
			SheetID       *string `json:"sheet_id"`
			Level         string  `json:"level"`
			ReadFiltered  bool    `json:"read_filtered"`
			WriteFiltered bool    `json:"write_filtered"`
		} `json:"tables"`
		CustomRoles []string `json:"custom_roles"`
	}
	must(db.AsUser(t, compta.PGRole, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.my_capabilities($1::uuid)`, wb).Scan(&caps)
	}))
	f, n := caps.Tables[tid], caps.Tables[other]
	if len(caps.CustomRoles) != 1 || f.SheetID == nil || *f.SheetID != "feuille-1" || f.Level != "editor" || n.Level != "none" {
		t.Fatalf("niveau par feuille : %+v", caps)
	}
	if !f.ReadFiltered || f.WriteFiltered {
		t.Fatalf("filtres : %+v", f)
	}

	var members []struct {
		Email string   `json:"email"`
		Roles []string `json:"roles"`
	}
	must(db.AsUser(t, designer.PGRole, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.workbook_members($1::uuid)`, wb).Scan(&members)
	}))
	if len(members) != 3 {
		t.Fatalf("membres vus par le concepteur : %+v", members)
	}
	if err := db.AsUser(t, compta.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.workbook_members($1::uuid)`, wb)
		return err
	}); pgCode(err) != "42501" {
		t.Fatalf("membres vus par un éditeur : %v", err)
	}
	if err := db.AsUser(t, owner.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT lattis.invite('x@example.com', $1::uuid, '{inconnu}')`, wb)
		return err
	}); pgCode(err) != "22023" {
		t.Fatalf("rôle inconnu : %v", err)
	}
}
