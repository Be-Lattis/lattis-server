-- Droits avancés (spécification §8.1 à §8.5, jalon 4) : rôles personnalisés,
-- niveaux par nœud avec filtres, restrictions, contrôle des écarts et
-- « Voir en tant que… ».

-- Empreinte des droits compilés, pour check_drift.
CREATE TABLE lattis.acl_fingerprints (
    workbook_id uuid NOT NULL REFERENCES lattis.workbooks (id) ON DELETE CASCADE,
    table_id    uuid NOT NULL,
    fingerprint text NOT NULL,
    compiled_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workbook_id, table_id)
);
REVOKE ALL ON lattis.acl_fingerprints FROM PUBLIC;

ALTER TABLE lattis.roles ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();

-- Filtres et prédicats ---------------------------------------------------------

-- Un filtre est une expression booléenne sur les colonnes du tableau. Il est
-- recopié dans une règle par EXECUTE : on refuse tout ce qui pourrait sortir
-- de l'expression (point-virgule hors chaîne, commentaires, chaînes
-- dollar), puis on le fait analyser par PostgreSQL sur le tableau lui-même.
CREATE FUNCTION lattis.check_predicate(p_schema text, p_table text, p_expr text) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_out    text := '';
    v_in_str boolean := false;
    v_in_id  boolean := false;
    v_depth  integer := 0;
    v_c      text;
