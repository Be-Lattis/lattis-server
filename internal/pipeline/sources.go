package pipeline

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// MaxBytes borne la taille d'une source (fichier, réponse HTTP).
const MaxBytes = 100 << 20

// MaxRows borne le nombre de lignes lues.
const MaxRows = 2_000_000

// Table est une source lue : en-têtes, puis lignes de textes (nil : vide).
type Table struct {
	Header []string
	Rows   [][]*string
}

// Source décrit où lire (colonne source de lattis.pipelines).
type Source struct {
	URL       string            `json:"url"`
	Auth      string            `json:"auth"` // http : bearer, header, basic
	Header    string            `json:"header"`
	Headers   map[string]string `json:"headers"`
	Host      string            `json:"host"`
	Port      int               `json:"port"`
	User      string            `json:"user"`
	Path      string            `json:"path"`
	HostKey   string            `json:"host_key"` // empreinte SHA256:… de la clé du serveur SFTP
	Pattern   string            `json:"pattern"`
	Query     string            `json:"query"`
	Sheet     string            `json:"sheet"`
	Delimiter string            `json:"delimiter"`
	JSONPath  string            `json:"json_path"`
}

// fetchBytes lit une source de fichier (envoi, dépôt, HTTP, SFTP).
func (r *Runner) fetchBytes(ctx context.Context, p *Pipeline) ([]byte, string, error) {
	switch p.SourceKind {
	case "upload":
		if p.PendingFile == "" {
			return nil, "", errors.New("aucun fichier envoyé : utilisez « Exécuter avec un fichier »")
		}
		name := filepath.Join(r.dir(p), "uploads", p.PendingFile)
		b, err := readLimited(name)
		return b, name, err
	case "file":
		name, err := r.nextDropped(p)
		if err != nil {
			return nil, "", err
		}
		b, err := readLimited(name)
		return b, name, err
	case "http":
		return r.fetchHTTP(ctx, p)
	case "sftp":
		b, err := r.fetchSFTP(ctx, p)
		return b, p.Source.Path, err
	}
	return nil, "", fmt.Errorf("source %q sans fichier", p.SourceKind)
}

func readLimited(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err == nil && len(b) > MaxBytes {
		err = fmt.Errorf("fichier trop gros (plus de %d Mo)", MaxBytes>>20)
	}
	return b, err
}

// nextDropped : plus ancien fichier du dossier de dépôt qui correspond au motif.
func (r *Runner) nextDropped(p *Pipeline) (string, error) {
	inbox := filepath.Join(r.dir(p), "inbox")
	if err := os.MkdirAll(inbox, 0o750); err != nil {
		return "", err
	}
	pattern := p.Source.Pattern
	if pattern == "" {
		pattern = "*"
	}
	matches, err := filepath.Glob(filepath.Join(inbox, pattern))
	if err != nil {
		return "", fmt.Errorf("motif invalide : %w", err)
	}
	type file struct {
		name string
		mod  time.Time
	}
	var files []file
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && st.Mode().IsRegular() {
			files = append(files, file{m, st.ModTime()})
		}
	}
	if len(files) == 0 {
		return "", errNothing
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	return files[0].name, nil
}

// errNothing : rien à lire (dossier de dépôt vide) ; l'exécution n'est pas une erreur.
var errNothing = errors.New("aucun fichier à traiter")

