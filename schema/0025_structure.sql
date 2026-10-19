-- Opérations de structure (spécification §9.3, audit du 19/10 point I12) :
-- renommer ou supprimer un tableau, ajouter, renommer, supprimer une colonne
-- ou en changer le type, choisir la clé, poser ou retirer un index.
--
-- Comme create_table : concepteurs et propriétaires du classeur, verrou par
-- classeur, DDL au nom du propriétaire NOLOGIN, sur le brouillon, la version
-- de travail ou le classeur collaboratif publié (publish_target). Dans la
-- même transaction : droits recompilés (compile_acl), vues du Data Lake
-- refaites, pipelines et colonnes masquées suivis. Une colonne citée par un
-- filtre (rôle, restriction, audience) n'est ni renommée, ni supprimée, ni
-- convertie : le filtre est gardé en texte et ne suivrait pas.
--
-- Notifications : *meta (déclencheurs de lattis_meta_*), plus
-- *structure:<table_id> sur un classeur publié : le client recharge ce
-- tableau en entier (colonnes et valeurs ont pu changer).

-- Nom d'un tableau du schéma, ou erreur.
CREATE FUNCTION lattis.structure_table(p_schema text, p_table_id uuid) RETURNS text
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_name text;
BEGIN
    EXECUTE format('SELECT name FROM %I.lattis_meta_tables WHERE table_id = $1', p_schema) INTO v_name USING p_table_id;
    IF v_name IS NULL THEN
        RAISE EXCEPTION 'tableau introuvable dans ce classeur' USING ERRCODE = 'undefined_table';
    END IF;
    RETURN v_name;
END
$$;

-- Nom d'une colonne d'un tableau, ou erreur.
CREATE FUNCTION lattis.structure_column(p_schema text, p_table_id uuid, p_column_id uuid) RETURNS text
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_name text;
BEGIN
    EXECUTE format('SELECT name FROM %I.lattis_meta_columns WHERE table_id = $1 AND column_id = $2', p_schema)
        INTO v_name USING p_table_id, p_column_id;
    IF v_name IS NULL THEN
        RAISE EXCEPTION 'colonne introuvable dans ce tableau' USING ERRCODE = 'undefined_column';
    END IF;
    RETURN v_name;
END
$$;

-- Données du Data Lake tirées d'un tableau (classeur publié seulement).
CREATE FUNCTION lattis.table_datasets(v lattis.workbooks, p_schema text, p_table_id uuid) RETURNS lattis.datasets[]
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT coalesce(array_agg(d ORDER BY d.name), '{}') FROM lattis.datasets d
    WHERE v.state = 'published' AND p_schema = v.schema_name AND d.workbook_id = v.id AND d.table_id = p_table_id
$$;

-- Ce qui cite une colonne et empêche de la changer (NULL : rien). Les clés
-- de pipeline ne comptent que pour une suppression (un renommage les suit).
CREATE FUNCTION lattis.column_use(v lattis.workbooks, p_schema text, p_table_id uuid, p_name text, p_drop boolean) RETURNS text
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_re    text := '(^|[^[:alnum:]_])"?' || regexp_replace(p_name, '([^[:alnum:]_ ])', '\\\1', 'g') || '"?([^[:alnum:]_]|$)';
    v_sheet text;
    v_what  text;
