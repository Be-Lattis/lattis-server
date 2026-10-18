-- Lectures de l'interface d'administration (spécification §15). Toutes
-- réservées aux propriétaires du serveur, sauf workbook_acl (propriétaires du
-- classeur aussi). Les écritures passent par les fonctions déjà publiées.

-- Classeurs avec propriétaires, membres et taille.
CREATE FUNCTION lattis.admin_workbooks() RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_server_owner();
    RETURN (SELECT coalesce(jsonb_agg(jsonb_build_object(
            'id', w.id, 'title', w.title, 'mode', w.mode, 'state', w.state, 'version', w.version,
            'schema', w.schema_name, 'created_by', w.created_by, 'created_at', w.created_at,
            'published_at', w.published_at,
            'owners', (SELECT coalesce(jsonb_agg(u.email ORDER BY u.email), '[]')
                       FROM lattis.roles r JOIN lattis.members m ON m.role_id = r.id
                       JOIN lattis.users u ON u.id = m.principal
                       WHERE r.workbook_id = w.id AND r.builtin = 'owner'),
            'members', (SELECT count(DISTINCT m.principal) FROM lattis.roles r
                        JOIN lattis.members m ON m.role_id = r.id WHERE r.workbook_id = w.id),
            'size', (SELECT coalesce(sum(pg_total_relation_size(c.oid)), 0) FROM pg_class c
                     JOIN pg_namespace n ON n.oid = c.relnamespace
                     WHERE n.nspname IN (w.schema_name, coalesce(w.staging_schema, '')) AND c.relkind IN ('r', 'p', 'm')))
        ORDER BY w.title), '[]') FROM lattis.workbooks w);
END
$$;

-- État du serveur : PostgreSQL, connexions, réplicas.
CREATE FUNCTION lattis.admin_status() RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_server_owner();
    RETURN jsonb_build_object(
        'pg_version', current_setting('server_version'),
        'in_recovery', pg_is_in_recovery(),
        'database', current_database(),
        'database_size', pg_database_size(current_database()),
        'started_at', pg_postmaster_start_time(),
        'max_connections', current_setting('max_connections')::integer,
        'connections', (SELECT coalesce(jsonb_object_agg(state, n), '{}') FROM (
                            SELECT coalesce(state, 'unknown') AS state, count(*) AS n FROM pg_stat_activity
                            WHERE backend_type = 'client backend' GROUP BY 1) s),
        'users', (SELECT count(*) FROM lattis.users),
        'workbooks', (SELECT count(*) FROM lattis.workbooks),
        'replicas', (SELECT coalesce(jsonb_agg(jsonb_build_object(
                         'name', application_name, 'address', client_addr::text, 'state', state,
                         'sync_state', sync_state,
                         'lag_seconds', extract(epoch FROM replay_lag),
                         'lag_bytes', pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn))
                         ORDER BY application_name), '[]')
                     FROM pg_stat_replication WHERE NOT pg_is_in_recovery()));
END
$$;

