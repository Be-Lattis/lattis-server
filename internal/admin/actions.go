package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Be-Lattis/lattis-server/internal/mail"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// actionFunc exécute une action sous l'identité de l'administrateur et
// renvoie le message de confirmation.
type actionFunc func(ctx context.Context, tx pgx.Tx, f form) (string, error)

// form simplifie la lecture des champs.
type form struct{ r *http.Request }

func (f form) s(name string) string { return strings.TrimSpace(f.r.PostFormValue(name)) }

// n renvoie nil pour un champ vide (NULL en SQL).
func (f form) n(name string) any {
	if v := f.s(name); v != "" {
		return v
	}
	return nil
}

func (f form) checked(name string) bool { return f.r.PostFormValue(name) != "" }

func (f form) list(name string) []string {
	var out []string
	for _, v := range f.r.PostForm[name] {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

var errConfirm = errors.New("cochez « Confirmer » pour cette action")

// inputError est une erreur de saisie détectée avant d'appeler PostgreSQL.
type inputError string

func (e inputError) Error() string { return string(e) }

func (s *Service) actions() map[string]actionFunc {
	return map[string]actionFunc{
		"server_settings":      s.saveSettings,
		"add_provider":         s.addProvider,
		"update_provider":      s.updateProvider,
		"remove_provider":      exec1(`SELECT lattis.remove_provider($1::uuid)`, "Fournisseur supprimé.", true, "id"),
		"allow_tenant":         allowTenant,
		"disallow_tenant":      exec1(`SELECT lattis.disallow_tenant($1::uuid)`, "Tenant retiré.", true, "id"),
		"add_user":             s.addUser,
		"reset_password":       s.resetPassword,
		"disable_user":         s.disableUser,
		"workbook_creator_on":  exec1(`SELECT lattis.set_workbook_creator($1::uuid, true)`, "Cette personne peut maintenant créer des classeurs.", false, "id"),
		"workbook_creator_off": exec1(`SELECT lattis.set_workbook_creator($1::uuid, false)`, "Rôle de créateur de classeurs retiré.", false, "id"),
		"enable_user":          exec1(`SELECT lattis.enable_user($1::uuid)`, "Compte réactivé.", false, "id"),
		"revoke_invitation":    exec1(`SELECT lattis.revoke_invitation($1::uuid)`, "Invitation retirée.", false, "id"),
		"drop_workbook":        dropWorkbook,
		"create_team":          exec1(`SELECT lattis.create_team($1, $2::uuid)`, "Équipe créée.", false, "name", "parent_id"),
		"data_publisher_on":    exec1(`SELECT lattis.set_data_publisher($1::uuid, true)`, "Cette personne peut maintenant partager des tableaux au Data Lake Lattis.", false, "id"),
		"data_publisher_off":   exec1(`SELECT lattis.set_data_publisher($1::uuid, false)`, "Rôle de publieur au Data Lake retiré.", false, "id"),
		"create_data_folder":   exec1(`SELECT lattis.create_data_folder($1, $2::uuid)`, "Dossier créé.", false, "name", "parent_id"),
		"rename_data_folder":   exec1(`SELECT lattis.rename_data_folder($1::uuid, $2)`, "Dossier renommé.", false, "id", "name"),
		"move_data_folder":     exec1(`SELECT lattis.move_data_folder($1::uuid, $2::uuid)`, "Dossier déplacé.", false, "id", "parent_id"),
		"drop_data_folder":     exec1(`SELECT lattis.drop_data_folder($1::uuid)`, "Dossier supprimé.", true, "id"),
		"drop_dataset":         exec1(`SELECT lattis.drop_dataset($1::uuid)`, "Partage retiré.", true, "id"),
		"remove_data_audience": exec1(`SELECT lattis.remove_data_audience($1::uuid)`, "Retiré de l'audience.", false, "id"),
		"add_data_audience":    addDataAudience,
		"create_pipeline":      s.createPipeline,
		"simulate_pipeline":    s.simulatePipeline,
		"create_workbook":      createWorkbook,
		"run_pipeline":         exec1(`SELECT lattis.run_pipeline($1::uuid, NULL)`, "Exécution demandée : elle démarre dans les secondes qui viennent.", false, "id"),
		"toggle_pipeline":      exec1(`SELECT lattis.update_pipeline($1::uuid, jsonb_build_object('enabled', $2::boolean))`, "Pipeline mis à jour.", false, "id", "enabled"),
		"mail_test":            s.mailTest,
		"pipeline_alert_email": exec1(`SELECT lattis.set_pipeline_alert_email($1::uuid, $2)`, "Alerte par e-mail enregistrée.", false, "id", "alert_email"),
		"form_state":           exec1(`SELECT lattis.set_form_state($1::uuid, $2)`, "Formulaire mis à jour.", false, "id", "state"),
		"forms_public":         exec1(`SELECT lattis.set_config('forms_public', to_jsonb($1::boolean))`, "Réglage enregistré.", false, "public"),
		"drop_pipeline":        exec1(`SELECT lattis.drop_pipeline($1::uuid)`, "Pipeline supprimé.", true, "id"),
		"drop_pushed_source":   exec1(`SELECT lattis.drop_pushed_source($1::uuid)`, "Source poussée supprimée (donnée du Data Lake, pipeline et classeur technique).", true, "id"),
		"set_team_parent":      exec1(`SELECT lattis.set_team_parent($1::uuid, $2::uuid)`, "Équipe déplacée.", false, "id", "parent_id"),
		"rename_team":          exec1(`SELECT lattis.rename_team($1::uuid, $2)`, "Équipe renommée.", false, "id", "name"),
		"drop_team":            exec1(`SELECT lattis.drop_team($1::uuid)`, "Équipe supprimée.", true, "id"),
		"add_team_member":      addTeamMember,
		"team_manager_on":      addTeamManager,
		"team_manager_off":     exec1(`SELECT lattis.set_team_manager($1::uuid, $2::uuid, false)`, "Gestionnaire retiré de l'équipe.", false, "team_id", "user_id"),
		"remove_team_member":   exec1(`SELECT lattis.remove_team_member($1::uuid, $2::uuid)`, "Membre retiré de l'équipe.", false, "team_id", "user_id"),
		"grant_team":           exec1(`SELECT lattis.grant_role($1::uuid, $2::uuid)`, "Équipe ajoutée au rôle.", false, "role_id", "team_id"),
		"invite_workbook":      inviteWorkbook,
		"create_role":          createRole,
		"drop_role":            exec1(`SELECT lattis.drop_role($1::uuid)`, "Rôle supprimé.", true, "id"),
		"set_level":            setLevel,
		"grant_role":           grantRole,
		"revoke_role":          exec1(`SELECT lattis.revoke_role($1::uuid, $2::uuid)`, "Membre retiré.", false, "role_id", "user_id"),
		"add_restriction":      addRestriction,
		"drop_restriction":     exec1(`SELECT lattis.drop_restriction($1::uuid)`, "Restriction supprimée.", false, "id"),
	}
}

// managerActions : actions ouvertes aux gestionnaires d'équipe ; les
// fonctions SQL appelées vérifient le périmètre.
var managerActions = map[string]bool{
	"workbook_creator_on": true, "workbook_creator_off": true, "data_publisher_on": true, "data_publisher_off": true,
	"create_team": true, "rename_team": true, "drop_team": true, "set_team_parent": true,
	"add_team_member": true, "remove_team_member": true, "team_manager_on": true, "team_manager_off": true,
	"create_workbook": true, "drop_workbook": true, "revoke_invitation": true, "invite_workbook": true,
	"create_role": true, "drop_role": true, "set_level": true, "grant_role": true, "revoke_role": true, "grant_team": true,
	"add_restriction": true, "drop_restriction": true,
	"create_data_folder": true, "rename_data_folder": true, "move_data_folder": true, "drop_data_folder": true,
	"drop_dataset": true, "add_data_audience": true, "remove_data_audience": true,
	"form_state": true,
}

func (s *Service) do(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil || !s.checkCSRF(r) {
		s.message(w, r, http.StatusForbidden, "Demande refusée", "Cette page était restée ouverte trop longtemps : revenez-y et recommencez.")
		return
	}
	name := r.PathValue("action")
	fn, ok := s.actions()[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.performAs(w, r, !managerActions[name], fn)
}

// perform exécute une action réservée aux propriétaires du serveur.
func (s *Service) perform(w http.ResponseWriter, r *http.Request, fn actionFunc) {
	s.performAs(w, r, true, fn)
}

// performAs exécute une action sous l'identité de l'administrateur connecté
// (formulaire déjà lu et jeton anti-CSRF vérifié), puis revient à la page.
// ownerOnly : refusée aux gestionnaires d'équipe.
func (s *Service) performAs(w http.ResponseWriter, r *http.Request, ownerOnly bool, fn actionFunc) {
	u, err := s.session(r)
	if err != nil {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	back := safeBack(r.PostFormValue("back"))
	if back == "" {
		back = "/admin/"
	}
	f := form{r}
	var msg string
	err = s.Users.As(r.Context(), u, func(tx pgx.Tx) error {
		// Propriétaires du serveur et gestionnaires d'équipe ; les fonctions
		// vérifient de toute façon leurs propres droits.
		access, err := adminAccess(r.Context(), tx)
		if err != nil {
			return err
		}
		if access == "" {
			return inputError("Réservé aux propriétaires du serveur et aux gestionnaires d'équipe.")
		}
		if ownerOnly && access != accessOwner {
			return inputError("Réservé aux propriétaires du serveur.")
		}
		msg, err = fn(r.Context(), tx, f)
		return err
	})
	var ie inputError
	switch {
	case err == nil:
		s.setFlash(w, flash{OK: msg})
	case errors.Is(err, errConfirm):
		s.setFlash(w, flash{Err: "Cochez « Confirmer » pour cette action.", Form: keepInput(r)})
	case errors.As(err, &ie):
		s.setFlash(w, flash{Err: string(ie), Form: keepInput(r)})
	default:
		if m, ok := userMessage(err); ok {
			s.setFlash(w, flash{Err: m, Form: keepInput(r)})
		} else {
			s.fail(w, r, err)
			return
		}
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// exec1 : action qui appelle une fonction avec des champs uuid ou texte.
func exec1(sql, msg string, confirm bool, fields ...string) actionFunc {
	return func(ctx context.Context, tx pgx.Tx, f form) (string, error) {
		if confirm && !f.checked("confirm") {
			return "", errConfirm
		}
		args := make([]any, len(fields))
		for i, name := range fields {
			args[i] = f.n(name)
		}
		_, err := tx.Exec(ctx, sql, args...)
		return msg, err
	}
}

// --- Serveur -------------------------------------------------------------------------

func (s *Service) saveSettings(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	cur, err := readSettings(ctx, tx)
	if err != nil {
		return "", err
	}
	changed := 0
	for _, st := range settings {
		var v any
		switch st.Kind {
		case "bool":
			v = f.checked(st.Key)
		case "number", "duration":
			n, err := strconv.ParseFloat(strings.NewReplacer(" ", "", ",", ".").Replace(f.s(st.Key)), 64)
			if err != nil || n <= 0 {
				return "", inputError(st.Label + " : nombre positif attendu.")
			}
			if st.Kind == "duration" {
				n = math.Round(n * st.Scale)
			}
			v = n
		case "limit":
			txt := strings.NewReplacer(" ", "", ",", ".").Replace(f.s(st.Key))
			n := 0.0
			if txt != "" {
				var err error
				if n, err = strconv.ParseFloat(txt, 64); err != nil || n < 0 || n != math.Trunc(n) {
					return "", inputError(st.Label + " : nombre entier attendu (vide ou 0 : sans limite).")
				}
			}
			v = n
		case "url":
			v = f.n(st.Key)
		default:
			v = f.s(st.Key)
		}
		if same(cur[st.Key], v) {
			continue
		}
		b, _ := json.Marshal(v)
		if _, err := tx.Exec(ctx, `SELECT lattis.set_config($1, $2::jsonb)`, st.Key, string(b)); err != nil {
			return "", err
		}
		changed++
	}
	if changed == 0 {
		return "Aucun changement.", nil
	}
	return "Réglages enregistrés.", nil
}

func same(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// --- Connexion ---------------------------------------------------------------------------

func scopes(s string) any {
	f := strings.Fields(strings.ReplaceAll(s, ",", " "))
	if len(f) == 0 {
		return nil
	}
	return f
}

// secretRef : référence du secret d'un formulaire. Une valeur saisie
// (secret_value) est chiffrée et gardée en base, en remplaçant old ; sinon
// le nom de fichier saisi (secret_ref, avancé) ; sinon keep et old inchangé.
func (s *Service) secretRef(ctx context.Context, tx pgx.Tx, f form, old string) (ref any, changed bool, err error) {
	if v := f.r.PostFormValue("secret_value"); strings.TrimSpace(v) != "" {
		if s.Vault == nil {
			return nil, false, inputError("Secrets en valeur indisponibles sur ce serveur : indiquez un fichier du dossier des secrets.")
		}
		r, err := s.Vault.Store(ctx, tx, strings.TrimSpace(v), old)
		return r, true, err
	}
	if fr := f.s("secret_ref"); fr != "" && fr != old {
		return fr, true, nil
	}
	if f.checked("secret_clear") {
		return nil, true, nil
	}
	return nil, false, nil
}

func (s *Service) addProvider(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	ref, _, err := s.secretRef(ctx, tx, f, "")
	if err != nil {
		return "", err
	}
	var id string
	if err := tx.QueryRow(ctx, `SELECT lattis.add_provider($1, $2, $3, $4, $5, $6::text[])::text`,
		f.s("kind"), f.s("label"), f.n("issuer"), f.n("client_id"), ref, scopes(f.s("scopes"))).Scan(&id); err != nil {
		return "", err
	}
	return "Fournisseur ajouté. Déclarez chez lui l'URI de redirection affichée sur sa carte : " + s.Config.URL("/callback/"+id), nil
}

func (s *Service) updateProvider(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	pos, err := strconv.Atoi(f.s("position"))
	if err != nil {
		return "", inputError("Ordre : nombre entier attendu.")
	}
	changes := map[string]any{"label": f.s("label"), "enabled": f.checked("enabled"), "position": pos}
	if f.s("kind") != "password" {
		if _, ok := f.r.PostForm["issuer"]; ok {
			changes["issuer"] = f.n("issuer")
		}
		changes["client_id"] = f.n("client_id")
		var old string
		if err := tx.QueryRow(ctx, `SELECT coalesce(client_secret_ref, '') FROM lattis.identity_providers WHERE id = $1::uuid`, f.s("id")).Scan(&old); err != nil {
			return "", err
		}
		ref, changed, err := s.secretRef(ctx, tx, f, old)
		if err != nil {
			return "", err
		}
		if changed {
			changes["client_secret_ref"] = ref
		}
		if _, ok := f.r.PostForm["claim_name"]; ok {
			changes["claim_name"] = f.n("claim_name")
			changes["claim_value"] = f.n("claim_value")
		}
		if sc := scopes(f.s("scopes")); sc != nil {
			changes["scopes"] = sc
		}
	}
	b, _ := json.Marshal(changes)
	_, err = tx.Exec(ctx, `SELECT lattis.update_provider($1::uuid, $2::jsonb)`, f.s("id"), string(b))
	return "Fournisseur enregistré.", err
}

func allowTenant(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	_, err := tx.Exec(ctx, `SELECT lattis.allow_tenant($1::uuid, $2, $3, $4)`,
		f.s("provider_id"), f.s("kind"), f.s("value"), f.n("default_role"))
	return "Tenant autorisé.", err
}

// --- Utilisateurs -------------------------------------------------------------------------

func (s *Service) setupLink(token string) string { return s.Config.URL("/setup?token=" + token) }

// mailLink met en file l'e-mail du lien de mot de passe (vrai si l'envoi
// est configuré).
func (s *Service) mailLink(ctx context.Context, tx pgx.Tx, email, token, reason string) bool {
	if s.Mailer == nil {
		return false
	}
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT lattis.mail_password_link($1, $2, $3)`, email, token, reason).Scan(&ok); err != nil {
		slog.Warn("e-mail du lien de mot de passe", "err", err)
		return false
	}
	return ok
}

// mailTest envoie tout de suite un e-mail d'essai à l'administrateur.
func (s *Service) mailTest(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	if s.Mailer == nil {
		return "", inputError("Aucun serveur d'envoi configuré (LATTIS_SMTP_HOST).")
	}
	var email, server string
	if err := tx.QueryRow(ctx, `SELECT u.email, coalesce(lattis.config('display_name') #>> '{}', 'Serveur Lattis')
		FROM lattis.users u WHERE u.pg_role = session_user`).Scan(&email, &server); err != nil {
		return "", err
	}
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := mail.Test(sctx, s.Mailer, email, server, s.Config.URL("/")); err != nil {
		return "", inputError("Envoi impossible : " + err.Error())
	}
	return "E-mail d'essai envoyé à " + email + ".", nil
}

// addUser : seul ajout manuel, un compte à mot de passe et son lien
// pré-signé. Propriétaire du serveur : par une invitation, acceptée à la
// première connexion.
func (s *Service) addUser(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	var token string
	if err := tx.QueryRow(ctx, `SELECT lattis.create_password_account($1, $2)`, f.s("email"), f.s("display_name")).
		Scan(&token); err != nil {
		return "", err
	}
	if f.checked("server_owner") {
		if _, err := tx.Exec(ctx, `SELECT lattis.invite($1, NULL, '{server_owner}')`, f.s("email")); err != nil {
			return "", err
		}
	}
	if f.checked("workbook_creator") {
		if _, err := tx.Exec(ctx, `SELECT lattis.set_workbook_creator(u.id, true) FROM lattis.users u
			JOIN lattis.identity_providers p ON p.id = u.provider_id AND p.kind = 'password'
			WHERE u.email = lower(trim($1))`, f.s("email")); err != nil {
			return "", err
		}
	}
	if s.mailLink(ctx, tx, f.s("email"), token, "new") {
		return "Compte créé pour " + strings.ToLower(f.s("email")) + " : un e-mail avec son lien vient de partir. Le même lien, au besoin : " + s.setupLink(token), nil
	}
	return "Compte créé pour " + strings.ToLower(f.s("email")) + ". Lien à lui transmettre : " + s.setupLink(token), nil
}

func (s *Service) resetPassword(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	if !f.checked("confirm") {
		return "", errConfirm
	}
	var token string
	if err := tx.QueryRow(ctx, `SELECT lattis.reset_password($1::uuid)`, f.s("id")).Scan(&token); err != nil {
		return "", err
	}
	var email string
	if err := tx.QueryRow(ctx, `SELECT email FROM lattis.users WHERE id = $1::uuid`, f.s("id")).Scan(&email); err == nil && s.mailLink(ctx, tx, email, token, "reset") {
		return "Mot de passe réinitialisé : un e-mail avec le lien vient de partir à " + email + ". Le même lien, au besoin : " + s.setupLink(token), nil
	}
	return "Mot de passe réinitialisé. Lien à transmettre : " + s.setupLink(token), nil
}

func (s *Service) disableUser(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	if !f.checked("confirm") {
		return "", errConfirm
	}
	if _, err := tx.Exec(ctx, `SELECT lattis.disable_user($1::uuid)`, f.s("id")); err != nil {
		return "", err
	}
	if fg, ok := s.Users.(interface{ Forget(string) }); ok {
		fg.Forget(f.s("id"))
	}
	return "Compte désactivé : connexions fermées et sessions révoquées.", nil
}

// --- Droits d'un classeur -------------------------------------------------------------------

func inviteWorkbook(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	roles := f.list("roles")
	if len(roles) == 0 {
		return "", inputError("Choisissez au moins un rôle.")
	}
	_, err := tx.Exec(ctx, `SELECT lattis.invite($1, $2::uuid, $3::text[])`, f.s("email"), f.s("workbook_id"), roles)
	return "Invitation enregistrée pour " + strings.ToLower(f.s("email")) + ".", err
}

func createRole(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	_, err := tx.Exec(ctx, `SELECT lattis.create_role($1::uuid, $2)`, f.s("workbook_id"), f.s("name"))
	return "Rôle créé : choisissez maintenant ses niveaux.", err
}

func setLevel(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	kind, id, _ := strings.Cut(f.s("node"), ":")
	level := f.n("level")
	if level != nil && !slices.Contains(levels, level.(string)) {
		return "", inputError("Niveau inconnu.")
	}
	_, err := tx.Exec(ctx, `SELECT lattis.set_level($1::uuid, $2, $3, $4, $5, $6)`,
		f.s("role_id"), kind, id, level, f.n("read_filter"), f.n("write_filter"))
	return "Niveau enregistré.", err
}

// userByEmail : compte le plus récemment connecté pour une adresse.
func userByEmail(ctx context.Context, tx pgx.Tx, email string) (string, error) {
	var found *struct {
		ID       string `json:"id"`
		Verified bool   `json:"verified"`
	}
	if err := tx.QueryRow(ctx, `SELECT lattis.find_user($1)`, email).Scan(&found); err != nil {
		return "", err
	}
	if found == nil {
		return "", inputError("Aucun compte pour " + email + " : créez-le (page Utilisateurs) ou invitez la personne à un classeur.")
	}
	if !found.Verified {
		return "", inputError("L'adresse " + email + " a été choisie par la personne elle-même et n'est pas vérifiée : elle ne peut pas recevoir de droits par adresse.")
	}
	return found.ID, nil
}

func grantRole(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	email := strings.ToLower(f.s("email"))
	uid, err := userByEmail(ctx, tx, email)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `SELECT lattis.grant_role($1::uuid, $2::uuid)`, f.s("role_id"), uid)
	return email + " ajouté au rôle.", err
}

func addTeamMember(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	email := strings.ToLower(f.s("email"))
	uid, err := userByEmail(ctx, tx, email)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `SELECT lattis.add_team_member($1::uuid, $2::uuid)`, f.s("team_id"), uid)
	return email + " ajouté à l'équipe.", err
}

func addTeamManager(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	email := strings.ToLower(f.s("email"))
	uid, err := userByEmail(ctx, tx, email)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `SELECT lattis.set_team_manager($1::uuid, $2::uuid, true)`, f.s("team_id"), uid)
	return email + " gère maintenant cette équipe et ses sous-équipes.", err
}

func addRestriction(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	if f.s("table_id") == "" {
		return "", inputError("Ce classeur n'a pas encore de tableau.")
	}
	_, err := tx.Exec(ctx, `SELECT lattis.add_restriction($1::uuid, $2::uuid, $3, $4::uuid[], $5, $6)`,
		f.s("workbook_id"), f.s("table_id"), f.s("command"), f.list("roles"), f.s("predicate"), f.s("label"))
	return "Restriction ajoutée.", err
}

func addDataAudience(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	principal := f.s("team_id")
	if f.s("kind") == "team" && principal == "" {
		return "", inputError("Choisissez une équipe (page Équipes pour en créer une).")
	}
	if f.s("kind") != "team" {
		uid, err := userByEmail(ctx, tx, strings.ToLower(f.s("email")))
		if err != nil {
			return "", err
		}
		principal = uid
	}
	_, err := tx.Exec(ctx, `SELECT lattis.add_data_audience($1::uuid, $2::uuid, $3::uuid, $4)`,
		f.n("dataset_id"), f.n("folder_id"), principal, f.s("predicate"))
	return "Ajouté à l'audience.", err
}

// createPipeline : formulaire de /admin/pipelines → lattis.create_pipeline.
func (s *Service) createPipeline(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	auto := f.s("target") == "auto"
	wb, tbl, _ := strings.Cut(f.s("target"), "|")
	if !auto && (wb == "" || tbl == "") {
		return "", inputError("Choisissez le tableau cible.")
	}
	src := map[string]any{}
	for _, k := range []string{"url", "auth", "header", "host", "user", "path", "host_key", "pattern", "query", "sheet", "json_path"} {
		if v := f.s(k); v != "" {
			src[k] = v
		}
	}
	if v := f.r.PostFormValue("delimiter"); v != "" {
		src["delimiter"] = v
	}
	if v := f.s("port"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return "", inputError("Port : nombre attendu.")
		}
		src["port"] = n
	}
	var keys []string
	for _, k := range strings.Split(f.s("key_columns"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	if f.s("mode") == "upsert" && len(keys) == 0 {
		return "", inputError("Mise à jour par clé : indiquez au moins une colonne clé (par exemple « Code article »).")
	}
	if f.s("name") == "" {
		return "", inputError("Donnez un nom au pipeline.")
	}
	role := f.s("role")
	if auto {
		// Donnée neuve du Data Lake, colonnes créées d'après la source :
		// l'identité technique en est conceptrice (ajout de colonnes).
		name := f.s("auto_name")
		if name == "" {
			name = f.s("name")
		}
		b, _ := json.Marshal(map[string]any{"name": name, "folder_id": f.s("auto_folder"),
			"description": f.s("auto_description"), "key_columns": keys})
		var t struct {
			WorkbookID string `json:"workbook_id"`
			TableID    string `json:"table_id"`
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.create_datalake_target($1::jsonb)`, string(b)).Scan(&t); err != nil {
			return "", err
		}
		wb, tbl, role = t.WorkbookID, t.TableID, "designer"
		src["auto_columns"] = true
	}
	cmap := map[string]string{}
	for _, line := range strings.Split(f.r.PostFormValue("column_map"), "\n") {
		dst, srcCol, ok := strings.Cut(line, "=")
		if dst, srcCol = strings.TrimSpace(dst), strings.TrimSpace(srcCol); ok && dst != "" && srcCol != "" {
			cmap[dst] = srcCol
		}
	}
	format := f.s("format")
	if f.s("source_kind") == "sql" {
		format = "sql"
	}
	secretAny, _, err := s.secretRef(ctx, tx, f, "")
	if err != nil {
		return "", err
	}
	secret, _ := secretAny.(string)
	p := map[string]any{"name": f.s("name"), "workbook_id": wb, "table_id": tbl, "role": role,
		"source_kind": f.s("source_kind"), "source": src, "secret_ref": secret, "format": format,
		"mode": f.s("mode"), "key_columns": keys, "column_map": cmap, "schedule_kind": f.s("schedule_kind"),
		"webhook_url": f.s("webhook_url")}
	if keys == nil {
		p["key_columns"] = []string{}
	}
	switch f.s("schedule_kind") {
	case "every":
		p["schedule_minutes"] = f.s("schedule_minutes")
	case "daily":
		p["schedule_time"] = f.s("schedule_time")
	}
	b, _ := json.Marshal(p)
	var id string
	if err := tx.QueryRow(ctx, `SELECT lattis.create_pipeline($1::jsonb)::text`, string(b)).Scan(&id); err != nil {
		return "", err
	}
	if e := strings.TrimSpace(f.s("alert_email")); e != "" {
		if _, err := tx.Exec(ctx, `SELECT lattis.set_pipeline_alert_email($1::uuid, $2)`, id, e); err != nil {
			return "", err
		}
	}
	return "Pipeline créé.", nil
}

var columnTypes = map[string]string{"texte": "text", "text": "text", "entier": "integer", "integer": "integer",
	"décimal": "decimal", "decimal": "decimal", "nombre": "decimal", "date": "date", "date et heure": "datetime",
	"datetime": "datetime", "booléen": "boolean", "boolean": "boolean", "oui/non": "boolean"}

// createWorkbook : classeur publié vide (et son premier tableau), créé par
// l'administrateur ; un autre propriétaire peut être désigné.
func createWorkbook(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	var cols []map[string]string
	if f.s("table") != "" {
		for _, line := range strings.Split(f.r.PostFormValue("columns"), "\n") {
			name, typ, _ := strings.Cut(line, ":")
			name, typ = strings.TrimSpace(name), strings.ToLower(strings.TrimSpace(typ))
			if name == "" {
				continue
			}
			if typ == "" {
				typ = "texte"
			}
			t, ok := columnTypes[typ]
			if !ok {
				return "", inputError(fmt.Sprintf("Colonne « %s » : type « %s » inconnu (texte, entier, décimal, date, date et heure, booléen).", name, typ))
			}
			cols = append(cols, map[string]string{"name": name, "type": t})
		}
		if len(cols) == 0 {
			return "", inputError("Donnez au moins une colonne au premier tableau.")
		}
	}
	var wb string
	if err := tx.QueryRow(ctx, `SELECT lattis.create_workbook($1, $2)::text`, f.s("title"), f.s("mode")).Scan(&wb); err != nil {
		return "", err
	}
	if cols != nil {
		b, _ := json.Marshal(cols)
		if _, err := tx.Exec(ctx, `SELECT lattis.create_table($1::uuid, $2, $3::jsonb)`, wb, f.s("table"), string(b)); err != nil {
			return "", err
		}
	}
	if _, err := tx.Exec(ctx, `SELECT lattis.finish_publish($1::uuid)`, wb); err != nil {
		return "", err
	}
	msg := "Classeur « " + f.s("title") + " » créé."
	if owner := strings.ToLower(f.s("owner")); owner != "" {
		if _, err := tx.Exec(ctx, `SELECT lattis.invite($1, $2::uuid, ARRAY['owner'])`, owner, wb); err != nil {
			return "", err
		}
		msg += " " + owner + " en est propriétaire."
	}
	return msg, nil
}

// dropWorkbook : titre retapé et confirmation, plus une seconde confirmation
// si le classeur alimente le Data Lake.
func dropWorkbook(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	if !f.checked("confirm") {
		return "", errConfirm
	}
	var title string
	if err := tx.QueryRow(ctx, `SELECT lattis.workbook_acl($1::uuid) #>> '{workbook,title}'`, f.s("id")).Scan(&title); err != nil {
		return "", err
	}
	if strings.TrimSpace(f.s("title")) != title {
		return "", inputError("Le titre tapé ne correspond pas : le classeur n'a pas été supprimé.")
	}
	ds, err := workbookDatasets(ctx, tx, f.s("id"))
	if err != nil {
		return "", err
	}
	if len(ds) > 0 && !f.checked("confirm_datasets") {
		return "", inputError(fmt.Sprintf("Ce classeur alimente %d donnée(s) du Data Lake : cochez aussi leur retrait.", len(ds)))
	}
	for _, d := range ds {
		if _, err := tx.Exec(ctx, `SELECT lattis.drop_dataset($1::uuid)`, d["id"]); err != nil {
			return "", err
		}
	}
	if _, err := tx.Exec(ctx, `SELECT lattis.drop_workbook($1::uuid)`, f.s("id")); err != nil {
		return "", err
	}
	return "Classeur « " + title + " » supprimé.", nil
}

// simulatePipeline : bilan d'une exécution sans rien écrire.
func (s *Service) simulatePipeline(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	if s.Pipelines == nil {
		return "", inputError("Simulation indisponible sur ce serveur.")
	}
	res, err := s.Pipelines.Simulate(ctx, f.s("id"), "")
	if err != nil {
		return "", err
	}
	if res.Status == "error" {
		return "", inputError("Simulation : " + res.Summary())
	}
	return "Simulation (rien n'a été écrit) : " + res.Summary(), nil
}
