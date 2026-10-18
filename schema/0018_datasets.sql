-- Données d'entreprise (choix de Louis, 18/10) : un tableau d'un classeur
-- publié est partagé dans un catalogue rangé en dossiers, lisible depuis
-- n'importe quel classeur selon une audience (personnes, équipes, filtres).
--
-- Chaque donnée est une vue au nom SQL stable dans le schéma « donnees ».
-- La vue appartient au rôle propriétaire du classeur source (qui voit toutes
-- les lignes par sa règle lattis_owner) et filtre elle-même selon
-- l'audience : une ligne n'est rendue que si current_user est membre d'un
-- principal de l'audience et que le filtre de cette entrée est vrai. La vue
-- est security_barrier, et seules les personnes de l'audience ont SELECT.
-- Rien n'est ajouté au tableau source (règles, droits) : check_drift et
-- compile_acl ne la voient pas.
--
-- Les vues sont recompilées quand le classeur source change de version, quand
-- l'audience ou un dossier change, et quand une équipe disparaît.

CREATE SCHEMA IF NOT EXISTS donnees;
GRANT USAGE ON SCHEMA donnees TO PUBLIC;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'lattis_data_publishers') THEN
        CREATE ROLE lattis_data_publishers NOLOGIN;
    END IF;
END
$$;

