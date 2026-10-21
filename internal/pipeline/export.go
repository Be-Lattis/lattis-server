package pipeline

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Pipelines OUT (direction « out ») : le tableau cible est lu sous
// l'identité technique (lecteur) et déversé vers l'extérieur, avec les
// mêmes protocoles que l'ingestion :
//   - file : fichier dans <dossier>/<pipeline>/exports (téléchargeable
//     depuis l'administration, les 20 derniers gardés) ;
//   - http : POST (ou PUT) du fichier vers une adresse ;
//   - sftp : dépôt du fichier sur un serveur SFTP ({date} dans le chemin) ;
//   - sql : lignes copiées dans une table PostgreSQL externe (replace : la
//     table est vidée d'abord ; append : ajoutées).
// Formats de fichier : csv, json, xlsx.

// keepExports : fichiers d'export gardés par pipeline.
const keepExports = 20

// column : colonne exportée et son type PostgreSQL.
type column struct{ name, pgType string }

// ExportDir : dossier des fichiers exportés d'un pipeline.
func (r *Runner) ExportDir(id string) string { return filepath.Join(r.Dir, id, "exports") }

// export exécute un pipeline OUT ; dry : compte seulement les lignes.
func (r *Runner) export(ctx context.Context, p *Pipeline, dry bool) (Result, error) {
	var res Result
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
	ext := map[string]string{"csv": "csv", "json": "json", "xlsx": "xlsx"}[p.Format]
	if ext == "" && p.SourceKind != "sql" {
		return res, fmt.Errorf("format d'export : csv, json ou xlsx, pas %q", p.Format)
	}
	err = r.Users.As(ctx, u, func(tx pgx.Tx) error {
		tbl := pgx.Identifier{schema, p.TableName}
		cols, err := exportColumns(ctx, tx, tbl)
		if err != nil {
			return err
		}
		names := make([]string, len(cols))
		for i, c := range cols {
			names[i] = pgx.Identifier{c.name}.Sanitize()
		}
		rows, err := tx.Query(ctx, `SELECT `+strings.Join(names, ", ")+` FROM `+tbl.Sanitize()+` ORDER BY lattis_pos COLLATE "C"`,
			pgx.QueryResultFormats{pgx.TextFormatCode})
		if err != nil {
			return err
		}
		defer rows.Close()
		next := func() ([]*string, error) {
			if !rows.Next() {
				if err := rows.Err(); err != nil {
					return nil, err
				}
				return nil, io.EOF
			}
			raw := rows.RawValues()
			vals := make([]*string, len(raw))
			for i, b := range raw {
				if b != nil {
					s := string(b)
					vals[i] = &s
				}
			}
			res.Read++
			if res.Read > MaxRows {
				return nil, fmt.Errorf("plus de %d lignes", MaxRows)
			}
			return vals, nil
		}
		if dry {
			for {
				if _, err := next(); err == io.EOF {
					break
				} else if err != nil {
					return err
				}
			}
			res.Inserted = res.Read
			return nil
		}
		if p.SourceKind == "sql" {
			n, err := r.exportSQL(ctx, p, cols, next)
			res.Inserted = n
			return err
		}
		// Fichier écrit d'abord sur disque, puis remis à sa destination.
		dir := r.ExportDir(p.ID)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
		name := safeBase(p.Name) + "-" + r.now().Format("20060102-150405") + "." + ext
		tmp := filepath.Join(dir, "."+name+".part")
		f, err := os.Create(tmp)
		if err != nil {
			return err
		}
		werr := writeFile(f, p.Format, p.Source.Delimiter, cols, next)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			os.Remove(tmp)
			return werr
		}
		res.Inserted = res.Read
		final := filepath.Join(dir, name)
		if err := os.Rename(tmp, final); err != nil {
			return err
		}
		pruneExports(dir)
		switch p.SourceKind {
		case "file":
			return nil
		case "http":
			return r.exportHTTP(ctx, p, final, p.Format)
		case "sftp":
			return r.exportSFTP(ctx, p, final)
		}
		return fmt.Errorf("destination inconnue : %s", p.SourceKind)
	})
	return res, err
}

func exportColumns(ctx context.Context, tx pgx.Tx, tbl pgx.Identifier) ([]column, error) {
	rows, err := tx.Query(ctx, `SELECT attname, format_type(atttypid, atttypmod) FROM pg_attribute
		WHERE attrelid = $1::regclass AND attnum > 0 AND NOT attisdropped AND attname NOT LIKE 'lattis\_%' ORDER BY attnum`, tbl.Sanitize())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []column
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.name, &c.pgType); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, errors.New("tableau sans colonne à exporter")
	}
	return cols, nil
}

