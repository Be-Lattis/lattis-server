-- Corrections de l'audit du 19/10 (importants).
--
-- I5. Filtres (rôles, restrictions, audiences) : pas de sous-requête, sauf
--     « (SELECT lattis.my_teams()) ». Un filtre ne s'appuie que sur les
--     colonnes du tableau.
-- I6. Pipelines : une exécution à la fois par pipeline (running_since).
-- I7. Pipelines : un fichier envoyé en attente n'est pas écrasé ; un
--     pipeline « fichier envoyé » s'exécute à la demande seulement.

CREATE OR REPLACE FUNCTION lattis.check_predicate(p_schema text, p_table text, p_expr text) RETURNS text
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
    -- Pas de sous-requête (lecture d'autres tables, coût) hors « mes équipes ».
    IF regexp_replace(regexp_replace(p_expr, '''[^'']*''|"[^"]*"', '', 'g'),
                      '\(\s*select\s+lattis\.my_teams\(\s*\)\s*\)', '', 'gi') ~* '\m(select|from|with|union)\M' THEN
        RAISE EXCEPTION 'filtre refusé : sous-requête non permise (seule « (SELECT lattis.my_teams()) » l''est)'
            USING ERRCODE = 'invalid_parameter_value';
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

ALTER TABLE lattis.pipelines ADD COLUMN running_since timestamptz;
ALTER TABLE lattis.pipelines ADD CONSTRAINT pipelines_upload_manual CHECK (source_kind <> 'upload' OR schedule_kind = 'manual') NOT VALID;

CREATE OR REPLACE FUNCTION lattis.run_pipeline(p_id uuid, p_file text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_server_owner();
    IF p_file IS NOT NULL AND p_file !~ '^[A-Za-z0-9._-]{1,200}$' THEN
        RAISE EXCEPTION 'nom de fichier invalide' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF p_file IS NOT NULL AND EXISTS (SELECT 1 FROM lattis.pipelines WHERE id = p_id AND pending_file IS NOT NULL) THEN
        RAISE EXCEPTION 'un fichier attend déjà d''être traité : patientez la fin de son exécution' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    UPDATE lattis.pipelines SET next_run_at = now(), pending_file = p_file,
           pending_trigger = CASE WHEN p_file IS NULL THEN 'manual' ELSE 'upload' END
    WHERE id = p_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'pipeline inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    PERFORM lattis.audit_log('run_pipeline', p_id::text, NULL, jsonb_build_object('file', p_file));
END
$$;