func (r *Runner) fetchHTTP(ctx context.Context, p *Pipeline) ([]byte, string, error) {
	u := p.Source.URL
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return nil, "", errors.New("adresse http(s):// attendue")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	for k, v := range p.Source.Headers {
		req.Header.Set(k, v)
	}
	if p.SecretRef != "" {
		secret, err := r.secret(p.SecretRef)
		if err != nil {
			return nil, "", err
		}
		switch p.Source.Auth {
		case "header":
			req.Header.Set(p.Source.Header, secret)
		case "basic":
			user, pass, _ := strings.Cut(secret, ":")
			req.SetBasicAuth(user, pass)
		default:
			req.Header.Set("Authorization", "Bearer "+secret)
		}
	}
	resp, err := r.client().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, "", fmt.Errorf("réponse HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxBytes+1))
	if err == nil && len(b) > MaxBytes {
		err = fmt.Errorf("réponse trop grosse (plus de %d Mo)", MaxBytes>>20)
	}
	return b, u, err
}

func (r *Runner) fetchSFTP(ctx context.Context, p *Pipeline) ([]byte, error) {
	s := p.Source
	if s.Host == "" || s.User == "" || s.Path == "" {
		return nil, errors.New("SFTP : hôte, utilisateur et chemin attendus")
	}
	if !strings.HasPrefix(s.HostKey, "SHA256:") {
		return nil, errors.New("SFTP : empreinte de la clé du serveur (SHA256:…) obligatoire")
	}
	if p.SecretRef == "" {
		return nil, errors.New("SFTP : secret (mot de passe ou clé privée) manquant")
	}
	secret, err := r.secret(p.SecretRef)
	if err != nil {
		return nil, err
	}
	var auth ssh.AuthMethod
	if strings.Contains(secret, "PRIVATE KEY") {
		signer, err := ssh.ParsePrivateKey([]byte(secret))
		if err != nil {
			return nil, fmt.Errorf("SFTP : clé privée illisible : %w", err)
		}
		auth = ssh.PublicKeys(signer)
	} else {
		auth = ssh.Password(secret)
	}
	port := s.Port
	if port == 0 {
		port = 22
	}
	cfg := &ssh.ClientConfig{User: s.User, Auth: []ssh.AuthMethod{auth}, Timeout: 30 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if ssh.FingerprintSHA256(key) != s.HostKey {
				return fmt.Errorf("SFTP : clé du serveur inattendue (%s)", ssh.FingerprintSHA256(key))
			}
			return nil
		}}
	d := net.Dialer{Timeout: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(s.Host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, s.Host, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	client := ssh.NewClient(c, chans, reqs)
	defer client.Close()
	sc, err := sftp.NewClient(client)
	if err != nil {
		return nil, err
	}
	defer sc.Close()
	f, err := sc.Open(s.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err == nil && len(b) > MaxBytes {
		err = fmt.Errorf("fichier trop gros (plus de %d Mo)", MaxBytes>>20)
	}
	return b, err
}

// fetchSQL lit une base PostgreSQL externe, en lecture seule.
func (r *Runner) fetchSQL(ctx context.Context, p *Pipeline) (*Table, error) {
	if p.SecretRef == "" || strings.TrimSpace(p.Source.Query) == "" {
		return nil, errors.New("SQL : chaîne de connexion (secret) et requête attendues")
	}
	dsn, err := r.secret(p.SecretRef)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(cctx, strings.TrimSpace(dsn))
	if err != nil {
		return nil, fmt.Errorf("SQL : connexion : %w", err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.BeginTx(cctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	rows, err := tx.Query(cctx, p.Source.Query, pgx.QueryResultFormats{pgx.TextFormatCode})
	if err != nil {
		return nil, fmt.Errorf("SQL : %w", err)
	}
	defer rows.Close()
	t := &Table{}
	for _, f := range rows.FieldDescriptions() {
		t.Header = append(t.Header, f.Name)
	}
	for rows.Next() {
		raw := rows.RawValues()
		row := make([]*string, len(raw))
		for i, v := range raw {
			if v != nil {
				s := string(v)
				row[i] = &s
			}
		}
		t.Rows = append(t.Rows, row)
		if len(t.Rows) > MaxRows {
			return nil, fmt.Errorf("plus de %d lignes", MaxRows)
		}
	}
	return t, rows.Err()
}

// parse lit un fichier selon son format.
func parse(format string, b []byte, s Source) (*Table, error) {
	switch format {
	case "csv":
		return parseCSV(b, s.Delimiter)
	case "xlsx":
		return parseXLSX(b, s.Sheet)
	case "json":
		return parseJSON(b, s.JSONPath)
	}
	return nil, fmt.Errorf("format %q inconnu", format)
}

func parseCSV(b []byte, delim string) (*Table, error) {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(b) {
		// Fichiers Excel « CSV » enregistrés en Windows-1252 / Latin-1.
		r := make([]rune, len(b))
		for i, c := range b {
			r[i] = rune(c)
		}
		b = []byte(string(r))
	}
	sep := ';'
	switch delim {
	case ",", ";", "\t", "|":
		sep = rune(delim[0])
	default:
		first, _, _ := bytes.Cut(b, []byte("\n"))
		best := 0
		for _, c := range []rune{';', ',', '\t', '|'} {
			if n := bytes.Count(first, []byte(string(c))); n > best {
				best, sep = n, c
			}
		}
	}
	cr := csv.NewReader(bytes.NewReader(b))
	cr.Comma = sep
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	t := &Table{}
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("CSV : %w", err)
		}
		if t.Header == nil {
			for _, h := range rec {
				t.Header = append(t.Header, strings.TrimSpace(h))
			}
			continue
		}
		row := make([]*string, len(rec))
		for i := range rec {
			v := rec[i]
			row[i] = &v
		}
		t.Rows = append(t.Rows, row)
		if len(t.Rows) > MaxRows {
			return nil, fmt.Errorf("plus de %d lignes", MaxRows)
		}
	}
	if t.Header == nil {
		return nil, errors.New("CSV vide")
	}
	return t, nil
}

func parseJSON(b []byte, path string) (*Table, error) {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("JSON : %w", err)
	}
	for _, k := range strings.FieldsFunc(path, func(r rune) bool { return r == '.' }) {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("JSON : « %s » introuvable", path)
		}
		v = m[k]
	}
	list, ok := v.([]any)
	if !ok {
		return nil, errors.New("JSON : un tableau d'objets est attendu")
	}
	t := &Table{}
	index := map[string]int{}
	for _, it := range list {
		obj, ok := it.(map[string]any)
		if !ok {
			return nil, errors.New("JSON : un tableau d'objets est attendu")
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, ok := index[k]; !ok {
				index[k] = len(t.Header)
				t.Header = append(t.Header, k)
			}
		}
	}
	for _, it := range list {
		obj := it.(map[string]any)
		row := make([]*string, len(t.Header))
		for k, val := range obj {
			if val == nil {
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
			row[index[k]] = &s
		}
		t.Rows = append(t.Rows, row)
		if len(t.Rows) > MaxRows {
			return nil, fmt.Errorf("plus de %d lignes", MaxRows)
		}
	}
	return t, nil
}

// parseXLSX lit la première feuille (ou celle nommée) d'un classeur .xlsx :
// textes partagés et en ligne, nombres (les dates restent des numéros de série
// Excel, convertis selon le type de la colonne cible).
func parseXLSX(b []byte, sheet string) (*Table, error) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, fmt.Errorf("xlsx : %w", err)
	}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	read := func(name string, v any) error {
		f, ok := files[name]
		if !ok {
			return fmt.Errorf("xlsx : %s absent", name)
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		return xml.NewDecoder(io.LimitReader(rc, MaxBytes*4)).Decode(v)
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
	strs := make([]string, len(shared.SI))
	for i, si := range shared.SI {
		s := si.T
		for _, r := range si.R {
			s += r.T
		}
		strs[i] = s
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
	var ws struct {
		Rows []struct {
			C []struct {
				R  string `xml:"r,attr"`
				T  string `xml:"t,attr"`
				V  string `xml:"v"`
				IS struct {
					T string `xml:"t"`
				} `xml:"is"`
			} `xml:"c"`
		} `xml:"sheetData>row"`
	}
	if err := read(target, &ws); err != nil {
		return nil, err
	}
	t := &Table{}
	for _, row := range ws.Rows {
		var cells []*string
		for _, c := range row.C {
			col := colIndex(c.R)
			if col < 0 {
				col = len(cells)
			}
			for len(cells) <= col {
				cells = append(cells, nil)
			}
			var v string
			switch c.T {
			case "s":
				n, err := strconv.Atoi(c.V)
				if err == nil && n >= 0 && n < len(strs) {
					v = strs[n]
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
		if t.Header == nil {
			for _, c := range cells {
				h := ""
				if c != nil {
					h = strings.TrimSpace(*c)
				}
				t.Header = append(t.Header, h)
			}
			continue
		}
		t.Rows = append(t.Rows, cells)
		if len(t.Rows) > MaxRows {
			return nil, fmt.Errorf("plus de %d lignes", MaxRows)
		}
	}
	if t.Header == nil {
		return nil, errors.New("xlsx : feuille vide")
	}
	return t, nil
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
