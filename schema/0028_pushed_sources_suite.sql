-- Sources poussées, retours de l'application :
--  * update_pushed_source : le créateur change le mode, la clé ou suspend ;
--  * chaque envoi reçoit l'identifiant de son exécution (pending_run_id,
--    repris par lattisd), que l'application suit (pushed_run) ;
--  * data_catalog donne, pour une source poussée, son type, son pipeline et
--    le classeur d'origine (source.kind, source.pipeline_id, source.origin).

ALTER TABLE lattis.pipelines ADD COLUMN pending_run_id uuid;

DROP FUNCTION lattis.push_pipeline(uuid, text);
CREATE FUNCTION lattis.push_pipeline(p_id uuid, p_file text) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    x     lattis.pipelines := lattis.require_push(p_id);
    v_run uuid := gen_random_uuid();
BEGIN
    IF p_file !~ '^[A-Za-z0-9._-]{1,200}$' THEN
        RAISE EXCEPTION 'nom de fichier invalide' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF x.pending_file IS NOT NULL THEN
        RAISE EXCEPTION 'un envoi attend déjà d''être traité : patientez la fin de son exécution' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF NOT x.enabled THEN
        RAISE EXCEPTION 'source poussée suspendue' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    UPDATE lattis.pipelines SET next_run_at = now(), pending_file = p_file, pending_trigger = 'push', pending_run_id = v_run
    WHERE id = p_id;
    PERFORM lattis.audit_log('push_pipeline', p_id::text, NULL, jsonb_build_object('file', p_file, 'run_id', v_run));
    RETURN v_run;
END
$$;