func safeBase(name string) string {
	b := strings.Trim(unsafeChars.Replace(strings.TrimSpace(name)), "._-")
	if b == "" {
		b = "export"
	}
	if len(b) > 80 {
		b = b[:80]
	}
	return b
}

var unsafeChars = strings.NewReplacer(" ", "_", "/", "_", `\`, "_", ":", "_", "*", "_", "?", "_", `"`, "_", "<", "_", ">", "_", "|", "_")

// pruneExports garde les keepExports fichiers les plus récents.
func pruneExports(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files) // la date est dans le nom : ordre chronologique par nom de pipeline
	if len(files) > keepExports {
		for _, f := range files[:len(files)-keepExports] {
			os.Remove(filepath.Join(dir, f))
		}
	}
}

// LatestExport : dernier fichier exporté d'un pipeline ("" sans).
func (r *Runner) LatestExport(id string) string {
	entries, err := os.ReadDir(r.ExportDir(id))
	if err != nil {
		return ""
	}
	var best string
	var at time.Time
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().After(at) {
			best, at = e.Name(), info.ModTime()
		}
	}
	if best == "" {
		return ""
	}
	return filepath.Join(r.ExportDir(id), best)
}

func writeFile(w io.Writer, format, delim string, cols []column, next func() ([]*string, error)) error {
	switch format {
	case "json":
		return writeJSON(w, cols, next)
	case "xlsx":
		return writeXLSX(w, cols, next)
	}
	return writeCSV(w, delim, cols, next)
}

