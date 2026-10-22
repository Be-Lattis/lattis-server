-- Sources poussées depuis l'application : le pipeline écrit sous le compte
-- de la personne qui pousse (son adresse dans le journal, lattis_updated_by),
-- et non sous un compte technique « pipeline+…@lattis.invalid ». Elle est
-- propriétaire du classeur technique de la source. Les pipelines créés dans
-- l'administration gardent leur identité technique.

-- Pipeline confié à une personne : l'identité technique est retirée.
CREATE FUNCTION lattis.pipeline_to_person(p_id uuid, p_user_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    x lattis.pipelines;
    t lattis.users;
    r record;
BEGIN
    SELECT * INTO x FROM lattis.pipelines WHERE id = p_id FOR UPDATE;
    SELECT * INTO t FROM lattis.users WHERE id = x.user_id;
    IF x.id IS NULL OR t.id = p_user_id THEN
        RETURN;
    END IF;
    UPDATE lattis.pipelines SET user_id = p_user_id, updated_at = now() WHERE id = p_id;
    IF t.email LIKE '%@lattis.invalid' THEN
        FOR r IN SELECT m.role_id FROM lattis.members m WHERE m.principal = t.id LOOP
            PERFORM lattis.revoke_role(r.role_id, t.id);
        END LOOP;
        UPDATE lattis.users SET disabled = true WHERE id = t.id;
        EXECUTE format('ALTER ROLE %I NOLOGIN', t.pg_role);
    END IF;
END
$$;

-- Suppression d'un pipeline : seule une identité technique est désactivée.
CREATE OR REPLACE FUNCTION lattis.drop_pipeline_as(p_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    x lattis.pipelines;
    t lattis.users;
    r record;
BEGIN
    SELECT * INTO x FROM lattis.pipelines WHERE id = p_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'pipeline inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    SELECT * INTO t FROM lattis.users WHERE id = x.user_id;
    DELETE FROM lattis.pipelines WHERE id = p_id;
    IF t.email LIKE '%@lattis.invalid' THEN
        FOR r IN SELECT m.role_id FROM lattis.members m WHERE m.principal = t.id LOOP
            PERFORM lattis.revoke_role(r.role_id, t.id);
        END LOOP;
        UPDATE lattis.users SET disabled = true WHERE id = t.id;
        EXECUTE format('ALTER ROLE %I NOLOGIN', t.pg_role);
    END IF;
    PERFORM lattis.audit_log('drop_pipeline', p_id::text, to_jsonb(x), NULL);
END
$$;

CREATE OR REPLACE FUNCTION lattis.create_pushed_source(p jsonb) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_name text := trim(coalesce(p ->> 'name', ''));
    v_mode text := coalesce(nullif(p ->> 'mode', ''), 'replace');
    v_me   uuid := (SELECT id FROM lattis.users WHERE pg_role = session_user);
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
        'source', jsonb_build_object('origin', coalesce(p -> 'origin', '{}'), 'pushed_by', v_me)));
    -- Les envois s'écrivent sous le compte de la personne (propriétaire du
    -- classeur technique), pas sous une identité technique.
    IF v_me IS NOT NULL THEN
        PERFORM lattis.pipeline_to_person(v_pid, v_me);
    END IF;
    v_ds := lattis.create_dataset(v_wb, v_tid, nullif(p ->> 'folder_id', '')::uuid, v_name, p ->> 'description',
                                  p ->> 'sql_name', '{}');
    RETURN jsonb_build_object('pipeline_id', v_pid, 'dataset_id', v_ds, 'workbook_id', v_wb, 'table_id', v_tid,
                              'sql_name', (SELECT sql_name FROM lattis.datasets WHERE id = v_ds));
END
$$;

-- Sources déjà poussées : confiées à la personne qui les a créées, si son
-- compte est actif.
DO $$
DECLARE
    x lattis.pipelines;
    v_user uuid;
BEGIN
    FOR x IN SELECT p.* FROM lattis.pipelines p JOIN lattis.users u ON u.id = p.user_id
             WHERE p.source_kind = 'push' AND u.email LIKE '%@lattis.invalid' LOOP
        SELECT id INTO v_user FROM lattis.users
        WHERE id = nullif(x.source ->> 'pushed_by', '')::uuid AND NOT disabled AND email NOT LIKE '%@lattis.invalid';
        IF v_user IS NOT NULL THEN
            PERFORM lattis.pipeline_to_person(x.id, v_user);
        END IF;
    END LOOP;
END
$$;

REVOKE EXECUTE ON FUNCTION lattis.pipeline_to_person(uuid, uuid) FROM PUBLIC;
