package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

// Opérations de structure (§9.3) sur un classeur collaboratif publié.
func TestStructure(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	owner := openUsers(t, db, "structure")[0]
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// one : une valeur (ou rien) lue sous l'identité du propriétaire.
	one := func(dst any, q string, args ...any) error {
		return db.AsUser(t, owner.PGRole, func(tx pgx.Tx) error {
			if dst == nil {
				_, err := tx.Exec(ctx, q, args...)
				return err
			}
			return tx.QueryRow(ctx, q, args...).Scan(dst)
		})
	}
	refused := func(want, q string, args ...any) {
		t.Helper()
		err := one(nil, q, args...)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s : attendu « %s », obtenu %v", q, want, err)
		}
	}

	var wb, tid, schema string
	must(one(&wb, `SELECT lattis.create_workbook('Stock', 'collaborative')::text`))
	must(one(&tid, `SELECT lattis.create_table($1::uuid, 'Articles', '[{"name":"Code","type":"text"},{"name":"Prix","type":"text"}]')::text`, wb))
	must(one(&schema, `SELECT lattis.my_capabilities($1::uuid) ->> 'schema'`, wb))
	tbl := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
	must(one(nil, `INSERT INTO `+tbl("Articles")+` (lattis_id, "Code", "Prix") VALUES
		('00000000-0000-7000-8000-000000000001', 'A1', '1,50'),
		('00000000-0000-7000-8000-000000000002', 'A2', '')`))
	must(one(new(int), `SELECT lattis.finish_publish($1::uuid)`, wb))
	col := func(name string) string {
		t.Helper()
		var id string
		must(one(&id, `SELECT column_id::text FROM `+tbl("lattis_meta_columns")+` WHERE table_id = $1 AND name = $2`, tid, name))
		return id
	}

	// Ajout après « Code » : positions décalées.
	var stock string
	must(one(&stock, `SELECT lattis.add_column($1::uuid, $2::uuid, 'Stock', 'integer', $3::uuid)::text`, wb, tid, col("Code")))
	var order string
	must(one(&order, `SELECT string_agg(name, ',' ORDER BY position) FROM `+tbl("lattis_meta_columns")+` WHERE table_id = $1`, tid))
	if order != "Code,Stock,Prix" {
		t.Fatalf("ordre des colonnes : %s", order)
	}
	// Insertion en première position.
	must(one(new(string), `SELECT lattis.insert_column($1::uuid, $2::uuid, 'Rang', 'integer', 1)::text`, wb, tid))
	must(one(&order, `SELECT string_agg(name, ',' ORDER BY position) FROM `+tbl("lattis_meta_columns")+` WHERE table_id = $1`, tid))
	if order != "Rang,Code,Stock,Prix" {
		t.Fatalf("ordre après insertion : %s", order)
	}
	must(one(nil, `SELECT lattis.drop_column($1::uuid, $2::uuid, $3::uuid)`, wb, tid, col("Rang")))
	refused("déjà une colonne", `SELECT lattis.add_column($1::uuid, $2::uuid, 'stock', 'text')`, wb, tid)
	refused("type inconnu", `SELECT lattis.add_column($1::uuid, $2::uuid, 'Poids', 'float')`, wb, tid)

	// Conversion du texte en décimal (virgule acceptée, vide → NULL).
	must(one(nil, `SELECT lattis.alter_column_type($1::uuid, $2::uuid, $3::uuid, 'decimal')`, wb, tid, col("Prix")))
	var price float64
	must(one(&price, `SELECT "Prix" FROM `+tbl("Articles")+` WHERE "Code" = 'A1'`))
	if price != 1.5 {
		t.Fatalf("prix converti : %v", price)
	}
	refused("ne se convertissent pas", `SELECT lattis.alter_column_type($1::uuid, $2::uuid, $3::uuid, 'date')`, wb, tid, col("Code"))

	// Renommage de colonne et de tableau.
	must(one(nil, `SELECT lattis.rename_column($1::uuid, $2::uuid, $3::uuid, 'Référence')`, wb, tid, col("Code")))
	must(one(nil, `SELECT lattis.rename_table($1::uuid, $2::uuid, 'Produits')`, wb, tid))
	must(one(new(int), `SELECT count("Référence") FROM `+tbl("Produits")))
	must(one(new(string), `SELECT lattis.create_table($1::uuid, 'Fournisseurs', '[{"name":"Nom","type":"text"}]')::text`, wb))
	refused("existe déjà", `SELECT lattis.rename_table($1::uuid, $2::uuid, 'fournisseurs')`, wb, tid)

	// Clé : valeurs en double refusées, puis index.
	must(one(nil, `UPDATE `+tbl("Produits")+` SET "Stock" = 3`))
	refused("en double", `SELECT lattis.set_key($1::uuid, $2::uuid, $3::uuid)`, wb, tid, stock)
	must(one(nil, `SELECT lattis.set_key($1::uuid, $2::uuid, $3::uuid)`, wb, tid, col("Référence")))
	refused("23505", `INSERT INTO `+tbl("Produits")+` (lattis_id, "Référence") VALUES ('00000000-0000-7000-8000-000000000003', 'A1')`)
	var ix string
	must(one(&ix, `SELECT lattis.create_index($1::uuid, $2::uuid, ARRAY[$3::uuid, $4::uuid])`, wb, tid, stock, col("Prix")))
	var indexes string
	must(one(&indexes, `SELECT lattis.table_indexes($1::uuid, $2::uuid)::text`, wb, tid))
	if !strings.Contains(indexes, `"columns": ["Stock", "Prix"]`) {
		t.Fatalf("index : %s", indexes)
	}
	must(one(nil, `SELECT lattis.drop_index($1::uuid, $2::uuid, $3)`, wb, tid, ix))
	refused("introuvable", `SELECT lattis.drop_index($1::uuid, $2::uuid, 'lattis_key_x')`, wb, tid)

	// Une colonne citée par une restriction ne se renomme ni ne se supprime.
	must(one(nil, `SELECT lattis.add_restriction($1::uuid, $2::uuid, 'select', '{}', '"Stock" > 0', 'En stock')`, wb, tid))
	refused("restriction « En stock »", `SELECT lattis.rename_column($1::uuid, $2::uuid, $3::uuid, 'Quantité')`, wb, tid, stock)
	refused("restriction « En stock »", `SELECT lattis.drop_column($1::uuid, $2::uuid, $3::uuid)`, wb, tid, stock)

	// Suppression d'une colonne, puis du tableau.
	must(one(nil, `SELECT lattis.drop_column($1::uuid, $2::uuid, $3::uuid)`, wb, tid, col("Prix")))
	must(one(&order, `SELECT string_agg(name || position, ',' ORDER BY position) FROM `+tbl("lattis_meta_columns")+` WHERE table_id = $1`, tid))
	if order != "Référence1,Stock2" {
		t.Fatalf("colonnes après suppression : %s", order)
	}
	must(one(nil, `SELECT lattis.drop_table($1::uuid, $2::uuid)`, wb, tid))
	var left int
	must(db.Super.QueryRow(ctx, `SELECT count(*) FROM lattis.restrictions WHERE table_id = $1`, tid).Scan(&left))
	if left != 0 {
		t.Fatal("restriction restée après la suppression du tableau")
	}
	refused("introuvable", `SELECT lattis.rename_table($1::uuid, $2::uuid, 'X')`, wb, tid)

	// Classeur figé publié : republier d'abord.
	var frozen, ft string
	must(one(&frozen, `SELECT lattis.create_workbook('Figé', 'frozen')::text`))
	must(one(&ft, `SELECT lattis.create_table($1::uuid, 'T', '[{"name":"A","type":"text"}]')::text`, frozen))
	must(one(new(int), `SELECT lattis.finish_publish($1::uuid)`, frozen))
	refused("republier", `SELECT lattis.add_column($1::uuid, $2::uuid, 'B', 'text')`, frozen, ft)
}
