package admin

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lalamefine/lattis/protocol"

	"github.com/Be-Lattis/lattis-server/internal/config"
)

// Lien vers un classeur : /w/<id>. La personne se connecte si besoin, le
// serveur vérifie son accès sous sa propre identité, puis propose de
// l'ouvrir dans l'application de bureau (lattis://open…) ou, en édition
// web, dans le navigateur. Le choix est retenu par un cookie.

const openCookie = "lattis_open"

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// WorkbookLink est l'adresse à partager pour un classeur.
func WorkbookLink(cfg *config.Config, id string) string {
	return protocol.WorkbookLink(cfg.Issuer(), id)
}

// DesktopLink est le lien qui ouvre le classeur dans l'application de bureau.
func DesktopLink(cfg *config.Config, id string) string {
	return protocol.DesktopLink(cfg.Issuer(), id)
}

type linkData struct {
	ID, Title   string
	Desktop     string
	Web         string // vide hors édition web
	Download    string
	Choice      string // desktop, web ou vide
	AutoDesktop bool   // ouvrir l'application tout de suite
	NoAccess    bool
	Email       string
}

func (s *Service) workbookLink(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !uuidRe.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	u, err := s.session(r)
	if err != nil {
		http.Redirect(w, r, "/admin/login?next="+url.QueryEscape("/w/"+id), http.StatusFound)
		return
	}
	d := linkData{ID: id, Desktop: DesktopLink(s.Config, id), Download: s.Config.DownloadURL, Email: u.Email}
	if s.Config.Edition == config.EditionWeb {
		d.Web = "/app/?workbook=" + id
	}
	v := view{Title: "Ouvrir un classeur", Page: "link", Server: "Serveur Lattis", Version: s.Version}
	err = s.Users.As(r.Context(), u, func(tx pgx.Tx) error {
		if err := tx.QueryRow(r.Context(), `SELECT lattis.login_summary() #>> '{server,display_name}'`).Scan(&v.Server); err != nil {
			return err
		}
		return tx.QueryRow(r.Context(), `SELECT lattis.my_capabilities($1::uuid) ->> 'title'`, id).Scan(&d.Title)
	})
	var pe *pgconn.PgError
	switch {
	case err == nil:
	case errors.As(err, &pe) && (pe.Code == "42501" || pe.Code == "P0002" || pe.Code == "22023"), errors.Is(err, pgx.ErrNoRows):
		// Même réponse pour un classeur inexistant ou interdit : rien ne fuit.
		d.NoAccess = true
		v.Title = "Accès refusé"
		v.Data = d
		w.Header().Set("Cache-Control", "no-store")
		s.render(w, http.StatusForbidden, "link", v)
		return
	default:
		s.fail(w, r, err)
		return
	}

	switch r.URL.Query().Get("open") {
	case "desktop":
		s.setCookie(w, openCookie, "desktop", "/w/", 365*24*time.Hour, http.SameSiteLaxMode)
		d.Choice, d.AutoDesktop = "desktop", true
	case "web":
		if d.Web != "" {
			s.setCookie(w, openCookie, "web", "/w/", 365*24*time.Hour, http.SameSiteLaxMode)
			http.Redirect(w, r, d.Web, http.StatusFound)
			return
		}
	case "choose":
		s.clearCookie(w, openCookie, "/w/")
	default:
		if c, err := r.Cookie(openCookie); err == nil {
			switch {
			case c.Value == "web" && d.Web != "":
				http.Redirect(w, r, d.Web, http.StatusFound)
				return
			case c.Value == "desktop":
				d.Choice, d.AutoDesktop = "desktop", true
			}
		}
	}
	v.Data = d
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, http.StatusOK, "link", v)
}
