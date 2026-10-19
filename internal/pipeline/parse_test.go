package pipeline

import (
	"archive/zip"
	"bytes"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestParseCSV(t *testing.T) {
	// BOM, point-virgule détecté, guillemets, Latin-1.
	b := append([]byte("\xef\xbb\xbfCode;Libellé;Prix\nA1;\"Vis, inox\";1,50\nA2;Écrou;2\n"), []byte{}...)
	tb, err := parseCSV(b, "")
	if err != nil || len(tb.Header) != 3 || tb.Header[1] != "Libellé" || len(tb.Rows) != 2 || *tb.Rows[0][1] != "Vis, inox" {
		t.Fatalf("CSV : %+v %v", tb, err)
	}
	latin1 := []byte("Code,Libell\xe9\nB1,Caf\xe9\n")
	tb, err = parseCSV(latin1, "")
	if err != nil || tb.Header[1] != "Libellé" || *tb.Rows[0][1] != "Café" {
		t.Fatalf("CSV Latin-1 : %+v %v", tb, err)
	}
}

func TestParseJSON(t *testing.T) {
	tb, err := parseJSON([]byte(`{"data":{"items":[{"code":"A","prix":1.5,"actif":true},{"code":"B","prix":null}]}}`), "data.items")
	if err != nil || len(tb.Rows) != 2 {
		t.Fatalf("JSON : %+v %v", tb, err)
	}
	h := append([]string{}, tb.Header...)
	sort.Strings(h)
	if len(h) != 3 || h[0] != "actif" {
		t.Fatalf("en-têtes : %v", tb.Header)
	}
}

func TestParseXLSX(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, body string) {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(body))
	}
	add("xl/sharedStrings.xml", `<sst><si><t>Code</t></si><si><t>Date</t></si><si><r><t>A</t></r><r><t>1</t></r></si></sst>`)
	add("xl/workbook.xml", `<workbook xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Feuil1" r:id="rId1"/></sheets></workbook>`)
	add("xl/_rels/workbook.xml.rels", `<Relationships><Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`)
	add("xl/worksheets/sheet1.xml", `<worksheet><sheetData>
		<row><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row>
		<row><c r="A2" t="s"><v>2</v></c><c r="C2"><v>45580</v></c></row></sheetData></worksheet>`)
	_ = zw.Close()
	tb, err := parseXLSX(buf.Bytes(), "")
	if err != nil || len(tb.Header) != 2 || len(tb.Rows) != 1 || *tb.Rows[0][0] != "A1" || tb.Rows[0][1] != nil || *tb.Rows[0][2] != "45580" {
		t.Fatalf("xlsx : %+v %v", tb, err)
	}
}

func TestConvert(t *testing.T) {
	s := func(v string) *string { return &v }
	if v, err := convert(s("1 234,5"), "numeric"); err != nil || v.(pgtype.Numeric).Valid == false {
		t.Fatalf("décimal : %v %v", v, err)
	}
	if v, err := convert(s("12"), "bigint"); err != nil || v.(int64) != 12 {
		t.Fatalf("entier : %v %v", v, err)
	}
	if _, err := convert(s("12,5"), "bigint"); err == nil {
		t.Fatal("12,5 accepté comme entier")
	}
	if v, err := convert(s("18/10/2026"), "date"); err != nil || v.(pgtype.Date).Time.Day() != 18 {
		t.Fatalf("date : %v %v", v, err)
	}
	if v, err := convert(s("45580"), "date"); err != nil || v.(pgtype.Date).Time.Year() != 2024 {
		t.Fatalf("date Excel : %v %v", v, err)
	}
	if v, err := convert(s("Oui"), "boolean"); err != nil || v != true {
		t.Fatalf("booléen : %v %v", v, err)
	}
	if v, _ := convert(s("  "), "text"); v != nil {
		t.Fatal("vide : NULL attendu")
	}
	if v, err := convert(s("2026-10-18 06:30"), "timestamp with time zone"); err != nil || v.(time.Time).Hour() != 6 {
		t.Fatalf("date et heure : %v %v", v, err)
	}
}

func TestPositions(t *testing.T) {
	// Des milliers d'ajouts successifs : clés croissantes, valides, courtes.
	key := validKey("a5")
	prev := key
	for i := 0; i < 20000; i++ {
		key = afterKey(key)
		if key <= prev || key[len(key)-1] == '0' {
			t.Fatalf("clé %q après %q", key, prev)
		}
		prev = key
	}
	if len(key) > 6 {
		t.Fatalf("clé trop longue après 20 000 ajouts : %q", key)
	}
	if afterKey("") == "" || validKey("a50") != "a5" || validKey("a!b") != "a" {
		t.Fatal("cas limites")
	}
}
