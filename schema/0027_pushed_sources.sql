-- Tableau publié seul au Data Lake (retour de l'application) : depuis un
-- classeur local, sans publier le classeur, l'application crée une « source
-- poussée » : un classeur technique (kind = 'source', absent des listes de
-- classeurs) qui ne contient que ce tableau, un pipeline de type « push »
-- qui l'alimente, et la donnée du Data Lake tirée de ce tableau. Ensuite,
-- l'application envoie le contenu du tableau (POST /api/push/<pipeline>, CSV)
-- et lattisd l'écrit par le pipeline (remplacement, ajout ou mise à jour par
-- clé), sous son identité technique, comme tout pipeline.

ALTER TABLE lattis.workbooks ADD COLUMN kind text NOT NULL DEFAULT 'workbook' CHECK (kind IN ('workbook', 'source'));
ALTER TABLE lattis.pipelines DROP CONSTRAINT pipelines_source_kind_check,
    ADD CONSTRAINT pipelines_source_kind_check CHECK (source_kind IN ('upload', 'file', 'http', 'sftp', 'sql', 'push'));

-- Création d'un classeur sans le contrôle des créateurs (classeurs techniques).
CREATE FUNCTION lattis.create_workbook_as(p_title text, p_mode text, p_kind text) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_id    uuid := gen_random_uuid();
    v_slug  text;
    v_owner text;
    v_user  lattis.users;
    v_role  record;
    v_rid   uuid;
BEGIN
    IF p_mode NOT IN ('frozen', 'collaborative') THEN
        RAISE EXCEPTION 'mode : frozen ou collaborative' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF coalesce(trim(p_title), '') = '' THEN
        RAISE EXCEPTION 'titre manquant' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    SELECT * INTO v_user FROM lattis.users WHERE pg_role = session_user AND NOT disabled;
    IF v_user.id IS NULL THEN
        RAISE EXCEPTION 'seul un utilisateur Lattis crée un classeur' USING ERRCODE = 'insufficient_privilege';
    END IF;

    v_slug := lattis.slugify(p_title, v_id);
    v_owner := 'wb_' || v_slug || '_owner';
    EXECUTE format('CREATE ROLE %I NOLOGIN', v_owner);
    -- lattis_service agit comme propriétaire (DDL, ALTER … OWNER).
    EXECUTE format('GRANT %I TO lattis_service WITH INHERIT TRUE, SET TRUE', v_owner);
    PERFORM lattis.create_workbook_schema('wb_' || v_slug, v_owner);

    INSERT INTO lattis.workbooks (id, slug, schema_name, title, mode, created_by, kind)
    VALUES (v_id, v_slug, 'wb_' || v_slug, trim(p_title), p_mode, session_user, p_kind);

    FOR v_role IN
        SELECT * FROM (VALUES ('owner', 'Propriétaire', 1), ('designer', 'Concepteur', 2), ('editor', 'Éditeur', 3),
                              ('participant', 'Participant', 4), ('reader', 'Lecteur', 5)) AS r(builtin, label, ord)
        WHERE p_mode = 'collaborative' OR builtin IN ('owner', 'reader')
        ORDER BY ord
    LOOP
        INSERT INTO lattis.roles (workbook_id, name, builtin, pg_role)
        VALUES (v_id, v_role.label, v_role.builtin, 'wb_' || v_slug || '_r_' || v_role.builtin)
        RETURNING id INTO v_rid;
        EXECUTE format('CREATE ROLE %I NOLOGIN', 'wb_' || v_slug || '_r_' || v_role.builtin);
        EXECUTE format('GRANT USAGE ON SCHEMA %I TO %I', 'wb_' || v_slug, 'wb_' || v_slug || '_r_' || v_role.builtin);
        -- Niveau par défaut sur tout le classeur.
        INSERT INTO lattis.role_grants (role_id, node_kind, node_id, level)
        VALUES (v_rid, 'workbook', v_id::text, CASE v_role.builtin
            WHEN 'reader' THEN 'reader' WHEN 'participant' THEN 'participant' ELSE 'editor' END);
    END LOOP;

    PERFORM lattis.add_member(v_id, 'owner', v_user.id);
    PERFORM lattis.audit_log('create_workbook', v_id::text, NULL,
        jsonb_build_object('title', p_title, 'mode', p_mode, 'schema', 'wb_' || v_slug));
    RETURN v_id;
END
$$;
REVOKE EXECUTE ON FUNCTION lattis.create_workbook_as(text, text, text) FROM PUBLIC;

CREATE OR REPLACE FUNCTION lattis.create_workbook(p_title text, p_mode text) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    IF NOT lattis.can_create_workbooks() THEN
        RAISE EXCEPTION 'la création de classeurs est réservée aux propriétaires du serveur et aux créateurs désignés'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN lattis.create_workbook_as(p_title, p_mode, 'workbook');
