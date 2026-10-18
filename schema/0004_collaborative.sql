-- Classeurs collaboratifs (spécification §6, §8.1 à §8.3, §10).
--
-- Chaque tableau : RLS activée et forcée, déclencheurs qui tamponnent les
-- colonnes système, écrivent les pierres tombales et notifient le canal
-- wb_<slug>. compile_acl traduit les rôles fournis en GRANT et en règles.
--
-- Les règles et déclencheurs comparent à current_user (et non session_user)
-- pour que « Voir en tant que… » (SET ROLE, jalon 4) voie exactement ce que
-- voit l'utilisateur.

-- Déclencheurs ----------------------------------------------------------------

CREATE FUNCTION lattis.trg_stamp() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        NEW.lattis_author := current_user;
        NEW.lattis_pos := coalesce(NEW.lattis_pos, '');
    ELSE
        IF NEW.lattis_id IS DISTINCT FROM OLD.lattis_id THEN
            RAISE EXCEPTION 'lattis_id ne se modifie pas' USING ERRCODE = 'check_violation';
        END IF;
        NEW.lattis_author := OLD.lattis_author;
    END IF;
    NEW.lattis_xid := pg_current_xact_id();
    NEW.lattis_updated_by := current_user;
    NEW.lattis_updated_at := now();
    RETURN NEW;
END
$$;

