-- Historique de l'audience du Data Lake (idée 2 de l'audit du 19/10) : à
-- chaque recompilation d'une donnée, les rôles qui peuvent la lire sont
-- notés s'ils ont changé. On peut ainsi dire qui pouvait la lire à une date.

CREATE TABLE lattis.data_audience_history (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    dataset_id uuid NOT NULL REFERENCES lattis.datasets (id) ON DELETE CASCADE,
    at         timestamptz NOT NULL DEFAULT now(),
    roles      text[] NOT NULL,
    actor      text NOT NULL DEFAULT lattis.actor()
);
CREATE INDEX data_audience_history_recent ON lattis.data_audience_history (dataset_id, at DESC);
REVOKE ALL ON lattis.data_audience_history FROM PUBLIC;

CREATE FUNCTION lattis.record_audience(p_id uuid, p_roles text[]) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_new  text[] := ARRAY(SELECT DISTINCT r FROM unnest(coalesce(p_roles, '{}')) r ORDER BY r);
    v_last text[];
BEGIN
    SELECT roles INTO v_last FROM lattis.data_audience_history WHERE dataset_id = p_id ORDER BY at DESC, id DESC LIMIT 1;
    IF v_last IS DISTINCT FROM v_new THEN
        INSERT INTO lattis.data_audience_history (dataset_id, roles) VALUES (p_id, v_new);
    END IF;
END
$$;
REVOKE EXECUTE ON FUNCTION lattis.record_audience(uuid, text[]) FROM PUBLIC;

CREATE OR REPLACE FUNCTION lattis.compile_dataset(p_id uuid) RETURNS void
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
        PERFORM lattis.record_audience(d.id, '{}');
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
    PERFORM lattis.record_audience(d.id, v_roles);
    PERFORM pg_notify('lattis_data', d.id::text);
END
$$;

-- Historique lisible (personnes et équipes nommées), pour qui gère la donnée.
CREATE FUNCTION lattis.dataset_audience_history(p_id uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_manage_dataset(p_id);
    RETURN (SELECT coalesce(jsonb_agg(jsonb_build_object('at', h.at, 'actor', h.actor,
                'readers', (SELECT coalesce(jsonb_agg(coalesce(u.email, 'Équipe ' || t.name, r) ORDER BY 1), '[]')
                            FROM unnest(h.roles) r
                            LEFT JOIN lattis.users u ON u.pg_role = r
                            LEFT JOIN lattis.teams t ON t.pg_role = r))
             ORDER BY h.at DESC, h.id DESC), '[]')
            FROM (SELECT * FROM lattis.data_audience_history WHERE dataset_id = p_id ORDER BY at DESC, id DESC LIMIT 20) h);
END
$$;
GRANT EXECUTE ON FUNCTION lattis.dataset_audience_history(uuid) TO PUBLIC;

-- Point de départ pour les données existantes.
SELECT lattis.compile_dataset(id) FROM lattis.datasets;
