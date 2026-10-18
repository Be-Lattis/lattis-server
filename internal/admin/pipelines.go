package admin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

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
	return map[string]any{"Pipelines": pipelines, "Targets": targets, "Dir": s.Config.PipelineDir}, nil
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
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+1<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil || !s.checkCSRF(r) {
		s.message(w, r, http.StatusForbidden, "Demande refusée", "Fichier trop gros (100 Mo au plus) ou formulaire expiré : rechargez la page.")
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
	s.perform(w, r, func(ctx context.Context, tx pgx.Tx, _ form) (string, error) {
		// Droits vérifiés par run_pipeline (propriétaire du serveur) avant
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
var pipelineTriggers = map[string]string{"schedule": "horaire", "manual": "à la demande", "upload": "fichier envoyé", "file": "dépôt"}
var pipelineStatuses = map[string]string{"ok": "réussi", "warning": "lignes rejetées", "error": "échec", "running": "en cours"}

func pipelineSource(x map[string]any) string {
	src, _ := x["source"].(map[string]any)
	str := func(k string) string { v, _ := src[k].(string); return v }
	format := map[string]string{"csv": "CSV", "xlsx": "Excel", "json": "JSON"}[fmt.Sprint(x["format"])]
	switch x["source_kind"] {
	case "upload":
		return "fichier " + format + " envoyé depuis cette page"
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
