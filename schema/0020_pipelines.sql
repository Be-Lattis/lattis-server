-- Pipelines d'ingestion (plan, phase 9 point 10) : lattisd lit une source
-- (fichier déposé ou envoyé, HTTP, SFTP, base PostgreSQL externe) et écrit
-- dans un tableau d'un classeur collaboratif publié, par ajout, mise à jour
-- par clé ou remplacement, à la demande ou selon un horaire.
--
-- Chaque pipeline écrit sous une identité technique : un utilisateur Lattis
-- « Pipeline : <nom> », membre d'un rôle du classeur (Éditeur par défaut).
-- Ses écritures passent donc par la RLS, les déclencheurs (pierres tombales,
-- NOTIFY) et le journal comme celles d'une personne, et « lattis_author »
-- dit quel pipeline a écrit une ligne. Il ne se connecte jamais lui-même :
-- lattisd ouvre la connexion avec un jeton signé pour lui.
--
-- Les secrets (mot de passe SFTP, jeton d'API, chaîne de connexion) ne sont
-- jamais en base : secret_ref nomme un fichier du dossier des secrets.

CREATE TABLE lattis.pipelines (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name             text NOT NULL CHECK (length(trim(name)) BETWEEN 1 AND 200),
    workbook_id      uuid NOT NULL REFERENCES lattis.workbooks (id) ON DELETE CASCADE,
    table_local_id   text,
    table_id         uuid NOT NULL,
    table_name       text NOT NULL,
    source_kind      text NOT NULL CHECK (source_kind IN ('upload', 'file', 'http', 'sftp', 'sql')),
    source           jsonb NOT NULL DEFAULT '{}',
    secret_ref       text CHECK (secret_ref IS NULL OR secret_ref ~ '^[A-Za-z0-9._-]{1,200}$'),
    format           text NOT NULL DEFAULT 'csv' CHECK (format IN ('csv', 'xlsx', 'json', 'sql')),
    mode             text NOT NULL CHECK (mode IN ('append', 'upsert', 'replace')),
    key_columns      text[] NOT NULL DEFAULT '{}',
    column_map       jsonb NOT NULL DEFAULT '{}',
    schedule_kind    text NOT NULL DEFAULT 'manual' CHECK (schedule_kind IN ('manual', 'every', 'daily')),
    schedule_minutes integer CHECK (schedule_minutes IS NULL OR schedule_minutes >= 5),
    schedule_time    text CHECK (schedule_time IS NULL OR schedule_time ~ '^([01][0-9]|2[0-3]):[0-5][0-9]$'),
    next_run_at      timestamptz,
    pending_file     text,
    pending_trigger  text,
    enabled          boolean NOT NULL DEFAULT true,
    webhook_url      text CHECK (webhook_url IS NULL OR webhook_url ~ '^https://'),
    user_id          uuid NOT NULL REFERENCES lattis.users (id),
    created_by       text NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CHECK (mode <> 'upsert' OR cardinality(key_columns) > 0),
    CHECK (schedule_kind <> 'every' OR schedule_minutes IS NOT NULL),
    CHECK (schedule_kind <> 'daily' OR schedule_time IS NOT NULL)
);
CREATE INDEX pipelines_due ON lattis.pipelines (next_run_at) WHERE enabled;
REVOKE ALL ON lattis.pipelines FROM PUBLIC;

CREATE TABLE lattis.pipeline_runs (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    pipeline_id uuid NOT NULL REFERENCES lattis.pipelines (id) ON DELETE CASCADE,
    trigger     text NOT NULL,
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    status      text NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'ok', 'warning', 'error')),
    rows_read   integer NOT NULL DEFAULT 0,
    inserted    integer NOT NULL DEFAULT 0,
    updated     integer NOT NULL DEFAULT 0,
    deleted     integer NOT NULL DEFAULT 0,
    rejected    integer NOT NULL DEFAULT 0,
    rejects     jsonb NOT NULL DEFAULT '[]',
    error       text
);
CREATE INDEX pipeline_runs_recent ON lattis.pipeline_runs (pipeline_id, started_at DESC);
REVOKE ALL ON lattis.pipeline_runs FROM PUBLIC;

