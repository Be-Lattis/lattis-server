package pipeline

import (
	"context"
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

	"github.com/jackc/pgx/v5"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// MaxBytes borne la taille d'une source (fichier, réponse HTTP).
const MaxBytes = 100 << 20

// MaxRows borne le nombre de lignes lues, MaxCells lignes × colonnes.
const (
	MaxRows  = 2_000_000
	MaxCells = 20_000_000
)

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
	// AutoColumns : colonnes du tableau créées d'après la source (cible
	// « Créer automatiquement » du Data Lake).
	AutoColumns bool `json:"auto_columns"`
}

// openSource ouvre la source d'un pipeline, lue ensuite ligne à ligne : la
// mémoire ne dépend pas de sa taille. file : fichier envoyé ou déposé, à
// ranger après l'exécution.
func (r *Runner) openSource(ctx context.Context, p *Pipeline) (src rowSource, file string, err error) {
	if p.SourceKind == "sql" {
		src, err = r.querySQL(ctx, p)
		return src, "", err
	}
	path, file, cleanup, err := r.fetchFile(ctx, p)
	if err != nil {
		return nil, file, err
	}
	format := p.Format
	if format == "sql" {
		format = "csv"
	}
	src, err = openFile(format, path, p.Source)
	if err != nil {
		cleanup()
		return nil, file, err
	}
	return withCleanup{src, cleanup}, file, nil
}

// withCleanup efface la copie locale d'une source distante à la fermeture.
type withCleanup struct {
	rowSource
	cleanup func()
}

func (w withCleanup) Close() error {
	err := w.rowSource.Close()
	w.cleanup()
	return err
}

// fetchFile donne le chemin local d'une source de fichier. Envoi et dépôt
// sont lus sur place ; HTTP et SFTP sont d'abord copiés sur disque (bornés
// à MaxBytes), copie effacée par cleanup.
func (r *Runner) fetchFile(ctx context.Context, p *Pipeline) (path, file string, cleanup func(), err error) {
	cleanup = func() {}
	switch p.SourceKind {
	case "upload", "push":
		if p.PendingFile == "" {
			return "", "", cleanup, errors.New("aucun fichier envoyé : utilisez « Exécuter avec un fichier »")
		}
		path = filepath.Join(r.dir(p), "uploads", p.PendingFile)
		return path, path, cleanup, checkSize(path)
	case "file":
		path, err = r.nextDropped(p)
		if err != nil {
			return "", "", cleanup, err
		}
		return path, path, cleanup, checkSize(path)
	case "http", "sftp":
		if err := os.MkdirAll(r.dir(p), 0o750); err != nil {
			return "", "", cleanup, err
		}
		f, err := os.CreateTemp(r.dir(p), "source-*")
		if err != nil {
			return "", "", cleanup, err
		}
		cleanup = func() { f.Close(); os.Remove(f.Name()) }
		if p.SourceKind == "http" {
			err = r.fetchHTTP(ctx, p, f)
		} else {
			err = r.fetchSFTP(ctx, p, f)
		}
		if err == nil {
			err = f.Close()
		}
		if err != nil {
			cleanup()
			return "", "", func() {}, err
		}
		return f.Name(), "", cleanup, nil
	}
	return "", "", cleanup, fmt.Errorf("source %q sans fichier", p.SourceKind)
}

func checkSize(name string) error {
	st, err := os.Stat(name)
	if err != nil {
		return err
	}
	if st.Size() > MaxBytes {
		return fmt.Errorf("fichier trop gros (plus de %d Mo)", MaxBytes>>20)
	}
	return nil
}