-- workbook_acl : ajoute le classeur et l'arborescence feuilles → tableaux.
CREATE OR REPLACE FUNCTION lattis.workbook_acl(p_workbook_id uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner}', true);
BEGIN
    RETURN jsonb_build_object(
        'workbook', jsonb_build_object('id', v.id, 'title', v.title, 'mode', v.mode, 'state', v.state,
                                       'version', v.version, 'schema', v.schema_name),
        'tables', (SELECT coalesce(jsonb_agg(jsonb_build_object('table_id', t.table_id, 'name', t.name,
                                                                'sheet_id', t.sheet_id)), '[]')
                   FROM lattis.data_tables(v.schema_name) t),
        'roles', (SELECT coalesce(jsonb_agg(jsonb_build_object(
                     'id', r.id, 'name', r.name, 'builtin', r.builtin,
                     'grants', (SELECT coalesce(jsonb_agg(jsonb_build_object('node_kind', g.node_kind, 'node_id', g.node_id,
                                    'level', g.level, 'read_filter', g.read_filter, 'write_filter', g.write_filter)
                                    ORDER BY g.node_kind DESC, g.node_id), '[]')
                                FROM lattis.role_grants g WHERE g.role_id = r.id),
                     'members', (SELECT coalesce(jsonb_agg(jsonb_build_object('user_id', u.id, 'email', u.email,
                                    'display_name', u.display_name) ORDER BY u.email), '[]')
                                 FROM lattis.members m JOIN lattis.users u ON u.id = m.principal WHERE m.role_id = r.id))
                  ORDER BY array_position(ARRAY['owner', 'designer', 'editor', 'participant', 'reader'], r.builtin),
                           r.created_at, r.name), '[]')
                  FROM lattis.roles r WHERE r.workbook_id = p_workbook_id),
        'restrictions', (SELECT coalesce(jsonb_agg(to_jsonb(x) - 'workbook_id' ORDER BY x.label), '[]') FROM lattis.restrictions x
                         WHERE x.workbook_id = p_workbook_id),
        'invitations', (SELECT coalesce(jsonb_agg(jsonb_build_object('id', i.id, 'email', i.email, 'roles', i.roles,
                                                                     'expires_at', i.expires_at) ORDER BY i.email), '[]')
                        FROM lattis.invitations i WHERE i.workbook_id = p_workbook_id
                          AND i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > now()));
END
$$;

-- set_config : valide aussi les réglages ajoutés par 0003 et le logo.
CREATE OR REPLACE FUNCTION lattis.set_config(p_key text, p_value jsonb) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_before jsonb;
BEGIN
    PERFORM lattis.require_server_owner();
    SELECT value INTO v_before FROM lattis.server_config WHERE key = p_key FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'réglage inconnu : %', p_key USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF p_key = 'access_mode' AND (p_value #>> '{}') NOT IN ('invitation', 'open') THEN
        RAISE EXCEPTION 'access_mode : invitation ou open' USING ERRCODE = 'invalid_parameter_value';
    ELSIF p_key = 'default_role' AND (p_value #>> '{}') NOT IN ('none', 'reader', 'participant', 'editor') THEN
        RAISE EXCEPTION 'default_role : none, reader, participant ou editor' USING ERRCODE = 'invalid_parameter_value';
    ELSIF p_key = 'web_scripting' AND jsonb_typeof(p_value) <> 'boolean' THEN
        RAISE EXCEPTION 'web_scripting : booléen attendu' USING ERRCODE = 'invalid_parameter_value';
    ELSIF p_key = 'workbook_creators' AND (p_value #>> '{}') NOT IN ('everyone', 'owners') THEN
        RAISE EXCEPTION 'workbook_creators : everyone ou owners' USING ERRCODE = 'invalid_parameter_value';
    ELSIF p_key = 'logo_url' AND jsonb_typeof(p_value) <> 'null'
          AND (jsonb_typeof(p_value) <> 'string' OR (p_value #>> '{}') !~ '^https://[^\s"<>]+$') THEN
        RAISE EXCEPTION 'logo_url : adresse https:// ou vide' USING ERRCODE = 'invalid_parameter_value';
    ELSIF p_key LIKE '%\_ttl' OR p_key IN ('tombstone_retention', 'password_max_failures', 'password_lock_seconds',
                                           'republish_grace') THEN
        IF jsonb_typeof(p_value) <> 'number' OR (p_value #>> '{}')::numeric <= 0 THEN
            RAISE EXCEPTION '% : nombre positif attendu', p_key USING ERRCODE = 'invalid_parameter_value';
        END IF;
    ELSIF p_key IN ('display_name') AND (jsonb_typeof(p_value) <> 'string' OR p_value #>> '{}' = '') THEN
        RAISE EXCEPTION '% : texte attendu', p_key USING ERRCODE = 'invalid_parameter_value';
    END IF;
    UPDATE lattis.server_config SET value = p_value WHERE key = p_key;
    PERFORM lattis.audit_log('set_config', p_key, v_before, p_value);
END
$$;

GRANT EXECUTE ON FUNCTION lattis.admin_workbooks(), lattis.admin_status() TO PUBLIC;
