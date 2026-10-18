-- Classeurs partagés (spécification §5.1, §5.2, §6, §9.2, §9.3).
--
-- Un classeur = un schéma wb_<slug>, propriété du rôle NOLOGIN
-- wb_<slug>_owner. Les rôles fournis sont des groupes NOLOGIN
-- wb_<slug>_r_<nom>. lattis_service est membre (avec héritage) des
-- propriétaires qu'il crée : c'est lui qui fait le DDL, dans les fonctions
-- SECURITY DEFINER ci-dessous, jamais les membres.
--
-- Publication (figé comme collaboratif) : create_workbook ouvre un brouillon
-- où propriétaires et concepteurs créent les tableaux (create_table) et y
-- copient les données (COPY) ; finish_publish rend le classeur visible aux
-- membres. Un classeur figé se republie : republish ouvre une version de
-- travail wb_<slug>__v<n>, finish_publish la met en place d'un coup.

INSERT INTO lattis.server_config (key, value) VALUES
    ('workbook_creators', '"everyone"'),
    ('republish_grace', '600');

CREATE TABLE lattis.workbooks (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug            text NOT NULL UNIQUE,
    schema_name     text NOT NULL UNIQUE,
    title           text NOT NULL,
    mode            text NOT NULL CHECK (mode IN ('frozen', 'collaborative')),
    state           text NOT NULL DEFAULT 'draft' CHECK (state IN ('draft', 'published', 'republishing')),
    version         integer NOT NULL DEFAULT 1,
    staging_schema  text,
    created_by      text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    published_at    timestamptz
);

CREATE TABLE lattis.roles (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workbook_id uuid NOT NULL REFERENCES lattis.workbooks (id) ON DELETE CASCADE,
    name        text NOT NULL,
    builtin     text CHECK (builtin IN ('owner', 'designer', 'editor', 'participant', 'reader')),
    pg_role     text NOT NULL UNIQUE,
    UNIQUE (workbook_id, name)
);

CREATE TABLE lattis.role_grants (
    role_id      uuid NOT NULL REFERENCES lattis.roles (id) ON DELETE CASCADE,
    node_kind    text NOT NULL CHECK (node_kind IN ('workbook', 'sheet', 'table')),
    node_id      text NOT NULL,
    level        text NOT NULL CHECK (level IN ('none', 'reader', 'participant', 'editor')),
    read_filter  text,
    write_filter text,
    PRIMARY KEY (role_id, node_kind, node_id)
);

CREATE TABLE lattis.members (
    role_id    uuid NOT NULL REFERENCES lattis.roles (id) ON DELETE CASCADE,
    principal  uuid NOT NULL REFERENCES lattis.users (id) ON DELETE CASCADE,
    granted_by text NOT NULL,
    granted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (role_id, principal)
);

CREATE TABLE lattis.restrictions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workbook_id uuid NOT NULL REFERENCES lattis.workbooks (id) ON DELETE CASCADE,
    table_id    uuid NOT NULL,
    command     text NOT NULL CHECK (command IN ('select', 'insert', 'update', 'delete', 'all')),
    applies_to  uuid[] NOT NULL DEFAULT '{}',
    predicate   text NOT NULL,
    label       text NOT NULL DEFAULT ''
);

CREATE TABLE lattis.tombstones (
    workbook_id uuid NOT NULL REFERENCES lattis.workbooks (id) ON DELETE CASCADE,
    table_id    uuid NOT NULL,
    row_id      uuid NOT NULL,
    xid         xid8 NOT NULL DEFAULT pg_current_xact_id(),
    deleted_by  text NOT NULL DEFAULT session_user,
    deleted_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (table_id, row_id, xid)
);

CREATE INDEX tombstones_since ON lattis.tombstones (table_id, xid);

-- Anciennes versions d'un classeur figé, supprimées après un délai de grâce.
CREATE TABLE lattis.pending_drops (
    schema_name text PRIMARY KEY,
    owner_role  text NOT NULL,
    drop_after  timestamptz NOT NULL
);

ALTER TABLE lattis.invitations
    ADD CONSTRAINT invitations_workbook_fk FOREIGN KEY (workbook_id) REFERENCES lattis.workbooks (id) ON DELETE CASCADE;

REVOKE ALL ON lattis.workbooks, lattis.roles, lattis.role_grants, lattis.members, lattis.restrictions,
              lattis.tombstones, lattis.pending_drops FROM PUBLIC;
GRANT SELECT ON lattis.workbooks, lattis.roles, lattis.members TO lattis_server_owners;

-- Outils ----------------------------------------------------------------------

-- Identifiant ASCII court tiré du titre (rôles PostgreSQL : 63 octets).
CREATE FUNCTION lattis.slugify(p_title text) RETURNS text
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_base text;
    v_slug text;
    v_n    integer := 1;