END
$$;

-- Pipelines créés ou supprimés pour une source poussée (droits vérifiés par l'appelant).
CREATE FUNCTION lattis.create_pipeline_as(p jsonb) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_id   uuid := gen_random_uuid();
    v      lattis.workbooks;
    v_tab  record;
    v_user lattis.users;
    v_role uuid;
BEGIN
    v := lattis.workbook((p ->> 'workbook_id')::uuid);
    IF v.mode <> 'collaborative' OR v.state <> 'published' THEN
        RAISE EXCEPTION 'un pipeline écrit dans un classeur collaboratif publié' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    EXECUTE format('SELECT table_id, name, local_id FROM %I.lattis_meta_tables WHERE table_id = $1', v.schema_name)
        INTO v_tab USING (p ->> 'table_id')::uuid;
    IF v_tab.table_id IS NULL THEN
        RAISE EXCEPTION 'tableau inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    PERFORM lattis.pipeline_check(p, v, v_tab.name);
    SELECT id INTO v_role FROM lattis.roles WHERE workbook_id = v.id AND builtin = coalesce(nullif(p ->> 'role', ''), 'editor');
    IF v_role IS NULL THEN
        RAISE EXCEPTION 'rôle « % » inexistant sur ce classeur', p ->> 'role' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    -- Identité technique : un compte sans fournisseur, jamais connecté en personne.
    v_user := lattis.create_user('pipeline+' || left(replace(v_id::text, '-', ''), 12) || '@lattis.invalid',
                                 'Pipeline : ' || trim(p ->> 'name'), NULL, NULL);
    PERFORM lattis.activate_user(v_user.id);
    INSERT INTO lattis.pipelines (id, name, workbook_id, table_local_id, table_id, table_name, source_kind, source, secret_ref,
                                  format, mode, key_columns, column_map, schedule_kind, schedule_minutes, schedule_time,
                                  webhook_url, user_id, created_by)
    VALUES (v_id, trim(p ->> 'name'), v.id, v_tab.local_id, v_tab.table_id, v_tab.name, p ->> 'source_kind',
            coalesce(p -> 'source', '{}'), nullif(p ->> 'secret_ref', ''), coalesce(nullif(p ->> 'format', ''), 'csv'), p ->> 'mode',
            ARRAY(SELECT jsonb_array_elements_text(coalesce(p -> 'key_columns', '[]'))), coalesce(p -> 'column_map', '{}'),
            coalesce(nullif(p ->> 'schedule_kind', ''), 'manual'), (nullif(p ->> 'schedule_minutes', ''))::integer,
            nullif(p ->> 'schedule_time', ''), nullif(p ->> 'webhook_url', ''), v_user.id, lattis.actor());
    PERFORM lattis.grant_role(v_role, v_user.id);
    PERFORM lattis.audit_log('create_pipeline', v_id::text, NULL, p);
    RETURN v_id;
END
$$;
REVOKE EXECUTE ON FUNCTION lattis.create_pipeline_as(jsonb) FROM PUBLIC;

CREATE FUNCTION lattis.drop_pipeline_as(p_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    x lattis.pipelines;
    r record;
BEGIN
    SELECT * INTO x FROM lattis.pipelines WHERE id = p_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'pipeline inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    FOR r IN SELECT m.role_id FROM lattis.members m WHERE m.principal = x.user_id LOOP
        PERFORM lattis.revoke_role(r.role_id, x.user_id);
    END LOOP;
    DELETE FROM lattis.pipelines WHERE id = p_id;
    -- Identité technique désactivée (disable_user est réservée aux propriétaires du serveur).
    UPDATE lattis.users SET disabled = true WHERE id = x.user_id;
    EXECUTE format('ALTER ROLE %I NOLOGIN', (SELECT pg_role FROM lattis.users WHERE id = x.user_id));
    PERFORM lattis.audit_log('drop_pipeline', p_id::text, to_jsonb(x), NULL);
END
$$;
REVOKE EXECUTE ON FUNCTION lattis.drop_pipeline_as(uuid) FROM PUBLIC;

-- Les classeurs techniques n'apparaissent pas parmi les classeurs, ni parmi
-- les cibles des pipelines créés dans l'administration.
CREATE OR REPLACE FUNCTION lattis.my_workbooks() RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT coalesce(jsonb_agg(jsonb_build_object(
               'id', w.id, 'title', w.title, 'mode', w.mode, 'state', w.state, 'version', w.version,
               'schema', w.schema_name, 'staging_schema', CASE WHEN lattis.my_builtin_roles(w.id) && '{owner,designer}'
                                                              THEN w.staging_schema END,
               'roles', lattis.my_builtin_roles(w.id), 'published_at', w.published_at)
           ORDER BY w.title), '[]')
    FROM lattis.workbooks w
    WHERE w.kind = 'workbook' AND cardinality(lattis.my_builtin_roles(w.id)) > 0
      AND (w.state <> 'draft' OR lattis.my_builtin_roles(w.id) && '{owner,designer}')
$$;

CREATE OR REPLACE FUNCTION lattis.admin_workbooks() RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_server_owner();
    RETURN (SELECT coalesce(jsonb_agg(jsonb_build_object(
            'id', w.id, 'title', w.title, 'kind', w.kind, 'mode', w.mode, 'state', w.state, 'version', w.version,
            'schema', w.schema_name, 'created_by', w.created_by, 'created_at', w.created_at,
            'published_at', w.published_at,
            'last_edit', lattis.workbook_last_edit(w.id),
            'last_read', (SELECT jsonb_build_object('email', u.email, 'at', r.read_at)
                          FROM lattis.workbook_reads r JOIN lattis.users u ON u.id = r.user_id
                          WHERE r.workbook_id = w.id ORDER BY r.read_at DESC LIMIT 1),
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

CREATE OR REPLACE FUNCTION lattis.pipeline_targets() RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    w     lattis.workbooks;
    t     record;
    v_out jsonb := '[]';
    v_cols jsonb;
BEGIN
    PERFORM lattis.require_server_owner();
    FOR w IN SELECT * FROM lattis.workbooks WHERE mode = 'collaborative' AND state = 'published' AND kind = 'workbook' ORDER BY lower(title) LOOP
        FOR t IN SELECT * FROM lattis.data_tables(w.schema_name) LOOP
            EXECUTE format('SELECT coalesce(jsonb_agg(jsonb_build_object(''name'', name, ''type'', type) ORDER BY position), ''[]'')
                            FROM %I.lattis_meta_columns WHERE table_id = $1', w.schema_name) INTO v_cols USING t.table_id;
            v_out := v_out || jsonb_build_object('workbook_id', w.id, 'workbook', w.title, 'table_id', t.table_id,
                                                 'table', t.name, 'columns', v_cols);
        END LOOP;
    END LOOP;
    RETURN v_out;
END
$$;

CREATE OR REPLACE FUNCTION lattis.admin_pipelines() RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_server_owner();
    RETURN (SELECT coalesce(jsonb_agg(to_jsonb(x) || jsonb_build_object(
                'workbook', w.title, 'workbook_kind', w.kind, 'identity', u.email,
                'datasets', (SELECT coalesce(jsonb_agg(jsonb_build_object('id', d.id, 'name', d.name, 'sql_name', d.sql_name)), '[]')
                             FROM lattis.datasets d WHERE d.workbook_id = w.id AND d.table_id = x.table_id),
                'runs', (SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY r.started_at DESC), '[]')
                         FROM (SELECT * FROM lattis.pipeline_runs WHERE pipeline_id = x.id ORDER BY started_at DESC LIMIT 10) r))
            ORDER BY lower(x.name)), '[]')
            FROM lattis.pipelines x JOIN lattis.workbooks w ON w.id = x.workbook_id JOIN lattis.users u ON u.id = x.user_id);
END
$$;

-- p : name, description, folder_id, sql_name (facultatif), table_name
-- (facultatif : name), columns [{"name", "type"}], mode (replace par défaut,
-- append, upsert), key_columns, origin (libre : classeur et tableau locaux,
-- par exemple {"workbook": "Ventes.lattis", "table": "Clients", "table_local_id": "12"}).
-- Réservé aux publieurs du Data Lake. Renvoie {pipeline_id, dataset_id,
-- workbook_id, table_id, sql_name}.
CREATE FUNCTION lattis.create_pushed_source(p jsonb) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_name text := trim(coalesce(p ->> 'name', ''));
    v_mode text := coalesce(nullif(p ->> 'mode', ''), 'replace');
    v_wb   uuid;
    v_tid  uuid;
    v_pid  uuid;
    v_ds   uuid;
BEGIN
    PERFORM lattis.require_data_publisher();
    IF v_name = '' THEN
        RAISE EXCEPTION 'nom de la donnée manquant' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF v_mode NOT IN ('replace', 'append', 'upsert') THEN
        RAISE EXCEPTION 'mode : replace, append ou upsert' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    v_wb := lattis.create_workbook_as('Source : ' || v_name, 'collaborative', 'source');
    v_tid := lattis.create_table(v_wb, coalesce(nullif(trim(p ->> 'table_name'), ''), v_name), p -> 'columns');
    PERFORM lattis.finish_publish(v_wb);
    v_pid := lattis.create_pipeline_as(jsonb_build_object(
        'name', v_name, 'workbook_id', v_wb, 'table_id', v_tid, 'source_kind', 'push', 'format', 'csv', 'mode', v_mode,
        'key_columns', coalesce(p -> 'key_columns', '[]'),
        'source', jsonb_build_object('origin', coalesce(p -> 'origin', '{}'),
                                     'pushed_by', (SELECT id FROM lattis.users WHERE pg_role = session_user))));
    v_ds := lattis.create_dataset(v_wb, v_tid, nullif(p ->> 'folder_id', '')::uuid, v_name, p ->> 'description',
                                  p ->> 'sql_name', '{}');
    RETURN jsonb_build_object('pipeline_id', v_pid, 'dataset_id', v_ds, 'workbook_id', v_wb, 'table_id', v_tid,
                              'sql_name', (SELECT sql_name FROM lattis.datasets WHERE id = v_ds));
END
$$;

-- Qui pousse : les propriétaires du classeur technique (son créateur) et
-- les propriétaires du serveur.
CREATE FUNCTION lattis.require_push(p_id uuid) RETURNS lattis.pipelines
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    x lattis.pipelines;
BEGIN
    SELECT * INTO x FROM lattis.pipelines WHERE id = p_id AND source_kind = 'push';
    IF NOT FOUND THEN
        RAISE EXCEPTION 'source poussée inconnue' USING ERRCODE = 'no_data_found';
    END IF;
    PERFORM lattis.require_workbook_role(x.workbook_id, '{owner}', true);
    RETURN x;
END
$$;
REVOKE EXECUTE ON FUNCTION lattis.require_push(uuid) FROM PUBLIC;

-- Appelée par lattisd sous l'identité de l'utilisateur, une fois le fichier
-- déposé : le pipeline le prend au prochain relevé.
CREATE FUNCTION lattis.push_pipeline(p_id uuid, p_file text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    x lattis.pipelines := lattis.require_push(p_id);
BEGIN
    IF p_file !~ '^[A-Za-z0-9._-]{1,200}$' THEN
        RAISE EXCEPTION 'nom de fichier invalide' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF x.pending_file IS NOT NULL THEN
        RAISE EXCEPTION 'un envoi attend déjà d''être traité : patientez la fin de son exécution' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF NOT x.enabled THEN
        RAISE EXCEPTION 'source poussée désactivée' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    UPDATE lattis.pipelines SET next_run_at = now(), pending_file = p_file, pending_trigger = 'push' WHERE id = p_id;
    PERFORM lattis.audit_log('push_pipeline', p_id::text, NULL, jsonb_build_object('file', p_file));
END
$$;

-- Suppression d'une source poussée : donnée(s), pipeline et classeur technique.
CREATE FUNCTION lattis.drop_pushed_source(p_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    x lattis.pipelines := lattis.require_push(p_id);
    d uuid;
BEGIN
    IF (SELECT kind FROM lattis.workbooks WHERE id = x.workbook_id) <> 'source' THEN
        RAISE EXCEPTION 'ce pipeline n''est pas une source poussée' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    FOR d IN SELECT id FROM lattis.datasets WHERE workbook_id = x.workbook_id LOOP
        PERFORM lattis.drop_dataset(d);
    END LOOP;
    PERFORM lattis.drop_pipeline_as(p_id);
    PERFORM lattis.drop_workbook(x.workbook_id);
END
$$;

-- Sources poussées de l'utilisateur : pipeline, donnée, dernier envoi.
CREATE FUNCTION lattis.my_pushed_sources() RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT coalesce(jsonb_agg(jsonb_build_object(
               'pipeline_id', x.id, 'name', x.name, 'mode', x.mode, 'key_columns', to_jsonb(x.key_columns),
               'origin', x.source -> 'origin', 'workbook_id', x.workbook_id, 'table_id', x.table_id,
               'enabled', x.enabled, 'pending', x.pending_file IS NOT NULL,
               'datasets', (SELECT coalesce(jsonb_agg(jsonb_build_object('id', d.id, 'name', d.name, 'sql_name', d.sql_name)), '[]')
                            FROM lattis.datasets d WHERE d.workbook_id = x.workbook_id),
               'last_run', (SELECT to_jsonb(r) - 'pipeline_id' FROM lattis.pipeline_runs r
                            WHERE r.pipeline_id = x.id ORDER BY r.started_at DESC LIMIT 1))
           ORDER BY lower(x.name)), '[]')
    FROM lattis.pipelines x JOIN lattis.workbooks w ON w.id = x.workbook_id
    WHERE x.source_kind = 'push' AND w.kind = 'source' AND 'owner' = ANY (lattis.my_builtin_roles(w.id))
$$;

GRANT EXECUTE ON FUNCTION lattis.create_pushed_source(jsonb), lattis.push_pipeline(uuid, text),
    lattis.drop_pushed_source(uuid), lattis.my_pushed_sources() TO PUBLIC;
