package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

// publicAPI est la liste exacte des fonctions appelables par tout rôle
// connecté. Toute autre fonction lattis.* est interne à lattis_service.
var publicAPI = []string{
	"abort_publish", "actor", "add_data_audience", "add_data_audience_by_email", "add_provider", "add_restriction",
	"add_team_member", "admin_pipelines", "admin_status", "admin_workbooks", "allow_tenant", "can_create_workbooks",
	"can_publish_data", "changes_since", "check_drift", "compile_acl", "create_data_folder", "create_dataset",
	"create_password_account", "create_pipeline", "create_role", "create_table", "create_team", "create_workbook",
	"data_catalog", "dataset_changed_ids", "dataset_tombstones", "disable_user", "disallow_tenant", "drop_data_folder", "drop_dataset",
	"drop_pipeline", "drop_restriction", "drop_role", "drop_team", "drop_workbook", "enable_user", "finish_publish",
	"grant_role", "invite", "is_server_owner", "link_dataset", "list_teams", "login_summary", "move_data_folder",
	"my_capabilities", "my_teams", "my_workbooks", "pipeline_targets", "publish_script", "record_open",
	"remove_data_audience", "remove_provider", "remove_team_member", "rename_data_folder", "rename_team", "republish",
	"reset_password", "revoke_invitation", "revoke_role", "row_state", "run_pipeline", "save_meta", "set_config",
	"set_data_publisher", "set_level", "set_team_parent", "set_workbook_creator", "since_covered", "sync_point",
	"table_ref", "tombstones_since", "trg_notify", "trg_stamp", "unlink_dataset", "update_dataset", "update_pipeline",
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