BEGIN
    EXECUTE format('SELECT sheet_id FROM %I.lattis_meta_tables WHERE table_id = $1', p_schema) INTO v_sheet USING p_table_id;
    SELECT 'le filtre du rôle « ' || r.name || ' »' INTO v_what
    FROM lattis.role_grants g JOIN lattis.roles r ON r.id = g.role_id
    WHERE r.workbook_id = v.id
      AND ((g.node_kind = 'table' AND g.node_id = p_table_id::text) OR (g.node_kind = 'sheet' AND g.node_id = v_sheet)
           OR g.node_kind = 'workbook')
      AND (g.read_filter ~* v_re OR g.write_filter ~* v_re)
    ORDER BY r.name LIMIT 1;
    IF v_what IS NOT NULL THEN
        RETURN v_what;
    END IF;
    SELECT 'la restriction « ' || coalesce(nullif(label, ''), predicate) || ' »' INTO v_what
    FROM lattis.restrictions WHERE workbook_id = v.id AND table_id = p_table_id AND predicate ~* v_re LIMIT 1;
    IF v_what IS NOT NULL THEN
        RETURN v_what;
    END IF;
    SELECT 'l''audience de la donnée « ' || d.name || ' »' INTO v_what
    FROM unnest(lattis.table_datasets(v, p_schema, p_table_id)) d
    JOIN lattis.data_audience x ON x.dataset_id = d.id
    WHERE x.predicate ~* v_re LIMIT 1;
    IF v_what IS NOT NULL THEN
        RETURN v_what;
    END IF;
    SELECT 'l''audience du dossier « ' || f.name || ' »' INTO v_what
    FROM unnest(lattis.table_datasets(v, p_schema, p_table_id)) d
    JOIN lattis.data_folder_closure() c ON c.descendant = d.folder_id
    JOIN lattis.data_audience x ON x.folder_id = c.ancestor
    JOIN lattis.data_folders f ON f.id = x.folder_id
    WHERE d.inherit_audience AND x.predicate ~* v_re LIMIT 1;
    IF v_what IS NOT NULL THEN
        RETURN v_what;
    END IF;
    IF p_drop AND p_schema = v.schema_name THEN
        SELECT 'la clé du pipeline « ' || name || ' »' INTO v_what
        FROM lattis.pipelines WHERE workbook_id = v.id AND table_id = p_table_id AND p_name = ANY (key_columns) LIMIT 1;
    END IF;
    RETURN v_what;
END
$$;

