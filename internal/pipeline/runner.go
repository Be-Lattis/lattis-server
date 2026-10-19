// Package pipeline exécute les pipelines d'ingestion (lattis.pipelines) :
// lecture d'une source, conversion selon les types du tableau cible, puis
// écriture sous l'identité technique du pipeline (RLS, déclencheurs et journal
// comme pour une personne). Plusieurs lattisd peuvent tourner : chaque
// exécution est réservée par FOR UPDATE SKIP LOCKED.
package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/userdb"
)

// Interval : période du relevé des pipelines dus.
const Interval = 15 * time.Second

// Pipeline est une ligne de lattis.pipelines.
type Pipeline struct {
	ID, Name, WorkbookID, TableName string
	TableID                         string
	SourceKind, Format, Mode        string
	Source                          Source
	SecretRef                       string
	KeyColumns                      []string
	ColumnMap                       map[string]string // colonne cible → colonne source
	ScheduleKind                    string
	ScheduleMinutes                 int
	ScheduleTime                    string
	PendingFile, PendingTrigger     string
	WebhookURL                      string
	UserID                          string
}

// Result est le bilan d'une exécution.
type Result struct {
	Status                                     string // ok, warning, error
	Read, Inserted, Updated, Deleted, Rejected int
	Rejects                                    []Reject
	Err                                        string
}

// Reject est une ligne écartée (les 20 premières sont gardées).
type Reject struct {
	Row    int    `json:"row"`
	Column string `json:"column,omitempty"`
	Value  string `json:"value,omitempty"`
	Error  string `json:"error"`
}

// Counter compte les exécutions par statut (mesures).
type Counter interface {
	Inc(status string)
}

// Runner relève et exécute les pipelines.
type Runner struct {
	Store      *store.Store
	Users      userdb.Runner
	Dir        string // dossier des fichiers (LATTIS_PIPELINE_DIR)
	SecretsDir string
	AdminURL   string // pour les alertes
	Metrics    Counter
	// AllowedHosts : hôtes ou réseaux internes que les sources et alertes
	// peuvent joindre malgré la garde réseau (LATTIS_PIPELINE_ALLOWED_HOSTS).
	AllowedHosts []string
	// HTTP remplace le client des sources et alertes (tests) ; nil : client
	// soumis à la garde réseau.
	HTTP *http.Client
	Now  func() time.Time
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) client() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return r.guardedClient(5 * time.Minute)
}

func (r *Runner) dir(p *Pipeline) string { return filepath.Join(r.Dir, p.ID) }

