-- Retours de l'application sur les opérations de structure :
--  * insert_column place la nouvelle colonne à une position donnée (1 : en
--    premier), add_column ne sachant que « après telle colonne » ou à la fin ;
--  * set_key émet aussi *structure:<table_id>.

CREATE OR REPLACE FUNCTION lattis.set_key(p_workbook_id uuid, p_table_id uuid, p_column_id uuid) RETURNS void
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
    IF v.state = 'published' AND v_sch = v.schema_name THEN
        PERFORM pg_notify('wb_' || v.slug, '*structure:' || p_table_id);
    END IF;
    PERFORM lattis.audit_log('set_key', p_table_id::text, NULL,
        jsonb_build_object('workbook_id', v.id, 'schema', v_sch, 'column', v_col));
END
$$;

-- p_position : 1 pour la première place ; au-delà du nombre de colonnes, à la fin.
CREATE FUNCTION lattis.insert_column(p_workbook_id uuid, p_table_id uuid, p_name text, p_type text,
                                     p_position integer, p_local_id text DEFAULT NULL) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_cid uuid := lattis.add_column(p_workbook_id, p_table_id, p_name, p_type, NULL, p_local_id);
    v     lattis.workbooks := lattis.workbook(p_workbook_id);
    v_sch text := lattis.publish_target(v);
    v_n   integer;
BEGIN
    EXECUTE format('SELECT count(*) FROM %I.lattis_meta_columns WHERE table_id = $1', v_sch) INTO v_n USING p_table_id;
    p_position := greatest(1, least(coalesce(p_position, v_n), v_n));
    IF p_position < v_n THEN
        EXECUTE format('UPDATE %I.lattis_meta_columns SET position = position + 1, lattis_xid = pg_current_xact_id()
                        WHERE table_id = $1 AND position >= $2 AND column_id <> $3', v_sch)
            USING p_table_id, p_position, v_cid;
        EXECUTE format('UPDATE %I.lattis_meta_columns SET position = $1, lattis_xid = pg_current_xact_id()
                        WHERE table_id = $2 AND column_id = $3', v_sch)
            USING p_position, p_table_id, v_cid;
    END IF;
    RETURN v_cid;
END
$$;
GRANT EXECUTE ON FUNCTION lattis.insert_column(uuid, uuid, text, text, integer, text) TO PUBLIC;