-- Gestion (propriétaires du serveur) -----------------------------------------------

CREATE FUNCTION lattis.pipeline_check(p jsonb, v lattis.workbooks, p_table text) RETURNS void
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_col text;
BEGIN
    FOR v_col IN SELECT jsonb_array_elements_text(coalesce(p -> 'key_columns', '[]')) LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = format('%I.%I', v.schema_name, p_table)::regclass
                       AND attname = v_col AND attnum > 0 AND NOT attisdropped) THEN
            RAISE EXCEPTION 'colonne clé inconnue : %', v_col USING ERRCODE = 'invalid_parameter_value';
        END IF;
    END LOOP;
END
$$;

-- p : name, workbook_id, table_id, source_kind, source, secret_ref, format,
-- mode, key_columns, column_map, schedule_kind, schedule_minutes,
-- schedule_time, webhook_url, role (rôle fourni du classeur, editor par défaut).
CREATE FUNCTION lattis.create_pipeline(p jsonb) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_id   uuid := gen_random_uuid();
    v      lattis.workbooks;
    v_tab  record;
    v_user lattis.users;
    v_role uuid;
BEGIN
    PERFORM lattis.require_server_owner();
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

-- p : champs modifiables (pas le classeur ni le tableau).
CREATE FUNCTION lattis.update_pipeline(p_id uuid, p jsonb) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    x     lattis.pipelines;
    v     lattis.workbooks;
    v_key text;
BEGIN
    PERFORM lattis.require_server_owner();
    SELECT * INTO x FROM lattis.pipelines WHERE id = p_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'pipeline inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    FOR v_key IN SELECT jsonb_object_keys(p) LOOP
        IF v_key NOT IN ('name', 'source_kind', 'source', 'secret_ref', 'format', 'mode', 'key_columns', 'column_map',
                         'schedule_kind', 'schedule_minutes', 'schedule_time', 'webhook_url', 'enabled') THEN
            RAISE EXCEPTION 'champ inconnu : %', v_key USING ERRCODE = 'invalid_parameter_value';
        END IF;
    END LOOP;
    v := lattis.workbook(x.workbook_id);
    PERFORM lattis.pipeline_check(p, v, x.table_name);
    UPDATE lattis.pipelines SET
        name             = CASE WHEN p ? 'name' THEN trim(p ->> 'name') ELSE name END,
        source_kind      = CASE WHEN p ? 'source_kind' THEN p ->> 'source_kind' ELSE source_kind END,
        source           = CASE WHEN p ? 'source' THEN coalesce(p -> 'source', '{}') ELSE source END,
        secret_ref       = CASE WHEN p ? 'secret_ref' THEN nullif(p ->> 'secret_ref', '') ELSE secret_ref END,
        format           = CASE WHEN p ? 'format' THEN p ->> 'format' ELSE format END,
        mode             = CASE WHEN p ? 'mode' THEN p ->> 'mode' ELSE mode END,
        key_columns      = CASE WHEN p ? 'key_columns' THEN ARRAY(SELECT jsonb_array_elements_text(p -> 'key_columns')) ELSE key_columns END,
        column_map       = CASE WHEN p ? 'column_map' THEN coalesce(p -> 'column_map', '{}') ELSE column_map END,
        schedule_kind    = CASE WHEN p ? 'schedule_kind' THEN p ->> 'schedule_kind' ELSE schedule_kind END,
        schedule_minutes = CASE WHEN p ? 'schedule_minutes' THEN (nullif(p ->> 'schedule_minutes', ''))::integer ELSE schedule_minutes END,
        schedule_time    = CASE WHEN p ? 'schedule_time' THEN nullif(p ->> 'schedule_time', '') ELSE schedule_time END,
        webhook_url      = CASE WHEN p ? 'webhook_url' THEN nullif(p ->> 'webhook_url', '') ELSE webhook_url END,
        enabled          = CASE WHEN p ? 'enabled' THEN (p ->> 'enabled')::boolean ELSE enabled END,
        next_run_at      = NULL, -- recalculé par lattisd
        updated_at       = now()
    WHERE id = p_id;
    PERFORM lattis.audit_log('update_pipeline', p_id::text, to_jsonb(x), p);