func (r *Runner) secret(ref string) (string, error) {
	if r.SecretsDir == "" {
		return "", fmt.Errorf("secret %q : LATTIS_PIPELINE_SECRETS_DIR manquant", ref)
	}
	if strings.ContainsAny(ref, `/\`) || ref == ".." || ref == "." {
		return "", fmt.Errorf("référence de secret invalide : %q", ref)
	}
	b, err := os.ReadFile(filepath.Join(r.SecretsDir, ref))
	if err != nil {
		return "", fmt.Errorf("secret %q : %w", ref, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// Run relève les pipelines dus jusqu'à l'arrêt du contexte.
// Workers : exécutions simultanées par lattisd.
const Workers = 3

// MaxRun : durée maximale d'une exécution ; au-delà, une exécution « en
// cours » est tenue pour interrompue (arrêt de lattisd).
const MaxRun = 30 * time.Minute

func (r *Runner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(Interval)
			defer t.Stop()
			for {
				for {
					ran, err := r.RunDue(ctx)
					if err != nil {
						slog.Error("pipelines", "err", err)
					}
					if !ran || ctx.Err() != nil {
						break
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}
	go r.housekeeping(ctx)
	wg.Wait()
}

// housekeeping : exécutions interrompues passées en échec, historique borné.
func (r *Runner) housekeeping(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := r.Clean(ctx); err != nil {
			slog.Warn("pipelines : nettoyage", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Clean passe en échec les exécutions restées « en cours » au-delà de MaxRun
// et garde les 200 dernières exécutions de chaque pipeline.
func (r *Runner) Clean(ctx context.Context) error {
	limit := r.now().Add(-MaxRun - 5*time.Minute)
	if _, err := r.Store.DB.Exec(ctx, `UPDATE lattis.pipeline_runs SET status = 'error', finished_at = now(),
		error = 'exécution interrompue (arrêt de lattisd)' WHERE status = 'running' AND started_at < $1`, limit); err != nil {
		return err
	}
	if _, err := r.Store.DB.Exec(ctx, `UPDATE lattis.pipelines SET running_since = NULL WHERE running_since < $1`, limit); err != nil {
		return err
	}
	_, err := r.Store.DB.Exec(ctx, `DELETE FROM lattis.pipeline_runs r WHERE r.id IN (
		SELECT id FROM (SELECT id, row_number() OVER (PARTITION BY pipeline_id ORDER BY started_at DESC) AS n
		                FROM lattis.pipeline_runs) x WHERE x.n > 200)`)
	return err
}

// RunDue planifie les pipelines sans échéance, puis exécute au plus un
// pipeline dû. Vrai si un pipeline a tourné.
func (r *Runner) RunDue(ctx context.Context) (bool, error) {
	if err := r.schedule(ctx); err != nil {
		return false, err
	}
	var p *Pipeline
	var runID string
	err := r.Store.AsActor(ctx, "lattisd (pipelines)", func(tx pgx.Tx) error {
		var err error
		p, err = scanPipeline(tx.QueryRow(ctx, selectPipeline+` WHERE enabled AND next_run_at <= now() AND running_since IS NULL
			ORDER BY next_run_at LIMIT 1 FOR UPDATE SKIP LOCKED`))
		if errors.Is(err, pgx.ErrNoRows) {
			p = nil
			return nil
		}
		if err != nil {
			return err
		}
		trigger := p.PendingTrigger
		if trigger == "" {
			trigger = "schedule"
			if p.SourceKind == "file" {
				trigger = "file"
			}
		}
		if err := tx.QueryRow(ctx, `INSERT INTO lattis.pipeline_runs (pipeline_id, trigger) VALUES ($1, $2) RETURNING id::text`,
			p.ID, trigger).Scan(&runID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE lattis.pipelines SET next_run_at = $2, pending_file = NULL, pending_trigger = NULL,
			running_since = now() WHERE id = $1`, p.ID, r.next(p, r.now()))
		return err
	})
	if err != nil || p == nil {
		return false, err
	}
	res := r.Execute(ctx, p)
	if err := r.finish(ctx, p, runID, res); err != nil {
		return true, err
	}
	return true, nil
}

// schedule donne une échéance aux pipelines planifiés qui n'en ont pas.
func (r *Runner) schedule(ctx context.Context) error {
	rows, err := r.Store.DB.Query(ctx, selectPipeline+` WHERE enabled AND next_run_at IS NULL AND schedule_kind <> 'manual'`)
	if err != nil {
		return err
	}
	ps, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Pipeline, error) { return scanPipeline(row) })
	if err != nil {
		return err
	}
	for _, p := range ps {
		if _, err := r.Store.DB.Exec(ctx, `UPDATE lattis.pipelines SET next_run_at = $2 WHERE id = $1 AND next_run_at IS NULL`,
			p.ID, r.next(p, r.now())); err != nil {
			return err
		}
	}
	return nil
}

// next : prochaine échéance après t (nil : à la demande seulement). Un dépôt de
// fichiers sans horaire est relevé chaque minute.
func (r *Runner) next(p *Pipeline, t time.Time) *time.Time {
	var n time.Time
	switch p.ScheduleKind {
	case "every":
		n = t.Add(time.Duration(p.ScheduleMinutes) * time.Minute)
	case "daily":
		hm, err := time.ParseInLocation("15:04", p.ScheduleTime, time.Local)
		if err != nil {
			return nil
		}
		n = time.Date(t.Year(), t.Month(), t.Day(), hm.Hour(), hm.Minute(), 0, 0, time.Local)
		if !n.After(t) {
			n = n.AddDate(0, 0, 1)
		}
	default:
		if p.SourceKind != "file" {
			return nil
		}
		n = t.Add(time.Minute)
	}
	return &n
}

const selectPipeline = `SELECT id::text, name, workbook_id::text, table_id::text, table_name, source_kind, format, mode,
	source, coalesce(secret_ref, ''), key_columns, column_map, schedule_kind, coalesce(schedule_minutes, 0),
	coalesce(schedule_time, ''), coalesce(pending_file, ''), coalesce(pending_trigger, ''), coalesce(webhook_url, ''),
	user_id::text FROM lattis.pipelines`

