package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/store"
)

var levels = []string{"none", "reader", "participant", "editor"}

func levelName(l any) string {
	switch l {
	case "none":
		return "Aucun accès"
	case "reader":
		return "Lecteur"
	case "participant":
		return "Participant"
	case "editor":
		return "Éditeur"
	case nil, "":
		return "Hérité"
	}
	return l.(string)
}

func builtinName(b any) string {
	switch b {
	case "owner":
		return "Propriétaires"
	case "designer":
		return "Concepteurs"
	case "editor":
		return "Éditeurs"
	case "participant":
		return "Participants"
	case "reader":
		return "Lecteurs"
	}
	return ""
}

func queryJSON(ctx context.Context, tx pgx.Tx, q string, args ...any) (any, error) {
	var v any
	err := tx.QueryRow(ctx, q, args...).Scan(&v)
	return v, err
}

// --- Serveur -------------------------------------------------------------------------

// setting décrit un champ de l'écran « Serveur ».
type setting struct {
	Key, Label, Help, Kind string // Kind : text, url, number, duration, bool, select
	Options                [][2]string
	// Unit, Scale : durée affichée dans cette unité, enregistrée en secondes.
	Unit  string
	Scale float64
	// ShowIf : n'afficher que si un autre champ a cette valeur (data-show-if).
	ShowIf string
	Value  any
}

// settingGroup regroupe les réglages par thème, du plus courant au plus
// technique ; Advanced se replie.
type settingGroup struct {
	Title, Help string
	Advanced    bool
	Settings    []setting
}

const (
	minute = 60.0
	day    = 86400.0
)

var settingGroups = []settingGroup{
	{Title: "Identité", Help: "Ce que voient les personnes à la connexion et dans Lattis.", Settings: []setting{
		{Key: "display_name", Label: "Nom du serveur", Kind: "text"},
		{Key: "logo_url", Label: "Logo (adresse https://)", Kind: "url"},
	}},
	{Title: "Accès et publication", Help: "Qui peut entrer sur le serveur et qui peut y publier des classeurs.", Settings: []setting{
		{Key: "access_mode", Label: "Mode d'accès", Kind: "select",
			Options: [][2]string{{"invitation", "Sur invitation"}, {"open", "Entrée libre"}},
			Help:    "Sur invitation : seules les personnes invitées ou créées ici entrent."},
		{Key: "default_role", Label: "Rôle des arrivants (entrée libre)", Kind: "select",
			Options: [][2]string{{"none", "Aucun"}, {"reader", "Lecteur"}, {"participant", "Participant"}, {"editor", "Éditeur"}},
			Help:    "Rôle donné sur les classeurs ouverts aux arrivants.", ShowIf: "access_mode=open"},
		{Key: "workbook_creators", Label: "Qui publie des classeurs", Kind: "select",
			Options: [][2]string{{"everyone", "Tout utilisateur"}, {"owners", "Propriétaires et créateurs"}},
			Help:    "Le rôle « créateur de classeurs » se donne sur la page Utilisateurs."},
		{Key: "invitation_ttl", Label: "Validité des invitations", Kind: "duration", Unit: "jours", Scale: day},
	}},
	{Title: "Connexion et sécurité", Help: "Durée des sessions et protection des comptes à mot de passe.", Settings: []setting{
		{Key: "refresh_token_ttl", Label: "Durée d'une session", Kind: "duration", Unit: "jours", Scale: day,
			Help: "Sans usage pendant ce temps, il faut se reconnecter."},
		{Key: "access_token_ttl", Label: "Durée des jetons d'accès", Kind: "duration", Unit: "minutes", Scale: minute,
			Help: "Renouvelés sans rien demander ; plus court, un compte désactivé est coupé plus vite."},
		{Key: "password_max_failures", Label: "Essais de mot de passe avant verrouillage", Kind: "number"},
		{Key: "password_lock_seconds", Label: "Durée du verrouillage", Kind: "duration", Unit: "minutes", Scale: minute},
	}},
	{Title: "Avancé", Help: "Rarement à changer.", Advanced: true, Settings: []setting{
		{Key: "web_scripting", Label: "Scripts dans l'application web", Kind: "bool", Help: "Édition web seulement."},
		{Key: "tombstone_retention", Label: "Conservation des suppressions pour la synchronisation", Kind: "duration", Unit: "jours", Scale: day,
			Help: "Un poste resté hors ligne plus longtemps recharge le classeur entier."},
		{Key: "republish_grace", Label: "Délai avant suppression d'une ancienne version", Kind: "duration", Unit: "minutes", Scale: minute,
			Help: "Après une republication, laisse aux postes ouverts le temps de basculer."},
	}},
}