BEGIN
    p_expr := trim(coalesce(p_expr, ''));
    IF p_expr = '' THEN
        RETURN NULL;
    END IF;
    IF octet_length(p_expr) > 4000 THEN
        RAISE EXCEPTION 'filtre trop long' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    FOR i IN 1 .. length(p_expr) LOOP
        v_c := substr(p_expr, i, 1);
        IF v_c = '''' AND NOT v_in_id THEN
            v_in_str := NOT v_in_str;
        ELSIF v_c = '"' AND NOT v_in_str THEN
            v_in_id := NOT v_in_id;
        ELSIF v_in_str OR v_in_id THEN
            CONTINUE;
        ELSIF v_c IN (';', '$', '\') OR substr(p_expr, i, 2) IN ('--', '/*', '*/') THEN
            RAISE EXCEPTION 'filtre refusé : « % » n''est pas permis hors d''une chaîne', v_c
                USING ERRCODE = 'invalid_parameter_value';
        ELSIF v_c = '(' THEN
            v_depth := v_depth + 1;
        ELSIF v_c = ')' THEN
            v_depth := v_depth - 1;
            -- Une parenthèse fermée de trop sortirait de l'expression qui
            -- l'entoure dans la règle (« … AND (filtre) »).
            IF v_depth < 0 THEN
                RAISE EXCEPTION 'filtre refusé : parenthèses déséquilibrées' USING ERRCODE = 'invalid_parameter_value';
            END IF;
        END IF;
    END LOOP;
    IF v_in_str OR v_in_id THEN
        RAISE EXCEPTION 'filtre refusé : chaîne ou nom non fermé' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF v_depth <> 0 THEN
        RAISE EXCEPTION 'filtre refusé : parenthèses déséquilibrées' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    -- Analyse et typage (booléen) sans rien exécuter.
    BEGIN
        EXECUTE format('EXPLAIN SELECT 1 FROM %I.%I WHERE (%s)', p_schema, p_table, p_expr);
    EXCEPTION WHEN OTHERS THEN
        RAISE EXCEPTION 'filtre invalide : %', SQLERRM USING ERRCODE = 'invalid_parameter_value';
    END;
    RETURN p_expr;
END
$$;

-- Index sur les colonnes citées par un filtre (§8.6).
CREATE FUNCTION lattis.index_filter_columns(p_schema text, p_table text, p_expr text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_col text;
BEGIN
    IF p_expr IS NULL THEN
        RETURN;
    END IF;
    FOR v_col IN SELECT attname FROM pg_attribute
                 WHERE attrelid = format('%I.%I', p_schema, p_table)::regclass AND attnum > 0 AND NOT attisdropped
                   AND attname NOT IN ('lattis_id', 'lattis_pos', 'lattis_xid') LOOP
        IF position(quote_ident(v_col) IN p_expr) > 0 OR p_expr ~* ('(^|[^a-z0-9_"])' || regexp_replace(v_col, '([^a-zA-Z0-9_])', '\\\1', 'g') || '($|[^a-z0-9_"])') THEN
            EXECUTE format('CREATE INDEX IF NOT EXISTS %I ON %I.%I (%I)',
                           left('lattis_f_' || md5(p_table || '.' || v_col), 40), p_schema, p_table, v_col);
        END IF;
    END LOOP;
END
$$;

-- Rôles personnalisés ---------------------------------------------------------

CREATE FUNCTION lattis.create_role(p_workbook_id uuid, p_name text) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v    lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner}', true);
    v_id uuid := gen_random_uuid();
    v_pg text;
BEGIN
    IF v.mode <> 'collaborative' THEN
        RAISE EXCEPTION 'un classeur figé n''a que des lecteurs' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    p_name := trim(coalesce(p_name, ''));
    IF p_name = '' OR length(p_name) > 80 THEN
        RAISE EXCEPTION 'nom de rôle vide ou trop long' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF EXISTS (SELECT 1 FROM lattis.roles WHERE workbook_id = p_workbook_id AND lower(name) = lower(p_name)) THEN
        RAISE EXCEPTION 'le rôle « % » existe déjà', p_name USING ERRCODE = 'unique_violation';
    END IF;
    v_pg := 'wb_' || v.slug || '_c_' || left(replace(v_id::text, '-', ''), 12);
    EXECUTE format('CREATE ROLE %I NOLOGIN', v_pg);
    INSERT INTO lattis.roles (id, workbook_id, name, pg_role) VALUES (v_id, p_workbook_id, p_name, v_pg);
    -- Sans niveau, un rôle personnalisé ne donne rien : le propriétaire choisit.
    INSERT INTO lattis.role_grants (role_id, node_kind, node_id, level) VALUES (v_id, 'workbook', p_workbook_id::text, 'none');
    PERFORM lattis.compile_acl(p_workbook_id);
    PERFORM lattis.audit_log('create_role', v_id::text, NULL, jsonb_build_object('workbook_id', p_workbook_id, 'name', p_name));
    RETURN v_id;
END
$$;

CREATE FUNCTION lattis.drop_role(p_role_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_role lattis.roles;
    v      lattis.workbooks;
    v_tab  record;
    v_sch  text;
BEGIN
    SELECT * INTO v_role FROM lattis.roles WHERE id = p_role_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'rôle inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    v := lattis.require_workbook_role(v_role.workbook_id, '{owner}', true);
    IF v_role.builtin IS NOT NULL THEN
        RAISE EXCEPTION 'les rôles fournis ne se suppriment pas' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended(v.id::text, 0));
    DELETE FROM lattis.roles WHERE id = p_role_id;
    UPDATE lattis.restrictions SET applies_to = array_remove(applies_to, p_role_id) WHERE workbook_id = v.id;
    -- Recompiler retire les règles qui le citent ; restent ses GRANT.
    PERFORM lattis.compile_acl(v.id);
    FOREACH v_sch IN ARRAY array_remove(ARRAY[v.schema_name, v.staging_schema], NULL) LOOP
        FOR v_tab IN SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
                     WHERE n.nspname = v_sch AND c.relkind IN ('r', 'p') LOOP
            EXECUTE format('REVOKE ALL ON %I.%I FROM %I', v_sch, v_tab.relname, v_role.pg_role);
        END LOOP;
        EXECUTE format('REVOKE ALL ON SCHEMA %I FROM %I', v_sch, v_role.pg_role);
    END LOOP;
    EXECUTE format('DROP ROLE %I', v_role.pg_role);
    PERFORM lattis.audit_log('drop_role', p_role_id::text, to_jsonb(v_role), NULL);
END
$$;

-- Niveau d'un rôle sur un nœud (classeur, feuille, tableau), hérité par les
-- nœuds inférieurs. Les filtres ne se posent que sur un tableau.
-- p_level NULL : retire le niveau propre au nœud (retour à l'héritage).
CREATE FUNCTION lattis.set_level(
    p_role_id uuid, p_node_kind text, p_node_id text, p_level text, p_read_filter text, p_write_filter text
) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_role lattis.roles;
    v      lattis.workbooks;
    v_tab  text;
    v_rd   text;
    v_wr   text;
    v_before jsonb;
BEGIN
    SELECT * INTO v_role FROM lattis.roles WHERE id = p_role_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'rôle inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    v := lattis.require_workbook_role(v_role.workbook_id, '{owner}', true);
    IF v.mode <> 'collaborative' THEN
        RAISE EXCEPTION 'niveaux : classeurs collaboratifs seulement' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF v_role.builtin IN ('owner', 'designer') THEN
        RAISE EXCEPTION 'propriétaires et concepteurs ont tous les droits' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF p_node_kind NOT IN ('workbook', 'sheet', 'table') THEN
        RAISE EXCEPTION 'nœud : workbook, sheet ou table' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF p_node_kind = 'workbook' THEN
        p_node_id := v.id::text;
        IF p_level IS NULL THEN
            p_level := 'none';
        END IF;
    END IF;
    IF p_level IS NOT NULL AND p_level NOT IN ('none', 'reader', 'participant', 'editor') THEN
        RAISE EXCEPTION 'niveau : none, reader, participant ou editor' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF p_node_kind = 'table' THEN
        EXECUTE format('SELECT name FROM %I.lattis_meta_tables WHERE table_id::text = $1', v.schema_name)
            INTO v_tab USING p_node_id;
        IF v_tab IS NULL THEN
            RAISE EXCEPTION 'tableau inconnu' USING ERRCODE = 'no_data_found';
        END IF;
        v_rd := lattis.check_predicate(v.schema_name, v_tab, p_read_filter);
        v_wr := lattis.check_predicate(v.schema_name, v_tab, p_write_filter);
    ELSIF coalesce(trim(p_read_filter), '') <> '' OR coalesce(trim(p_write_filter), '') <> '' THEN
        RAISE EXCEPTION 'les filtres se posent sur un tableau' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended(v.id::text, 0));
    SELECT to_jsonb(g) INTO v_before FROM lattis.role_grants g
    WHERE role_id = p_role_id AND node_kind = p_node_kind AND node_id = p_node_id;
    IF p_level IS NULL THEN
        DELETE FROM lattis.role_grants WHERE role_id = p_role_id AND node_kind = p_node_kind AND node_id = p_node_id;
    ELSE
        INSERT INTO lattis.role_grants (role_id, node_kind, node_id, level, read_filter, write_filter)
        VALUES (p_role_id, p_node_kind, p_node_id, p_level, v_rd, v_wr)
        ON CONFLICT (role_id, node_kind, node_id) DO UPDATE
            SET level = excluded.level, read_filter = excluded.read_filter, write_filter = excluded.write_filter;
    END IF;
    IF v_tab IS NOT NULL THEN
        PERFORM lattis.index_filter_columns(v.schema_name, v_tab, v_rd);
        PERFORM lattis.index_filter_columns(v.schema_name, v_tab, v_wr);
    END IF;
    PERFORM lattis.compile_acl(v.id);
    PERFORM lattis.audit_log('set_level', p_role_id::text, v_before,
        jsonb_build_object('node_kind', p_node_kind, 'node_id', p_node_id, 'level', p_level,
                           'read_filter', v_rd, 'write_filter', v_wr));
END
$$;

-- Restrictions (§8.2) ---------------------------------------------------------

-- p_applies_to : rôles visés ; vide = tous les rôles du classeur.
CREATE FUNCTION lattis.add_restriction(
    p_workbook_id uuid, p_table_id uuid, p_command text, p_applies_to uuid[], p_predicate text, p_label text
) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v     lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner}', true);
    v_tab text;
    v_pred text;
    v_id  uuid;
BEGIN
    IF v.mode <> 'collaborative' THEN
        RAISE EXCEPTION 'restrictions : classeurs collaboratifs seulement' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF p_command NOT IN ('select', 'insert', 'update', 'delete', 'all') THEN
        RAISE EXCEPTION 'commande : select, insert, update, delete ou all' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    EXECUTE format('SELECT name FROM %I.lattis_meta_tables WHERE table_id = $1', v.schema_name) INTO v_tab USING p_table_id;
    IF v_tab IS NULL THEN
        RAISE EXCEPTION 'tableau inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    IF EXISTS (SELECT 1 FROM unnest(coalesce(p_applies_to, '{}')) r
               WHERE NOT EXISTS (SELECT 1 FROM lattis.roles WHERE id = r AND workbook_id = p_workbook_id)) THEN
        RAISE EXCEPTION 'rôle étranger au classeur' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    v_pred := lattis.check_predicate(v.schema_name, v_tab, p_predicate);
    IF v_pred IS NULL THEN
        RAISE EXCEPTION 'prédicat manquant' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    INSERT INTO lattis.restrictions (workbook_id, table_id, command, applies_to, predicate, label)
    VALUES (p_workbook_id, p_table_id, p_command, coalesce(p_applies_to, '{}'), v_pred, coalesce(p_label, ''))
    RETURNING id INTO v_id;
    PERFORM lattis.index_filter_columns(v.schema_name, v_tab, v_pred);
    PERFORM lattis.compile_acl(p_workbook_id);
    PERFORM lattis.audit_log('add_restriction', v_id::text, NULL,
        jsonb_build_object('table_id', p_table_id, 'command', p_command, 'applies_to', p_applies_to,
                           'predicate', v_pred, 'label', p_label));
    RETURN v_id;
END
$$;

CREATE FUNCTION lattis.drop_restriction(p_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_r lattis.restrictions;
BEGIN
    SELECT * INTO v_r FROM lattis.restrictions WHERE id = p_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'restriction inconnue' USING ERRCODE = 'no_data_found';
    END IF;
    PERFORM lattis.require_workbook_role(v_r.workbook_id, '{owner}', true);
    DELETE FROM lattis.restrictions WHERE id = p_id;
    PERFORM lattis.compile_acl(v_r.workbook_id);
    PERFORM lattis.audit_log('drop_restriction', p_id::text, to_jsonb(v_r), NULL);
END
$$;

-- Compilation ------------------------------------------------------------------

-- Empreinte des règles et des droits d'un tableau (pour check_drift).
CREATE FUNCTION lattis.table_fingerprint(p_schema text, p_table text) RETURNS text
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT md5(coalesce((
        SELECT string_agg(format('%s|%s|%s|%s|%s', policyname, permissive, cmd, roles::text,
                                 coalesce(qual, '') || '|' || coalesce(with_check, '')), E'\n' ORDER BY policyname)
        FROM pg_policies WHERE schemaname = p_schema AND tablename = p_table), '')
        || E'\n' || coalesce((SELECT relacl::text FROM pg_class
                              WHERE oid = format('%I.%I', p_schema, p_table)::regclass), '')
        || E'\n' || (SELECT relrowsecurity::text || relforcerowsecurity::text FROM pg_class
                     WHERE oid = format('%I.%I', p_schema, p_table)::regclass))
$$;

-- compile_table : règles des rôles (fournis et personnalisés), puis restrictions.
CREATE OR REPLACE FUNCTION lattis.compile_table(v lattis.workbooks, p_schema text, p_table text, p_table_id uuid, p_sheet_id text)
RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_pol  record;
    v_role lattis.roles;
    g      lattis.role_grants;
    v_cols text;
    v_rd   text;
    v_wr   text;
    v_n    integer := 0;
    v_res  lattis.restrictions;
    v_to   text;
BEGIN
    EXECUTE format('ALTER TABLE %I.%I ENABLE ROW LEVEL SECURITY', p_schema, p_table);
    EXECUTE format('ALTER TABLE %I.%I FORCE ROW LEVEL SECURITY', p_schema, p_table);
    FOR v_pol IN SELECT policyname FROM pg_policies WHERE schemaname = p_schema AND tablename = p_table
                 AND policyname LIKE 'lattis\_%' LOOP
        EXECUTE format('DROP POLICY %I ON %I.%I', v_pol.policyname, p_schema, p_table);
    END LOOP;
    EXECUTE format('CREATE POLICY lattis_owner ON %I.%I TO %I USING (true) WITH CHECK (true)',
                   p_schema, p_table, 'wb_' || v.slug || '_owner');

    SELECT string_agg(quote_ident(attname), ', ' ORDER BY attnum) INTO v_cols
    FROM pg_attribute
    WHERE attrelid = format('%I.%I', p_schema, p_table)::regclass AND attnum > 0 AND NOT attisdropped
      AND attname NOT IN ('lattis_id', 'lattis_author');

    FOR v_role IN SELECT * FROM lattis.roles WHERE workbook_id = v.id ORDER BY created_at, pg_role LOOP
        v_n := v_n + 1;
        EXECUTE format('REVOKE ALL ON %I.%I FROM %I', p_schema, p_table, v_role.pg_role);
        g := lattis.effective_grant(v_role, p_table_id, p_sheet_id);
        v_rd := coalesce(nullif(g.read_filter, ''), 'true');
        v_wr := coalesce(nullif(g.write_filter, ''), 'true');
        IF g.level = 'reader' THEN
            EXECUTE format('GRANT SELECT ON %I.%I TO %I', p_schema, p_table, v_role.pg_role);
            EXECUTE format('CREATE POLICY %I ON %I.%I FOR SELECT TO %I USING (%s)',
                           'lattis_r' || v_n || '_select', p_schema, p_table, v_role.pg_role, v_rd);
        ELSIF g.level = 'participant' THEN
            EXECUTE format('GRANT SELECT, INSERT, DELETE ON %I.%I TO %I', p_schema, p_table, v_role.pg_role);
            EXECUTE format('GRANT UPDATE (%s) ON %I.%I TO %I', v_cols, p_schema, p_table, v_role.pg_role);
            EXECUTE format('CREATE POLICY %I ON %I.%I FOR SELECT TO %I USING (%s)',
                           'lattis_r' || v_n || '_select', p_schema, p_table, v_role.pg_role, v_rd);
            EXECUTE format('CREATE POLICY %I ON %I.%I FOR INSERT TO %I WITH CHECK (lattis_author = current_user AND (%s))',
                           'lattis_r' || v_n || '_insert', p_schema, p_table, v_role.pg_role, v_wr);
            EXECUTE format('CREATE POLICY %I ON %I.%I FOR UPDATE TO %I USING (lattis_author = current_user)
                            WITH CHECK (lattis_author = current_user AND (%s))',
                           'lattis_r' || v_n || '_update', p_schema, p_table, v_role.pg_role, v_wr);
            EXECUTE format('CREATE POLICY %I ON %I.%I FOR DELETE TO %I USING (lattis_author = current_user)',
                           'lattis_r' || v_n || '_delete', p_schema, p_table, v_role.pg_role);
        ELSIF g.level = 'editor' THEN
            EXECUTE format('GRANT SELECT, INSERT, DELETE ON %I.%I TO %I', p_schema, p_table, v_role.pg_role);
            EXECUTE format('GRANT UPDATE (%s) ON %I.%I TO %I', v_cols, p_schema, p_table, v_role.pg_role);
            EXECUTE format('CREATE POLICY %I ON %I.%I FOR ALL TO %I USING (%s) WITH CHECK (%s)',
                           'lattis_r' || v_n || '_all', p_schema, p_table, v_role.pg_role, v_rd, v_wr);
        END IF;
    END LOOP;

    -- Restrictions : AS RESTRICTIVE, combinées en ET (§8.2).
    v_n := 0;
    FOR v_res IN SELECT * FROM lattis.restrictions WHERE workbook_id = v.id AND table_id = p_table_id ORDER BY id LOOP
        v_n := v_n + 1;
        SELECT string_agg(quote_ident(pg_role), ', ') INTO v_to FROM lattis.roles
        WHERE workbook_id = v.id
          AND (id = ANY (v_res.applies_to)
               -- « Tous » : sauf propriétaires et concepteurs, qui gardent la main.
               OR (cardinality(v_res.applies_to) = 0 AND builtin IS DISTINCT FROM 'owner' AND builtin IS DISTINCT FROM 'designer'));
        IF v_to IS NULL THEN
            CONTINUE;
        END IF;
        IF v_res.command IN ('select', 'delete') THEN
            EXECUTE format('CREATE POLICY %I ON %I.%I AS RESTRICTIVE FOR %s TO %s USING (%s)',
                           'lattis_x' || v_n, p_schema, p_table, upper(v_res.command), v_to, v_res.predicate);
        ELSIF v_res.command = 'insert' THEN
            EXECUTE format('CREATE POLICY %I ON %I.%I AS RESTRICTIVE FOR INSERT TO %s WITH CHECK (%s)',
                           'lattis_x' || v_n, p_schema, p_table, v_to, v_res.predicate);
        ELSE
            EXECUTE format('CREATE POLICY %I ON %I.%I AS RESTRICTIVE FOR %s TO %s USING (%s) WITH CHECK (%s)',
                           'lattis_x' || v_n, p_schema, p_table, upper(v_res.command), v_to, v_res.predicate, v_res.predicate);
        END IF;
    END LOOP;

    INSERT INTO lattis.acl_fingerprints (workbook_id, table_id, fingerprint)
    VALUES (v.id, p_table_id, lattis.table_fingerprint(p_schema, p_table))
    ON CONFLICT (workbook_id, table_id) DO UPDATE SET fingerprint = excluded.fingerprint, compiled_at = now();
END
$$;

-- Écarts (§8.3) : règles ou droits ajoutés ou retirés à la main depuis la
-- dernière compilation.
CREATE FUNCTION lattis.check_drift(p_workbook_id uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v     lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', true);
    v_tab record;
    v_out jsonb := '[]';
    v_fp  text;
BEGIN
    IF v.mode <> 'collaborative' OR v.state <> 'published' THEN
        RETURN v_out;
    END IF;
    FOR v_tab IN SELECT * FROM lattis.data_tables(v.schema_name) LOOP
        SELECT fingerprint INTO v_fp FROM lattis.acl_fingerprints WHERE workbook_id = v.id AND table_id = v_tab.table_id;
        IF v_fp IS DISTINCT FROM lattis.table_fingerprint(v.schema_name, v_tab.name) THEN
            v_out := v_out || jsonb_build_object('table_id', v_tab.table_id, 'name', v_tab.name,
                'foreign_policies', (SELECT coalesce(jsonb_agg(policyname), '[]') FROM pg_policies
                                     WHERE schemaname = v.schema_name AND tablename = v_tab.name
                                       AND policyname NOT LIKE 'lattis\_%'));
        END IF;
    END LOOP;
    RETURN v_out;
END
$$;

-- « Voir en tant que… » (§8.5) ----------------------------------------------------

-- Vérifie qu'un concepteur ou propriétaire (p_actor, rôle PostgreSQL
-- authentifié par lattisd) peut voir le classeur comme p_user_id, journalise
-- et renvoie le rôle à endosser. La lecture elle-même est faite par lattisd
-- (SET LOCAL ROLE dans une transaction en lecture seule), qu'une fonction
-- SECURITY DEFINER ne peut pas faire.
CREATE FUNCTION lattis.authorize_preview(p_actor text, p_workbook_id uuid, p_user_id uuid) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v      lattis.workbooks := lattis.workbook(p_workbook_id);
    v_user lattis.users;
BEGIN
    PERFORM lattis.require_service();
    IF NOT EXISTS (SELECT 1 FROM lattis.roles r WHERE r.workbook_id = p_workbook_id AND r.builtin IN ('owner', 'designer')
                   AND pg_has_role(p_actor, r.pg_role, 'MEMBER')) THEN
        RAISE EXCEPTION 'réservé aux propriétaires et concepteurs du classeur' USING ERRCODE = 'insufficient_privilege';
    END IF;
    SELECT * INTO v_user FROM lattis.users WHERE id = p_user_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'utilisateur inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    -- lattis_service peut endosser les rôles qu'il administre.
    IF NOT pg_has_role('lattis_service', v_user.pg_role, 'SET') THEN
        EXECUTE format('GRANT %I TO lattis_service WITH INHERIT FALSE, SET TRUE', v_user.pg_role);
    END IF;
    PERFORM set_config('lattis.actor', p_actor, true);
    PERFORM lattis.audit_log('preview_as', p_workbook_id::text, NULL, jsonb_build_object('user_id', p_user_id, 'pg_role', v_user.pg_role));
    RETURN v_user.pg_role;
END
$$;

-- Capacités : union des rôles (fournis et personnalisés), restrictions visées.
CREATE OR REPLACE FUNCTION lattis.my_capabilities(p_workbook_id uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v       lattis.workbooks := lattis.workbook(p_workbook_id);
    v_roles text[] := lattis.my_builtin_roles(p_workbook_id);
    v_mine  uuid[];
    v_tab   record;
    v_role  lattis.roles;
    g       lattis.role_grants;
    v_best  text;
    v_rank  integer;
    v_filt  boolean;
    v_out   jsonb := '{}';
BEGIN
    SELECT coalesce(array_agg(id), '{}') INTO v_mine FROM lattis.roles
    WHERE workbook_id = p_workbook_id AND pg_has_role(session_user, pg_role, 'MEMBER');
    IF cardinality(v_mine) = 0 THEN
        RAISE EXCEPTION 'aucun accès à ce classeur' USING ERRCODE = 'insufficient_privilege';
    END IF;
    FOR v_tab IN SELECT * FROM lattis.data_tables(v.schema_name) LOOP
        v_filt := false;
        IF v.mode = 'frozen' THEN
            v_best := 'reader';
        ELSE
            v_rank := 0;
            v_best := 'none';
            FOR v_role IN SELECT * FROM lattis.roles WHERE id = ANY (v_mine) LOOP
                g := lattis.effective_grant(v_role, v_tab.table_id, v_tab.sheet_id);
                IF array_position(ARRAY['none', 'reader', 'participant', 'editor'], g.level) > v_rank THEN
                    v_rank := array_position(ARRAY['none', 'reader', 'participant', 'editor'], g.level);
                    v_best := g.level;
                END IF;
                v_filt := v_filt OR coalesce(g.read_filter, g.write_filter) IS NOT NULL;
            END LOOP;
        END IF;
        v_out := v_out || jsonb_build_object(v_tab.table_id::text,
            jsonb_build_object('name', v_tab.name, 'level', v_best, 'filtered', v_filt));
    END LOOP;
    RETURN jsonb_build_object('workbook_id', v.id, 'mode', v.mode, 'version', v.version, 'schema', v.schema_name,
        'channel', 'wb_' || v.slug, 'roles', v_roles,
        'custom_roles', (SELECT coalesce(jsonb_agg(name ORDER BY name), '[]') FROM lattis.roles
                         WHERE id = ANY (v_mine) AND builtin IS NULL),
        'can_design', v_roles && '{owner,designer}', 'can_manage', 'owner' = ANY (v_roles),
        'tables', v_out,
        'restrictions', (SELECT coalesce(jsonb_agg(jsonb_build_object('table_id', r.table_id, 'command', r.command,
                                                                      'label', r.label) ORDER BY r.label), '[]')
                         FROM lattis.restrictions r
                         WHERE r.workbook_id = p_workbook_id AND (cardinality(r.applies_to) = 0 OR r.applies_to && v_mine)));
END
$$;

-- Rôles et membres d'un classeur (écran « Droits d'un classeur »).
CREATE FUNCTION lattis.workbook_acl(p_workbook_id uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner}', true);
BEGIN
    RETURN jsonb_build_object(
        'roles', (SELECT coalesce(jsonb_agg(jsonb_build_object(
                     'id', r.id, 'name', r.name, 'builtin', r.builtin,
                     'grants', (SELECT coalesce(jsonb_agg(jsonb_build_object('node_kind', g.node_kind, 'node_id', g.node_id,
                                    'level', g.level, 'read_filter', g.read_filter, 'write_filter', g.write_filter)), '[]')
                                FROM lattis.role_grants g WHERE g.role_id = r.id),
                     'members', (SELECT coalesce(jsonb_agg(jsonb_build_object('user_id', u.id, 'email', u.email,
                                    'display_name', u.display_name) ORDER BY u.email), '[]')
                                 FROM lattis.members m JOIN lattis.users u ON u.id = m.principal WHERE m.role_id = r.id))
                  ORDER BY r.created_at, r.name), '[]')
                  FROM lattis.roles r WHERE r.workbook_id = p_workbook_id),
        'restrictions', (SELECT coalesce(jsonb_agg(to_jsonb(x) - 'workbook_id'), '[]') FROM lattis.restrictions x
                         WHERE x.workbook_id = p_workbook_id),
        'invitations', (SELECT coalesce(jsonb_agg(jsonb_build_object('id', i.id, 'email', i.email, 'roles', i.roles,
                                                                     'expires_at', i.expires_at)), '[]')
                        FROM lattis.invitations i WHERE i.workbook_id = p_workbook_id
                          AND i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > now()));
END
$$;

-- Un propriétaire de classeur retire ses propres invitations.
CREATE OR REPLACE FUNCTION lattis.revoke_invitation(p_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_inv  lattis.invitations;
    v_user lattis.users;
BEGIN
    SELECT * INTO v_inv FROM lattis.invitations WHERE id = p_id AND accepted_at IS NULL AND revoked_at IS NULL;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'invitation inconnue ou déjà acceptée' USING ERRCODE = 'no_data_found';
    END IF;
    IF v_inv.workbook_id IS NULL THEN
        PERFORM lattis.require_server_owner();
    ELSE
        PERFORM lattis.require_workbook_role(v_inv.workbook_id, '{owner}', true);
    END IF;
    UPDATE lattis.invitations SET revoked_at = now() WHERE id = p_id;
    FOR v_user IN
        SELECT * FROM lattis.users
        WHERE email = v_inv.email AND subject IS NULL AND last_login_at IS NULL
          AND NOT EXISTS (SELECT 1 FROM lattis.invitations
                          WHERE email = v_inv.email AND accepted_at IS NULL AND revoked_at IS NULL)
    LOOP
        DELETE FROM lattis.users WHERE id = v_user.id;
        EXECUTE format('DROP ROLE IF EXISTS %I', v_user.pg_role);
    END LOOP;
    PERFORM lattis.audit_log('revoke_invitation', p_id::text, to_jsonb(v_inv), NULL);
END
$$;

GRANT EXECUTE ON FUNCTION
    lattis.create_role(uuid, text), lattis.drop_role(uuid),
    lattis.set_level(uuid, text, text, text, text, text),
    lattis.add_restriction(uuid, uuid, text, uuid[], text, text), lattis.drop_restriction(uuid),
    lattis.check_drift(uuid), lattis.workbook_acl(uuid)
TO PUBLIC;
