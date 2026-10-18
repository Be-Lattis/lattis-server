package admin

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"

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
		"add_provider":         addProvider,
		"update_provider":      updateProvider,
		"remove_provider":      exec1(`SELECT lattis.remove_provider($1::uuid)`, "Fournisseur supprimé.", true, "id"),
		"allow_tenant":         allowTenant,
		"disallow_tenant":      exec1(`SELECT lattis.disallow_tenant($1::uuid)`, "Tenant retiré.", false, "id"),
		"add_user":             s.addUser,
		"reset_password":       s.resetPassword,
		"disable_user":         s.disableUser,
		"workbook_creator_on":  exec1(`SELECT lattis.set_workbook_creator($1::uuid, true)`, "Cette personne peut maintenant créer des classeurs.", false, "id"),
		"workbook_creator_off": exec1(`SELECT lattis.set_workbook_creator($1::uuid, false)`, "Rôle de créateur de classeurs retiré.", false, "id"),
		"enable_user":          exec1(`SELECT lattis.enable_user($1::uuid)`, "Compte réactivé.", false, "id"),
		"revoke_invitation":    exec1(`SELECT lattis.revoke_invitation($1::uuid)`, "Invitation retirée.", false, "id"),
		"drop_workbook":        exec1(`SELECT lattis.drop_workbook($1::uuid)`, "Classeur supprimé.", true, "id"),
		"create_team":          exec1(`SELECT lattis.create_team($1, $2::uuid)`, "Équipe créée.", false, "name", "parent_id"),
		"data_publisher_on":    exec1(`SELECT lattis.set_data_publisher($1::uuid, true)`, "Cette personne peut maintenant partager des données.", false, "id"),
		"data_publisher_off":   exec1(`SELECT lattis.set_data_publisher($1::uuid, false)`, "Rôle de publieur de données retiré.", false, "id"),
		"create_data_folder":   exec1(`SELECT lattis.create_data_folder($1, $2::uuid)`, "Dossier créé.", false, "name", "parent_id"),
		"rename_data_folder":   exec1(`SELECT lattis.rename_data_folder($1::uuid, $2)`, "Dossier renommé.", false, "id", "name"),
		"move_data_folder":     exec1(`SELECT lattis.move_data_folder($1::uuid, $2::uuid)`, "Dossier déplacé.", false, "id", "parent_id"),
		"drop_data_folder":     exec1(`SELECT lattis.drop_data_folder($1::uuid)`, "Dossier supprimé.", true, "id"),
		"drop_dataset":         exec1(`SELECT lattis.drop_dataset($1::uuid)`, "Partage retiré.", true, "id"),
		"remove_data_audience": exec1(`SELECT lattis.remove_data_audience($1::uuid)`, "Retiré de l'audience.", false, "id"),
		"add_data_audience":    addDataAudience,
		"create_pipeline":      createPipeline,
		"run_pipeline":         exec1(`SELECT lattis.run_pipeline($1::uuid, NULL)`, "Exécution demandée : elle démarre dans les secondes qui viennent.", false, "id"),
		"toggle_pipeline":      exec1(`SELECT lattis.update_pipeline($1::uuid, jsonb_build_object('enabled', $2::boolean))`, "Pipeline mis à jour.", false, "id", "enabled"),
		"drop_pipeline":        exec1(`SELECT lattis.drop_pipeline($1::uuid)`, "Pipeline supprimé.", true, "id"),
		"set_team_parent":      exec1(`SELECT lattis.set_team_parent($1::uuid, $2::uuid)`, "Équipe déplacée.", false, "id", "parent_id"),
		"rename_team":          exec1(`SELECT lattis.rename_team($1::uuid, $2)`, "Équipe renommée.", false, "id", "name"),
		"drop_team":            exec1(`SELECT lattis.drop_team($1::uuid)`, "Équipe supprimée.", true, "id"),
		"add_team_member":      addTeamMember,
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

