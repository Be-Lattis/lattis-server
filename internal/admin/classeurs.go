package admin

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/config"
)

// Mes classeurs : /classeurs, où l'on arrive après la connexion en édition
// web (décidé par Louis le 08/11/2026) : les classeurs auxquels on a accès,
// ouverts dans le navigateur, et l'administration pour qui y a droit.

type myWorkbook struct {
	ID    string   `json:"id"`
	Title string   `json:"title"`
	State string   `json:"state"`
	Roles []string `json:"roles"`
	Role  string   `json:"-"`
}

type myWorkbooksData struct {
	Workbooks []myWorkbook
	Email     string
	Web       bool
	Admin     bool
	Download  string
}

// roleLabels : rôle le plus fort en premier.
var roleLabels = []struct{ role, label string }{
	{"owner", "Propriétaire"}, {"designer", "Concepteur"}, {"editor", "Éditeur"},
	{"participant", "Participant"}, {"reader", "Lecteur"},
}

func roleLabel(roles []string) string {
	for _, l := range roleLabels {
		for _, r := range roles {
			if r == l.role {
				return l.label
			}
		}
	}
	return "Accès personnalisé"
}

// defaultLanding : où arriver après la connexion sans lien d'origine.
func (s *Service) defaultLanding() string {
	if s.Config.Edition == config.EditionWeb {
		return "/classeurs"
	}
	return "/admin/"
}

func (s *Service) myWorkbooks(w http.ResponseWriter, r *http.Request) {
	u, err := s.session(r)
	if err != nil {
		http.Redirect(w, r, "/admin/login?next="+url.QueryEscape("/classeurs"), http.StatusFound)
		return
	}
	d := myWorkbooksData{Email: u.Email, Web: s.Config.Edition == config.EditionWeb, Download: s.Config.DownloadURL}
	v := view{Title: "Mes classeurs", Page: "classeurs", Server: "Serveur Lattis", Version: s.Version}
	err = s.Users.As(r.Context(), u, func(tx pgx.Tx) error {
		if err := tx.QueryRow(r.Context(), `SELECT lattis.login_summary() #>> '{server,display_name}'`).Scan(&v.Server); err != nil {
			return err
		}
		access, err := adminAccess(r.Context(), tx)
		if err != nil {
			return err
		}
		d.Admin = access != ""
		var raw []byte
		if err := tx.QueryRow(r.Context(), `SELECT lattis.my_workbooks()`).Scan(&raw); err != nil {
			return err
		}
		return json.Unmarshal(raw, &d.Workbooks)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for i := range d.Workbooks {
		d.Workbooks[i].Role = roleLabel(d.Workbooks[i].Roles)
	}
	v.Data = d
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, http.StatusOK, "classeurs", v)
}
