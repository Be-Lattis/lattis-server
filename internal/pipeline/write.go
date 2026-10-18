package pipeline

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// target est une colonne du tableau cible alimentée par la source.
type target struct {
	name, pgType string
	src          int // index dans la source
}

const maxRejects = 20

// write convertit les lignes et les écrit dans le tableau, sous l'identité
// technique du pipeline, dans une seule transaction.
func (r *Runner) write(ctx context.Context, p *Pipeline, t *Table) (Result, error) {
	res := Result{Read: len(t.Rows)}
	u, err := r.Store.User(ctx, p.UserID)
	if err != nil {
		return res, fmt.Errorf("identité technique : %w", err)
	}
	if u.Disabled {
		return res, errors.New("identité technique désactivée")
	}
	var schema string
	if err := r.Store.DB.QueryRow(ctx, `SELECT schema_name FROM lattis.workbooks WHERE id = $1`, p.WorkbookID).Scan(&schema); err != nil {
		return res, fmt.Errorf("classeur : %w", err)
	}
	err = r.Users.As(ctx, u, func(tx pgx.Tx) error {
		tbl := pgx.Identifier{schema, p.TableName}
		cols, err := r.targets(ctx, tx, tbl, p, t)
		if err != nil {
			return err
		}
		// Conversion, lignes rejetées comptées et échantillonnées.
		var rows [][]any
		keys := map[string]int{}
		keyIdx := keyIndexes(cols, p.KeyColumns)
		for i, src := range t.Rows {
			vals := make([]any, len(cols)+1)
			bad := false
			for j, c := range cols {
				var raw *string
				if c.src < len(src) {
					raw = src[c.src]
				}
				v, err := convert(raw, c.pgType)
				if err != nil {
					res.reject(i+2, c.name, raw, err)
					bad = true
					break
				}
				vals[j+1] = v
			}
			if bad {
				continue
			}
			if p.Mode != "append" && len(keyIdx) > 0 {
				k := keyString(vals[1:], keyIdx)
				if k == "" {
					res.reject(i+2, strings.Join(p.KeyColumns, ", "), nil, errors.New("clé vide"))
					continue
				}
				if prev, ok := keys[k]; ok {
					// Même clé plus bas dans la source : la dernière l'emporte.
					rows[prev] = nil
					res.reject(i+2, strings.Join(p.KeyColumns, ", "), &k, errors.New("clé en double (la ligne suivante l'emporte)"))
				}
				keys[k] = len(rows)
			}
			rows = append(rows, vals)
		}
		kept := rows[:0]
		for _, v := range rows {
			if v != nil {
				kept = append(kept, v)
			}
		}
		rows = kept

		// Positions des nouvelles lignes, après la dernière (clés d'ordre).
		var last *string
		if p.Mode != "replace" || len(p.KeyColumns) > 0 {
			if err := tx.QueryRow(ctx, `SELECT max(lattis_pos COLLATE "C") FROM `+tbl.Sanitize()).Scan(&last); err != nil {
				return err
			}
		}
		base := ""
		if last != nil {
			base = *last
		}
		width := len(strconv.FormatInt(int64(len(rows)), 36)) + 1
		for i := range rows {
			rows[i][0] = base + pad62(i+1, width) + "1"
		}

		names := make([]string, len(cols))
		for i, c := range cols {
			names[i] = pgx.Identifier{c.name}.Sanitize()
		}
		colList := strings.Join(names, ", ")
		tmpCols := []string{`"__pos" text`}
		for i, c := range cols {
			tmpCols = append(tmpCols, names[i]+" "+c.pgType)
		}
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE lattis_in (`+strings.Join(tmpCols, ", ")+`) ON COMMIT DROP`); err != nil {
			return err
		}
		copyCols := append([]string{"__pos"}, columnNames(cols)...)
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"lattis_in"}, copyCols, pgx.CopyFromRows(rows)); err != nil {
			return fmt.Errorf("chargement : %w", err)
		}

		switch {
		case p.Mode == "append" || (p.Mode == "replace" && len(p.KeyColumns) == 0):
			if p.Mode == "replace" {
				tag, err := tx.Exec(ctx, `DELETE FROM `+tbl.Sanitize())
				if err != nil {
					return err
				}
				res.Deleted = int(tag.RowsAffected())
			}
			tag, err := tx.Exec(ctx, `INSERT INTO `+tbl.Sanitize()+` (lattis_id, lattis_pos, `+colList+`)
				SELECT uuidv7(), "__pos", `+colList+` FROM lattis_in`)
			if err != nil {
				return err
			}
			res.Inserted = int(tag.RowsAffected())
		default: // upsert, ou replace avec clé
			var on, set, diffT, diffS []string
			for _, k := range p.KeyColumns {
				id := pgx.Identifier{k}.Sanitize()
				on = append(on, "t."+id+" IS NOT DISTINCT FROM s."+id)
			}
			for _, n := range names {
				set = append(set, n+" = s."+n)
				diffT = append(diffT, "t."+n)
				diffS = append(diffS, "s."+n)
			}
			q := `MERGE INTO ` + tbl.Sanitize() + ` t USING lattis_in s ON ` + strings.Join(on, " AND ") + `
				WHEN MATCHED AND (` + strings.Join(diffT, ", ") + `) IS DISTINCT FROM (` + strings.Join(diffS, ", ") + `)
					THEN UPDATE SET ` + strings.Join(set, ", ") + `
				WHEN NOT MATCHED THEN INSERT (lattis_id, lattis_pos, ` + colList + `)
					VALUES (uuidv7(), s."__pos", ` + strings.Join(diffS, ", ") + `)
				RETURNING merge_action()`
			mrows, err := tx.Query(ctx, q)
			if err != nil {
				return err
			}
			for mrows.Next() {
				var action string
				if err := mrows.Scan(&action); err != nil {
					mrows.Close()
					return err
				}
				if action == "INSERT" {
					res.Inserted++
				} else {
					res.Updated++
				}
			}
			if err := mrows.Err(); err != nil {
				return err
			}
			if p.Mode == "replace" {
				tag, err := tx.Exec(ctx, `DELETE FROM `+tbl.Sanitize()+` t WHERE NOT EXISTS (SELECT 1 FROM lattis_in s WHERE `+
					strings.Join(on, " AND ")+`)`)
				if err != nil {
					return err
				}
				res.Deleted = int(tag.RowsAffected())
			}
		}
		return nil
	})
	return res, err
}

func (res *Result) reject(row int, col string, raw *string, err error) {
	res.Rejected++
	if len(res.Rejects) < maxRejects {
		v := ""
		if raw != nil {
			v = *raw
			if len(v) > 200 {
				v = v[:200] + "…"
			}
		}
		res.Rejects = append(res.Rejects, Reject{Row: row, Column: col, Value: v, Error: err.Error()})
	}
}

// targets : colonnes du tableau alimentées, avec leur type et leur colonne
// source (correspondance explicite, sinon même nom sans tenir compte de la casse).
func (r *Runner) targets(ctx context.Context, tx pgx.Tx, tbl pgx.Identifier, p *Pipeline, t *Table) ([]target, error) {
	rows, err := tx.Query(ctx, `SELECT attname, format_type(atttypid, atttypmod) FROM pg_attribute
		WHERE attrelid = $1::regclass AND attnum > 0 AND NOT attisdropped AND attname NOT LIKE 'lattis\_%' ORDER BY attnum`, tbl.Sanitize())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	index := map[string]int{}
	for i, h := range t.Header {
		if _, ok := index[strings.ToLower(h)]; !ok {
			index[strings.ToLower(h)] = i
		}
	}
	var out []target
	for rows.Next() {
		var c target
		if err := rows.Scan(&c.name, &c.pgType); err != nil {
			return nil, err
		}
		src := c.name
		if m, ok := p.ColumnMap[c.name]; ok && m != "" {
			src = m
		}
		i, ok := index[strings.ToLower(src)]
		if !ok {
			continue
		}
		c.src = i
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("aucune colonne de la source ne correspond au tableau « %s » (en-têtes lus : %s)",
			p.TableName, strings.Join(t.Header, ", "))
	}
	for _, k := range p.KeyColumns {
		found := false
		for _, c := range out {
			found = found || c.name == k
		}
		if !found {
			return nil, fmt.Errorf("la colonne clé « %s » n'est pas dans la source", k)
		}
	}
	return out, nil
}

func columnNames(cols []target) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.name
	}
	return out
}

func keyIndexes(cols []target, keys []string) []int {
	var out []int
	for _, k := range keys {
		for i, c := range cols {
			if c.name == k {
				out = append(out, i)
			}
		}
	}
	return out
}

func keyString(vals []any, idx []int) string {
	var b strings.Builder
	empty := true
	for _, i := range idx {
		if vals[i] != nil {
			empty = false
		}
		fmt.Fprintf(&b, "%v\x00", vals[i])
	}
	if empty {
		return ""
	}
	return b.String()
}

// pad62 écrit n en base 62 sur width chiffres (clés d'ordre de Lattis).
func pad62(n, width int) string {
	const digits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	b := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		b[i] = digits[n%62]
		n /= 62
	}
	return string(b)
}

// convert lit un texte selon le type PostgreSQL de la colonne cible.
func convert(raw *string, pgType string) (any, error) {
	if raw == nil {
		return nil, nil
	}
	s := strings.TrimSpace(*raw)
	if s == "" {
		return nil, nil
	}
	switch {
	case pgType == "text":
		return *raw, nil
	case pgType == "bigint" || pgType == "integer":
		clean := strings.NewReplacer(" ", "", " ", "", " ", "").Replace(s)
		if n, err := strconv.ParseInt(clean, 10, 64); err == nil {
			return n, nil
		}
		f, err := strconv.ParseFloat(strings.Replace(clean, ",", ".", 1), 64)
		if err != nil || f != math.Trunc(f) {
			return nil, errors.New("entier attendu")
		}
		return int64(f), nil
	case strings.HasPrefix(pgType, "numeric"):
		clean := strings.NewReplacer(" ", "", " ", "", " ", "").Replace(s)
		if strings.Count(clean, ",") == 1 && !strings.Contains(clean, ".") {
			clean = strings.Replace(clean, ",", ".", 1)
		} else {
			clean = strings.ReplaceAll(clean, ",", "")
		}
		var n pgtype.Numeric
		if err := n.Scan(clean); err != nil || !n.Valid {
			return nil, errors.New("nombre attendu")
		}
		return n, nil
	case pgType == "boolean":
		switch strings.ToLower(s) {
		case "true", "vrai", "oui", "yes", "1", "x", "o", "y":
			return true, nil
		case "false", "faux", "non", "no", "0", "n":
			return false, nil
		}
		return nil, errors.New("booléen attendu (oui/non, vrai/faux, 1/0)")
	case pgType == "date":
		t, err := parseTime(s, true)
		if err != nil {
			return nil, err
		}
		return pgtype.Date{Time: t, Valid: true}, nil
	case strings.HasPrefix(pgType, "timestamp"):
		t, err := parseTime(s, false)
		if err != nil {
			return nil, err
		}
		return t, nil
	}
	return *raw, nil
}

var dateLayouts = []string{"2006-01-02", "02/01/2006", "2/1/2006", "02-01-2006", "02.01.2006", "2006/01/02"}
var timeLayouts = []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04",
	"02/01/2006 15:04:05", "02/01/2006 15:04"}

// parseTime : formats ISO et français, ou numéro de série Excel.
func parseTime(s string, dateOnly bool) (time.Time, error) {
	if f, err := strconv.ParseFloat(strings.Replace(s, ",", ".", 1), 64); err == nil && f > 0 && f < 2958466 {
		// Excel : jours depuis le 30/12/1899.
		t := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC).Add(time.Duration(f * 24 * float64(time.Hour)))
		return t.Round(time.Second), nil
	}
	layouts := timeLayouts
	if dateOnly {
		layouts = append(append([]string{}, dateLayouts...), timeLayouts...)
	} else {
		layouts = append(append([]string{}, timeLayouts...), dateLayouts...)
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t, nil
		}
	}
	if dateOnly {
		return time.Time{}, errors.New("date attendue (AAAA-MM-JJ ou JJ/MM/AAAA)")
	}
	return time.Time{}, errors.New("date et heure attendues (AAAA-MM-JJ HH:MM)")
}