// settings : tous les réglages, dans l'ordre de l'écran.
var settings = func() []setting {
	var out []setting
	for _, g := range settingGroups {
		out = append(out, g.Settings...)
	}
	return out
}()

func readSettings(ctx context.Context, tx pgx.Tx) (map[string]any, error) {
	rows, err := tx.Query(ctx, `SELECT key, value FROM lattis.server_config`)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for rows.Next() {
		var k string
		var v any
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func loadServer(ctx context.Context, tx pgx.Tx, r *http.Request, u store.User) (any, error) {
	cur, err := readSettings(ctx, tx)
	if err != nil {
		return nil, err
	}
	out := make([]settingGroup, 0, len(settingGroups))
	for _, g := range settingGroups {
		g.Settings = append([]setting(nil), g.Settings...)
		for i := range g.Settings {
			st := &g.Settings[i]
			st.Value = cur[st.Key]
			if f, ok := st.Value.(float64); ok && st.Kind == "duration" {
				st.Value = strconv.FormatFloat(f/st.Scale, 'f', -1, 64)
			}
		}
		out = append(out, g)
	}
	return out, nil
}

// --- Connexion ---------------------------------------------------------------------------

type providerRow struct {
	store.Provider
	Tenants []map[string]any
}

var providerKinds = [][2]string{
	{"google", "Google"}, {"google_workspace", "Google Workspace"}, {"microsoft", "Microsoft"},
	{"keycloak", "Keycloak"}, {"oidc", "OIDC générique"},
}

func loadProviders(ctx context.Context, tx pgx.Tx, r *http.Request, u store.User) (any, error) {
	rows, err := tx.Query(ctx, `SELECT id::text, kind, label, coalesce(issuer, ''), coalesce(client_id, ''),
		coalesce(client_secret_ref, ''), scopes, coalesce(claim_name, ''), coalesce(claim_value, ''), enabled, position
		FROM lattis.identity_providers ORDER BY position, label`)
	if err != nil {
		return nil, err
	}
	var list []*providerRow
	for rows.Next() {
		p := &providerRow{}
		if err := rows.Scan(&p.ID, &p.Kind, &p.Label, &p.Issuer, &p.ClientID, &p.SecretRef, &p.Scopes,
			&p.ClaimName, &p.ClaimValue, &p.Enabled, &p.Position); err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	trows, err := tx.Query(ctx, `SELECT provider_id::text, jsonb_build_object('id', id, 'kind', kind, 'value', value,
		'default_role', default_role) FROM lattis.allowed_tenants ORDER BY kind, value`)
	if err != nil {
		return nil, err
	}
	for trows.Next() {
		var pid string
		var t map[string]any
		if err := trows.Scan(&pid, &t); err != nil {
			return nil, err
		}
		for _, p := range list {
			if p.ID == pid {
				p.Tenants = append(p.Tenants, t)
			}
		}
	}
	return map[string]any{"Providers": list, "Kinds": providerKinds}, trows.Err()
}

// --- Utilisateurs -------------------------------------------------------------------------

func loadUsers(ctx context.Context, tx pgx.Tx, r *http.Request, u store.User) (any, error) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	users, err := queryJSON(ctx, tx, `
		SELECT coalesce(jsonb_agg(jsonb_build_object(
			'id', u.id, 'email', u.email, 'display_name', u.display_name, 'provider', p.label, 'kind', p.kind,
			'created_at', u.created_at, 'last_login_at', u.last_login_at, 'disabled', u.disabled,
			'pending', u.subject IS NULL AND u.last_login_at IS NULL,
			'server_owner', pg_has_role(u.pg_role, 'lattis_server_owners', 'MEMBER'),
			'workbook_creator', pg_has_role(u.pg_role, 'lattis_workbook_creators', 'MEMBER'))
			ORDER BY u.email), '[]')
		FROM (SELECT * FROM lattis.users
		      WHERE $1 = '' OR email ILIKE '%' || $1 || '%' OR display_name ILIKE '%' || $1 || '%'
		      ORDER BY email LIMIT 500) u
		LEFT JOIN lattis.identity_providers p ON p.id = u.provider_id`, q)
	if err != nil {
		return nil, err
	}
	invitations, err := queryJSON(ctx, tx, `
		SELECT coalesce(jsonb_agg(jsonb_build_object('id', i.id, 'email', i.email, 'roles', i.roles,
			'workbook', w.title, 'invited_by', i.invited_by, 'expires_at', i.expires_at) ORDER BY i.created_at DESC), '[]')
		FROM lattis.invitations i LEFT JOIN lattis.workbooks w ON w.id = i.workbook_id
		WHERE i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > now()`)
	if err != nil {
		return nil, err
	}
	return map[string]any{"Q": q, "Users": users, "Invitations": invitations, "Me": u.ID}, nil
}

// --- Équipes -------------------------------------------------------------------------------

func loadTeams(ctx context.Context, tx pgx.Tx, r *http.Request, u store.User) (any, error) {
	teams, err := queryJSON(ctx, tx, `SELECT lattis.list_teams()`)
	return map[string]any{"Teams": teams}, err
}

// --- Classeurs -----------------------------------------------------------------------------

func loadWorkbooks(ctx context.Context, tx pgx.Tx, r *http.Request, u store.User) (any, error) {
	return queryJSON(ctx, tx, `SELECT lattis.admin_workbooks()`)
}

// nodeLevel : niveau propre d'un rôle sur un nœud, pour l'arborescence.
type nodeLevel struct {
	Kind, ID, Label string
	Depth           int
	Level           any
	ReadFilter      any
	WriteFilter     any
}

type roleView struct {
	Role  map[string]any
	Nodes []nodeLevel
}

func (s *Service) loadWorkbook(ctx context.Context, tx pgx.Tx, r *http.Request, u store.User) (any, error) {
	id := r.PathValue("id")
	var acl map[string]any
	if err := tx.QueryRow(ctx, `SELECT lattis.workbook_acl($1::uuid)`, id).Scan(&acl); err != nil {
		return nil, err
	}
	wb := acl["workbook"].(map[string]any)
	tables, _ := acl["tables"].([]any)
	// Arborescence classeur → feuilles → tableaux.
	type node struct {
		kind, id, label string
		depth           int
	}
	nodes := []node{{"workbook", id, "Classeur entier", 0}}
	var sheets []string
	for _, t := range tables {
		if sid, _ := t.(map[string]any)["sheet_id"].(string); sid != "" && !slices.Contains(sheets, sid) {
			sheets = append(sheets, sid)
		}
	}
	for _, sid := range sheets {
		nodes = append(nodes, node{"sheet", sid, "Feuille " + sid, 1})
		for _, t := range tables {
			tm := t.(map[string]any)
			if tm["sheet_id"] == sid {
				nodes = append(nodes, node{"table", tm["table_id"].(string), tm["name"].(string), 2})
			}
		}
	}
	for _, t := range tables {
		tm := t.(map[string]any)
		if sid, _ := tm["sheet_id"].(string); sid == "" {
			nodes = append(nodes, node{"table", tm["table_id"].(string), tm["name"].(string), 1})
		}
	}
	var roles []roleView
	for _, ra := range acl["roles"].([]any) {
		rm := ra.(map[string]any)
		rv := roleView{Role: rm}
		if b := rm["builtin"]; b != "owner" && b != "designer" && wb["mode"] == "collaborative" {
			for _, n := range nodes {
				nl := nodeLevel{Kind: n.kind, ID: n.id, Label: n.label, Depth: n.depth}
				for _, g := range rm["grants"].([]any) {
					gm := g.(map[string]any)
					if gm["node_kind"] == n.kind && (gm["node_id"] == n.id || n.kind == "workbook") {
						nl.Level, nl.ReadFilter, nl.WriteFilter = gm["level"], gm["read_filter"], gm["write_filter"]
					}
				}
				rv.Nodes = append(rv.Nodes, nl)
			}
		}
		roles = append(roles, rv)
	}
	teams, err := queryJSON(ctx, tx, `SELECT lattis.list_teams()`)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"ACL": acl, "Workbook": wb, "Tables": tables, "Roles": roles, "Levels": levels, "Teams": teams}
	if wb["mode"] == "collaborative" && wb["state"] == "published" {
		drift, err := queryJSON(ctx, tx, `SELECT lattis.check_drift($1::uuid)`, id)
		if err != nil {
			return nil, err
		}
		out["Drift"] = drift
	}
	// « Voir en tant que… »
	q := r.URL.Query()
	if email, table := strings.TrimSpace(q.Get("preview_user")), q.Get("preview_table"); email != "" && table != "" {
		out["PreviewUser"], out["PreviewTable"] = email, table
		var uid string
		err := tx.QueryRow(ctx, `SELECT id::text FROM lattis.users WHERE email = lower($1)
			ORDER BY last_login_at DESC NULLS LAST LIMIT 1`, email).Scan(&uid)
		if err == pgx.ErrNoRows {
			out["PreviewError"] = "Aucun compte pour " + email + "."
		} else if err != nil {
			return nil, err
		} else if rows, err := s.Store.Preview(ctx, u.PGRole, id, uid, table); err != nil {
			if msg, ok := userMessage(err); ok {
				out["PreviewError"] = msg
			} else {
				return nil, err
			}
		} else {
			out["Preview"] = previewTable(rows)
		}
	}
	return out, nil
}

type preview struct {
	Columns []string
	Rows    [][]string
	Limited bool
}

// previewTable met les lignes JSON en tableau, colonnes système à la fin.
func previewTable(rows []json.RawMessage) preview {
	var p preview
	var objs []map[string]any
	for _, raw := range rows {
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil {
			objs = append(objs, m)
			for k := range m {
				if !slices.Contains(p.Columns, k) {
					p.Columns = append(p.Columns, k)
				}
			}
		}
	}
	slices.SortStableFunc(p.Columns, func(a, b string) int {
		sa, sb := strings.HasPrefix(a, "lattis_"), strings.HasPrefix(b, "lattis_")
		if sa != sb {
			if sa {
				return 1
			}
			return -1
		}
		return strings.Compare(a, b)
	})
	for _, m := range objs {
		var line []string
		for _, c := range p.Columns {
			line = append(line, funcs["str"].(func(any) string)(m[c]))
		}
		p.Rows = append(p.Rows, line)
	}
	p.Limited = len(rows) == store.PreviewLimit
	return p
}

// --- Journal -------------------------------------------------------------------------------

const auditPage = 100

func loadAudit(ctx context.Context, tx pgx.Tx, r *http.Request, u store.User) (any, error) {
	q := r.URL.Query()
	actor, action := strings.TrimSpace(q.Get("actor")), strings.TrimSpace(q.Get("action"))
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	var since *time.Time
	if t, err := time.ParseInLocation("2006-01-02", q.Get("since"), time.Local); err == nil {
		since = &t
	}
	rows, err := tx.Query(ctx, `
		SELECT id, at, actor, action, coalesce(target, ''), before, after FROM lattis.audit
		WHERE ($1 = '' OR actor ILIKE '%' || $1 || '%')
		  AND ($2 = '' OR action = $2)
		  AND ($3 = 0 OR id < $3)
		  AND ($4::timestamptz IS NULL OR at >= $4)
		ORDER BY id DESC LIMIT $5`, actor, action, before, since, auditPage+1)
	if err != nil {
		return nil, err
	}
	type entry struct {
		ID                    int64
		At                    time.Time
		Actor, Action, Target string
		Before, After         any
	}
	var list []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Action, &e.Target, &e.Before, &e.After); err != nil {
			return nil, err
		}
		list = append(list, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var next int64
	if len(list) > auditPage {
		list = list[:auditPage]
		next = list[auditPage-1].ID
	}
	var actions []string
	arows, err := tx.Query(ctx, `SELECT DISTINCT action FROM lattis.audit ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	actions, err = pgx.CollectRows(arows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	nextURL := ""
	if next > 0 {
		v := url.Values{"actor": {actor}, "action": {action}, "since": {q.Get("since")}, "before": {strconv.FormatInt(next, 10)}}
		nextURL = "/admin/audit?" + v.Encode()
	}
	return map[string]any{"Entries": list, "Actor": actor, "Action": action, "Since": q.Get("since"),
		"Actions": actions, "Next": nextURL}, nil
}

// --- État ----------------------------------------------------------------------------------

func (s *Service) loadStatus(ctx context.Context, tx pgx.Tx, r *http.Request, u store.User) (any, error) {
	st, err := queryJSON(ctx, tx, `SELECT lattis.admin_status()`)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"PG": st, "Endpoints": s.Config.PGPublicRW, "RO": s.Config.PGPublicRO}
	if s.Replicas != nil {
		out["Monitored"] = s.Replicas.States()
		out["Announced"] = s.Replicas.Available()
		out["MaxLag"] = s.Config.ReplicaMaxLag.String()
	}
	return out, nil
}
