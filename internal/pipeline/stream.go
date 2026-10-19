package pipeline

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// rowSource : source lue ligne à ligne (en-têtes, puis lignes de textes,
// nil : vide). Next renvoie io.EOF à la fin.
type rowSource interface {
	Header() []string
	Next() ([]*string, error)
	Close() error
}

// Table est une source entièrement lue (tests, petites sources).
type Table struct {
	Header []string
	Rows   [][]*string
}

// collect lit toute une source.
func collect(src rowSource, err error) (*Table, error) {
	if err != nil {
		return nil, err
	}
	defer src.Close()
	t := &Table{Header: src.Header()}
	for {
		row, err := src.Next()
		if err == io.EOF {
			return t, nil
		}
		if err != nil {
			return nil, err
		}
		t.Rows = append(t.Rows, row)
	}
}

// openFile ouvre un fichier local selon son format.
func openFile(format, path string, s Source) (rowSource, error) {
	switch format {
	case "csv":
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		src, err := newCSV(f, s.Delimiter, f)
		if err != nil {
			f.Close()
		}
		return src, err
	case "json":
		return newJSON(func() (io.ReadCloser, error) { return os.Open(path) }, s.JSONPath)
	case "xlsx":
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		st, err := f.Stat()
		if err == nil {
			var src rowSource
			if src, err = newXLSX(f, st.Size(), s.Sheet, f); err == nil {
				return src, nil
			}
		}
		f.Close()
		return nil, err
	}
	return nil, fmt.Errorf("format %q inconnu", format)
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// --- CSV -------------------------------------------------------------------------------

func parseCSV(b []byte, delim string) (*Table, error) {
	return collect(newCSV(bytes.NewReader(b), delim, nopCloser{}))
}

type csvSource struct {
	cr     *csv.Reader
	header []string
	closer io.Closer
}

// newCSV : BOM retiré, séparateur deviné sur la première ligne, fichiers
// Excel « CSV » en Windows-1252 / Latin-1 reconnus sur leur début.
func newCSV(r io.Reader, delim string, closer io.Closer) (rowSource, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	if bom, _ := br.Peek(3); bytes.Equal(bom, []byte("\xef\xbb\xbf")) {
		_, _ = br.Discard(3)
	}
	start, _ := br.Peek(64 << 10)
	sep := ';'
	switch delim {
	case ",", ";", "\t", "|":
		sep = rune(delim[0])
	default:
		first, _, _ := bytes.Cut(start, []byte("\n"))
		best := 0
		for _, c := range []rune{';', ',', '\t', '|'} {
			if n := bytes.Count(first, []byte(string(c))); n > best {
				best, sep = n, c
			}
		}
	}
	var in io.Reader = br
	if !validStart(start) {
		in = &latin1Reader{r: br}
	}
	cr := csv.NewReader(in)
	cr.Comma = sep
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	cr.ReuseRecord = true
	c := &csvSource{cr: cr, closer: closer}
	rec, err := cr.Read()
	if err == io.EOF {
		return nil, errors.New("CSV vide")
	}
	if err != nil {
		return nil, fmt.Errorf("CSV : %w", err)
	}
	for _, h := range rec {
		c.header = append(c.header, strings.TrimSpace(latin1IfInvalid(h)))
	}
	return c, nil
}

func (c *csvSource) Header() []string { return c.header }

func (c *csvSource) Next() ([]*string, error) {
	rec, err := c.cr.Read()
	if err == io.EOF {
		return nil, io.EOF
	}
	if err != nil {
		return nil, fmt.Errorf("CSV : %w", err)
	}
	row := make([]*string, len(rec))
	for i := range rec {
		v := latin1IfInvalid(rec[i])
		row[i] = &v
	}
	return row, nil
}

func (c *csvSource) Close() error { return c.closer.Close() }

// validStart : le début est de l'UTF-8 (un caractère coupé en fin de bloc compte).
func validStart(b []byte) bool {
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if r == utf8.RuneError && n <= 1 {
			return len(b) < utf8.UTFMax && !utf8.FullRune(b)
		}
		b = b[n:]
	}
	return true
}

// latin1IfInvalid : une valeur qui n'est pas de l'UTF-8 est lue en Latin-1.
func latin1IfInvalid(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	r := make([]rune, len(s))
	for i := 0; i < len(s); i++ {
		r[i] = rune(s[i])
	}
	return string(r)
}

