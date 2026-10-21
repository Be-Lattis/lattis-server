package admin

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/forms"
)

// uploadLogo : logo du serveur envoyé (image PNG ou JPEG, réduite à 512 px)
// ou retiré (champ remove), depuis la page Serveur.
func (s *Service) uploadLogo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 6<<20)
	if err := r.ParseMultipartForm(6 << 20); err != nil {
		s.message(w, r, http.StatusRequestEntityTooLarge, "Image refusée", "L'image dépasse 5 Mo, ou n'est pas arrivée en entier.")
		return
	}
	defer r.MultipartForm.RemoveAll()
	if !s.checkCSRF(r) {
		s.message(w, r, http.StatusForbidden, "Page expirée", "Cette page était restée ouverte trop longtemps : revenez-y et recommencez.")
		return
	}
	s.perform(w, r, func(ctx context.Context, tx pgx.Tx, f form) (string, error) {
		if f.s("remove") != "" {
			if _, err := tx.Exec(ctx, `SELECT lattis.clear_server_logo()`); err != nil {
				return "", err
			}
			return "Logo retiré : le serveur affiche le logo par adresse s'il y en a un, sinon l'icône Lattis.", nil
		}
		file, _, err := f.r.FormFile("file")
		if err != nil {
			return "", inputError("Choisissez une image PNG ou JPEG.")
		}
		defer file.Close()
		data, ct, err := forms.ShrinkLogo(file)
		if errors.Is(err, forms.ErrTooBig) {
			return "", inputError("L'image dépasse 5 Mo.")
		}
		if err != nil {
			return "", inputError("Image illisible : envoyez un fichier PNG ou JPEG.")
		}
		if _, err := tx.Exec(ctx, `SELECT lattis.set_server_logo($1, $2)`, ct, data); err != nil {
			return "", err
		}
		return "Logo enregistré.", nil
	})
}