-- Exécution d'un envoi : le bilan, {"status": "pending"} tant qu'elle n'a
-- pas commencé, NULL si inconnue.
CREATE FUNCTION lattis.pushed_run(p_id uuid, p_run_id uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    x lattis.pipelines := lattis.require_push(p_id);
BEGIN
    IF x.pending_run_id = p_run_id THEN
        RETURN jsonb_build_object('id', p_run_id, 'status', 'pending');
    END IF;
    RETURN (SELECT to_jsonb(r) - 'pipeline_id' FROM lattis.pipeline_runs r WHERE r.id = p_run_id AND r.pipeline_id = p_id);
END
$$;

-- p : mode (replace, append, upsert), key_columns, enabled ; champs facultatifs.
CREATE FUNCTION lattis.update_pushed_source(p_id uuid, p jsonb) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    x     lattis.pipelines := lattis.require_push(p_id);
    v_key text;
    v_mode text;
    v_keys text[];
BEGIN
    FOR v_key IN SELECT jsonb_object_keys(p) LOOP
        IF v_key NOT IN ('mode', 'key_columns', 'enabled') THEN
            RAISE EXCEPTION 'champ inconnu : %', v_key USING ERRCODE = 'invalid_parameter_value';
        END IF;
    END LOOP;
    v_mode := CASE WHEN p ? 'mode' THEN p ->> 'mode' ELSE x.mode END;
    IF v_mode NOT IN ('replace', 'append', 'upsert') THEN
        RAISE EXCEPTION 'mode : replace, append ou upsert' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    v_keys := CASE WHEN p ? 'key_columns' THEN ARRAY(SELECT jsonb_array_elements_text(p -> 'key_columns')) ELSE x.key_columns END;
    IF v_mode = 'upsert' AND cardinality(v_keys) = 0 THEN
        RAISE EXCEPTION 'mise à jour par clé : choisissez au moins une colonne clé' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    PERFORM lattis.pipeline_check(jsonb_build_object('key_columns', to_jsonb(v_keys)), lattis.workbook(x.workbook_id), x.table_name);
    UPDATE lattis.pipelines SET mode = v_mode, key_columns = v_keys,
           enabled = CASE WHEN p ? 'enabled' THEN (p ->> 'enabled')::boolean ELSE enabled END, updated_at = now()
    WHERE id = p_id;
    PERFORM lattis.audit_log('update_pushed_source', p_id::text, to_jsonb(x), p);
END
$$;

CREATE OR REPLACE FUNCTION lattis.data_catalog() RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_pub boolean := lattis.can_publish_data();
BEGIN
    RETURN (
    WITH vis AS (
        SELECT d.*, lattis.can_read_dataset(d) AS readable, lattis.can_manage_dataset(d) AS manageable
        FROM lattis.datasets d
    ), shown AS (
        SELECT * FROM vis WHERE readable OR manageable
    ), folders AS (
        SELECT f.*, (SELECT string_agg(a.name, ' › ' ORDER BY c.depth DESC) FROM lattis.data_folder_closure() c
                     JOIN lattis.data_folders a ON a.id = c.ancestor WHERE c.descendant = f.id) AS path,
               (SELECT max(c.depth) FROM lattis.data_folder_closure() c WHERE c.descendant = f.id) AS depth
        FROM lattis.data_folders f
        WHERE v_pub OR EXISTS (SELECT 1 FROM shown s JOIN lattis.data_folder_closure() c ON c.descendant = s.folder_id
                               WHERE c.ancestor = f.id)
    )
    SELECT jsonb_build_object(
        'can_publish', v_pub,
        'folders', (SELECT coalesce(jsonb_agg(jsonb_build_object('id', f.id, 'parent_id', f.parent_id, 'name', f.name,
                        'path', f.path, 'depth', f.depth,
                        'audience', CASE WHEN v_pub THEN lattis.data_audience_json(NULL, f.id) END) ORDER BY lower(f.path)), '[]')
                    FROM folders f),
        'datasets', (SELECT coalesce(jsonb_agg(jsonb_build_object(
                        'id', s.id, 'folder_id', s.folder_id, 'name', s.name, 'description', s.description,
                        'sql_name', s.sql_name, 'readable', s.readable, 'manageable', s.manageable,
                        'available', s.table_id IS NOT NULL AND s.workbook_id IS NOT NULL,
                        'table_id', s.table_id, 'channel', CASE WHEN w.id IS NOT NULL THEN 'wb_' || w.slug END,
                        'columns', (SELECT coalesce(jsonb_agg(jsonb_build_object('name', a.attname,
                                        'type', format_type(a.atttypid, a.atttypmod)) ORDER BY a.attnum), '[]')
                                    FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
                                    JOIN pg_namespace n ON n.oid = c.relnamespace
                                    WHERE n.nspname = 'donnees' AND c.relname = s.sql_name AND a.attnum > 0 AND NOT a.attisdropped),
                        'updated_at', s.updated_at,
                        'source', CASE WHEN s.manageable THEN jsonb_build_object('workbook_id', s.workbook_id, 'title', w.title,
                                        'kind', coalesce(w.kind, 'workbook'),
                                        'pipeline_id', (SELECT x.id FROM lattis.pipelines x WHERE w.kind = 'source'
                                                        AND x.workbook_id = w.id AND x.source_kind = 'push' LIMIT 1),
                                        'origin', (SELECT x.source -> 'origin' FROM lattis.pipelines x WHERE w.kind = 'source'
                                                   AND x.workbook_id = w.id AND x.source_kind = 'push' LIMIT 1),
                                        'table', s.table_name, 'hidden_columns', to_jsonb(s.hidden_columns),
                                        'inherit_audience', s.inherit_audience) END,
                        'audience', CASE WHEN s.manageable THEN lattis.data_audience_json(s.id, NULL) END,
                        'used_by', CASE WHEN s.manageable THEN (SELECT coalesce(jsonb_agg(jsonb_build_object(
                                        'workbook_id', l.workbook_id, 'title', lw.title, 'linked_at', l.linked_at,
                                        'last_edit', lattis.workbook_last_edit(l.workbook_id),
                                        'last_read', (SELECT jsonb_build_object('email', u.email, 'at', r.read_at)
                                                      FROM lattis.workbook_reads r JOIN lattis.users u ON u.id = r.user_id
                                                      WHERE r.workbook_id = l.workbook_id ORDER BY r.read_at DESC LIMIT 1))
                                        ORDER BY lw.title), '[]')
                                        FROM lattis.dataset_links l JOIN lattis.workbooks lw ON lw.id = l.workbook_id
                                        WHERE l.dataset_id = s.id) END)
                    ORDER BY lower(s.name)), '[]')
                     FROM shown s LEFT JOIN lattis.workbooks w ON w.id = s.workbook_id)));
END
$$;

GRANT EXECUTE ON FUNCTION lattis.push_pipeline(uuid, text), lattis.pushed_run(uuid, uuid),
    lattis.update_pushed_source(uuid, jsonb) TO PUBLIC;
