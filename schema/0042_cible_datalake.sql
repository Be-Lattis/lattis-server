-- Pipelines d'ingestion vers le Data Lake :
-- - « Créer automatiquement en fonction des données » : une donnée neuve du
--   Data Lake (classeur technique, tableau, donnée partagée dans un dossier),
--   dont les colonnes sont créées par le pipeline d'après la source à chaque
--   exécution (colonnes nouvelles ajoutées, types devinés) ;
-- - cible au choix parmi tous les tableaux des classeurs et toutes les
--   données du Data Lake (classeurs techniques des sources).

-- Donnée neuve du Data Lake (propriétaires du serveur) : p = name,
-- folder_id, description, key_columns (colonnes clés, créées en texte).
-- Sans clé, un tableau d'une colonne d'attente, retirée par la première
-- exécution du pipeline.
CREATE FUNCTION lattis.create_datalake_target(p jsonb) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_name text := trim(coalesce(p ->> 'name', ''));
    v_cols jsonb;
    v_wb   uuid;
    v_tid  uuid;
    v_ds   uuid;
BEGIN
    PERFORM lattis.require_server_owner();
    IF v_name = '' THEN
        RAISE EXCEPTION 'nom de la donnée manquant' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    SELECT coalesce(jsonb_agg(jsonb_build_object('name', k, 'type', 'text')), '[]') INTO v_cols
    FROM jsonb_array_elements_text(coalesce(p -> 'key_columns', '[]')) k;
    IF jsonb_array_length(v_cols) = 0 THEN
        v_cols := '[{"name": "Colonne en attente", "type": "text"}]';
    END IF;
    v_wb := lattis.create_workbook_as('Source : ' || v_name, 'collaborative', 'source');
    v_tid := lattis.create_table(v_wb, v_name, v_cols);
    PERFORM lattis.finish_publish(v_wb);
    v_ds := lattis.create_dataset(v_wb, v_tid, nullif(p ->> 'folder_id', '')::uuid, v_name, p ->> 'description', NULL, '{}');
    RETURN jsonb_build_object('workbook_id', v_wb, 'table_id', v_tid, 'dataset_id', v_ds);
END
$$;

-- Cibles : tableaux des classeurs collaboratifs publiés, et données du Data
-- Lake (classeurs techniques des sources), étiquetées par leur nom.
CREATE OR REPLACE FUNCTION lattis.pipeline_targets() RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    w      lattis.workbooks;
    t      record;
    v_out  jsonb := '[]';
    v_cols jsonb;
    v_ds   text;
BEGIN
    PERFORM lattis.require_server_owner();
    FOR w IN SELECT * FROM lattis.workbooks WHERE mode = 'collaborative' AND state = 'published' AND kind IN ('workbook', 'source')
             ORDER BY kind DESC, lower(title) LOOP
        FOR t IN SELECT * FROM lattis.data_tables(w.schema_name) LOOP
            EXECUTE format('SELECT coalesce(jsonb_agg(jsonb_build_object(''name'', name, ''type'', type) ORDER BY position), ''[]'')
                            FROM %I.lattis_meta_columns WHERE table_id = $1', w.schema_name) INTO v_cols USING t.table_id;
            SELECT string_agg(d.name, ', ' ORDER BY d.name) INTO v_ds FROM lattis.datasets d
            WHERE d.workbook_id = w.id AND d.table_id = t.table_id;
            IF w.kind = 'source' AND v_ds IS NULL THEN
                CONTINUE;
            END IF;
            v_out := v_out || jsonb_build_object('workbook_id', w.id, 'workbook', w.title, 'table_id', t.table_id,
                                                 'table', t.name, 'columns', v_cols, 'datalake', w.kind = 'source',
                                                 'dataset', v_ds);
        END LOOP;
    END LOOP;
    RETURN v_out;
END
$$;

GRANT EXECUTE ON FUNCTION lattis.create_datalake_target(jsonb) TO PUBLIC;