func writeCSV(w io.Writer, delim string, cols []column, next func() ([]*string, error)) error {
	bw := bufio.NewWriter(w)
	cw := csv.NewWriter(bw)
	cw.Comma = ';'
	if r := []rune(delim); len(r) == 1 {
		cw.Comma = r[0]
	}
	head := make([]string, len(cols))
	for i, c := range cols {
		head[i] = c.name
	}
	if err := cw.Write(head); err != nil {
		return err
	}
	rec := make([]string, len(cols))
	for {
		row, err := next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		for i := range rec {
			rec[i] = ""
			if row[i] != nil {
				rec[i] = *row[i]
			}
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return err
	}
	return bw.Flush()
}

// writeJSON : tableau d'objets ; nombres et booléens en valeurs JSON.
func writeJSON(w io.Writer, cols []column, next func() ([]*string, error)) error {
	bw := bufio.NewWriter(w)
	keys := make([][]byte, len(cols))
	for i, c := range cols {
		keys[i], _ = json.Marshal(c.name)
	}
	bw.WriteString("[")
	first := true
	for {
		row, err := next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if !first {
			bw.WriteString(",")
		}
		first = false
		bw.WriteString("\n{")
		for i, c := range cols {
			if i > 0 {
				bw.WriteString(",")
			}
			bw.Write(keys[i])
			bw.WriteString(":")
			bw.Write(jsonValue(row[i], c.pgType))
		}
		bw.WriteString("}")
	}
	bw.WriteString("\n]\n")
	return bw.Flush()
}

func jsonValue(v *string, pgType string) []byte {
	if v == nil {
		return []byte("null")
	}
	switch {
	case pgType == "bigint" || pgType == "integer" || strings.HasPrefix(pgType, "numeric"):
		if _, err := strconv.ParseFloat(*v, 64); err == nil {
			return []byte(*v)
		}
	case pgType == "boolean":
		if *v == "t" {
			return []byte("true")
		}
		return []byte("false")
	}
	b, _ := json.Marshal(*v)
	return b
}

// writeXLSX : classeur d'une feuille, en-têtes en première ligne ; nombres
// en cellules numériques, le reste en texte.
func writeXLSX(w io.Writer, cols []column, next func() ([]*string, error)) error {
	z := zip.NewWriter(w)
	add := func(name, body string) error {
		f, err := z.Create(name)
		if err != nil {
			return err
		}
		_, err = io.WriteString(f, body)
		return err
	}
	if err := add("[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/></Types>`); err != nil {
		return err
	}
	if err := add("_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`); err != nil {
		return err
	}
	if err := add("xl/workbook.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Export" sheetId="1" r:id="rId1"/></sheets></workbook>`); err != nil {
		return err
	}
	if err := add("xl/_rels/workbook.xml.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/></Relationships>`); err != nil {
		return err
	}
	f, err := z.Create("xl/worksheets/sheet1.xml")
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(f)
	bw.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n" +
		`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	text := func(s string) {
		bw.WriteString(`<c t="inlineStr"><is><t xml:space="preserve">`)
		bw.WriteString(html.EscapeString(xmlSafe(s)))
		bw.WriteString(`</t></is></c>`)
	}
	bw.WriteString("<row>")
	for _, c := range cols {
		text(c.name)
	}
	bw.WriteString("</row>")
	for {
		row, err := next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		bw.WriteString("<row>")
		for i, c := range cols {
			v := row[i]
			switch {
			case v == nil:
				bw.WriteString("<c/>")
			case c.pgType == "bigint" || c.pgType == "integer" || strings.HasPrefix(c.pgType, "numeric"):
				bw.WriteString("<c><v>" + html.EscapeString(*v) + "</v></c>")
			case c.pgType == "boolean":
				text(map[bool]string{true: "VRAI", false: "FAUX"}[*v == "t"])
			default:
				text(*v)
			}
		}
		bw.WriteString("</row>")
	}
	bw.WriteString("</sheetData></worksheet>")
	if err := bw.Flush(); err != nil {
		return err
	}
	return z.Close()
}

// xmlSafe retire les caractères de contrôle interdits en XML.
func xmlSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return -1
		}
		return r
	}, s)
}

var contentTypes = map[string]string{"csv": "text/csv; charset=utf-8", "json": "application/json",
	"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"}

func (r *Runner) exportHTTP(ctx context.Context, p *Pipeline, path, format string) error {
	u := p.Source.URL
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return errors.New("adresse http(s):// attendue")
	}
	if p.SecretRef != "" && !strings.HasPrefix(u, "https://") {
		return errors.New("un secret ne part que sur une adresse https://")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	method := http.MethodPost
	if strings.EqualFold(p.Source.Method, "PUT") {
		method = http.MethodPut
	}
	if err := r.teamGuardURL(ctx, p, u); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, f)
	if err != nil {
		return err
	}
	if info, err := f.Stat(); err == nil {
		req.ContentLength = info.Size()
	}
	req.Header.Set("Content-Type", contentTypes[format])
	if err := r.authorize(req, p); err != nil {
		return err
	}
	resp, err := r.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("réponse HTTP %d", resp.StatusCode)
	}
	return nil
}

func (r *Runner) exportSFTP(ctx context.Context, p *Pipeline, path string) error {
	sc, done, err := r.sftpClient(ctx, p)
	if err != nil {
		return err
	}
	defer done()
	dst := strings.ReplaceAll(p.Source.Path, "{date}", r.now().Format("20060102-150405"))
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := sc.Create(dst)
	if err != nil {
		return fmt.Errorf("SFTP : %w", err)
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return fmt.Errorf("SFTP : %w", err)
	}
	return out.Close()
}

// exportSQL copie les lignes dans une table PostgreSQL externe (schéma.table
// dans source.table) ; replace : vidée d'abord, dans la même transaction.
func (r *Runner) exportSQL(ctx context.Context, p *Pipeline, cols []column, next func() ([]*string, error)) (int, error) {
	if p.SecretRef == "" || strings.TrimSpace(p.Source.Table) == "" {
		return 0, errors.New("SQL : chaîne de connexion (secret) et table de destination attendues")
	}
	dsn, err := r.secret(p.SecretRef)
	if err != nil {
		return 0, err
	}
	if err := r.teamGuardDSN(ctx, p, dsn); err != nil {
		return 0, err
	}
	conn, err := pgx.Connect(ctx, strings.TrimSpace(dsn))
	if err != nil {
		return 0, fmt.Errorf("SQL : connexion : %w", err)
	}
	defer conn.Close(context.Background())
	tbl := pgx.Identifier(strings.Split(strings.TrimSpace(p.Source.Table), "."))
	n := 0
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if p.Mode == "replace" {
			if _, err := tx.Exec(ctx, `DELETE FROM `+tbl.Sanitize()); err != nil {
				return fmt.Errorf("SQL : %w", err)
			}
		}
		names := make([]string, len(cols))
		for i, c := range cols {
			names[i] = c.name
		}
		src := &copyRows{next: next, cols: cols}
		c, err := tx.CopyFrom(ctx, tbl, names, src)
		n = int(c)
		if src.err != nil {
			return src.err
		}
		if err != nil {
			return fmt.Errorf("SQL : %w", err)
		}
		return nil
	})
	return n, err
}

// copyRows passe les lignes lues (texte) à COPY.
type copyRows struct {
	next func() ([]*string, error)
	cols []column
	row  []any
	err  error
}

func (c *copyRows) Next() bool {
	row, err := c.next()
	if err != nil {
		if err != io.EOF {
			c.err = err
		}
		return false
	}
	c.row = make([]any, len(row))
	for i, v := range row {
		// Valeur typée (COPY binaire) d'après le type de la colonne exportée.
		val, err := convert(v, c.cols[i].pgType)
		if err != nil {
			c.err = fmt.Errorf("colonne « %s » : %w", c.cols[i].name, err)
			return false
		}
		c.row[i] = val
	}
	return true
}

func (c *copyRows) Values() ([]any, error) { return c.row, nil }
func (c *copyRows) Err() error             { return c.err }
