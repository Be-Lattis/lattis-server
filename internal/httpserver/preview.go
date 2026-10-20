package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/tokens"
)

// PreviewRequest est le corps de POST /api/preview (« Voir en tant que… », §8.5).
type PreviewRequest struct {
	WorkbookID string `json:"workbook_id"`
	UserID     string `json:"user_id"`
	TableID    string `json:"table_id"`
}

// PreviewResponse renvoie les lignes que verrait l'utilisateur, au plus
// store.PreviewLimit.
type PreviewResponse struct {
	Rows      []json.RawMessage `json:"rows"`
	Truncated bool              `json:"truncated"`
}

// bearer vérifie le jeton d'accès présenté par l'application (le même que
// celui donné à PostgreSQL) et renvoie l'utilisateur, s'il est actif.
func (s *Server) bearer(r *http.Request) (store.User, error) {
	u, _, err := s.bearerExp(r)
	return u, err
}

// bearerExp fait bearer et rend aussi l'expiration du jeton (flux SSE :
// fermé à l'expiration).
func (s *Server) bearerExp(r *http.Request) (store.User, time.Time, error) {
	h := r.Header.Get("Authorization")
	raw, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		return store.User{}, time.Time{}, errors.New("jeton manquant")
	}
	c, err := s.Keys.Verify(strings.TrimSpace(raw), tokens.TypeAccess, s.Config.Issuer(), tokens.AudiencePostgres, time.Now())
	if err != nil {
		return store.User{}, time.Time{}, err
	}
	u, err := s.Store.User(r.Context(), c.Subject)
	if err != nil {
		return store.User{}, time.Time{}, err
	}
	if u.Disabled || u.PGRole != c.PGRole {
		return store.User{}, time.Time{}, errors.New("compte désactivé")
	}
	var exp time.Time
	if c.ExpiresAt > 0 {
		exp = time.Unix(c.ExpiresAt, 0)
	}
	return u, exp, nil
}

func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	u, err := s.bearer(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		apiError(w, http.StatusUnauthorized, "invalid_token", "jeton refusé")
		return
	}
	var req PreviewRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil ||
		req.WorkbookID == "" || req.UserID == "" || req.TableID == "" {
		apiError(w, http.StatusBadRequest, "invalid_request", "workbook_id, user_id et table_id sont requis")
		return
	}
	rows, err := s.Store.Preview(r.Context(), u.PGRole, req.WorkbookID, req.UserID, req.TableID)
	if err != nil {
		var pe *pgconn.PgError
		switch {
		case errors.As(err, &pe) && pe.Code == "42501":
			apiError(w, http.StatusForbidden, "forbidden", pe.Message)
		case errors.As(err, &pe) && (pe.Code == "P0002" || pe.Code == "22P02"),
			errors.Is(err, store.ErrNotFound):
			apiError(w, http.StatusNotFound, "not_found", "classeur, tableau ou utilisateur inconnu")
		default:
			s.unavailable(w, err)
		}
		return
	}
	if rows == nil {
		rows = []json.RawMessage{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, PreviewResponse{Rows: rows, Truncated: len(rows) == store.PreviewLimit})
}

func apiError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": msg})
}
