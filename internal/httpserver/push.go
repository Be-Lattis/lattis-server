package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Be-Lattis/lattis-server/internal/pipeline"
)

// Kicker réveille les pipelines (pipeline.Runner).
type Kicker interface{ Kick() }

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// pushWait : attente par défaut du résultat d'un envoi (paramètre wait, en
// secondes, de 0 à 300) avant de répondre 202.
const pushWait = 60 * time.Second

// push reçoit le contenu d'un tableau poussé depuis l'application (source
// poussée du Data Lake, lattis.create_pushed_source) : CSV en UTF-8, première
// ligne = noms des colonnes. Le pipeline l'écrit aussitôt ; la réponse porte
// le bilan de l'exécution (ou 202 si elle dure plus que wait).
func (s *Server) push(w http.ResponseWriter, r *http.Request) {
	u, err := s.bearer(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		apiError(w, http.StatusUnauthorized, "invalid_token", "jeton refusé")
		return
	}
	id := r.PathValue("id")
	if !uuidRe.MatchString(id) {
		apiError(w, http.StatusNotFound, "not_found", "source poussée inconnue")
		return
	}
	if s.Users == nil || s.Pipelines == nil {
		apiError(w, http.StatusServiceUnavailable, "unavailable", "pipelines indisponibles sur ce serveur")
		return
	}
	wait := pushWait
	if v := r.URL.Query().Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 300 {
			apiError(w, http.StatusBadRequest, "invalid_request", "wait : de 0 à 300 secondes")
			return
		}
		wait = time.Duration(n) * time.Second
	}
	// Un gros tableau sur une liaison modeste dépasse les délais du serveur public.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(15 * time.Minute))
	_ = rc.SetWriteDeadline(time.Now().Add(15*time.Minute + wait))

	dir := filepath.Join(s.Config.PipelineDir, id, "uploads")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		s.unavailable(w, err)
		return
	}
	name := "push-" + time.Now().Format("20060102-150405.000000000") + ".csv"
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		s.unavailable(w, err)
		return
	}
	n, err := io.Copy(f, io.LimitReader(r.Body, pipeline.MaxBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > pipeline.MaxBytes {
		err = errTooBig
	}
	if err != nil {
		os.Remove(path)
		if errors.Is(err, errTooBig) {
			apiError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("tableau trop gros (plus de %d Mo)", pipeline.MaxBytes>>20))
			return
		}
		apiError(w, http.StatusBadRequest, "invalid_request", "envoi interrompu")
		return
	}

	var since time.Time
	err = s.Users.As(r.Context(), u, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), `SELECT lattis.push_pipeline($1::uuid, $2)`, id, name); err != nil {
			return err
		}
		return tx.QueryRow(r.Context(), `SELECT clock_timestamp()`).Scan(&since)
	})
	if err != nil {
		os.Remove(path)
		var pe *pgconn.PgError
		switch {
		case errors.As(err, &pe) && pe.Code == "42501":
			apiError(w, http.StatusForbidden, "forbidden", pe.Message)
		case errors.As(err, &pe) && pe.Code == "P0002":
			apiError(w, http.StatusNotFound, "not_found", pe.Message)
		case errors.As(err, &pe) && (pe.Code == "55000" || pe.Code == "22023"):
			apiError(w, http.StatusConflict, "conflict", pe.Message)
		default:
			s.unavailable(w, err)
		}
		return
	}
	s.Pipelines.Kick()

	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	for {
		var run json.RawMessage
		err := s.Store.DB.QueryRow(ctx, `SELECT to_jsonb(x) - 'pipeline_id' FROM lattis.pipeline_runs x
			WHERE x.pipeline_id = $1 AND x.started_at >= $2 - interval '1 second' AND x.finished_at IS NOT NULL
			ORDER BY x.started_at DESC LIMIT 1`, id, since).Scan(&run)
		if err == nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(run)
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil {
			s.unavailable(w, err)
			return
		}
		select {
		case <-ctx.Done():
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "running"})
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

var errTooBig = errors.New("trop gros")

// pushStatus : état d'une source poussée de l'utilisateur (dernier envoi).
func (s *Server) pushStatus(w http.ResponseWriter, r *http.Request) {
	u, err := s.bearer(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		apiError(w, http.StatusUnauthorized, "invalid_token", "jeton refusé")
		return
	}
	if s.Users == nil {
		apiError(w, http.StatusServiceUnavailable, "unavailable", "pipelines indisponibles sur ce serveur")
		return
	}
	var list []map[string]any
	if err := s.Users.As(r.Context(), u, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `SELECT lattis.my_pushed_sources()`).Scan(&list)
	}); err != nil {
		s.unavailable(w, err)
		return
	}
	for _, x := range list {
		if x["pipeline_id"] == r.PathValue("id") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(x)
			return
		}
	}
	apiError(w, http.StatusNotFound, "not_found", "source poussée inconnue")
}