CREATE TABLE lattis.data_folders (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    parent_id  uuid REFERENCES lattis.data_folders (id) ON DELETE RESTRICT,
    name       text NOT NULL CHECK (length(trim(name)) BETWEEN 1 AND 200),
    created_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX data_folders_name ON lattis.data_folders (coalesce(parent_id, '00000000-0000-0000-0000-000000000000'), lower(name));
REVOKE ALL ON lattis.data_folders FROM PUBLIC;

CREATE TABLE lattis.datasets (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    folder_id        uuid REFERENCES lattis.data_folders (id) ON DELETE RESTRICT,
    name             text NOT NULL CHECK (length(trim(name)) BETWEEN 1 AND 200),
    description      text NOT NULL DEFAULT '',
    sql_name         text NOT NULL UNIQUE CHECK (sql_name ~ '^[a-z][a-z0-9_]{0,62}$'),
    workbook_id      uuid REFERENCES lattis.workbooks (id) ON DELETE SET NULL,
    table_local_id   text,
    table_id         uuid,
    table_name       text,
    hidden_columns   text[] NOT NULL DEFAULT '{}',
    inherit_audience boolean NOT NULL DEFAULT true,
    created_by       text NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX datasets_workbook ON lattis.datasets (workbook_id);
CREATE INDEX datasets_folder ON lattis.datasets (folder_id);
REVOKE ALL ON lattis.datasets FROM PUBLIC;

-- Audience d'une donnée ou d'un dossier : une personne ou une équipe, avec un
-- filtre facultatif sur les colonnes du tableau source.
CREATE TABLE lattis.data_audience (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    dataset_id     uuid REFERENCES lattis.datasets (id) ON DELETE CASCADE,
    folder_id      uuid REFERENCES lattis.data_folders (id) ON DELETE CASCADE,
    principal      uuid NOT NULL,
    principal_kind text NOT NULL CHECK (principal_kind IN ('user', 'team')),
    predicate      text,
    created_by     text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CHECK ((dataset_id IS NULL) <> (folder_id IS NULL))
);
CREATE INDEX data_audience_dataset ON lattis.data_audience (dataset_id);
CREATE INDEX data_audience_folder ON lattis.data_audience (folder_id);
CREATE INDEX data_audience_principal ON lattis.data_audience (principal);
REVOKE ALL ON lattis.data_audience FROM PUBLIC;

-- Classeurs qui utilisent une donnée (tableaux liés).
CREATE TABLE lattis.dataset_links (
    dataset_id  uuid NOT NULL REFERENCES lattis.datasets (id) ON DELETE CASCADE,
    workbook_id uuid NOT NULL REFERENCES lattis.workbooks (id) ON DELETE CASCADE,
    linked_by   text NOT NULL,
    linked_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (dataset_id, workbook_id)
);
CREATE INDEX dataset_links_workbook ON lattis.dataset_links (workbook_id);
REVOKE ALL ON lattis.dataset_links FROM PUBLIC;

-- Droits ---------------------------------------------------------------------

CREATE FUNCTION lattis.can_publish_data() RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT lattis.is_server_owner() OR pg_has_role(session_user, 'lattis_data_publishers', 'MEMBER')
$$;

CREATE FUNCTION lattis.require_data_publisher() RETURNS void
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    IF NOT lattis.can_publish_data() THEN
        RAISE EXCEPTION 'le catalogue des données est réservé aux publieurs de données et aux propriétaires du serveur'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
END
$$;

-- Gérer une donnée : publieur, et auteur du partage, propriétaire du serveur
-- ou propriétaire/concepteur du classeur source.
CREATE FUNCTION lattis.can_manage_dataset(d lattis.datasets) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT lattis.can_publish_data() AND (lattis.is_server_owner() OR d.created_by = session_user
        OR (d.workbook_id IS NOT NULL AND lattis.has_workbook_role(d.workbook_id, '{owner,designer}')))
$$;

CREATE FUNCTION lattis.dataset(p_id uuid) RETURNS lattis.datasets
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    d lattis.datasets;
BEGIN
    SELECT * INTO d FROM lattis.datasets WHERE id = p_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'donnée inconnue' USING ERRCODE = 'no_data_found';
    END IF;
    RETURN d;
END
$$;

CREATE FUNCTION lattis.require_manage_dataset(p_id uuid) RETURNS lattis.datasets
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    d lattis.datasets := lattis.dataset(p_id);
BEGIN
    IF NOT lattis.can_manage_dataset(d) THEN
        RAISE EXCEPTION 'droits insuffisants sur cette donnée' USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN d;
END
$$;

-- Lecture : SELECT sur la vue (donné à l'audience).
CREATE FUNCTION lattis.can_read_dataset(d lattis.datasets) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
                   WHERE n.nspname = 'donnees' AND c.relname = d.sql_name AND c.relkind = 'v'
                     AND has_table_privilege(session_user, c.oid, 'SELECT'))
$$;

CREATE FUNCTION lattis.set_data_publisher(p_user_id uuid, p_on boolean) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_role text;
BEGIN
    PERFORM lattis.require_server_owner();
    SELECT pg_role INTO v_role FROM lattis.users WHERE id = p_user_id;
    IF v_role IS NULL THEN
        RAISE EXCEPTION 'utilisateur inconnu' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF p_on THEN
        EXECUTE format('GRANT lattis_data_publishers TO %I', v_role);
    ELSIF pg_has_role(v_role, 'lattis_data_publishers', 'MEMBER') THEN
        EXECUTE format('REVOKE lattis_data_publishers FROM %I', v_role);
    END IF;
    PERFORM lattis.audit_log(CASE WHEN p_on THEN 'grant_data_publisher' ELSE 'revoke_data_publisher' END,
        p_user_id::text, NULL, NULL);
END
$$;

-- Dossiers -------------------------------------------------------------------

CREATE FUNCTION lattis.data_folder_closure() RETURNS TABLE (ancestor uuid, descendant uuid, depth integer)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    WITH RECURSIVE c(ancestor, descendant, depth) AS (
        SELECT id, id, 0 FROM lattis.data_folders
        UNION ALL
        SELECT c.ancestor, f.id, c.depth + 1 FROM c JOIN lattis.data_folders f ON f.parent_id = c.descendant
    )
    SELECT ancestor, descendant, depth FROM c
$$;

CREATE FUNCTION lattis.create_data_folder(p_name text, p_parent_id uuid) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_id uuid;
BEGIN
    PERFORM lattis.require_data_publisher();
    p_name := trim(coalesce(p_name, ''));
    IF p_parent_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM lattis.data_folders WHERE id = p_parent_id) THEN
        RAISE EXCEPTION 'dossier parent inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    IF EXISTS (SELECT 1 FROM lattis.data_folders WHERE parent_id IS NOT DISTINCT FROM p_parent_id AND lower(name) = lower(p_name)) THEN
        RAISE EXCEPTION 'un dossier « % » existe déjà à cet endroit', p_name USING ERRCODE = 'unique_violation';
    END IF;
    INSERT INTO lattis.data_folders (parent_id, name, created_by) VALUES (p_parent_id, p_name, lattis.actor()) RETURNING id INTO v_id;
    PERFORM lattis.audit_log('create_data_folder', v_id::text, NULL, jsonb_build_object('name', p_name, 'parent_id', p_parent_id));
    RETURN v_id;
END
$$;

CREATE FUNCTION lattis.rename_data_folder(p_id uuid, p_name text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_parent uuid;
BEGIN
    PERFORM lattis.require_data_publisher();
    p_name := trim(coalesce(p_name, ''));
    SELECT parent_id INTO v_parent FROM lattis.data_folders WHERE id = p_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'dossier inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    IF EXISTS (SELECT 1 FROM lattis.data_folders WHERE parent_id IS NOT DISTINCT FROM v_parent AND lower(name) = lower(p_name) AND id <> p_id) THEN
        RAISE EXCEPTION 'un dossier « % » existe déjà à cet endroit', p_name USING ERRCODE = 'unique_violation';
    END IF;
    UPDATE lattis.data_folders SET name = p_name WHERE id = p_id;
    PERFORM lattis.audit_log('rename_data_folder', p_id::text, NULL, jsonb_build_object('name', p_name));
    PERFORM pg_notify('lattis_data', '*catalog');
END
$$;

CREATE FUNCTION lattis.move_data_folder(p_id uuid, p_parent_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_data_publisher();
    IF NOT EXISTS (SELECT 1 FROM lattis.data_folders WHERE id = p_id) THEN
        RAISE EXCEPTION 'dossier inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    IF p_parent_id IS NOT NULL AND EXISTS (SELECT 1 FROM lattis.data_folder_closure() WHERE ancestor = p_id AND descendant = p_parent_id) THEN
        RAISE EXCEPTION 'un dossier ne peut pas être rangé dans lui-même ni dans l''un de ses sous-dossiers'
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    UPDATE lattis.data_folders SET parent_id = p_parent_id WHERE id = p_id;
    PERFORM lattis.compile_folder_datasets(p_id);
    PERFORM lattis.audit_log('move_data_folder', p_id::text, NULL, jsonb_build_object('parent_id', p_parent_id));
END
$$;

CREATE FUNCTION lattis.drop_data_folder(p_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_data_publisher();
    IF EXISTS (SELECT 1 FROM lattis.data_folders WHERE parent_id = p_id) OR EXISTS (SELECT 1 FROM lattis.datasets WHERE folder_id = p_id) THEN
        RAISE EXCEPTION 'le dossier n''est pas vide' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    DELETE FROM lattis.data_folders WHERE id = p_id;
    PERFORM lattis.audit_log('drop_data_folder', p_id::text, NULL, NULL);
    PERFORM pg_notify('lattis_data', '*catalog');
END
$$;

-- Compilation des vues --------------------------------------------------------

-- Recompile la vue d'une donnée ; sans source valable, la vue disparaît (la
-- donnée reste au catalogue, marquée « source indisponible »).
CREATE FUNCTION lattis.compile_dataset(p_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    d       lattis.datasets;
    v       lattis.workbooks;
    v_tid   uuid;
    v_tname text;
    v_owner text;
    v_cols  text;
    v_where text := '';
    a       record;
    v_role  text;
    v_pred  text;
    v_roles text[] := '{}';
BEGIN
    SELECT * INTO d FROM lattis.datasets WHERE id = p_id;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    EXECUTE format('DROP VIEW IF EXISTS donnees.%I', d.sql_name);
    IF d.workbook_id IS NULL THEN
        PERFORM pg_notify('lattis_data', d.id::text);
        RETURN;
    END IF;
    SELECT * INTO v FROM lattis.workbooks WHERE id = d.workbook_id;
    IF v.state <> 'published' THEN
        RETURN;
    END IF;
    -- Le tableau, retrouvé par son identifiant local (stable d'une version à
    -- l'autre), sinon par son identifiant, sinon par son nom.
    EXECUTE format('SELECT table_id, name FROM %I.lattis_meta_tables
                    WHERE ($1 IS NOT NULL AND local_id = $1) OR table_id = $2 OR name = $3
                    ORDER BY ($1 IS NOT NULL AND local_id = $1) DESC, (table_id = $2) DESC LIMIT 1', v.schema_name)
        INTO v_tid, v_tname USING d.table_local_id, d.table_id, d.table_name;
    IF v_tid IS NULL THEN
        UPDATE lattis.datasets SET table_id = NULL WHERE id = p_id;
        PERFORM pg_notify('lattis_data', d.id::text);
        RETURN;
    END IF;
    UPDATE lattis.datasets SET table_id = v_tid, table_name = v_tname WHERE id = p_id;

    -- Colonnes : système utiles à la synchronisation, puis colonnes exposées.
    SELECT string_agg(quote_ident(attname), ', ' ORDER BY
               CASE attname WHEN 'lattis_id' THEN 0 WHEN 'lattis_pos' THEN 1 WHEN 'lattis_xid' THEN 2 WHEN 'lattis_updated_at' THEN 3 ELSE 4 END, attnum)
      INTO v_cols
      FROM pg_attribute
     WHERE attrelid = format('%I.%I', v.schema_name, v_tname)::regclass AND attnum > 0 AND NOT attisdropped
       AND attname NOT IN ('lattis_author', 'lattis_updated_by')
       AND NOT (attname = ANY (d.hidden_columns));

    -- Audience : celle de la donnée, plus celle de ses dossiers si elle en hérite.
    FOR a IN SELECT x.* FROM lattis.data_audience x WHERE x.dataset_id = d.id
             UNION ALL
             SELECT x.* FROM lattis.data_audience x
             JOIN lattis.data_folder_closure() c ON c.ancestor = x.folder_id
             WHERE d.inherit_audience AND d.folder_id IS NOT NULL AND c.descendant = d.folder_id LOOP
        IF a.principal_kind = 'user' THEN
            SELECT pg_role INTO v_role FROM lattis.users WHERE id = a.principal;
        ELSE
            SELECT pg_role INTO v_role FROM lattis.teams WHERE id = a.principal;
        END IF;
        CONTINUE WHEN v_role IS NULL OR NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_role);
        v_pred := NULL;
        IF a.predicate IS NOT NULL THEN
            BEGIN
                v_pred := lattis.check_predicate(v.schema_name, v_tname, a.predicate);
            EXCEPTION WHEN OTHERS THEN
                CONTINUE; -- filtre devenu invalide (colonne disparue) : entrée ignorée
            END;
        END IF;
        v_where := v_where || CASE WHEN v_where = '' THEN '' ELSE ' OR ' END
                || format('((SELECT pg_has_role(current_user, %L, ''MEMBER'')) AND (%s))', v_role, coalesce(v_pred, 'true'));
        v_roles := array_append(v_roles, v_role);
    END LOOP;
    IF v_where = '' THEN
        v_where := 'false';
    END IF;

    EXECUTE format('CREATE VIEW donnees.%I WITH (security_barrier = true) AS SELECT %s FROM %I.%I WHERE %s',
                   d.sql_name, v_cols, v.schema_name, v_tname, v_where);
    EXECUTE format('COMMENT ON VIEW donnees.%I IS %L', d.sql_name, d.name || CASE WHEN d.description <> '' THEN ' — ' || d.description ELSE '' END);
    v_owner := 'wb_' || v.slug || '_owner';
    EXECUTE format('GRANT CREATE ON SCHEMA donnees TO %I', v_owner);
    EXECUTE format('ALTER VIEW donnees.%I OWNER TO %I', d.sql_name, v_owner);
    EXECUTE format('REVOKE CREATE ON SCHEMA donnees FROM %I', v_owner);
    EXECUTE format('REVOKE ALL ON donnees.%I FROM PUBLIC', d.sql_name);
    FOR v_role IN SELECT DISTINCT unnest(v_roles) LOOP
        EXECUTE format('GRANT SELECT ON donnees.%I TO %I', d.sql_name, v_role);
    END LOOP;
    PERFORM pg_notify('lattis_data', d.id::text);
END
$$;

CREATE FUNCTION lattis.compile_folder_datasets(p_folder_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_id uuid;
BEGIN
    FOR v_id IN SELECT d.id FROM lattis.datasets d JOIN lattis.data_folder_closure() c ON c.descendant = d.folder_id
                WHERE c.ancestor = p_folder_id LOOP
        PERFORM lattis.compile_dataset(v_id);
    END LOOP;
    PERFORM pg_notify('lattis_data', '*catalog');
END
$$;

-- Nom SQL proposé à partir du nom, unique.
CREATE FUNCTION lattis.dataset_sql_name(p_name text) RETURNS text
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_base text;
    v_name text;
    v_n    integer := 1;
BEGIN
    v_base := lower(translate(coalesce(p_name, ''),
        'àâäáãåçéèêëíìîïñóòôöõúùûüýÿÀÂÄÁÃÅÇÉÈÊËÍÌÎÏÑÓÒÔÖÕÚÙÛÜÝœŒæÆ',
        'aaaaaaceeeeiiiinooooouuuuyyAAAAAACEEEEIIIINOOOOOUUUUYoOaA'));
    v_base := trim(BOTH '_' FROM regexp_replace(v_base, '[^a-z0-9]+', '_', 'g'));
    v_base := left(coalesce(nullif(v_base, ''), 'donnee'), 55);
    IF v_base !~ '^[a-z]' THEN
        v_base := 'd_' || v_base;
    END IF;
    v_name := v_base;
    WHILE EXISTS (SELECT 1 FROM lattis.datasets WHERE sql_name = v_name)
       OR EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'donnees' AND c.relname = v_name) LOOP
        v_n := v_n + 1;
        v_name := v_base || '_' || v_n;
    END LOOP;
    RETURN v_name;
END
$$;

-- Données ----------------------------------------------------------------------

CREATE FUNCTION lattis.create_dataset(p_workbook_id uuid, p_table_id uuid, p_folder_id uuid, p_name text,
                                      p_description text, p_sql_name text, p_hidden_columns text[]) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v      lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', true);
    v_id   uuid;
    v_tab  record;
BEGIN
    PERFORM lattis.require_data_publisher();
    IF v.state <> 'published' THEN
        RAISE EXCEPTION 'seul un classeur publié peut partager ses tableaux' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    EXECUTE format('SELECT table_id, name, local_id FROM %I.lattis_meta_tables WHERE table_id = $1', v.schema_name)
        INTO v_tab USING p_table_id;
    IF v_tab.table_id IS NULL THEN
        RAISE EXCEPTION 'tableau inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    IF p_folder_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM lattis.data_folders WHERE id = p_folder_id) THEN
        RAISE EXCEPTION 'dossier inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    p_name := trim(coalesce(p_name, ''));
    p_sql_name := lower(trim(coalesce(p_sql_name, '')));
    IF p_sql_name = '' THEN
        p_sql_name := lattis.dataset_sql_name(p_name);
    ELSIF p_sql_name !~ '^[a-z][a-z0-9_]{0,62}$' THEN
        RAISE EXCEPTION 'nom SQL : lettres minuscules, chiffres et _, en commençant par une lettre' USING ERRCODE = 'invalid_parameter_value';
    ELSIF EXISTS (SELECT 1 FROM lattis.datasets WHERE sql_name = p_sql_name)
       OR EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'donnees' AND c.relname = p_sql_name) THEN
        RAISE EXCEPTION 'le nom SQL « % » est déjà pris', p_sql_name USING ERRCODE = 'unique_violation';
    END IF;
    INSERT INTO lattis.datasets (folder_id, name, description, sql_name, workbook_id, table_local_id, table_id, table_name,
                                 hidden_columns, created_by)
    VALUES (p_folder_id, p_name, coalesce(p_description, ''), p_sql_name, p_workbook_id, v_tab.local_id, v_tab.table_id, v_tab.name,
            coalesce(p_hidden_columns, '{}'), session_user)
    RETURNING id INTO v_id;
    PERFORM lattis.compile_dataset(v_id);
    PERFORM lattis.audit_log('create_dataset', v_id::text, NULL,
        jsonb_build_object('name', p_name, 'sql_name', p_sql_name, 'workbook_id', p_workbook_id, 'table_id', p_table_id));
    PERFORM pg_notify('lattis_data', '*catalog');
    RETURN v_id;
END
$$;

-- p_changes : name, description, folder_id, hidden_columns, inherit_audience.
CREATE FUNCTION lattis.update_dataset(p_id uuid, p_changes jsonb) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    d     lattis.datasets := lattis.require_manage_dataset(p_id);
    v_key text;
BEGIN
    FOR v_key IN SELECT jsonb_object_keys(p_changes) LOOP
        IF v_key NOT IN ('name', 'description', 'folder_id', 'hidden_columns', 'inherit_audience') THEN
            RAISE EXCEPTION 'champ inconnu : %', v_key USING ERRCODE = 'invalid_parameter_value';
        END IF;
    END LOOP;
    IF p_changes ? 'folder_id' AND p_changes ->> 'folder_id' IS NOT NULL
       AND NOT EXISTS (SELECT 1 FROM lattis.data_folders WHERE id = (p_changes ->> 'folder_id')::uuid) THEN
        RAISE EXCEPTION 'dossier inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    UPDATE lattis.datasets SET
        name             = CASE WHEN p_changes ? 'name' THEN trim(p_changes ->> 'name') ELSE name END,
        description      = CASE WHEN p_changes ? 'description' THEN coalesce(p_changes ->> 'description', '') ELSE description END,
        folder_id        = CASE WHEN p_changes ? 'folder_id' THEN (p_changes ->> 'folder_id')::uuid ELSE folder_id END,
        hidden_columns   = CASE WHEN p_changes ? 'hidden_columns'
                                THEN ARRAY(SELECT jsonb_array_elements_text(p_changes -> 'hidden_columns')) ELSE hidden_columns END,
        inherit_audience = CASE WHEN p_changes ? 'inherit_audience' THEN (p_changes ->> 'inherit_audience')::boolean ELSE inherit_audience END,
        updated_at = now()
    WHERE id = p_id;
    PERFORM lattis.compile_dataset(p_id);
    PERFORM lattis.audit_log('update_dataset', p_id::text, to_jsonb(d) - 'created_by', p_changes);
    PERFORM pg_notify('lattis_data', '*catalog');
END
$$;

-- Retirer un partage : la vue disparaît ; les classeurs qui l'utilisaient
-- reçoivent « *withdrawn:<id> » sur le canal lattis_data.
CREATE FUNCTION lattis.drop_dataset(p_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    d lattis.datasets := lattis.require_manage_dataset(p_id);
BEGIN
    EXECUTE format('DROP VIEW IF EXISTS donnees.%I', d.sql_name);
    DELETE FROM lattis.datasets WHERE id = p_id;
    PERFORM lattis.audit_log('drop_dataset', p_id::text, to_jsonb(d), NULL);
    PERFORM pg_notify('lattis_data', '*withdrawn:' || p_id::text);
END
$$;

-- Audience -----------------------------------------------------------------------

-- Ajoute une personne ou une équipe (p_principal) à l'audience d'une donnée
-- (p_dataset_id) ou d'un dossier (p_folder_id), avec un filtre facultatif.
CREATE FUNCTION lattis.add_data_audience(p_dataset_id uuid, p_folder_id uuid, p_principal uuid, p_predicate text) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    d      lattis.datasets;
    v      lattis.workbooks;
    v_kind text;
    v_id   uuid;
BEGIN
    IF (p_dataset_id IS NULL) = (p_folder_id IS NULL) THEN
        RAISE EXCEPTION 'une donnée ou un dossier' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF p_dataset_id IS NOT NULL THEN
        d := lattis.require_manage_dataset(p_dataset_id);
    ELSE
        PERFORM lattis.require_data_publisher();
        IF NOT EXISTS (SELECT 1 FROM lattis.data_folders WHERE id = p_folder_id) THEN
            RAISE EXCEPTION 'dossier inconnu' USING ERRCODE = 'no_data_found';
        END IF;
    END IF;
    IF EXISTS (SELECT 1 FROM lattis.users WHERE id = p_principal) THEN
        v_kind := 'user';
    ELSIF EXISTS (SELECT 1 FROM lattis.teams WHERE id = p_principal) THEN
        v_kind := 'team';
    ELSE
        RAISE EXCEPTION 'utilisateur ou équipe inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    p_predicate := nullif(trim(coalesce(p_predicate, '')), '');
    -- Filtre vérifié sur le tableau source quand on le connaît ; sur un
    -- dossier, il l'est à la compilation de chaque donnée (ignoré s'il ne s'y
    -- applique pas).
    IF p_predicate IS NOT NULL AND d.id IS NOT NULL AND d.workbook_id IS NOT NULL AND d.table_name IS NOT NULL THEN
        SELECT * INTO v FROM lattis.workbooks WHERE id = d.workbook_id;
        p_predicate := lattis.check_predicate(v.schema_name, d.table_name, p_predicate);
    END IF;
    INSERT INTO lattis.data_audience (dataset_id, folder_id, principal, principal_kind, predicate, created_by)
    VALUES (p_dataset_id, p_folder_id, p_principal, v_kind, p_predicate, lattis.actor()) RETURNING id INTO v_id;
    IF p_dataset_id IS NOT NULL THEN
        PERFORM lattis.compile_dataset(p_dataset_id);
    ELSE
        PERFORM lattis.compile_folder_datasets(p_folder_id);
    END IF;
    PERFORM lattis.audit_log('add_data_audience', v_id::text, NULL,
        jsonb_build_object('dataset_id', p_dataset_id, 'folder_id', p_folder_id, 'principal', p_principal, 'predicate', p_predicate));
    RETURN v_id;
END
$$;

CREATE FUNCTION lattis.remove_data_audience(p_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    a lattis.data_audience;
BEGIN
    SELECT * INTO a FROM lattis.data_audience WHERE id = p_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'entrée inconnue' USING ERRCODE = 'no_data_found';
    END IF;
    IF a.dataset_id IS NOT NULL THEN
        PERFORM lattis.require_manage_dataset(a.dataset_id);
    ELSE
        PERFORM lattis.require_data_publisher();
    END IF;
    DELETE FROM lattis.data_audience WHERE id = p_id;
    IF a.dataset_id IS NOT NULL THEN
        PERFORM lattis.compile_dataset(a.dataset_id);
    ELSE
        PERFORM lattis.compile_folder_datasets(a.folder_id);
    END IF;
    PERFORM lattis.audit_log('remove_data_audience', p_id::text, to_jsonb(a), NULL);
END
$$;

-- Tableaux liés ----------------------------------------------------------------

-- Note qu'un classeur du serveur utilise une donnée (« utilisée par »).
CREATE FUNCTION lattis.link_dataset(p_id uuid, p_workbook_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    d lattis.datasets := lattis.dataset(p_id);
BEGIN
    PERFORM lattis.require_workbook_role(p_workbook_id, '{owner,designer}', true);
    IF NOT lattis.can_read_dataset(d) AND NOT lattis.can_manage_dataset(d) THEN
        RAISE EXCEPTION 'cette donnée ne vous est pas accessible' USING ERRCODE = 'insufficient_privilege';
    END IF;
    INSERT INTO lattis.dataset_links (dataset_id, workbook_id, linked_by) VALUES (p_id, p_workbook_id, lattis.actor())
    ON CONFLICT DO NOTHING;
END
$$;

CREATE FUNCTION lattis.unlink_dataset(p_id uuid, p_workbook_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_workbook_role(p_workbook_id, '{owner,designer}', true);
    DELETE FROM lattis.dataset_links WHERE dataset_id = p_id AND workbook_id = p_workbook_id;
END
$$;

-- Lignes supprimées depuis p_since dans le tableau source (identifiants
-- seulement), pour le relevé d'un tableau lié.
CREATE FUNCTION lattis.dataset_tombstones(p_id uuid, p_since xid8) RETURNS TABLE (row_id uuid, xid xid8)
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    d lattis.datasets := lattis.dataset(p_id);
BEGIN
    IF NOT lattis.can_read_dataset(d) THEN
        RAISE EXCEPTION 'cette donnée ne vous est pas accessible' USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN QUERY SELECT t.row_id, t.xid FROM lattis.tombstones t
                 WHERE t.table_id = d.table_id AND t.xid > p_since ORDER BY t.xid;
END
$$;

-- Catalogue ------------------------------------------------------------------------

-- Ce que la personne voit du catalogue : dossiers (tous pour un publieur,
-- sinon ceux qui mènent à une donnée lisible) et données lisibles ou gérables.
-- Pour une donnée gérable : source, audience et classeurs qui l'utilisent.
CREATE FUNCTION lattis.data_catalog() RETURNS jsonb
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

CREATE FUNCTION lattis.data_audience_json(p_dataset_id uuid, p_folder_id uuid) RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT coalesce(jsonb_agg(jsonb_build_object('id', a.id, 'kind', a.principal_kind, 'principal', a.principal,
               'label', coalesce(u.email, t.name, '(supprimé)'), 'predicate', a.predicate) ORDER BY a.created_at), '[]')
    FROM lattis.data_audience a
    LEFT JOIN lattis.users u ON a.principal_kind = 'user' AND u.id = a.principal
    LEFT JOIN lattis.teams t ON a.principal_kind = 'team' AND t.id = a.principal
    WHERE a.dataset_id IS NOT DISTINCT FROM p_dataset_id AND a.folder_id IS NOT DISTINCT FROM p_folder_id
      AND (p_dataset_id IS NOT NULL OR p_folder_id IS NOT NULL)
$$;

-- Recompilations automatiques ----------------------------------------------------

-- Nouvelle version d'un classeur source (republication) ou publication.
CREATE FUNCTION lattis.trg_dataset_source() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_id uuid;
BEGIN
    FOR v_id IN SELECT id FROM lattis.datasets WHERE workbook_id = NEW.id LOOP
        PERFORM lattis.compile_dataset(v_id);
    END LOOP;
    RETURN NULL;
END
$$;
CREATE TRIGGER lattis_dataset_source AFTER UPDATE OF schema_name, state ON lattis.workbooks
    FOR EACH ROW WHEN (OLD.schema_name IS DISTINCT FROM NEW.schema_name OR OLD.state IS DISTINCT FROM NEW.state)
    EXECUTE FUNCTION lattis.trg_dataset_source();

-- Équipe supprimée : ses entrées d'audience partent, les vues sont refaites
-- (une vue ne doit jamais citer un rôle disparu).
CREATE FUNCTION lattis.trg_dataset_team_dropped() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    a record;
BEGIN
    FOR a IN DELETE FROM lattis.data_audience WHERE principal_kind = 'team' AND principal = OLD.id RETURNING dataset_id, folder_id LOOP
        IF a.dataset_id IS NOT NULL THEN
            PERFORM lattis.compile_dataset(a.dataset_id);
        ELSE
            PERFORM lattis.compile_folder_datasets(a.folder_id);
        END IF;
    END LOOP;
    RETURN NULL;
END
$$;
CREATE TRIGGER lattis_dataset_team_dropped AFTER DELETE ON lattis.teams
    FOR EACH ROW EXECUTE FUNCTION lattis.trg_dataset_team_dropped();

-- login_summary : droit de publier des données.
CREATE OR REPLACE FUNCTION lattis.login_summary() RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT jsonb_build_object(
        'server', jsonb_build_object(
            'display_name', lattis.config('display_name'),
            'access_mode', lattis.config('access_mode')),
        'user', (SELECT jsonb_build_object('id', u.id, 'email', u.email, 'display_name', u.display_name,
                                           'pg_role', u.pg_role, 'last_login_at', u.last_login_at)
                 FROM lattis.users u WHERE u.pg_role = session_user),
        'server_owner', pg_has_role(session_user, 'lattis_server_owners', 'MEMBER'),
        'workbook_creator', pg_has_role(session_user, 'lattis_workbook_creators', 'MEMBER'),
        'can_create_workbooks', lattis.can_create_workbooks(),
        'data_publisher', pg_has_role(session_user, 'lattis_data_publishers', 'MEMBER'),
        'can_publish_data', lattis.can_publish_data(),
        'workbooks', lattis.my_workbooks())
$$;

REVOKE EXECUTE ON FUNCTION lattis.can_manage_dataset(lattis.datasets), lattis.dataset(uuid), lattis.require_manage_dataset(uuid),
    lattis.can_read_dataset(lattis.datasets), lattis.require_data_publisher(), lattis.data_folder_closure(),
    lattis.compile_dataset(uuid), lattis.compile_folder_datasets(uuid), lattis.dataset_sql_name(text),
    lattis.data_audience_json(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION lattis.can_publish_data(), lattis.set_data_publisher(uuid, boolean),
    lattis.create_data_folder(text, uuid), lattis.rename_data_folder(uuid, text), lattis.move_data_folder(uuid, uuid),
    lattis.drop_data_folder(uuid),
    lattis.create_dataset(uuid, uuid, uuid, text, text, text, text[]), lattis.update_dataset(uuid, jsonb), lattis.drop_dataset(uuid),
    lattis.add_data_audience(uuid, uuid, uuid, text), lattis.remove_data_audience(uuid),
    lattis.link_dataset(uuid, uuid), lattis.unlink_dataset(uuid, uuid), lattis.dataset_tombstones(uuid, xid8),
    lattis.data_catalog() TO PUBLIC;