END
$$;

-- Suppression : l'identité technique perd ses rôles et est désactivée (elle
-- reste nommée dans lattis_author et le journal).
CREATE FUNCTION lattis.drop_pipeline(p_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    x lattis.pipelines;
    r record;
BEGIN
    PERFORM lattis.require_server_owner();
    SELECT * INTO x FROM lattis.pipelines WHERE id = p_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'pipeline inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    FOR r IN SELECT m.role_id FROM lattis.members m WHERE m.principal = x.user_id LOOP
        PERFORM lattis.revoke_role(r.role_id, x.user_id);
    END LOOP;
    DELETE FROM lattis.pipelines WHERE id = p_id;
    PERFORM lattis.disable_user(x.user_id);
    PERFORM lattis.audit_log('drop_pipeline', p_id::text, to_jsonb(x), NULL);
END
$$;

-- Exécution à la demande : prise en charge par lattisd au prochain relevé.
-- p_file : fichier déjà déposé par lattisd dans le dossier du pipeline.
CREATE FUNCTION lattis.run_pipeline(p_id uuid, p_file text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_server_owner();
    IF p_file IS NOT NULL AND p_file !~ '^[A-Za-z0-9._-]{1,200}$' THEN
        RAISE EXCEPTION 'nom de fichier invalide' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    UPDATE lattis.pipelines SET next_run_at = now(), pending_file = p_file,
           pending_trigger = CASE WHEN p_file IS NULL THEN 'manual' ELSE 'upload' END
    WHERE id = p_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'pipeline inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    PERFORM lattis.audit_log('run_pipeline', p_id::text, NULL, jsonb_build_object('file', p_file));
END
$$;

-- Liste pour l'administration, avec les dernières exécutions.
CREATE FUNCTION lattis.admin_pipelines() RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_server_owner();
    RETURN (SELECT coalesce(jsonb_agg(to_jsonb(x) || jsonb_build_object(
                'workbook', w.title, 'identity', u.email,
                'runs', (SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY r.started_at DESC), '[]')
                         FROM (SELECT * FROM lattis.pipeline_runs WHERE pipeline_id = x.id ORDER BY started_at DESC LIMIT 10) r))
            ORDER BY lower(x.name)), '[]')
            FROM lattis.pipelines x JOIN lattis.workbooks w ON w.id = x.workbook_id JOIN lattis.users u ON u.id = x.user_id);
END
$$;

-- Tableaux où un pipeline peut écrire (classeurs collaboratifs publiés),
-- avec leurs colonnes, pour le formulaire de création.
CREATE FUNCTION lattis.pipeline_targets() RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    w     lattis.workbooks;
    t     record;
    v_out jsonb := '[]';
    v_cols jsonb;
BEGIN
    PERFORM lattis.require_server_owner();
    FOR w IN SELECT * FROM lattis.workbooks WHERE mode = 'collaborative' AND state = 'published' ORDER BY lower(title) LOOP
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

REVOKE EXECUTE ON FUNCTION lattis.pipeline_check(jsonb, lattis.workbooks, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION lattis.create_pipeline(jsonb), lattis.update_pipeline(uuid, jsonb), lattis.drop_pipeline(uuid),
    lattis.run_pipeline(uuid, text), lattis.admin_pipelines(), lattis.pipeline_targets() TO PUBLIC;