-- Pierre tombale (table du catalogue, d'où SECURITY DEFINER).
-- TG_ARGV : identifiant du classeur, identifiant du tableau.
CREATE FUNCTION lattis.trg_tombstone() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    INSERT INTO lattis.tombstones (workbook_id, table_id, row_id, deleted_by)
    -- session_user : current_user est ici le propriétaire de la fonction.
    SELECT TG_ARGV[0]::uuid, TG_ARGV[1]::uuid, o.lattis_id, session_user FROM old_rows o
    ON CONFLICT DO NOTHING;
    RETURN NULL;
END
$$;

-- Une notification par transaction et par tableau : PostgreSQL fusionne les
-- notifications identiques d'une même transaction. Jamais de données (§8.6).
-- TG_ARGV : canal, charge (identifiant du tableau ou « *meta »).
CREATE FUNCTION lattis.trg_notify() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM pg_notify(TG_ARGV[0], TG_ARGV[1]);
    RETURN NULL;
END
$$;

GRANT EXECUTE ON FUNCTION lattis.trg_stamp(), lattis.trg_notify() TO PUBLIC;

-- Pose (ou remet) les déclencheurs d'un tableau ou d'une table de mise en page.
CREATE FUNCTION lattis.install_triggers(v lattis.workbooks, p_schema text, p_table text, p_table_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_chan text := 'wb_' || v.slug;
BEGIN
    IF p_table LIKE 'lattis\_meta\_%' THEN
        EXECUTE format('CREATE OR REPLACE TRIGGER lattis_notify AFTER INSERT OR UPDATE OR DELETE ON %I.%I
                        FOR EACH STATEMENT EXECUTE FUNCTION lattis.trg_notify(%L, %L)', p_schema, p_table, v_chan, '*meta');
        RETURN;
    END IF;
    EXECUTE format('CREATE OR REPLACE TRIGGER lattis_stamp BEFORE INSERT OR UPDATE ON %I.%I
                    FOR EACH ROW EXECUTE FUNCTION lattis.trg_stamp()', p_schema, p_table);
    EXECUTE format('CREATE OR REPLACE TRIGGER lattis_tombstone AFTER DELETE ON %I.%I
                    REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT
                    EXECUTE FUNCTION lattis.trg_tombstone(%L, %L)', p_schema, p_table, v.id, p_table_id);
    EXECUTE format('CREATE OR REPLACE TRIGGER lattis_notify AFTER INSERT OR UPDATE OR DELETE ON %I.%I
                    FOR EACH STATEMENT EXECUTE FUNCTION lattis.trg_notify(%L, %L)', p_schema, p_table, v_chan, p_table_id);
END
$$;

-- Droits ---------------------------------------------------------------------

-- Niveau effectif d'un rôle sur un tableau : tableau, sinon feuille, sinon
-- classeur (§8.1, héritage). Propriétaires et concepteurs : éditeurs partout.
CREATE FUNCTION lattis.effective_grant(p_role lattis.roles, p_table_id uuid, p_sheet_id text)
RETURNS lattis.role_grants
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    g lattis.role_grants;
BEGIN
    IF p_role.builtin IN ('owner', 'designer') THEN
        g.role_id := p_role.id;
        g.level := 'editor';
        RETURN g;
    END IF;
    SELECT * INTO g FROM lattis.role_grants
    WHERE role_id = p_role.id AND (
        (node_kind = 'table' AND node_id = p_table_id::text) OR
        (node_kind = 'sheet' AND node_id = p_sheet_id) OR
        (node_kind = 'workbook' AND node_id = p_role.workbook_id::text))
    ORDER BY CASE node_kind WHEN 'table' THEN 1 WHEN 'sheet' THEN 2 ELSE 3 END
    LIMIT 1;
    IF NOT FOUND THEN
        g.role_id := p_role.id;
        g.level := 'none';
    END IF;
    RETURN g;
END
$$;

-- Tableaux de données d'un schéma de classeur (hors mise en page).
CREATE FUNCTION lattis.data_tables(p_schema text)
RETURNS TABLE (table_id uuid, name text, sheet_id text)
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    RETURN QUERY EXECUTE format('SELECT table_id, name, sheet_id FROM %I.lattis_meta_tables ORDER BY position', p_schema);
END
$$;

-- Règles d'un tableau collaboratif pour chacun des rôles (§8.3).
CREATE FUNCTION lattis.compile_table(v lattis.workbooks, p_schema text, p_table text, p_table_id uuid, p_sheet_id text)
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
BEGIN
    EXECUTE format('ALTER TABLE %I.%I ENABLE ROW LEVEL SECURITY', p_schema, p_table);
    EXECUTE format('ALTER TABLE %I.%I FORCE ROW LEVEL SECURITY', p_schema, p_table);
    FOR v_pol IN SELECT policyname FROM pg_policies WHERE schemaname = p_schema AND tablename = p_table
                 AND policyname LIKE 'lattis\_%' LOOP
        EXECUTE format('DROP POLICY %I ON %I.%I', v_pol.policyname, p_schema, p_table);
    END LOOP;
    -- Le propriétaire NOLOGIN (et lattis_service à travers lui) voit tout :
    -- why_not et changes_since en ont besoin pour expliquer un refus.
    EXECUTE format('CREATE POLICY lattis_owner ON %I.%I TO %I USING (true) WITH CHECK (true)',
                   p_schema, p_table, 'wb_' || v.slug || '_owner');

    SELECT string_agg(quote_ident(attname), ', ' ORDER BY attnum) INTO v_cols
    FROM pg_attribute
    WHERE attrelid = format('%I.%I', p_schema, p_table)::regclass AND attnum > 0 AND NOT attisdropped
      AND attname NOT IN ('lattis_id', 'lattis_author');

    FOR v_role IN SELECT * FROM lattis.roles WHERE workbook_id = v.id ORDER BY pg_role LOOP
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
END
$$;

CREATE OR REPLACE FUNCTION lattis.compile_collaborative(p_workbook_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v     lattis.workbooks := lattis.workbook(p_workbook_id);
    v_tab record;
    v_role lattis.roles;
    v_meta text;
BEGIN
    FOR v_tab IN SELECT * FROM lattis.data_tables(v.schema_name) LOOP
        PERFORM lattis.compile_table(v, v.schema_name, v_tab.name, v_tab.table_id, v_tab.sheet_id);
    END LOOP;
    -- Mise en page : lue par tous les membres, écrite par les fonctions des concepteurs.
    FOREACH v_meta IN ARRAY ARRAY['lattis_meta_tables', 'lattis_meta_columns', 'lattis_meta_objects', 'lattis_meta_script_versions'] LOOP
        FOR v_role IN SELECT * FROM lattis.roles WHERE workbook_id = p_workbook_id LOOP
            EXECUTE format('REVOKE ALL ON %I.%I FROM %I', v.schema_name, v_meta, v_role.pg_role);
            EXECUTE format('GRANT SELECT ON %I.%I TO %I', v.schema_name, v_meta, v_role.pg_role);
        END LOOP;
    END LOOP;
END
$$;

-- compile_acl : pose aussi les déclencheurs (tous modes, toutes versions).
CREATE OR REPLACE FUNCTION lattis.compile_acl(p_workbook_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v      lattis.workbooks := lattis.workbook(p_workbook_id);
    v_sch  text;
    v_tab  record;
    v_role lattis.roles;
    v_publishers text;
    v_meta text;
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
        FOREACH v_meta IN ARRAY ARRAY['lattis_meta_tables', 'lattis_meta_columns', 'lattis_meta_objects', 'lattis_meta_script_versions'] LOOP
            PERFORM lattis.install_triggers(v, v_sch, v_meta, NULL);
        END LOOP;
        FOR v_tab IN SELECT * FROM lattis.data_tables(v_sch) LOOP
            PERFORM lattis.install_triggers(v, v_sch, v_tab.name, v_tab.table_id);
        END LOOP;
        IF v.mode = 'collaborative' AND v.state = 'published' AND v_sch = v.schema_name THEN
            CONTINUE;
        END IF;
        FOR v_tab IN SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
                     WHERE n.nspname = v_sch AND c.relkind IN ('r', 'p') LOOP
            EXECUTE format('REVOKE ALL ON %I.%I FROM PUBLIC', v_sch, v_tab.relname);
            FOR v_role IN SELECT * FROM lattis.roles WHERE workbook_id = p_workbook_id LOOP
                EXECUTE format('REVOKE ALL ON %I.%I FROM %I', v_sch, v_tab.relname, v_role.pg_role);
            END LOOP;
            IF (v.state = 'draft' AND v_sch = v.schema_name) OR v_sch = v.staging_schema THEN
                IF v_tab.relname LIKE 'lattis\_meta\_%' THEN
                    EXECUTE format('GRANT SELECT ON %I.%I TO %s', v_sch, v_tab.relname, v_publishers);
                ELSE
                    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I.%I TO %s', v_sch, v_tab.relname, v_publishers);
                END IF;
            ELSIF v.mode = 'frozen' THEN
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

-- Synchronisation (§10) -------------------------------------------------------

-- Schéma et nom d'un tableau, pour un membre du classeur.
CREATE FUNCTION lattis.table_ref(p_workbook_id uuid, p_table_id uuid, OUT schema_name text, OUT table_name text)
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v lattis.workbooks := lattis.workbook(p_workbook_id);
BEGIN
    IF cardinality(lattis.my_builtin_roles(p_workbook_id)) = 0 AND session_user <> 'lattis_service' THEN
        RAISE EXCEPTION 'aucun accès à ce classeur' USING ERRCODE = 'insufficient_privilege';
    END IF;
    schema_name := v.schema_name;
    EXECUTE format('SELECT name FROM %I.lattis_meta_tables WHERE table_id = $1', v.schema_name)
        INTO table_name USING p_table_id;
    IF table_name IS NULL THEN
        RAISE EXCEPTION 'tableau inconnu' USING ERRCODE = 'no_data_found';
    END IF;
END
$$;

CREATE FUNCTION lattis.tombstones_since(p_workbook_id uuid, p_table_id uuid, p_since xid8)
RETURNS TABLE (row_id uuid, xid xid8)
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.table_ref(p_workbook_id, p_table_id);
    RETURN QUERY SELECT t.row_id, t.xid FROM lattis.tombstones t
                 WHERE t.table_id = p_table_id AND t.xid >= p_since;
END
$$;

-- Point de reprise : à lire dans la même transaction (REPEATABLE READ) que
-- les deltas, et à mémoriser comme prochain « since ». Toute transaction
-- d'identifiant inférieur est validée et visible.
CREATE FUNCTION lattis.sync_point() RETURNS xid8
LANGUAGE sql STABLE SET search_path = pg_catalog, pg_temp
AS $$
    SELECT pg_snapshot_xmin(pg_current_snapshot())
$$;

-- Lignes modifiées et supprimées depuis p_since, filtrées par la RLS de
-- l'appelant (SECURITY INVOKER). Une ligne peut revenir deux fois d'un appel
-- à l'autre : l'application est idempotente côté client.
CREATE FUNCTION lattis.changes_since(p_workbook_id uuid, p_table_id uuid, p_since xid8)
RETURNS TABLE (row_data jsonb, deleted boolean, xid xid8)
LANGUAGE plpgsql STABLE SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    r record;
BEGIN
    r := lattis.table_ref(p_workbook_id, p_table_id);
    RETURN QUERY EXECUTE format(
        'SELECT to_jsonb(t), false, t.lattis_xid FROM %I.%I t WHERE t.lattis_xid >= $1', r.schema_name, r.table_name)
        USING p_since;
    RETURN QUERY SELECT jsonb_build_object('lattis_id', s.row_id), true, s.xid
                 FROM lattis.tombstones_since(p_workbook_id, p_table_id, p_since) s;
END
$$;

-- État d'une ligne vu par le propriétaire (pour why_not).
CREATE FUNCTION lattis.row_state(p_workbook_id uuid, p_table_id uuid, p_row_id uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    r     record;
    v_row jsonb;
    v_ts  lattis.tombstones;
BEGIN
    r := lattis.table_ref(p_workbook_id, p_table_id);
    EXECUTE format('SELECT to_jsonb(t) FROM %I.%I t WHERE lattis_id = $1', r.schema_name, r.table_name)
        INTO v_row USING p_row_id;
    IF v_row IS NOT NULL THEN
        RETURN jsonb_build_object('exists', true, 'author', v_row ->> 'lattis_author',
                                  'updated_by', v_row ->> 'lattis_updated_by', 'updated_at', v_row -> 'lattis_updated_at');
    END IF;
    SELECT * INTO v_ts FROM lattis.tombstones WHERE table_id = p_table_id AND row_id = p_row_id ORDER BY xid DESC LIMIT 1;
    IF FOUND THEN
        RETURN jsonb_build_object('exists', false, 'deleted_by', v_ts.deleted_by, 'deleted_at', v_ts.deleted_at);
    END IF;
    RETURN jsonb_build_object('exists', false);
END
$$;

-- Pourquoi une écriture n'a touché aucune ligne (§10) :
--   {"reason": "modified", "row": {...valeur actuelle...}, "updated_by", "updated_at"}
--   {"reason": "deleted", "deleted_by", "deleted_at"}
--   {"reason": "forbidden"}       (ligne invisible ou non modifiable)
--   {"reason": "missing"}         (jamais existé)
CREATE FUNCTION lattis.why_not(p_workbook_id uuid, p_table_id uuid, p_row_id uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    r       record;
    v_state jsonb := lattis.row_state(p_workbook_id, p_table_id, p_row_id);
    v_row   jsonb;
    v_level text;
BEGIN
    IF NOT (v_state ->> 'exists')::boolean THEN
        IF v_state ? 'deleted_by' THEN
            RETURN jsonb_build_object('reason', 'deleted') || (v_state - 'exists');
        END IF;
        RETURN jsonb_build_object('reason', 'missing');
    END IF;
    r := lattis.table_ref(p_workbook_id, p_table_id);
    EXECUTE format('SELECT to_jsonb(t) FROM %I.%I t WHERE lattis_id = $1', r.schema_name, r.table_name)
        INTO v_row USING p_row_id;
    IF v_row IS NULL THEN
        RETURN jsonb_build_object('reason', 'forbidden');
    END IF;
    v_level := lattis.my_capabilities(p_workbook_id) #>> ARRAY['tables', p_table_id::text, 'level'];
    IF v_level = 'reader' OR v_level IS NULL
       OR (v_level = 'participant' AND v_row ->> 'lattis_author' IS DISTINCT FROM current_user) THEN
        RETURN jsonb_build_object('reason', 'forbidden');
    END IF;
    RETURN jsonb_build_object('reason', 'modified', 'row', v_row,
                              'updated_by', v_row ->> 'lattis_updated_by', 'updated_at', v_row -> 'lattis_updated_at');
END
$$;

-- Purge des pierres tombales (lattisd, périodiquement).
CREATE FUNCTION lattis.purge_tombstones() RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_n integer;
BEGIN
    PERFORM lattis.require_service();
    DELETE FROM lattis.tombstones
    WHERE deleted_at < now() - make_interval(secs => (lattis.config('tombstone_retention') #>> '{}')::double precision);
    GET DIAGNOSTICS v_n = ROW_COUNT;
    RETURN v_n;
END
$$;

-- Capacités (§9.2) : niveau effectif par tableau, union des rôles.
CREATE OR REPLACE FUNCTION lattis.my_capabilities(p_workbook_id uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v       lattis.workbooks := lattis.workbook(p_workbook_id);
    v_roles text[] := lattis.my_builtin_roles(p_workbook_id);
    v_tab   record;
    v_role  lattis.roles;
    g       lattis.role_grants;
    v_best  text;
    v_rank  integer;
    v_out   jsonb := '{}';
BEGIN
    IF cardinality(v_roles) = 0 THEN
        RAISE EXCEPTION 'aucun accès à ce classeur' USING ERRCODE = 'insufficient_privilege';
    END IF;
    FOR v_tab IN SELECT * FROM lattis.data_tables(v.schema_name) LOOP
        IF v.mode = 'frozen' THEN
            v_best := 'reader';
        ELSE
            v_rank := 0;
            v_best := 'none';
            FOR v_role IN SELECT * FROM lattis.roles
                          WHERE workbook_id = p_workbook_id AND pg_has_role(session_user, pg_role, 'MEMBER') LOOP
                g := lattis.effective_grant(v_role, v_tab.table_id, v_tab.sheet_id);
                IF array_position(ARRAY['none', 'reader', 'participant', 'editor'], g.level) > v_rank THEN
                    v_rank := array_position(ARRAY['none', 'reader', 'participant', 'editor'], g.level);
                    v_best := g.level;
                END IF;
            END LOOP;
        END IF;
        v_out := v_out || jsonb_build_object(v_tab.table_id::text, jsonb_build_object('name', v_tab.name, 'level', v_best));
    END LOOP;
    RETURN jsonb_build_object('workbook_id', v.id, 'mode', v.mode, 'version', v.version, 'schema', v.schema_name,
        'channel', 'wb_' || v.slug, 'roles', v_roles, 'can_design', v_roles && '{owner,designer}',
        'can_manage', 'owner' = ANY (v_roles), 'tables', v_out, 'restrictions', '[]'::jsonb);
END
$$;

GRANT EXECUTE ON FUNCTION
    lattis.sync_point(), lattis.changes_since(uuid, uuid, xid8), lattis.why_not(uuid, uuid, uuid),
    lattis.table_ref(uuid, uuid), lattis.tombstones_since(uuid, uuid, xid8), lattis.row_state(uuid, uuid, uuid)
TO PUBLIC;