// latin1Reader convertit un flux Latin-1 en UTF-8.
type latin1Reader struct {
	r   io.ByteReader
	out []byte
}

func (l *latin1Reader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if len(l.out) > 0 {
			c := copy(p[n:], l.out)
			l.out = l.out[c:]
			n += c
			continue
		}
		b, err := l.r.ReadByte()
		if err != nil {
			if n > 0 {
				return n, nil
			}
			return 0, err
		}
		if b < 0x80 {
			p[n] = b
			n++
			continue
		}
		l.out = utf8.AppendRune(l.out[:0], rune(b))
	}
	return n, nil
}

// --- JSON ------------------------------------------------------------------------------

func parseJSON(b []byte, path string) (*Table, error) {
	return collect(newJSON(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }, path))
}

// jsonSource lit un tableau d'objets en deux passes : les clés de tous les
// objets (en-têtes), puis les lignes, un objet à la fois.
type jsonSource struct {
	f      io.ReadCloser
	dec    *json.Decoder
	header []string
	index  map[string]int
}

func newJSON(open func() (io.ReadCloser, error), path string) (rowSource, error) {
	j := &jsonSource{index: map[string]int{}}
	f, err := open()
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bufio.NewReader(f))
	err = seekJSONArray(dec, path)
	rows := 0
	for err == nil && dec.More() {
		var obj map[string]json.RawMessage
		if err = dec.Decode(&obj); err != nil {
			err = errors.New("JSON : un tableau d'objets est attendu")
			break
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, ok := j.index[k]; !ok {
				j.index[k] = len(j.header)
				j.header = append(j.header, k)
			}
		}
		if rows++; rows > MaxRows {
			err = fmt.Errorf("plus de %d lignes", MaxRows)
		}
	}
	f.Close()
	if err != nil {
		return nil, err
	}
	if j.f, err = open(); err != nil {
		return nil, err
	}
	j.dec = json.NewDecoder(bufio.NewReader(j.f))
	if err := seekJSONArray(j.dec, path); err != nil {
		j.f.Close()
		return nil, err
	}
	return j, nil
}

// seekJSONArray place le décodeur au début du tableau désigné par path
// (« data.items »).
func seekJSONArray(dec *json.Decoder, path string) error {
	for _, k := range strings.FieldsFunc(path, func(r rune) bool { return r == '.' }) {
		if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
			return fmt.Errorf("JSON : « %s » introuvable", path)
		}
		found := false
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				return fmt.Errorf("JSON : %w", err)
			}
			if tok == k {
				found = true
				break
			}
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return fmt.Errorf("JSON : %w", err)
			}
		}
		if !found {
			return fmt.Errorf("JSON : « %s » introuvable", path)
		}
	}
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("JSON : %w", err)
	}
	if tok != json.Delim('[') {
		return errors.New("JSON : un tableau d'objets est attendu")
	}
	return nil
}

func (j *jsonSource) Header() []string { return j.header }

func (j *jsonSource) Next() ([]*string, error) {
	if !j.dec.More() {
		return nil, io.EOF
	}
	var obj map[string]any
	if err := j.dec.Decode(&obj); err != nil {
		return nil, errors.New("JSON : un tableau d'objets est attendu")
	}
	row := make([]*string, len(j.header))
	for k, val := range obj {
		i, ok := j.index[k]
		if val == nil || !ok {
			continue
		}
		var s string
		switch x := val.(type) {
		case string:
			s = x
		case float64:
			s = strconv.FormatFloat(x, 'f', -1, 64)
		case bool:
			s = strconv.FormatBool(x)
		default:
			bb, _ := json.Marshal(x)
			s = string(bb)
		}
		row[i] = &s
	}
	return row, nil
}

func (j *jsonSource) Close() error { return j.f.Close() }

// --- XLSX ------------------------------------------------------------------------------

func parseXLSX(b []byte, sheet string) (*Table, error) {
	return collect(newXLSX(bytes.NewReader(b), int64(len(b)), sheet, nopCloser{}))
}

// maxPart borne la taille décompressée d'une partie XML d'un classeur.
const maxPart = 2 * MaxBytes

// xlsxSource lit la première feuille (ou celle nommée) d'un classeur .xlsx,
// une ligne à la fois : textes partagés et en ligne, nombres (les dates
// restent des numéros de série Excel, convertis selon le type de la colonne
// cible).
type xlsxSource struct {
	strs   []string
	part   io.ReadCloser
	dec    *xml.Decoder
	header []string
	closer io.Closer
}