BEGIN
    v_base := lower(translate(coalesce(p_title, ''),
        'àâäáãåçéèêëíìîïñóòôöõúùûüýÿÀÂÄÁÃÅÇÉÈÊËÍÌÎÏÑÓÒÔÖÕÚÙÛÜÝœŒæÆ',
        'aaaaaaceeeeiiiinooooouuuuyyAAAAAACEEEEIIIINOOOOOUUUUYoOaA'));
    v_base := trim(BOTH '_' FROM regexp_replace(v_base, '[^a-z0-9]+', '_', 'g'));
    v_base := left(coalesce(nullif(v_base, ''), 'classeur'), 30);
    v_slug := v_base;
    WHILE EXISTS (SELECT 1 FROM lattis.workbooks WHERE slug = v_slug)
       OR EXISTS (SELECT 1 FROM pg_namespace WHERE nspname LIKE 'wb\_' || v_slug || '%')
       OR EXISTS (SELECT 1 FROM pg_roles WHERE rolname LIKE 'wb\_' || v_slug || '\_%') LOOP
        v_n := v_n + 1;
        v_slug := v_base || '_' || v_n;
    END LOOP;
    RETURN v_slug;
END
$$;

CREATE FUNCTION lattis.workbook(p_id uuid) RETURNS lattis.workbooks
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v lattis.workbooks;
BEGIN
    SELECT * INTO v FROM lattis.workbooks WHERE id = p_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'classeur inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    RETURN v;
END
$$;

-- Rôles fournis de session_user sur un classeur.
CREATE FUNCTION lattis.my_builtin_roles(p_workbook_id uuid) RETURNS text[]
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT coalesce(array_agg(r.builtin ORDER BY r.builtin), '{}')
    FROM lattis.roles r
    WHERE r.workbook_id = p_workbook_id AND r.builtin IS NOT NULL
      AND pg_has_role(session_user, r.pg_role, 'MEMBER')
$$;

CREATE FUNCTION lattis.has_workbook_role(p_workbook_id uuid, p_roles text[]) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT session_user = 'lattis_service' OR lattis.my_builtin_roles(p_workbook_id) && p_roles
$$;

CREATE FUNCTION lattis.require_workbook_role(p_workbook_id uuid, p_roles text[], p_server_owner_ok boolean)
RETURNS lattis.workbooks
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v lattis.workbooks := lattis.workbook(p_workbook_id);
BEGIN
    IF NOT lattis.has_workbook_role(p_workbook_id, p_roles)
       AND NOT (p_server_owner_ok AND lattis.is_server_owner()) THEN
        RAISE EXCEPTION 'droits insuffisants sur le classeur « % »', v.title
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN v;
END
$$;

-- Schéma où l'on publie : le brouillon, ou la version de travail.
CREATE FUNCTION lattis.publish_target(v lattis.workbooks) RETURNS text
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    IF v.state = 'draft' THEN
        RETURN v.schema_name;
    ELSIF v.state = 'republishing' THEN
        RETURN v.staging_schema;
    ELSIF v.mode = 'collaborative' THEN
        RETURN v.schema_name;
    END IF;
    RAISE EXCEPTION 'classeur figé publié : republier pour le modifier' USING ERRCODE = 'object_not_in_prerequisite_state';
END
$$;