func scanPipeline(row pgx.Row) (*Pipeline, error) {
	p := &Pipeline{}
	var src, cmap []byte
	err := row.Scan(&p.ID, &p.Name, &p.WorkbookID, &p.TableID, &p.TableName, &p.SourceKind, &p.Format, &p.Mode,
		&src, &p.SecretRef, &p.KeyColumns, &cmap, &p.ScheduleKind, &p.ScheduleMinutes, &p.ScheduleTime,
		&p.PendingFile, &p.PendingTrigger, &p.WebhookURL, &p.UserID)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(src, &p.Source)
	_ = json.Unmarshal(cmap, &p.ColumnMap)
	return p, nil
}

// Execute lit la source et écrit dans le tableau ; le résultat dit comment.
func (r *Runner) Execute(ctx context.Context, p *Pipeline) Result {
	ctx, cancel := context.WithTimeout(ctx, MaxRun)
	defer cancel()
	var (
		t    *Table
		file string
		err  error
	)
	if p.SourceKind == "sql" {
		t, err = r.fetchSQL(ctx, p)
	} else {
		var b []byte
		b, file, err = r.fetchBytes(ctx, p)
		if err == nil {
			format := p.Format
			if format == "sql" {
				format = "csv"
			}
			t, err = parse(format, b, p.Source)
		}
	}
	if errors.Is(err, errNothing) {
		return Result{Status: "ok", Err: "aucun fichier à traiter"}
	}
	if err != nil {
		r.moveFile(p, file, "failed")
		return Result{Status: "error", Err: err.Error()}
	}
	res, err := r.write(ctx, p, t)
	if err != nil {
		r.moveFile(p, file, "failed")
		res.Status, res.Err = "error", err.Error()
		return res
	}
	r.moveFile(p, file, "done")
	res.Status = "ok"
	if res.Rejected > 0 {
		res.Status = "warning"
	}
	return res
}

// moveFile range un fichier traité (dépôt, envoi) dans done/ ou failed/.
func (r *Runner) moveFile(p *Pipeline, file, to string) {
	if file == "" || (p.SourceKind != "file" && p.SourceKind != "upload") {
		return
	}
	dst := filepath.Join(r.dir(p), to)
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return
	}
	name := r.now().Format("20060102-150405") + "-" + filepath.Base(file)
	if err := os.Rename(file, filepath.Join(dst, name)); err != nil {
		slog.Warn("pipeline : rangement du fichier", "pipeline", p.ID, "err", err)
	}
}

func (r *Runner) finish(ctx context.Context, p *Pipeline, runID string, res Result) error {
	// Plus d'exécution en cours : le pipeline peut repartir.
	defer func() {
		if _, err := r.Store.DB.Exec(context.Background(), `UPDATE lattis.pipelines SET running_since = NULL WHERE id = $1`, p.ID); err != nil {
			slog.Warn("pipeline : fin d'exécution", "pipeline", p.ID, "err", err)
		}
	}()
	rejects, _ := json.Marshal(res.Rejects)
	if res.Rejects == nil {
		rejects = []byte("[]")
	}
	_, err := r.Store.DB.Exec(ctx, `UPDATE lattis.pipeline_runs SET finished_at = now(), status = $2, rows_read = $3,
		inserted = $4, updated = $5, deleted = $6, rejected = $7, rejects = $8, error = nullif($9, '') WHERE id = $1`,
		runID, res.Status, res.Read, res.Inserted, res.Updated, res.Deleted, res.Rejected, rejects, res.Err)
	if r.Metrics != nil {
		r.Metrics.Inc(res.Status)
	}
	slog.Info("pipeline exécuté", "pipeline", p.Name, "status", res.Status, "read", res.Read, "inserted", res.Inserted,
		"updated", res.Updated, "deleted", res.Deleted, "rejected", res.Rejected, "err", res.Err)
	if res.Status == "error" && p.WebhookURL != "" {
		r.alert(p, runID, res)
	}
	return err
}

// alert prévient par un webhook (POST JSON) qu'une exécution a échoué.
func (r *Runner) alert(p *Pipeline, runID string, res Result) {
	body, _ := json.Marshal(map[string]any{
		"text":     fmt.Sprintf("Pipeline « %s » en échec : %s", p.Name, res.Err),
		"pipeline": p.Name, "pipeline_id": p.ID, "run_id": runID, "status": res.Status, "error": res.Err,
		"url": r.AdminURL,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client().Do(req)
	if err != nil {
		slog.Warn("pipeline : alerte", "pipeline", p.ID, "err", err)
		return
	}
	resp.Body.Close()
}
