package store_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

// publicAPI est la liste exacte des fonctions appelables par tout rôle
// connecté. Toute autre fonction lattis.* est interne à lattis_service.
var publicAPI = []string{
	"abort_publish", "actor", "add_column", "alter_column_type", "create_index", "drop_column", "drop_index", "drop_table", "add_data_audience", "add_data_audience_by_email", "add_provider", "add_restriction",
	"add_team_member", "admin_pipelines", "admin_status", "admin_workbooks", "allow_tenant", "can_create_workbooks",
	"can_publish_data", "changes_since", "check_drift", "compile_acl", "create_data_folder", "create_dataset",
	"create_password_account", "create_pipeline", "create_pushed_source", "create_role", "create_table", "create_team", "create_workbook",
	"data_catalog", "dataset_audience_history", "dataset_changed_ids", "dataset_tombstones", "disable_user", "disallow_tenant", "drop_data_folder", "drop_dataset",
	"drop_pipeline", "drop_pushed_source", "drop_restriction", "drop_role", "drop_team", "drop_workbook", "enable_user", "finish_publish",
	"grant_role", "insert_column", "invite", "is_server_owner", "link_dataset", "list_teams", "login_summary", "move_data_folder",
	"my_capabilities", "my_pushed_sources", "my_teams", "my_workbooks", "pipeline_targets", "publish_script", "push_pipeline", "record_open",
	"remove_data_audience", "remove_provider", "remove_team_member", "rename_column", "rename_data_folder", "rename_table", "rename_team", "republish",
	"reset_password", "revoke_invitation", "revoke_role", "row_state", "run_pipeline", "save_meta", "set_config",
	"set_data_publisher", "set_key", "set_level", "set_team_parent", "set_workbook_creator", "setup_link_expiry", "since_covered", "sync_point",
	"table_indexes", "table_ref", "tombstones_since", "trg_notify", "trg_stamp", "unlink_dataset", "update_dataset", "update_pipeline",
	"update_provider", "why_not", "workbook_acl", "workbook_members",
}

func TestPublicFunctions(t *testing.T) {
	db := testpg.New(t)
	rows, err := db.Super.Query(context.Background(), `
		SELECT DISTINCT p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'lattis' AND has_function_privilege('public', p.oid, 'EXECUTE')
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	for _, n := range got {
		if !slices.Contains(publicAPI, n) {
			t.Errorf("lattis.%s appelable par tous", n)
		}
	}
	for _, n := range publicAPI {
		if !slices.Contains(got, n) {
			t.Errorf("lattis.%s n'est pas accordée", n)
		}
	}
}

// Tables du schéma lattis : un utilisateur ordinaire n'en lit aucune (seuls
// les noms et pg_role des équipes, pour les listes de choix). Les noms des
// tables restent visibles dans le catalogue de PostgreSQL, pas leur contenu.
func TestInternalTablesHidden(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	plain := openUsers(t, db, "ordinaire")[0]
	rows, err := db.Super.Query(ctx, `
		SELECT c.relname || coalesce('(' || a.attname || ')', '')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		     AND has_column_privilege($1, c.oid, a.attnum, 'SELECT, INSERT, UPDATE, REFERENCES')
		WHERE n.nspname = 'lattis' AND c.relkind IN ('r', 'v', 'm', 'p', 'S')
		  AND (has_table_privilege($1, c.oid, 'SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER')
		       OR a.attname IS NOT NULL)
		ORDER BY 1`, plain.PGRole)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	want := []string{"teams(id)", "teams(name)", "teams(parent_id)", "teams(pg_role)"}
	if !slices.Equal(got, want) {
		t.Fatalf("lisible par un utilisateur ordinaire : %v", got)
	}
	err = db.AsUser(t, plain.PGRole, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT count(*) FROM lattis.audit`)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "42501") {
		t.Fatalf("journal lisible : %v", err)
	}
}
