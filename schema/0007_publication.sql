-- Publication (retours de l'application) :
--  * une republication n'avertit plus les lecteurs avant la bascule : la
--    version de travail n'a pas de déclencheur de notification (finish_publish
--    recompile les droits et les pose sur la nouvelle version) ;
--  * my_capabilities donne le titre du classeur.

-- Pose (ou remet) les déclencheurs d'un tableau ou d'une table de mise en page.
CREATE OR REPLACE FUNCTION lattis.install_triggers(v lattis.workbooks, p_schema text, p_table text, p_table_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_chan text := 'wb_' || v.slug;
BEGIN
    IF p_table LIKE 'lattis\_meta\_%' THEN
        IF p_schema IS NOT DISTINCT FROM v.staging_schema THEN
            EXECUTE format('DROP TRIGGER IF EXISTS lattis_notify ON %I.%I', p_schema, p_table);
            RETURN;
        END IF;
        EXECUTE format('CREATE OR REPLACE TRIGGER lattis_notify AFTER INSERT OR UPDATE OR DELETE ON %I.%I
                        FOR EACH STATEMENT EXECUTE FUNCTION lattis.trg_notify(%L, %L)', p_schema, p_table, v_chan, '*meta');
        RETURN;
    END IF;
    EXECUTE format('CREATE OR REPLACE TRIGGER lattis_stamp BEFORE INSERT OR UPDATE ON %I.%I
                    FOR EACH ROW EXECUTE FUNCTION lattis.trg_stamp()', p_schema, p_table);
    EXECUTE format('CREATE OR REPLACE TRIGGER lattis_tombstone AFTER DELETE ON %I.%I
                    REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT
                    EXECUTE FUNCTION lattis.trg_tombstone(%L, %L)', p_schema, p_table, v.id, p_table_id);
    IF p_schema IS NOT DISTINCT FROM v.staging_schema THEN
        EXECUTE format('DROP TRIGGER IF EXISTS lattis_notify ON %I.%I', p_schema, p_table);
        RETURN;
    END IF;
    EXECUTE format('CREATE OR REPLACE TRIGGER lattis_notify AFTER INSERT OR UPDATE OR DELETE ON %I.%I
                    FOR EACH STATEMENT EXECUTE FUNCTION lattis.trg_notify(%L, %L)', p_schema, p_table, v_chan, p_table_id);
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
    RETURN jsonb_build_object('workbook_id', v.id, 'title', v.title, 'mode', v.mode, 'version', v.version, 'schema', v.schema_name,
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