-- Tables de mise en page d'un schéma de classeur (miroir des _lattis_* locales).
CREATE FUNCTION lattis.create_meta_tables(p_schema text, p_owner text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_t text;
BEGIN
    EXECUTE format($f$
        CREATE TABLE %1$I.lattis_meta_tables (
            table_id    uuid PRIMARY KEY,
            name        text NOT NULL,
            local_id    text,
            sheet_id    text,
            position    integer NOT NULL DEFAULT 0,
            key_column  uuid,
            lattis_xid  xid8 NOT NULL DEFAULT pg_current_xact_id(),
            lattis_updated_by text NOT NULL DEFAULT session_user,
            lattis_updated_at timestamptz NOT NULL DEFAULT now()
        );
        CREATE UNIQUE INDEX ON %1$I.lattis_meta_tables (lower(name));
        CREATE TABLE %1$I.lattis_meta_columns (
            table_id    uuid NOT NULL REFERENCES %1$I.lattis_meta_tables ON DELETE CASCADE,
            column_id   uuid NOT NULL,
            name        text NOT NULL,
            local_id    text,
            type        text NOT NULL,
            position    integer NOT NULL,
            lattis_xid  xid8 NOT NULL DEFAULT pg_current_xact_id(),
            lattis_updated_by text NOT NULL DEFAULT session_user,
            lattis_updated_at timestamptz NOT NULL DEFAULT now(),
            PRIMARY KEY (table_id, column_id)
        );
        CREATE UNIQUE INDEX ON %1$I.lattis_meta_columns (table_id, lower(name));
        CREATE TABLE %1$I.lattis_meta_objects (
            kind        text NOT NULL,
            key         text NOT NULL,
            payload     jsonb NOT NULL,
            lattis_xid  xid8 NOT NULL DEFAULT pg_current_xact_id(),
            lattis_updated_by text NOT NULL DEFAULT session_user,
            lattis_updated_at timestamptz NOT NULL DEFAULT now(),
            PRIMARY KEY (kind, key)
        );
        CREATE INDEX ON %1$I.lattis_meta_objects (lattis_xid);
        CREATE TABLE %1$I.lattis_meta_script_versions (
            name         text NOT NULL,
            version      integer NOT NULL,
            source       text NOT NULL,
            sha256       text NOT NULL,
            published_by text NOT NULL DEFAULT session_user,
            published_at timestamptz NOT NULL DEFAULT now(),
            lattis_xid   xid8 NOT NULL DEFAULT pg_current_xact_id(),
            PRIMARY KEY (name, version)
        )$f$, p_schema);
    FOREACH v_t IN ARRAY ARRAY['lattis_meta_tables', 'lattis_meta_columns', 'lattis_meta_objects', 'lattis_meta_script_versions'] LOOP
        EXECUTE format('ALTER TABLE %I.%I OWNER TO %I', p_schema, v_t, p_owner);
        EXECUTE format('REVOKE ALL ON %I.%I FROM PUBLIC', p_schema, v_t);
    END LOOP;
END
$$;

-- Schéma de classeur vide, propriété de p_owner.
CREATE FUNCTION lattis.create_workbook_schema(p_schema text, p_owner text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    EXECUTE format('CREATE SCHEMA %I AUTHORIZATION %I', p_schema, p_owner);
    EXECUTE format('REVOKE ALL ON SCHEMA %I FROM PUBLIC', p_schema);
    PERFORM lattis.create_meta_tables(p_schema, p_owner);
END
$$;

-- §9.2 Classeurs ---------------------------------------------------------------

CREATE FUNCTION lattis.create_workbook(p_title text, p_mode text) RETURNS uuid
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
    IF lattis.config('workbook_creators') #>> '{}' = 'owners' AND NOT lattis.is_server_owner() THEN
        RAISE EXCEPTION 'la création de classeurs est réservée aux propriétaires du serveur'
            USING ERRCODE = 'insufficient_privilege';
    END IF;

    v_slug := lattis.slugify(p_title);
    v_owner := 'wb_' || v_slug || '_owner';
    EXECUTE format('CREATE ROLE %I NOLOGIN', v_owner);
    -- lattis_service agit comme propriétaire (DDL, ALTER … OWNER).
    EXECUTE format('GRANT %I TO lattis_service WITH INHERIT TRUE, SET TRUE', v_owner);
    PERFORM lattis.create_workbook_schema('wb_' || v_slug, v_owner);

    INSERT INTO lattis.workbooks (id, slug, schema_name, title, mode, created_by)
    VALUES (v_id, v_slug, 'wb_' || v_slug, trim(p_title), p_mode, session_user);

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

-- Ajoute un utilisateur à un rôle fourni (source de vérité + appartenance).
CREATE FUNCTION lattis.add_member(p_workbook_id uuid, p_builtin text, p_user_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_role lattis.roles;
    v_user text;
BEGIN
    SELECT * INTO v_role FROM lattis.roles WHERE workbook_id = p_workbook_id AND builtin = p_builtin;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'rôle « % » inexistant sur ce classeur', p_builtin USING ERRCODE = 'invalid_parameter_value';
    END IF;
    SELECT pg_role INTO STRICT v_user FROM lattis.users WHERE id = p_user_id;
    INSERT INTO lattis.members (role_id, principal, granted_by) VALUES (v_role.id, p_user_id, lattis.actor())
    ON CONFLICT DO NOTHING;
    EXECUTE format('GRANT %I TO %I', v_role.pg_role, v_user);
END
$$;

CREATE FUNCTION lattis.drop_workbook(p_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v      lattis.workbooks := lattis.require_workbook_role(p_id, '{owner}', true);
    v_role text;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(p_id::text, 0));
    EXECUTE format('DROP SCHEMA IF EXISTS %I CASCADE', v.schema_name);
    IF v.staging_schema IS NOT NULL THEN
        EXECUTE format('DROP SCHEMA IF EXISTS %I CASCADE', v.staging_schema);
    END IF;
    FOR v_role IN SELECT schema_name FROM lattis.pending_drops WHERE owner_role = 'wb_' || v.slug || '_owner' LOOP
        EXECUTE format('DROP SCHEMA IF EXISTS %I CASCADE', v_role);
    END LOOP;
    DELETE FROM lattis.pending_drops WHERE owner_role = 'wb_' || v.slug || '_owner';
    FOR v_role IN SELECT pg_role FROM lattis.roles WHERE workbook_id = p_id LOOP
        EXECUTE format('DROP ROLE IF EXISTS %I', v_role);
    END LOOP;
    DELETE FROM lattis.workbooks WHERE id = p_id;
    EXECUTE format('DROP ROLE IF EXISTS %I', 'wb_' || v.slug || '_owner');
    PERFORM lattis.audit_log('drop_workbook', p_id::text, to_jsonb(v), NULL);
END
$$;

-- §9.3 Structure ----------------------------------------------------------------

CREATE FUNCTION lattis.pg_type(p_type text) RETURNS text
LANGUAGE sql IMMUTABLE SET search_path = pg_catalog, pg_temp
AS $$
    SELECT CASE p_type
        WHEN 'text' THEN 'text' WHEN 'integer' THEN 'bigint' WHEN 'decimal' THEN 'numeric'
        WHEN 'date' THEN 'date' WHEN 'datetime' THEN 'timestamptz' WHEN 'boolean' THEN 'boolean'
    END
$$;

CREATE FUNCTION lattis.check_name(p_name text, p_what text) RETURNS text
LANGUAGE plpgsql IMMUTABLE SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    p_name := trim(coalesce(p_name, ''));
    IF p_name = '' OR octet_length(p_name) > 63 THEN
        RAISE EXCEPTION '% : nom vide ou trop long (63 octets)', p_what USING ERRCODE = 'invalid_name';
    END IF;
    IF lower(p_name) LIKE 'lattis\_%' THEN
        RAISE EXCEPTION '% : le préfixe lattis_ est réservé (« % »)', p_what, p_name USING ERRCODE = 'invalid_name';
    END IF;
    RETURN p_name;
END
$$;

-- Colonnes système communes à tous les tableaux (§5.2).
CREATE FUNCTION lattis.system_columns() RETURNS text
LANGUAGE sql IMMUTABLE SET search_path = pg_catalog, pg_temp
AS $$
    SELECT 'lattis_id uuid PRIMARY KEY,
            lattis_pos text COLLATE "C" NOT NULL DEFAULT '''',
            lattis_xid xid8 NOT NULL DEFAULT pg_current_xact_id(),
            lattis_author text NOT NULL DEFAULT session_user,
            lattis_updated_by text NOT NULL DEFAULT session_user,
            lattis_updated_at timestamptz NOT NULL DEFAULT now()'
$$;

-- p_columns : [{"name": "Client", "type": "text", "local_id": "12"}, …]
-- Renvoie l'identifiant du tableau ; les colonnes reçoivent un column_id
-- lisible dans lattis_meta_columns.
CREATE FUNCTION lattis.create_table(p_workbook_id uuid, p_name text, p_columns jsonb) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v       lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', false);
    v_sch   text;
    v_tid   uuid := gen_random_uuid();
    v_cols  text := '';
    v_col   jsonb;
    v_name  text;
    v_type  text;
    v_pos   integer := 0;
    v_owner text := 'wb_' || v.slug || '_owner';
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(p_workbook_id::text, 0));
    v_sch := lattis.publish_target(v);
    p_name := lattis.check_name(p_name, 'tableau');
    IF jsonb_typeof(p_columns) <> 'array' OR jsonb_array_length(p_columns) = 0 THEN
        RAISE EXCEPTION 'colonnes : liste non vide attendue' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    EXECUTE format('INSERT INTO %I.lattis_meta_tables (table_id, name, local_id, sheet_id, position)
                    VALUES ($1, $2, $3, $4, (SELECT coalesce(max(position), 0) + 1 FROM %I.lattis_meta_tables))',
                   v_sch, v_sch)
        USING v_tid, p_name, p_columns -> 0 ->> 'table_local_id', NULL::text;
    FOR v_col IN SELECT * FROM jsonb_array_elements(p_columns) LOOP
        v_name := lattis.check_name(v_col ->> 'name', 'colonne');
        v_type := lattis.pg_type(v_col ->> 'type');
        IF v_type IS NULL THEN
            RAISE EXCEPTION 'colonne « % » : type inconnu « % » (text, integer, decimal, date, datetime, boolean)',
                v_name, v_col ->> 'type' USING ERRCODE = 'invalid_parameter_value';
        END IF;
        v_pos := v_pos + 1;
        EXECUTE format('INSERT INTO %I.lattis_meta_columns (table_id, column_id, name, local_id, type, position)
                        VALUES ($1, gen_random_uuid(), $2, $3, $4, $5)', v_sch)
            USING v_tid, v_name, v_col ->> 'local_id', v_col ->> 'type', v_pos;
        v_cols := v_cols || format(', %I %s', v_name, v_type);
    END LOOP;
    EXECUTE format('CREATE TABLE %I.%I (%s%s)', v_sch, p_name, lattis.system_columns(), v_cols);
    EXECUTE format('CREATE INDEX ON %I.%I (lattis_pos)', v_sch, p_name);
    EXECUTE format('CREATE INDEX ON %I.%I (lattis_xid)', v_sch, p_name);
    EXECUTE format('ALTER TABLE %I.%I OWNER TO %I', v_sch, p_name, v_owner);
    EXECUTE format('REVOKE ALL ON %I.%I FROM PUBLIC', v_sch, p_name);
    EXECUTE format('COMMENT ON TABLE %I.%I IS %L', v_sch, p_name, 'lattis:' || v_tid);
    PERFORM lattis.compile_acl(p_workbook_id);
    PERFORM lattis.audit_log('create_table', v_tid::text, NULL,
        jsonb_build_object('workbook_id', p_workbook_id, 'schema', v_sch, 'name', p_name, 'columns', p_columns));
    RETURN v_tid;
END
$$;

-- Mise en page (§5.2) : p_payload doit porter une clé "key" quand un même
-- type d'objet existe en plusieurs exemplaires (feuille, graphique…).
CREATE FUNCTION lattis.save_meta(p_workbook_id uuid, p_kind text, p_payload jsonb) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v     lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', false);
    v_sch text := lattis.publish_target(v);
BEGIN
    IF coalesce(p_kind, '') = '' OR p_payload IS NULL THEN
        RAISE EXCEPTION 'type et contenu requis' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    EXECUTE format('INSERT INTO %I.lattis_meta_objects (kind, key, payload) VALUES ($1, $2, $3)
                    ON CONFLICT (kind, key) DO UPDATE SET payload = excluded.payload,
                        lattis_xid = pg_current_xact_id(), lattis_updated_by = session_user, lattis_updated_at = now()', v_sch)
        USING p_kind, coalesce(p_payload ->> 'key', ''), p_payload;
END
$$;

CREATE FUNCTION lattis.publish_script(p_workbook_id uuid, p_name text, p_source text) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v     lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', false);
    v_sch text := lattis.publish_target(v);
    v_ver integer;
BEGIN
    EXECUTE format('SELECT coalesce(max(version), 0) + 1 FROM %I.lattis_meta_script_versions WHERE name = $1', v_sch)
        INTO v_ver USING p_name;
    EXECUTE format('INSERT INTO %I.lattis_meta_script_versions (name, version, source, sha256) VALUES ($1, $2, $3, $4)', v_sch)
        USING p_name, v_ver, p_source, encode(sha256(convert_to(p_source, 'UTF8')), 'hex');
    PERFORM lattis.audit_log('publish_script', p_workbook_id::text, NULL,
        jsonb_build_object('name', p_name, 'version', v_ver));
    RETURN v_ver;
END
$$;

-- Droits ---------------------------------------------------------------------

-- Traduit roles/role_grants/members en droits PostgreSQL (§8.3).
-- Classeur figé : lecture pour tous les membres, écriture (COPY) pour
-- propriétaires pendant la publication. Les classeurs collaboratifs sont
-- compilés par lattis.compile_collaborative (migration suivante).
CREATE FUNCTION lattis.compile_acl(p_workbook_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v      lattis.workbooks := lattis.workbook(p_workbook_id);
    v_sch  text;
    v_tab  record;
    v_role lattis.roles;
    v_publishers text;
BEGIN
    IF session_user <> 'lattis_service' AND NOT lattis.has_workbook_role(p_workbook_id, '{owner,designer}')
       AND NOT lattis.is_server_owner() THEN
        RAISE EXCEPTION 'droits insuffisants' USING ERRCODE = 'insufficient_privilege';
    END IF;
    SELECT string_agg(quote_ident(pg_role), ', ') INTO v_publishers
    FROM lattis.roles WHERE workbook_id = p_workbook_id AND builtin IN ('owner', 'designer');

    FOREACH v_sch IN ARRAY array_remove(ARRAY[v.schema_name, v.staging_schema], NULL) LOOP
        FOR v_role IN SELECT * FROM lattis.roles WHERE workbook_id = p_workbook_id LOOP
            IF v_sch = v.schema_name OR v_role.builtin IN ('owner', 'designer') THEN
                EXECUTE format('GRANT USAGE ON SCHEMA %I TO %I', v_sch, v_role.pg_role);
            END IF;
        END LOOP;
        FOR v_tab IN SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
                     WHERE n.nspname = v_sch AND c.relkind IN ('r', 'p') LOOP
            EXECUTE format('REVOKE ALL ON %I.%I FROM PUBLIC', v_sch, v_tab.relname);
            FOR v_role IN SELECT * FROM lattis.roles WHERE workbook_id = p_workbook_id LOOP
                EXECUTE format('REVOKE ALL ON %I.%I FROM %I', v_sch, v_tab.relname, v_role.pg_role);
            END LOOP;
            -- Version en cours de publication : propriétaires et concepteurs remplissent.
            IF (v.state = 'draft' AND v_sch = v.schema_name) OR v_sch = v.staging_schema THEN
                IF v_tab.relname LIKE 'lattis\_meta\_%' THEN
                    EXECUTE format('GRANT SELECT ON %I.%I TO %s', v_sch, v_tab.relname, v_publishers);
                ELSE
                    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.%I TO %s', v_sch, v_tab.relname, v_publishers);
                END IF;
            ELSIF v.mode = 'frozen' AND v.state <> 'draft' THEN
                FOR v_role IN SELECT * FROM lattis.roles WHERE workbook_id = p_workbook_id LOOP
                    EXECUTE format('GRANT SELECT ON %I.%I TO %I', v_sch, v_tab.relname, v_role.pg_role);
                END LOOP;
            END IF;
        END LOOP;
    END LOOP;
    IF v.mode = 'collaborative' AND v.state = 'published' THEN
        PERFORM lattis.compile_collaborative(p_workbook_id);
    END IF;
END
$$;

-- Remplacée par la migration des classeurs collaboratifs.
CREATE FUNCTION lattis.compile_collaborative(p_workbook_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    RAISE EXCEPTION 'classeurs collaboratifs : pas encore disponibles' USING ERRCODE = 'feature_not_supported';
END
$$;

-- Fin de publication : le classeur devient visible aux membres. Pour une
-- republication, bascule atomique vers la nouvelle version.
CREATE FUNCTION lattis.finish_publish(p_workbook_id uuid) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v     lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', false);
    v_old text;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(p_workbook_id::text, 0));
    SELECT * INTO v FROM lattis.workbooks WHERE id = p_workbook_id FOR UPDATE;
    IF v.state = 'draft' THEN
        UPDATE lattis.workbooks SET state = 'published', published_at = now() WHERE id = p_workbook_id;
    ELSIF v.state = 'republishing' THEN
        v_old := v.schema_name;
        UPDATE lattis.workbooks
           SET schema_name = staging_schema, staging_schema = NULL, state = 'published',
               version = version + 1, published_at = now()
         WHERE id = p_workbook_id;
        -- Les clients ouverts finissent leur lecture sur l'ancienne version.
        INSERT INTO lattis.pending_drops (schema_name, owner_role, drop_after)
        VALUES (v_old, 'wb_' || v.slug || '_owner',
                now() + make_interval(secs => (lattis.config('republish_grace') #>> '{}')::double precision));
    ELSE
        RAISE EXCEPTION 'rien à publier' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    PERFORM lattis.compile_acl(p_workbook_id);
    SELECT * INTO v FROM lattis.workbooks WHERE id = p_workbook_id;
    -- Canal stable par classeur (§10) ; « * » = tout le classeur a changé.
    PERFORM pg_notify('wb_' || v.slug, '*version:' || v.version);
    PERFORM lattis.audit_log('publish', p_workbook_id::text, NULL,
        jsonb_build_object('version', v.version, 'schema', v.schema_name));
    RETURN v.version;
END
$$;

-- §9.2 republish : ouvre la version de travail d'un classeur figé et renvoie
-- son schéma ; l'auteur y recrée tableaux et mise en page, puis appelle
-- finish_publish (ou abort_publish).
CREATE FUNCTION lattis.republish(p_workbook_id uuid) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v     lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', false);
    v_sch text;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(p_workbook_id::text, 0));
    SELECT * INTO v FROM lattis.workbooks WHERE id = p_workbook_id FOR UPDATE;
    IF v.mode <> 'frozen' OR v.state <> 'published' THEN
        RAISE EXCEPTION 'seul un classeur figé publié se republie' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    v_sch := 'wb_' || v.slug || '__v' || (v.version + 1);
    IF EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = v_sch) THEN
        EXECUTE format('DROP SCHEMA %I CASCADE', v_sch);
        DELETE FROM lattis.pending_drops WHERE schema_name = v_sch;
    END IF;
    PERFORM lattis.create_workbook_schema(v_sch, 'wb_' || v.slug || '_owner');
    UPDATE lattis.workbooks SET state = 'republishing', staging_schema = v_sch WHERE id = p_workbook_id;
    PERFORM lattis.compile_acl(p_workbook_id);
    PERFORM lattis.audit_log('republish', p_workbook_id::text, NULL, jsonb_build_object('staging', v_sch));
    RETURN v_sch;
END
$$;

CREATE FUNCTION lattis.abort_publish(p_workbook_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', false);
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(p_workbook_id::text, 0));
    SELECT * INTO v FROM lattis.workbooks WHERE id = p_workbook_id FOR UPDATE;
    IF v.state = 'republishing' THEN
        EXECUTE format('DROP SCHEMA IF EXISTS %I CASCADE', v.staging_schema);
        UPDATE lattis.workbooks SET state = 'published', staging_schema = NULL WHERE id = p_workbook_id;
    ELSIF v.state = 'draft' THEN
        PERFORM lattis.drop_workbook(p_workbook_id);
    END IF;
END
$$;

-- Appelée par lattisd : supprime les anciennes versions après le délai de grâce.
CREATE FUNCTION lattis.drop_expired_versions() RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_s   text;
    v_n   integer := 0;
BEGIN
    PERFORM lattis.require_service();
    FOR v_s IN SELECT schema_name FROM lattis.pending_drops WHERE drop_after < now() FOR UPDATE SKIP LOCKED LOOP
        EXECUTE format('DROP SCHEMA IF EXISTS %I CASCADE', v_s);
        DELETE FROM lattis.pending_drops WHERE schema_name = v_s;
        v_n := v_n + 1;
    END LOOP;
    RETURN v_n;
END
$$;

-- Membres -------------------------------------------------------------------

CREATE FUNCTION lattis.grant_role(p_role_id uuid, p_principal uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_role lattis.roles;
    v_user text;
BEGIN
    SELECT * INTO v_role FROM lattis.roles WHERE id = p_role_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'rôle inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    PERFORM lattis.require_workbook_role(v_role.workbook_id, '{owner}', true);
    SELECT pg_role INTO v_user FROM lattis.users WHERE id = p_principal;
    IF v_user IS NULL THEN
        RAISE EXCEPTION 'utilisateur inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    INSERT INTO lattis.members (role_id, principal, granted_by) VALUES (p_role_id, p_principal, lattis.actor())
    ON CONFLICT DO NOTHING;
    EXECUTE format('GRANT %I TO %I', v_role.pg_role, v_user);
    PERFORM lattis.audit_log('grant_role', p_role_id::text, NULL, jsonb_build_object('principal', p_principal));
END
$$;

CREATE FUNCTION lattis.revoke_role(p_role_id uuid, p_principal uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_role lattis.roles;
    v_user text;
BEGIN
    SELECT * INTO v_role FROM lattis.roles WHERE id = p_role_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'rôle inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    PERFORM lattis.require_workbook_role(v_role.workbook_id, '{owner}', true);
    IF v_role.builtin = 'owner' AND (SELECT count(*) FROM lattis.members WHERE role_id = p_role_id) <= 1
       AND EXISTS (SELECT 1 FROM lattis.members WHERE role_id = p_role_id AND principal = p_principal) THEN
        RAISE EXCEPTION 'un classeur garde au moins un propriétaire' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    SELECT pg_role INTO v_user FROM lattis.users WHERE id = p_principal;
    DELETE FROM lattis.members WHERE role_id = p_role_id AND principal = p_principal;
    IF v_user IS NOT NULL THEN
        EXECUTE format('REVOKE %I FROM %I', v_role.pg_role, v_user);
    END IF;
    PERFORM lattis.audit_log('revoke_role', p_role_id::text, jsonb_build_object('principal', p_principal), NULL);
END
$$;

-- Invitations à un classeur : rôles fournis du classeur.
CREATE OR REPLACE FUNCTION lattis.apply_roles(p_user_id uuid, p_workbook_id uuid, p_roles text[]) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_role text;
    v_name text;
BEGIN
    SELECT pg_role INTO STRICT v_role FROM lattis.users WHERE id = p_user_id;
    IF p_workbook_id IS NULL THEN
        IF 'server_owner' = ANY (p_roles) THEN
            EXECUTE format('GRANT lattis_server_owners TO %I', v_role);
        END IF;
        RETURN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM lattis.workbooks WHERE id = p_workbook_id) THEN
        RETURN;
    END IF;
    FOREACH v_name IN ARRAY p_roles LOOP
        PERFORM lattis.add_member(p_workbook_id, v_name, p_user_id);
    END LOOP;
END
$$;

CREATE OR REPLACE FUNCTION lattis.invite(p_email text, p_workbook_id uuid, p_roles text[]) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_id   uuid;
    v_user lattis.users;
    v_bad  text;
BEGIN
    p_email := lower(trim(p_email));
    p_roles := coalesce(p_roles, '{}');
    IF p_workbook_id IS NULL THEN
        PERFORM lattis.require_server_owner();
        IF EXISTS (SELECT 1 FROM unnest(p_roles) r WHERE r <> 'server_owner') THEN
            RAISE EXCEPTION 'rôle de serveur inconnu (seul server_owner existe)' USING ERRCODE = 'invalid_parameter_value';
        END IF;
    ELSE
        PERFORM lattis.require_workbook_role(p_workbook_id, '{owner}', true);
        IF cardinality(p_roles) = 0 THEN
            RAISE EXCEPTION 'au moins un rôle' USING ERRCODE = 'invalid_parameter_value';
        END IF;
        SELECT r INTO v_bad FROM unnest(p_roles) r
        WHERE NOT EXISTS (SELECT 1 FROM lattis.roles WHERE workbook_id = p_workbook_id AND builtin = r) LIMIT 1;
        IF v_bad IS NOT NULL THEN
            RAISE EXCEPTION 'rôle « % » inexistant sur ce classeur', v_bad USING ERRCODE = 'invalid_parameter_value';
        END IF;
        -- Sur invitation, inviter à un classeur vaut invitation au serveur ;
        -- en entrée libre aussi. Rien à vérifier de plus.
    END IF;

    INSERT INTO lattis.invitations (email, workbook_id, roles, invited_by, expires_at)
    VALUES (p_email, p_workbook_id, p_roles, lattis.actor(),
            now() + make_interval(secs => (lattis.config('invitation_ttl') #>> '{}')::double precision))
    RETURNING id INTO v_id;

    IF NOT EXISTS (SELECT 1 FROM lattis.users WHERE email = p_email) THEN
        v_user := lattis.create_user(p_email, '', NULL, NULL);
    END IF;

    SELECT * INTO v_user FROM lattis.users
    WHERE email = p_email AND subject IS NOT NULL AND last_login_at IS NOT NULL AND NOT disabled
    ORDER BY last_login_at DESC LIMIT 1;
    IF FOUND THEN
        PERFORM lattis.apply_roles(v_user.id, p_workbook_id, p_roles);
        UPDATE lattis.invitations SET accepted_at = now() WHERE id = v_id;
    END IF;

    PERFORM lattis.audit_log('invite', v_id::text, NULL,
        jsonb_build_object('email', p_email, 'workbook_id', p_workbook_id, 'roles', p_roles));
    RETURN v_id;
END
$$;

-- Lecture --------------------------------------------------------------------

-- Classeurs accessibles à session_user (« Ouvrir depuis un serveur »).
CREATE FUNCTION lattis.my_workbooks() RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT coalesce(jsonb_agg(jsonb_build_object(
               'id', w.id, 'title', w.title, 'mode', w.mode, 'state', w.state, 'version', w.version,
               'schema', w.schema_name, 'staging_schema', CASE WHEN lattis.my_builtin_roles(w.id) && '{owner,designer}'
                                                              THEN w.staging_schema END,
               'roles', lattis.my_builtin_roles(w.id), 'published_at', w.published_at)
           ORDER BY w.title), '[]')
    FROM lattis.workbooks w
    WHERE cardinality(lattis.my_builtin_roles(w.id)) > 0
      AND (w.state <> 'draft' OR lattis.my_builtin_roles(w.id) && '{owner,designer}')
$$;

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
        'workbooks', lattis.my_workbooks())
$$;

-- Niveau effectif par tableau pour session_user (§9.2).
CREATE FUNCTION lattis.my_capabilities(p_workbook_id uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v       lattis.workbooks := lattis.workbook(p_workbook_id);
    v_roles text[] := lattis.my_builtin_roles(p_workbook_id);
    v_level text;
    v_out   jsonb;
BEGIN
    IF cardinality(v_roles) = 0 THEN
        RAISE EXCEPTION 'aucun accès à ce classeur' USING ERRCODE = 'insufficient_privilege';
    END IF;
    v_level := CASE
        WHEN v.mode = 'frozen' THEN 'reader'
        WHEN v_roles && '{owner,designer,editor}' THEN 'editor'
        WHEN 'participant' = ANY (v_roles) THEN 'participant'
        ELSE 'reader' END;
    EXECUTE format('SELECT coalesce(jsonb_object_agg(table_id, jsonb_build_object(''name'', name, ''level'', $1)), ''{}'')
                    FROM %I.lattis_meta_tables', v.schema_name)
        INTO v_out USING v_level;
    RETURN jsonb_build_object('workbook_id', v.id, 'mode', v.mode, 'version', v.version, 'schema', v.schema_name,
        'roles', v_roles, 'can_design', v_roles && '{owner,designer}', 'can_manage', 'owner' = ANY (v_roles),
        'tables', v_out, 'restrictions', '[]'::jsonb);
END
$$;

REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA lattis FROM PUBLIC;
GRANT EXECUTE ON FUNCTION
    lattis.actor(), lattis.is_server_owner(), lattis.login_summary(),
    lattis.set_config(text, jsonb),
    lattis.add_provider(text, text, text, text, text, text[]),
    lattis.update_provider(uuid, jsonb), lattis.remove_provider(uuid),
    lattis.allow_tenant(uuid, text, text, text), lattis.disallow_tenant(uuid),
    lattis.create_password_account(text, text), lattis.reset_password(uuid),
    lattis.disable_user(uuid), lattis.enable_user(uuid),
    lattis.invite(text, uuid, text[]), lattis.revoke_invitation(uuid),
    lattis.create_workbook(text, text), lattis.drop_workbook(uuid),
    lattis.create_table(uuid, text, jsonb), lattis.save_meta(uuid, text, jsonb),
    lattis.publish_script(uuid, text, text), lattis.compile_acl(uuid),
    lattis.finish_publish(uuid), lattis.republish(uuid), lattis.abort_publish(uuid),
    lattis.grant_role(uuid, uuid), lattis.revoke_role(uuid, uuid),
    lattis.my_workbooks(), lattis.my_capabilities(uuid)
TO PUBLIC;
