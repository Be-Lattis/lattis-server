package admin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/store"
)

// maxUpload borne un fichier envoyé à un pipeline.
const maxUpload = 100 << 20

func (s *Service) loadPipelines(ctx context.Context, tx pgx.Tx, r *http.Request, u store.User) (any, error) {
	pipelines, err := queryJSON(ctx, tx, `SELECT lattis.admin_pipelines()`)
	if err != nil {
		return nil, err
	}
	targets, err := queryJSON(ctx, tx, `SELECT lattis.pipeline_targets()`)
	if err != nil {
		return nil, err
	}
	var cat struct {
		Folders []map[string]any `json:"folders"`
	}
	if err := tx.QueryRow(ctx, `SELECT lattis.data_catalog()`).Scan(&cat); err != nil {
		return nil, err
	}
	return map[string]any{"Pipelines": pipelines, "Targets": targets, "Folders": cat.Folders, "Dir": s.Config.PipelineDir}, nil
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// uploadPipeline reçoit un fichier pour un pipeline « fichier envoyé », le
// range dans <dossier>/<pipeline>/uploads et demande l'exécution.
func (s *Service) uploadPipeline(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !uuidRe.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	// Un gros fichier sur une liaison lente dépasse le délai de lecture du
	// serveur public (30 s) : 15 minutes ici.
	_ = http.NewResponseController(w).SetReadDeadline(s.now().Add(15 * time.Minute))
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+1<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.message(w, r, http.StatusRequestEntityTooLarge, "Fichier trop gros", "Le fichier dépasse 100 Mo.")
		} else {
			s.message(w, r, http.StatusBadRequest, "Envoi interrompu", "Le fichier n'est pas arrivé en entier : réessayez.")
		}
		return
	}
	if !s.checkCSRF(r) {
		s.message(w, r, http.StatusForbidden, "Page expirée", "Cette page était restée ouverte trop longtemps : revenez-y et recommencez.")
		return
	}
	defer r.MultipartForm.RemoveAll()
	f, hdr, err := r.FormFile("file")
	if err != nil {
		s.message(w, r, http.StatusBadRequest, "Demande refusée", "Aucun fichier reçu.")
		return
	}
	defer f.Close()
	name := strings.Trim(unsafeName.ReplaceAllString(filepath.Base(hdr.Filename), "_"), "._")
	if name == "" {
		name = "fichier"
	}
	name = s.now().Format("20060102-150405") + "-" + name
	if len(name) > 200 {
		name = name[:200]
	}
	if r.PostFormValue("simulate") != "" {
		s.performAs(w, r, false, func(ctx context.Context, tx pgx.Tx, _ form) (string, error) {
			if err := requirePipeline(ctx, tx, id); err != nil {
				return "", err
			}
			return s.simulateUpload(ctx, id, name, f)
		})
		return
	}
	s.performAs(w, r, false, func(ctx context.Context, tx pgx.Tx, _ form) (string, error) {
		// Droits vérifiés par run_pipeline (pipeline administré) avant
		// l'écriture du fichier : la transaction échoue sinon.
		if _, err := tx.Exec(ctx, `SELECT lattis.run_pipeline($1::uuid, $2)`, id, name); err != nil {
			return "", err
		}
		dir := filepath.Join(s.Config.PipelineDir, id, "uploads")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", err
		}
		out, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(out, io.LimitReader(f, maxUpload+1)); err != nil {
			out.Close()
			return "", err
		}
		if err := out.Close(); err != nil {
			return "", err
		}
		return fmt.Sprintf("Fichier « %s » reçu : l'exécution démarre dans les secondes qui viennent.", hdr.Filename), nil
	})
}

var pipelineModes = map[string]string{"append": "ajout à la suite", "upsert": "mise à jour par clé", "replace": "remplacement"}
var pipelineTriggers = map[string]string{"schedule": "horaire", "manual": "à la demande", "upload": "fichier envoyé", "file": "dépôt", "push": "envoi depuis l'application"}
var pipelineStatuses = map[string]string{"ok": "réussi", "warning": "lignes rejetées", "error": "échec", "running": "en cours"}

func pipelineSource(x map[string]any) string {
	src, _ := x["source"].(map[string]any)
	str := func(k string) string { v, _ := src[k].(string); return v }
	format := map[string]string{"csv": "CSV", "xlsx": "Excel", "json": "JSON"}[fmt.Sprint(x["format"])]
	switch x["source_kind"] {
	case "upload":
		return "fichier " + format + " envoyé depuis cette page"
	case "push":
		o, _ := src["origin"].(map[string]any)
		s := "tableau poussé depuis l'application"
		if wb, _ := o["workbook"].(string); wb != "" {
			t, _ := o["table"].(string)
			s += fmt.Sprintf(" (classeur « %s », tableau « %s »)", wb, t)
		}
		return s
	case "file":
		p := str("pattern")
		if p == "" {
			p = "*"
		}
		return fmt.Sprintf("dossier de dépôt (%s, %s)", p, format)
	case "http":
		return str("url") + " (" + format + ")"
	case "sftp":
		return fmt.Sprintf("sftp://%s@%s%s (%s)", str("user"), str("host"), str("path"), format)
	case "sql":
		return "PostgreSQL externe : " + str("query")
	}
	return fmt.Sprint(x["source_kind"])
}

// simulateUpload : fichier envoyé simulé puis effacé.
func (s *Service) simulateUpload(ctx context.Context, id, name string, f io.Reader) (string, error) {
	if s.Pipelines == nil {
		return "", inputError("Simulation indisponible sur ce serveur.")
	}
	dir := filepath.Join(s.Config.PipelineDir, id, "uploads")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	name = "simulation-" + name
	path := filepath.Join(dir, name)
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return "", err
	}
	defer os.Remove(path)
	if _, err := io.Copy(out, io.LimitReader(f, maxUpload+1)); err != nil {
		out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	res, err := s.Pipelines.Simulate(ctx, id, name)
	if err != nil {
		return "", err
	}
	if res.Status == "error" {
		return "", inputError("Simulation : " + res.Summary())
	}
	return "Simulation (rien n'a été écrit) : " + res.Summary(), nil
}

// loadForms : tous les formulaires du serveur et le réglage du lien public.
func (s *Service) loadForms(ctx context.Context, tx pgx.Tx, r *http.Request, u store.User) (any, error) {
	forms, err := queryJSON(ctx, tx, `SELECT lattis.admin_forms()`)
	if err != nil {
		return nil, err
	}
	var public bool
	if err := tx.QueryRow(ctx, `SELECT (lattis.forms_settings() ->> 'forms_public')::boolean`).Scan(&public); err != nil {
		return nil, err
	}
	return map[string]any{"Forms": forms, "Public": public}, nil
}