// copyLimited copie au plus MaxBytes, sinon erreur.
func copyLimited(dst io.Writer, src io.Reader, what string) error {
	n, err := io.Copy(dst, io.LimitReader(src, MaxBytes+1))
	if err == nil && n > MaxBytes {
		err = fmt.Errorf("%s trop gros (plus de %d Mo)", what, MaxBytes>>20)
	}
	return err
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

func (r *Runner) fetchHTTP(ctx context.Context, p *Pipeline, dst io.Writer) error {
	u := p.Source.URL
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return errors.New("adresse http(s):// attendue")
	}
	if p.SecretRef != "" && !strings.HasPrefix(u, "https://") {
		return errors.New("un secret ne part que sur une adresse https://")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	for k, v := range p.Source.Headers {
		req.Header.Set(k, v)
	}
	if p.SecretRef != "" {
		secret, err := r.secret(p.SecretRef)
		if err != nil {
			return err
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
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("réponse HTTP %d", resp.StatusCode)
	}
	return copyLimited(dst, resp.Body, "réponse")
}

func (r *Runner) fetchSFTP(ctx context.Context, p *Pipeline, dst io.Writer) error {
	s := p.Source
	if s.Host == "" || s.User == "" || s.Path == "" {
		return errors.New("SFTP : hôte, utilisateur et chemin attendus")
	}
	if !strings.HasPrefix(s.HostKey, "SHA256:") {
		return errors.New("SFTP : empreinte de la clé du serveur (SHA256:…) obligatoire")
	}
	if p.SecretRef == "" {
		return errors.New("SFTP : secret (mot de passe ou clé privée) manquant")
	}
	secret, err := r.secret(p.SecretRef)
	if err != nil {
		return err
	}
	var auth ssh.AuthMethod
	if strings.Contains(secret, "PRIVATE KEY") {
		signer, err := ssh.ParsePrivateKey([]byte(secret))
		if err != nil {
			return fmt.Errorf("SFTP : clé privée illisible : %w", err)
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
	conn, err := r.dialer(s.Host).DialContext(ctx, "tcp", net.JoinHostPort(s.Host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, s.Host, cfg)
	if err != nil {
		conn.Close()
		return err
	}
	client := ssh.NewClient(c, chans, reqs)
	defer client.Close()
	sc, err := sftp.NewClient(client)
	if err != nil {
		return err
	}
	defer sc.Close()
	f, err := sc.Open(s.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	return copyLimited(dst, f, "fichier")
}

// querySQL lit une base PostgreSQL externe, en lecture seule, ligne à ligne.
func (r *Runner) querySQL(ctx context.Context, p *Pipeline) (rowSource, error) {
	if p.SecretRef == "" || strings.TrimSpace(p.Source.Query) == "" {
		return nil, errors.New("SQL : chaîne de connexion (secret) et requête attendues")
	}
	dsn, err := r.secret(p.SecretRef)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	conn, err := pgx.Connect(cctx, strings.TrimSpace(dsn))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("SQL : connexion : %w", err)
	}
	q := &sqlSource{conn: conn, cancel: cancel}
	q.tx, err = conn.BeginTx(cctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err == nil {
		q.rows, err = q.tx.Query(cctx, p.Source.Query, pgx.QueryResultFormats{pgx.TextFormatCode})
		if err != nil {
			err = fmt.Errorf("SQL : %w", err)
		}
	}
	if err != nil {
		q.Close()
		return nil, err
	}
	for _, f := range q.rows.FieldDescriptions() {
		q.header = append(q.header, f.Name)
	}
	return q, nil
}

type sqlSource struct {
	conn   *pgx.Conn
	tx     pgx.Tx
	rows   pgx.Rows
	cancel context.CancelFunc
	header []string
}

func (q *sqlSource) Header() []string { return q.header }

func (q *sqlSource) Next() ([]*string, error) {
	if !q.rows.Next() {
		if err := q.rows.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	raw := q.rows.RawValues()
	row := make([]*string, len(raw))
	for i, v := range raw {
		if v != nil {
			s := string(v)
			row[i] = &s
		}
	}
	return row, nil
}

func (q *sqlSource) Close() error {
	if q.rows != nil {
		q.rows.Close()
	}
	if q.tx != nil {
		_ = q.tx.Rollback(context.Background())
	}
	err := q.conn.Close(context.Background())
	q.cancel()
	return err
}
