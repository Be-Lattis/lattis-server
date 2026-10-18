package admin

import (
	"context"
	"encoding/json"
	"errors"
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
		"server_settings":         s.saveSettings,
		"add_provider":            addProvider,
		"update_provider":         updateProvider,
		"remove_provider":         exec1(`SELECT lattis.remove_provider($1::uuid)`, "Fournisseur supprimé.", true, "id"),
		"allow_tenant":            allowTenant,
		"disallow_tenant":         exec1(`SELECT lattis.disallow_tenant($1::uuid)`, "Tenant retiré.", false, "id"),
		"create_password_account": s.createPasswordAccount,
		"reset_password":          s.resetPassword,
		"disable_user":            s.disableUser,
		"enable_user":             exec1(`SELECT lattis.enable_user($1::uuid)`, "Compte réactivé.", false, "id"),
		"invite_server":           inviteServer,
		"revoke_invitation":       exec1(`SELECT lattis.revoke_invitation($1::uuid)`, "Invitation retirée.", false, "id"),
		"drop_workbook":           exec1(`SELECT lattis.drop_workbook($1::uuid)`, "Classeur supprimé.", true, "id"),
		"invite_workbook":         inviteWorkbook,
		"create_role":             createRole,
		"drop_role":               exec1(`SELECT lattis.drop_role($1::uuid)`, "Rôle supprimé.", true, "id"),
		"set_level":               setLevel,
		"grant_role":              grantRole,
		"revoke_role":             exec1(`SELECT lattis.revoke_role($1::uuid, $2::uuid)`, "Membre retiré.", false, "role_id", "user_id"),
		"add_restriction":         addRestriction,
		"drop_restriction":        exec1(`SELECT lattis.drop_restriction($1::uuid)`, "Restriction supprimée.", false, "id"),
	}
}

func (s *Service) do(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil || !s.checkCSRF(r) {
		s.message(w, r, http.StatusForbidden, "Demande refusée", "Jeton de formulaire invalide : rechargez la page.")
		return
	}
	u, err := s.session(r)
	if err != nil {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	fn, ok := s.actions()[r.PathValue("action")]
	if !ok {
		http.NotFound(w, r)
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
		case "number":
			n, err := strconv.ParseFloat(strings.ReplaceAll(f.s(st.Key), " ", ""), 64)
			if err != nil {
				return "", inputError(st.Label + " : nombre attendu.")
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

func (s *Service) createPasswordAccount(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	var token string
	if err := tx.QueryRow(ctx, `SELECT lattis.create_password_account($1, $2)`, f.s("email"), f.s("display_name")).
		Scan(&token); err != nil {
		return "", err
	}
	return "Compte créé. Lien à transmettre pour choisir le mot de passe : " + s.setupLink(token), nil
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

func inviteServer(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	roles := []string{}
	if f.checked("server_owner") {
		roles = append(roles, "server_owner")
	}
	_, err := tx.Exec(ctx, `SELECT lattis.invite($1, NULL, $2::text[])`, f.s("email"), roles)
	return "Invitation enregistrée pour " + strings.ToLower(f.s("email")) + ".", err
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

func grantRole(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	email := strings.ToLower(f.s("email"))
	var uid string
	err := tx.QueryRow(ctx, `SELECT id::text FROM lattis.users WHERE email = $1
		ORDER BY last_login_at DESC NULLS LAST LIMIT 1`, email).Scan(&uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", inputError("Aucun compte pour " + email + " : invitez-le d'abord.")
	}
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `SELECT lattis.grant_role($1::uuid, $2::uuid)`, f.s("role_id"), uid)
	return email + " ajouté au rôle.", err
}

func addRestriction(ctx context.Context, tx pgx.Tx, f form) (string, error) {
	_, err := tx.Exec(ctx, `SELECT lattis.add_restriction($1::uuid, $2::uuid, $3, $4::uuid[], $5, $6)`,
		f.s("workbook_id"), f.s("table_id"), f.s("command"), f.list("roles"), f.s("predicate"), f.s("label"))
	return "Restriction ajoutée.", err
}