-- Avant un changement de colonne : les vues du Data Lake qui la lisent
-- tombent (elles en dépendent) ; après : droits et vues recompilés,
-- clients avertis.
CREATE FUNCTION lattis.structure_before(v lattis.workbooks, p_schema text, p_table_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    d lattis.datasets;
BEGIN
    FOREACH d IN ARRAY lattis.table_datasets(v, p_schema, p_table_id) LOOP
        EXECUTE format('DROP VIEW IF EXISTS donnees.%I', d.sql_name);
    END LOOP;
END
$$;

CREATE FUNCTION lattis.structure_after(v lattis.workbooks, p_schema text, p_table_id uuid, p_action text, p_detail jsonb) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    d lattis.datasets;
BEGIN
    PERFORM lattis.compile_acl(v.id);
    FOREACH d IN ARRAY lattis.table_datasets(v, p_schema, p_table_id) LOOP
        PERFORM lattis.compile_dataset(d.id);
    END LOOP;
    IF v.state = 'published' AND p_schema = v.schema_name THEN
        PERFORM pg_notify('wb_' || v.slug, '*structure:' || p_table_id);
    END IF;
    PERFORM lattis.audit_log(p_action, p_table_id::text, NULL,
        jsonb_build_object('workbook_id', v.id, 'schema', p_schema) || coalesce(p_detail, '{}'));
END
$$;

REVOKE EXECUTE ON FUNCTION lattis.structure_table(text, uuid), lattis.structure_column(text, uuid, uuid),
    lattis.table_datasets(lattis.workbooks, text, uuid), lattis.column_use(lattis.workbooks, text, uuid, text, boolean),
    lattis.structure_before(lattis.workbooks, text, uuid),
    lattis.structure_after(lattis.workbooks, text, uuid, text, jsonb) FROM PUBLIC;

-- Tableaux --------------------------------------------------------------------------

CREATE FUNCTION lattis.rename_table(p_workbook_id uuid, p_table_id uuid, p_name text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v     lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', false);
    v_sch text;
    v_old text;
    v_dup boolean;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(p_workbook_id::text, 0));
    v_sch := lattis.publish_target(v);
    v_old := lattis.structure_table(v_sch, p_table_id);
    p_name := lattis.check_name(p_name, 'tableau');
    IF p_name = v_old THEN
        RETURN;
    END IF;
    EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.lattis_meta_tables WHERE lower(name) = lower($1) AND table_id <> $2)', v_sch)
        INTO v_dup USING p_name, p_table_id;
    IF v_dup THEN
        RAISE EXCEPTION 'un tableau « % » existe déjà dans ce classeur', p_name USING ERRCODE = 'duplicate_table';
    END IF;
    EXECUTE format('ALTER TABLE %I.%I RENAME TO %I', v_sch, v_old, p_name);
    EXECUTE format('UPDATE %I.lattis_meta_tables SET name = $1, lattis_xid = pg_current_xact_id(),
                        lattis_updated_by = session_user, lattis_updated_at = now() WHERE table_id = $2', v_sch)
        USING p_name, p_table_id;
    IF v_sch = v.schema_name THEN
        UPDATE lattis.pipelines SET table_name = p_name, updated_at = now() WHERE workbook_id = v.id AND table_id = p_table_id;
    END IF;
    PERFORM lattis.structure_after(v, v_sch, p_table_id, 'rename_table', jsonb_build_object('from', v_old, 'to', p_name));
END
$$;

CREATE FUNCTION lattis.drop_table(p_workbook_id uuid, p_table_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v      lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', false);
    v_sch  text;
    v_name text;
    v_uses text;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(p_workbook_id::text, 0));
    v_sch := lattis.publish_target(v);
    v_name := lattis.structure_table(v_sch, p_table_id);
    SELECT string_agg('« ' || d.name || ' »', ', ') INTO v_uses FROM unnest(lattis.table_datasets(v, v_sch, p_table_id)) d;
    IF v_uses IS NOT NULL THEN
        RAISE EXCEPTION 'le tableau « % » alimente le Data Lake (%) : retirez d''abord ces données', v_name, v_uses
            USING ERRCODE = 'dependent_objects_still_exist';
    END IF;
    IF v_sch = v.schema_name THEN
        SELECT string_agg('« ' || name || ' »', ', ') INTO v_uses FROM lattis.pipelines
        WHERE workbook_id = v.id AND table_id = p_table_id;
        IF v_uses IS NOT NULL THEN
            RAISE EXCEPTION 'le tableau « % » est alimenté par un pipeline (%) : supprimez-le d''abord', v_name, v_uses
                USING ERRCODE = 'dependent_objects_still_exist';
        END IF;
    END IF;
    EXECUTE format('DROP TABLE %I.%I', v_sch, v_name);
    EXECUTE format('DELETE FROM %I.lattis_meta_tables WHERE table_id = $1', v_sch) USING p_table_id;
    DELETE FROM lattis.role_grants g USING lattis.roles r
    WHERE r.id = g.role_id AND r.workbook_id = v.id AND g.node_kind = 'table' AND g.node_id = p_table_id::text;
    DELETE FROM lattis.restrictions WHERE workbook_id = v.id AND table_id = p_table_id;
    DELETE FROM lattis.acl_fingerprints WHERE workbook_id = v.id AND table_id = p_table_id;
    DELETE FROM lattis.tombstones WHERE workbook_id = v.id AND table_id = p_table_id;
    PERFORM lattis.structure_after(v, v_sch, p_table_id, 'drop_table', jsonb_build_object('name', v_name));
END
$$;

-- Colonnes --------------------------------------------------------------------------

-- p_after : colonne après laquelle placer la nouvelle (NULL : à la fin).
CREATE FUNCTION lattis.add_column(p_workbook_id uuid, p_table_id uuid, p_name text, p_type text,
                                  p_after uuid DEFAULT NULL, p_local_id text DEFAULT NULL) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v      lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', false);
    v_sch  text;
    v_tab  text;
    v_pg   text := lattis.pg_type(p_type);
    v_pos  integer;
    v_dup  boolean;
    v_cid  uuid := gen_random_uuid();
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(p_workbook_id::text, 0));
    v_sch := lattis.publish_target(v);
    v_tab := lattis.structure_table(v_sch, p_table_id);
    p_name := lattis.check_name(p_name, 'colonne');
    IF v_pg IS NULL THEN
        RAISE EXCEPTION 'colonne « % » : type inconnu « % » (text, integer, decimal, date, datetime, boolean)', p_name, p_type
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.lattis_meta_columns WHERE table_id = $1 AND lower(name) = lower($2))', v_sch)
        INTO v_dup USING p_table_id, p_name;
    IF v_dup OR lower(p_name) IN (SELECT attname FROM pg_attribute WHERE attrelid = format('%I.%I', v_sch, v_tab)::regclass
                                   AND attnum > 0 AND NOT attisdropped) THEN
        RAISE EXCEPTION 'le tableau « % » a déjà une colonne « % »', v_tab, p_name USING ERRCODE = 'duplicate_column';
    END IF;
    IF p_after IS NULL THEN
        EXECUTE format('SELECT coalesce(max(position), 0) + 1 FROM %I.lattis_meta_columns WHERE table_id = $1', v_sch)
            INTO v_pos USING p_table_id;
    ELSE
        PERFORM lattis.structure_column(v_sch, p_table_id, p_after);
        EXECUTE format('SELECT position + 1 FROM %I.lattis_meta_columns WHERE table_id = $1 AND column_id = $2', v_sch)
            INTO v_pos USING p_table_id, p_after;
        EXECUTE format('UPDATE %I.lattis_meta_columns SET position = position + 1 WHERE table_id = $1 AND position >= $2', v_sch)
            USING p_table_id, v_pos;
    END IF;
    PERFORM lattis.structure_before(v, v_sch, p_table_id);
    EXECUTE format('ALTER TABLE %I.%I ADD COLUMN %I %s', v_sch, v_tab, p_name, v_pg);
    EXECUTE format('INSERT INTO %I.lattis_meta_columns (table_id, column_id, name, local_id, type, position)
                    VALUES ($1, $2, $3, $4, $5, $6)', v_sch)
        USING p_table_id, v_cid, p_name, p_local_id, p_type, v_pos;
    PERFORM lattis.structure_after(v, v_sch, p_table_id, 'add_column',
        jsonb_build_object('column_id', v_cid, 'name', p_name, 'type', p_type));
    RETURN v_cid;
END
$$;

CREATE FUNCTION lattis.rename_column(p_workbook_id uuid, p_table_id uuid, p_column_id uuid, p_name text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v     lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', false);
    v_sch text;
    v_tab text;
    v_old text;
    v_use text;
    v_dup boolean;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(p_workbook_id::text, 0));
    v_sch := lattis.publish_target(v);
    v_tab := lattis.structure_table(v_sch, p_table_id);
    v_old := lattis.structure_column(v_sch, p_table_id, p_column_id);
    p_name := lattis.check_name(p_name, 'colonne');
    IF p_name = v_old THEN
        RETURN;
    END IF;
    EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.lattis_meta_columns WHERE table_id = $1 AND lower(name) = lower($2) AND column_id <> $3)', v_sch)
        INTO v_dup USING p_table_id, p_name, p_column_id;
    IF v_dup THEN
        RAISE EXCEPTION 'le tableau « % » a déjà une colonne « % »', v_tab, p_name USING ERRCODE = 'duplicate_column';
    END IF;
    v_use := lattis.column_use(v, v_sch, p_table_id, v_old, false);
    IF v_use IS NOT NULL THEN
        RAISE EXCEPTION 'la colonne « % » est citée par % : modifiez d''abord ce filtre', v_old, v_use
            USING ERRCODE = 'dependent_objects_still_exist';
    END IF;
    EXECUTE format('ALTER TABLE %I.%I RENAME COLUMN %I TO %I', v_sch, v_tab, v_old, p_name);
    EXECUTE format('UPDATE %I.lattis_meta_columns SET name = $1, lattis_xid = pg_current_xact_id(),
                        lattis_updated_by = session_user, lattis_updated_at = now() WHERE table_id = $2 AND column_id = $3', v_sch)
        USING p_name, p_table_id, p_column_id;
    IF v_sch = v.schema_name THEN
        UPDATE lattis.pipelines
           SET key_columns = array_replace(key_columns, v_old, p_name),
               column_map = CASE WHEN column_map ? v_old
                                 THEN (column_map - v_old) || jsonb_build_object(p_name, column_map -> v_old)
                                 ELSE column_map END,
               updated_at = now()
         WHERE workbook_id = v.id AND table_id = p_table_id;
        UPDATE lattis.datasets SET hidden_columns = array_replace(hidden_columns, v_old, p_name)
         WHERE workbook_id = v.id AND table_id = p_table_id;
    END IF;
    PERFORM lattis.structure_after(v, v_sch, p_table_id, 'rename_column',
        jsonb_build_object('column_id', p_column_id, 'from', v_old, 'to', p_name));
END
$$;

CREATE FUNCTION lattis.drop_column(p_workbook_id uuid, p_table_id uuid, p_column_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v     lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', false);
    v_sch text;
    v_tab text;
    v_col text;
    v_use text;
    v_pos integer;
    v_n   integer;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(p_workbook_id::text, 0));
    v_sch := lattis.publish_target(v);
    v_tab := lattis.structure_table(v_sch, p_table_id);
    v_col := lattis.structure_column(v_sch, p_table_id, p_column_id);
    EXECUTE format('SELECT count(*) FROM %I.lattis_meta_columns WHERE table_id = $1', v_sch) INTO v_n USING p_table_id;
    IF v_n <= 1 THEN
        RAISE EXCEPTION 'un tableau garde au moins une colonne : supprimez plutôt le tableau' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    v_use := lattis.column_use(v, v_sch, p_table_id, v_col, true);
    IF v_use IS NOT NULL THEN
        RAISE EXCEPTION 'la colonne « % » est citée par % : modifiez-le d''abord', v_col, v_use
            USING ERRCODE = 'dependent_objects_still_exist';
    END IF;
    PERFORM lattis.structure_before(v, v_sch, p_table_id);
    EXECUTE format('ALTER TABLE %I.%I DROP COLUMN %I', v_sch, v_tab, v_col);
    EXECUTE format('DELETE FROM %I.lattis_meta_columns WHERE table_id = $1 AND column_id = $2 RETURNING position', v_sch)
        INTO v_pos USING p_table_id, p_column_id;
    EXECUTE format('UPDATE %I.lattis_meta_columns SET position = position - 1 WHERE table_id = $1 AND position > $2', v_sch)
        USING p_table_id, v_pos;
    EXECUTE format('UPDATE %I.lattis_meta_tables SET key_column = NULL WHERE table_id = $1 AND key_column = $2', v_sch)
        USING p_table_id, p_column_id;
    IF v_sch = v.schema_name THEN
        UPDATE lattis.pipelines SET column_map = column_map - v_col, updated_at = now()
         WHERE workbook_id = v.id AND table_id = p_table_id AND column_map ? v_col;
        UPDATE lattis.datasets SET hidden_columns = array_remove(hidden_columns, v_col)
         WHERE workbook_id = v.id AND table_id = p_table_id;
    END IF;
    PERFORM lattis.structure_after(v, v_sch, p_table_id, 'drop_column',
        jsonb_build_object('column_id', p_column_id, 'name', v_col));
END
$$;

-- Conversion d'une colonne d'un type Lattis à un autre (expression USING).
CREATE FUNCTION lattis.convert_expr(p_col text, p_from text, p_to text) RETURNS text
LANGUAGE sql IMMUTABLE SET search_path = pg_catalog, pg_temp
AS $$
    SELECT CASE
        WHEN p_to = 'text' THEN format('%I::text', p_col)
        WHEN p_from = 'text' AND p_to = 'decimal' THEN format('replace(nullif(trim(%I), ''''), '','', ''.'')::numeric', p_col)
        WHEN p_from = 'text' AND p_to = 'integer' THEN format('round(replace(nullif(trim(%I), ''''), '','', ''.'')::numeric)::bigint', p_col)
        WHEN p_from = 'text' THEN format('nullif(trim(%I), '''')::%s', p_col, lattis.pg_type(p_to))
        WHEN p_from = 'decimal' AND p_to = 'integer' THEN format('round(%I)::bigint', p_col)
        WHEN p_from = 'boolean' AND p_to IN ('integer', 'decimal') THEN format('CASE WHEN %I THEN 1 WHEN NOT %I THEN 0 END::%s', p_col, p_col, lattis.pg_type(p_to))
        WHEN p_from IN ('integer', 'decimal') AND p_to = 'boolean' THEN format('(%I <> 0)', p_col)
        ELSE format('%I::%s', p_col, lattis.pg_type(p_to))
    END
$$;
REVOKE EXECUTE ON FUNCTION lattis.convert_expr(text, text, text) FROM PUBLIC;

CREATE FUNCTION lattis.alter_column_type(p_workbook_id uuid, p_table_id uuid, p_column_id uuid, p_type text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v      lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', false);
    v_sch  text;
    v_tab  text;
    v_col  text;
    v_from text;
    v_use  text;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(p_workbook_id::text, 0));
    v_sch := lattis.publish_target(v);
    v_tab := lattis.structure_table(v_sch, p_table_id);
    v_col := lattis.structure_column(v_sch, p_table_id, p_column_id);
    IF lattis.pg_type(p_type) IS NULL THEN
        RAISE EXCEPTION 'colonne « % » : type inconnu « % » (text, integer, decimal, date, datetime, boolean)', v_col, p_type
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    EXECUTE format('SELECT type FROM %I.lattis_meta_columns WHERE table_id = $1 AND column_id = $2', v_sch)
        INTO v_from USING p_table_id, p_column_id;
    IF v_from = p_type THEN
        RETURN;
    END IF;
    v_use := lattis.column_use(v, v_sch, p_table_id, v_col, false);
    IF v_use IS NOT NULL THEN
        RAISE EXCEPTION 'la colonne « % » est citée par % : modifiez d''abord ce filtre', v_col, v_use
            USING ERRCODE = 'dependent_objects_still_exist';
    END IF;
    PERFORM lattis.structure_before(v, v_sch, p_table_id);
    BEGIN
        EXECUTE format('ALTER TABLE %I.%I ALTER COLUMN %I TYPE %s USING %s', v_sch, v_tab, v_col,
                       lattis.pg_type(p_type), lattis.convert_expr(v_col, v_from, p_type));
    EXCEPTION WHEN data_exception OR cannot_coerce OR datatype_mismatch OR unique_violation THEN
        RAISE EXCEPTION 'colonne « % » : des valeurs ne se convertissent pas en % (%)', v_col, p_type, SQLERRM
            USING ERRCODE = 'invalid_parameter_value';
    END;
    EXECUTE format('UPDATE %I.lattis_meta_columns SET type = $1, lattis_xid = pg_current_xact_id(),
                        lattis_updated_by = session_user, lattis_updated_at = now() WHERE table_id = $2 AND column_id = $3', v_sch)
        USING p_type, p_table_id, p_column_id;
    PERFORM lattis.structure_after(v, v_sch, p_table_id, 'alter_column_type',
        jsonb_build_object('column_id', p_column_id, 'name', v_col, 'from', v_from, 'to', p_type));
END
$$;

-- Clé et index ----------------------------------------------------------------------

-- Clé du tableau : index unique sur la colonne (NULL : plus de clé). Une
-- valeur en double l'empêche. Voir §8.6 : une contrainte unique révèle
-- l'existence d'une ligne invisible.
CREATE FUNCTION lattis.set_key(p_workbook_id uuid, p_table_id uuid, p_column_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v     lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', false);
    v_sch text;
    v_tab text;
    v_col text;
    v_idx text := 'lattis_key_' || replace(p_table_id::text, '-', '');
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(p_workbook_id::text, 0));
    v_sch := lattis.publish_target(v);
    v_tab := lattis.structure_table(v_sch, p_table_id);
    EXECUTE format('DROP INDEX IF EXISTS %I.%I', v_sch, v_idx);
    IF p_column_id IS NOT NULL THEN
        v_col := lattis.structure_column(v_sch, p_table_id, p_column_id);
        BEGIN
            EXECUTE format('CREATE UNIQUE INDEX %I ON %I.%I (%I)', v_idx, v_sch, v_tab, v_col);
        EXCEPTION WHEN unique_violation THEN
            RAISE EXCEPTION 'la colonne « % » a des valeurs en double : elle ne peut pas servir de clé', v_col
                USING ERRCODE = 'unique_violation';
        END;
    END IF;
    EXECUTE format('UPDATE %I.lattis_meta_tables SET key_column = $1, lattis_xid = pg_current_xact_id(),
                        lattis_updated_by = session_user, lattis_updated_at = now() WHERE table_id = $2', v_sch)
        USING p_column_id, p_table_id;
    PERFORM lattis.audit_log('set_key', p_table_id::text, NULL,
        jsonb_build_object('workbook_id', v.id, 'schema', v_sch, 'column', v_col));
END
$$;

-- Index sur une ou plusieurs colonnes ; renvoie son nom (lattis_ix_…).
CREATE FUNCTION lattis.create_index(p_workbook_id uuid, p_table_id uuid, p_columns uuid[], p_unique boolean DEFAULT false) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v      lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', false);
    v_sch  text;
    v_tab  text;
    v_cid  uuid;
    v_cols text[] := '{}';
    v_idx  text;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(p_workbook_id::text, 0));
    v_sch := lattis.publish_target(v);
    v_tab := lattis.structure_table(v_sch, p_table_id);
    IF coalesce(cardinality(p_columns), 0) NOT BETWEEN 1 AND 8 THEN
        RAISE EXCEPTION 'index : de 1 à 8 colonnes' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    FOREACH v_cid IN ARRAY p_columns LOOP
        v_cols := v_cols || lattis.structure_column(v_sch, p_table_id, v_cid);
    END LOOP;
    v_idx := 'lattis_ix_' || left(md5(p_table_id::text || array_to_string(p_columns, ',') || coalesce(p_unique, false)::text), 20);
    BEGIN
        EXECUTE format('CREATE %s INDEX IF NOT EXISTS %I ON %I.%I (%s)', CASE WHEN p_unique THEN 'UNIQUE' ELSE '' END,
                       v_idx, v_sch, v_tab, (SELECT string_agg(quote_ident(c), ', ') FROM unnest(v_cols) c));
    EXCEPTION WHEN unique_violation THEN
        RAISE EXCEPTION 'index unique impossible : des valeurs en double (%)', array_to_string(v_cols, ', ')
            USING ERRCODE = 'unique_violation';
    END;
    PERFORM lattis.audit_log('create_index', p_table_id::text, NULL,
        jsonb_build_object('workbook_id', v.id, 'schema', v_sch, 'index', v_idx, 'columns', v_cols, 'unique', coalesce(p_unique, false)));
    RETURN v_idx;
END
$$;

CREATE FUNCTION lattis.drop_index(p_workbook_id uuid, p_table_id uuid, p_name text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v     lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', false);
    v_sch text;
    v_tab text;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(p_workbook_id::text, 0));
    v_sch := lattis.publish_target(v);
    v_tab := lattis.structure_table(v_sch, p_table_id);
    IF p_name NOT LIKE 'lattis\_ix\_%' OR NOT EXISTS (
        SELECT 1 FROM pg_indexes WHERE schemaname = v_sch AND tablename = v_tab AND indexname = p_name) THEN
        RAISE EXCEPTION 'index « % » introuvable sur ce tableau', p_name USING ERRCODE = 'undefined_object';
    END IF;
    EXECUTE format('DROP INDEX %I.%I', v_sch, p_name);
    PERFORM lattis.audit_log('drop_index', p_table_id::text, NULL,
        jsonb_build_object('workbook_id', v.id, 'schema', v_sch, 'index', p_name));
END
$$;

-- Index posés par create_index : [{"name", "unique", "columns": [noms]}].
CREATE FUNCTION lattis.table_indexes(p_workbook_id uuid, p_table_id uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v     lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', false);
    v_sch text := coalesce(v.staging_schema, v.schema_name);
    v_tab text := lattis.structure_table(v_sch, p_table_id);
BEGIN
    RETURN (SELECT coalesce(jsonb_agg(jsonb_build_object('name', c.relname, 'unique', i.indisunique,
                       'columns', (SELECT jsonb_agg(a.attname ORDER BY k.n) FROM unnest(i.indkey) WITH ORDINALITY k(attnum, n)
                                   JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum))
                   ORDER BY c.relname), '[]')
            FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
            WHERE i.indrelid = format('%I.%I', v_sch, v_tab)::regclass AND c.relname LIKE 'lattis\_ix\_%');
END
$$;

GRANT EXECUTE ON FUNCTION lattis.rename_table(uuid, uuid, text), lattis.drop_table(uuid, uuid),
    lattis.add_column(uuid, uuid, text, text, uuid, text), lattis.rename_column(uuid, uuid, uuid, text),
    lattis.drop_column(uuid, uuid, uuid), lattis.alter_column_type(uuid, uuid, uuid, text),
    lattis.set_key(uuid, uuid, uuid), lattis.create_index(uuid, uuid, uuid[], boolean),
    lattis.drop_index(uuid, uuid, text), lattis.table_indexes(uuid, uuid) TO PUBLIC;
