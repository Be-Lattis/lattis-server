package pipeline

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Colonnes créées d'après les données (cible « Créer automatiquement » du
// Data Lake, source.auto_columns) : à chaque exécution, les colonnes de la
// source absentes du tableau sont ajoutées, leur type deviné sur les
// premières lignes ; la colonne d'attente du tableau neuf est retirée.

// placeholder : colonne d'un tableau créé avant toute donnée.
const placeholder = "Colonne en attente"

// sampleRows : lignes lues pour deviner les types.
const sampleRows = 500

// peekSource rejoue les lignes lues d'avance, puis continue la source.
type peekSource struct {
	rowSource
	buf [][]*string
	i   int
	err error
}

func peek(src rowSource, n int) *peekSource {
	p := &peekSource{rowSource: src}
	for len(p.buf) < n {
		row, err := src.Next()
		if err != nil {
			p.err = err
			break
		}
		p.buf = append(p.buf, row)
	}
	return p
}

func (p *peekSource) Next() ([]*string, error) {
	if p.i < len(p.buf) {
		p.i++
		return p.buf[p.i-1], nil
	}
	if p.err != nil {
		return nil, p.err
	}
	return p.rowSource.Next()
}

// inferType : type Lattis le plus précis qui accepte toutes les valeurs
// non vides de la colonne i (texte sinon, ou sans valeur).
func inferType(sample [][]*string, i int) string {
	candidates := []struct{ lattis, pg string }{
		{"integer", "bigint"}, {"decimal", "numeric"}, {"date", "date"}, {"datetime", "timestamptz"}, {"boolean", "boolean"},
	}
	seen := false
	ok := make([]bool, len(candidates))
	for k := range ok {
		ok[k] = true
	}
	for _, row := range sample {
		if i >= len(row) || row[i] == nil || strings.TrimSpace(*row[i]) == "" {
			continue
		}
		seen = true
		v := strings.TrimSpace(*row[i])
		for k, c := range candidates {
			if !ok[k] {
				continue
			}
			// Un code à zéro initial (« 0123 ») reste du texte.
			if (c.lattis == "integer" || c.lattis == "decimal") && len(v) > 1 && v[0] == '0' && v[1] != '.' && v[1] != ',' {
				ok[k] = false
				continue
			}
			// 0/1 ne suffit pas à faire un booléen, ni un nombre une date.
			if c.lattis == "boolean" && (v == "0" || v == "1") {
				continue
			}
			if (c.lattis == "date" || c.lattis == "datetime") && !strings.ContainsAny(v, "-/.:T") {
				ok[k] = false
				continue
			}
			if _, err := convert(&v, c.pg); err != nil {
				ok[k] = false
			}
		}
	}
	if !seen {
		return "text"
	}
	for k, c := range candidates {
		if ok[k] {
			return c.lattis
		}
	}
	return "text"
}

// autoColumns ajoute au tableau les colonnes de la source qui lui manquent
// et retire la colonne d'attente ; renvoie la source à lire ensuite.
func (r *Runner) autoColumns(ctx context.Context, tx pgx.Tx, p *Pipeline, schema string, src rowSource) (rowSource, error) {
	ps := peek(src, sampleRows)
	if ps.err != nil && ps.err != io.EOF {
		return nil, ps.err
	}
	rows, err := tx.Query(ctx, `SELECT column_id::text, name FROM `+pgx.Identifier{schema, "lattis_meta_columns"}.Sanitize()+
		` WHERE table_id = $1::uuid`, p.TableID)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	holder := ""
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return nil, err
		}
		have[strings.ToLower(name)] = true
		if name == placeholder {
			holder = id
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Colonnes de la source déjà reliées à une colonne du tableau.
	mapped := map[string]bool{}
	for _, srcCol := range p.ColumnMap {
		mapped[strings.ToLower(srcCol)] = true
	}
	added := 0
	for i, h := range ps.Header() {
		name := strings.TrimSpace(h)
		low := strings.ToLower(name)
		if name == "" || have[low] || mapped[low] || strings.HasPrefix(low, "lattis_") {
			continue
		}
		typ := inferType(ps.buf, i)
		if _, err := tx.Exec(ctx, `SELECT lattis.add_column($1::uuid, $2::uuid, $3, $4)`, p.WorkbookID, p.TableID, name, typ); err != nil {
			return nil, fmt.Errorf("création de la colonne « %s » : %w", name, err)
		}
		have[low] = true
		added++
	}
	if holder != "" && (added > 0 || len(have) > 1) {
		if _, err := tx.Exec(ctx, `SELECT lattis.drop_column($1::uuid, $2::uuid, $3::uuid)`, p.WorkbookID, p.TableID, holder); err != nil {
			return nil, fmt.Errorf("retrait de la colonne d'attente : %w", err)
		}
	}
	return ps, nil
}