func (s *Service) do(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil || !s.checkCSRF(r) {
		s.message(w, r, http.StatusForbidden, "Demande refusée", "Jeton de formulaire invalide : rechargez la page.")
		return
	}
	fn, ok := s.actions()[r.PathValue("action")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.perform(w, r, fn)
}

// perform exécute une action sous l'identité de l'administrateur connecté
// (formulaire déjà lu et jeton anti-CSRF vérifié), puis revient à la page.
func (s *Service) perform(w http.ResponseWriter, r *http.Request, fn actionFunc) {
	u, err := s.session(r)
	if err != nil {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	back := safeBack(r.PostFormValue("back"))
	if back == "" {
		back = "/admin/server"
	}
	f := form{r}
	var msg string
	err = s.Users.As(r.Context(), u, func(tx pgx.Tx) error {
		// Seuls les propriétaires du serveur utilisent l'administration web ;
		// les fonctions vérifient de toute façon leurs propres droits.
		var owner bool
		if err := tx.QueryRow(r.Context(), `SELECT lattis.is_server_owner()`).Scan(&owner); err != nil {
			return err
		}
		if !owner {
			return inputError("Réservé aux propriétaires du serveur.")
		}
		var err error
		msg, err = fn(r.Context(), tx, f)
		return err
	})
	var ie inputError
	switch {
	case err == nil:
		s.setFlash(w, flash{OK: msg})
	case errors.Is(err, errConfirm):
		s.setFlash(w, flash{Err: "Cochez « Confirmer » pour cette action."})
	case errors.As(err, &ie):
		s.setFlash(w, flash{Err: string(ie)})
	default:
		if m, ok := userMessage(err); ok {
			s.setFlash(w, flash{Err: m})
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

func addProvider(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	_, err := tx.Exec(ctx, `SELECT lattis.add_provider($1, $2, $3, $4, $5, $6::text[])`,
		f.s("kind"), f.s("label"), f.n("issuer"), f.n("client_id"), f.n("secret_ref"), scopes(f.s("scopes")))
	return "Fournisseur ajouté.", err
}

func updateProvider(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	pos, err := strconv.Atoi(f.s("position"))
	if err != nil {
		return "", inputError("Ordre : nombre entier attendu.")
	}
	changes := map[string]any{"label": f.s("label"), "enabled": f.checked("enabled"), "position": pos}
	if f.s("kind") != "password" {
		changes["issuer"] = f.n("issuer")
		changes["client_id"] = f.n("client_id")
		changes["client_secret_ref"] = f.n("secret_ref")
		changes["claim_name"] = f.n("claim_name")
		changes["claim_value"] = f.n("claim_value")
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
	return "Compte créé pour " + strings.ToLower(f.s("email")) + ". Lien à lui transmettre : " + s.setupLink(token), nil
}

func (s *Service) resetPassword(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	var token string
	if err := tx.QueryRow(ctx, `SELECT lattis.reset_password($1::uuid)`, f.s("id")).Scan(&token); err != nil {
		return "", err
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
	var uid string
	err := tx.QueryRow(ctx, `SELECT id::text FROM lattis.users WHERE email = $1
		ORDER BY last_login_at DESC NULLS LAST LIMIT 1`, email).Scan(&uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", inputError("Aucun compte pour " + email + " : invitez-le d'abord.")
	}
	return uid, err
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

func addRestriction(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	_, err := tx.Exec(ctx, `SELECT lattis.add_restriction($1::uuid, $2::uuid, $3, $4::uuid[], $5, $6)`,
		f.s("workbook_id"), f.s("table_id"), f.s("command"), f.list("roles"), f.s("predicate"), f.s("label"))
	return "Restriction ajoutée.", err
}

func addDataAudience(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	principal := f.s("team_id")
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
func createPipeline(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	wb, tbl, _ := strings.Cut(f.s("target"), "|")
	if wb == "" || tbl == "" {
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
	p := map[string]any{"name": f.s("name"), "workbook_id": wb, "table_id": tbl, "role": f.s("role"),
		"source_kind": f.s("source_kind"), "source": src, "secret_ref": f.s("secret_ref"), "format": format,
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
	_, err := tx.Exec(ctx, `SELECT lattis.create_pipeline($1::jsonb)`, string(b))
	return "Pipeline créé.", err
}