type xlsxRow struct {
	C []struct {
		R  string `xml:"r,attr"`
		T  string `xml:"t,attr"`
		V  string `xml:"v"`
		IS struct {
			T string `xml:"t"`
		} `xml:"is"`
	} `xml:"c"`
}

func newXLSX(ra io.ReaderAt, size int64, sheet string, closer io.Closer) (rowSource, error) {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return nil, fmt.Errorf("xlsx : %w", err)
	}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	open := func(name string) (io.ReadCloser, error) {
		f, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("xlsx : %s absent", name)
		}
		return f.Open()
	}
	read := func(name string, v any) error {
		rc, err := open(name)
		if err != nil {
			return err
		}
		defer rc.Close()
		return xml.NewDecoder(io.LimitReader(rc, maxPart)).Decode(v)
	}
	var shared struct {
		SI []struct {
			T string `xml:"t"`
			R []struct {
				T string `xml:"t"`
			} `xml:"r"`
		} `xml:"si"`
	}
	if _, ok := files["xl/sharedStrings.xml"]; ok {
		if err := read("xl/sharedStrings.xml", &shared); err != nil {
			return nil, err
		}
	}
	x := &xlsxSource{strs: make([]string, len(shared.SI)), closer: closer}
	for i, si := range shared.SI {
		s := si.T
		for _, r := range si.R {
			s += r.T
		}
		x.strs[i] = s
	}
	// Feuille : par son nom dans workbook.xml, sinon la première.
	var wb struct {
		Sheets []struct {
			Name string `xml:"name,attr"`
			RID  string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
		} `xml:"sheets>sheet"`
	}
	var rels struct {
		Rel []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
		} `xml:"Relationship"`
	}
	target := "xl/worksheets/sheet1.xml"
	if read("xl/workbook.xml", &wb) == nil && read("xl/_rels/workbook.xml.rels", &rels) == nil && len(wb.Sheets) > 0 {
		rid := wb.Sheets[0].RID
		for _, s := range wb.Sheets {
			if sheet != "" && strings.EqualFold(s.Name, sheet) {
				rid = s.RID
			}
		}
		for _, r := range rels.Rel {
			if r.ID == rid {
				target = "xl/" + strings.TrimPrefix(strings.TrimPrefix(r.Target, "/xl/"), "/")
			}
		}
	}
	if x.part, err = open(target); err != nil {
		return nil, err
	}
	x.dec = xml.NewDecoder(io.LimitReader(x.part, maxPart))
	first, err := x.Next()
	if err != nil {
		x.part.Close()
		if err == io.EOF {
			err = errors.New("xlsx : feuille vide")
		}
		return nil, err
	}
	for _, c := range first {
		h := ""
		if c != nil {
			h = strings.TrimSpace(*c)
		}
		x.header = append(x.header, h)
	}
	return x, nil
}

func (x *xlsxSource) Header() []string { return x.header }

func (x *xlsxSource) Next() ([]*string, error) {
	for {
		tok, err := x.dec.Token()
		if err == io.EOF {
			return nil, io.EOF
		}
		if err != nil {
			return nil, fmt.Errorf("xlsx : %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "row" {
			continue
		}
		var row xlsxRow
		if err := x.dec.DecodeElement(&row, &se); err != nil {
			return nil, fmt.Errorf("xlsx : %w", err)
		}
		var cells []*string
		for _, c := range row.C {
			col := colIndex(c.R)
			if col < 0 {
				col = len(cells)
			}
			if col > 16383 {
				return nil, fmt.Errorf("xlsx : colonne hors limite (%s)", c.R)
			}
			for len(cells) <= col {
				cells = append(cells, nil)
			}
			var v string
			switch c.T {
			case "s":
				n, err := strconv.Atoi(c.V)
				if err == nil && n >= 0 && n < len(x.strs) {
					v = x.strs[n]
				}
			case "inlineStr":
				v = c.IS.T
			case "b":
				v = map[string]string{"1": "true", "0": "false"}[c.V]
			default:
				v = c.V
			}
			if v != "" {
				cells[col] = &v
			}
		}
		return cells, nil
	}
}

func (x *xlsxSource) Close() error {
	x.part.Close()
	return x.closer.Close()
}

// colIndex : « C12 » → 2.
func colIndex(ref string) int {
	n := 0
	for _, c := range ref {
		if c < 'A' || c > 'Z' {
			break
		}
		n = n*26 + int(c-'A'+1)
	}
	return n - 1
}
