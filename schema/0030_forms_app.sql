-- Formulaires : accord avec le lot 1 de l'application (lattis-app 6dbe054).
--  * champ position {"kind": "position", "latitude", "longitude"} (deux
--    colonnes décimales) ou {"kind": "position", "column"} (texte « lat, lon ») ;
--  * couleur « slate » (« gray » reste accepté) ;
--  * audience donnée en identifiants ou en objets {"principal"} ;
--  * create_form et list_forms rendent l'adresse complète (url) ; list_forms
--    rend aussi l'audience et le nombre de réponses par personne ;
--  * durée de modification « 7 days » acceptée.

ALTER TABLE lattis.forms DROP CONSTRAINT forms_edit_check,
    ADD CONSTRAINT forms_edit_check CHECK (edit ~ '^(none|until_close|[1-9][0-9]{0,3} ?(d|days?|jours?))$');

-- Adresse publique d'un formulaire (émetteur réglé dans PostgreSQL), sinon le chemin.
CREATE FUNCTION lattis.form_url(p_code text) RETURNS text
LANGUAGE sql STABLE SET search_path = pg_catalog, pg_temp
AS $$
    SELECT coalesce(rtrim(nullif(current_setting('lattis_oauth.issuer', true), ''), '/'), '') || '/f/' || p_code
$$;

CREATE OR REPLACE FUNCTION lattis.form_check(v lattis.workbooks, p_table_id uuid, d jsonb) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_cols   jsonb := lattis.form_columns(v.schema_name, p_table_id);
    v_err    jsonb := '{}';
    a        jsonb;
    f        jsonb;
    s        jsonb;
    i        integer := 0;
    v_col    jsonb;
    v_type   text;
    v_used   text[] := '{}';
    v_params text[] := '{}';
    v_widget text;
    v_list   jsonb;
    v_src    record;
BEGIN
    IF jsonb_typeof(d) IS DISTINCT FROM 'object' THEN
        RETURN jsonb_build_object('definition', 'objet attendu');
    END IF;
    IF coalesce(d ->> 'format', '1') <> '1' THEN
        v_err := v_err || jsonb_build_object('format', 'version inconnue');
    END IF;
    IF coalesce(d ->> 'lang', 'fr') NOT IN ('fr', 'en') THEN
        v_err := v_err || jsonb_build_object('lang', 'fr ou en');
    END IF;
    a := coalesce(d -> 'appearance', '{}');
    IF jsonb_typeof(a) <> 'object' THEN
        v_err := v_err || jsonb_build_object('appearance', 'objet attendu');
        a := '{}';
    END IF;
    IF length(trim(coalesce(a ->> 'title', ''))) NOT BETWEEN 1 AND 200 THEN
        v_err := v_err || jsonb_build_object('appearance.title', 'titre de 1 à 200 caractères');
    END IF;
    IF a ? 'accent' AND jsonb_typeof(a -> 'accent') <> 'null'
       AND (a ->> 'accent') NOT IN ('green', 'teal', 'blue', 'indigo', 'purple', 'red', 'orange', 'slate', 'gray') THEN
        v_err := v_err || jsonb_build_object('appearance.accent', 'couleur de la palette attendue');
    END IF;
    IF length(coalesce(a ->> 'intro', '') || coalesce(a ->> 'thanks', '') || coalesce(a ->> 'privacy', '')
              || coalesce(a ->> 'closed_message', '')) > 20000 THEN
        v_err := v_err || jsonb_build_object('appearance', 'textes trop longs');
    END IF;
    IF jsonb_typeof(coalesce(d -> 'fields', '[]')) <> 'array' OR jsonb_array_length(coalesce(d -> 'fields', '[]')) > 200 THEN
        RETURN v_err || jsonb_build_object('fields', 'liste de 200 champs au plus');
    END IF;
    FOR f IN SELECT * FROM jsonb_array_elements(coalesce(d -> 'fields', '[]')) LOOP
        i := i + 1;
        IF f ->> 'kind' = 'position' AND f ? 'latitude' THEN
            IF (v_cols -> (f ->> 'latitude') ->> 'type') IS DISTINCT FROM 'decimal'
               OR (v_cols -> (f ->> 'longitude') ->> 'type') IS DISTINCT FROM 'decimal' THEN
                v_err := v_err || jsonb_build_object('fields[' || i || ']', 'position : colonnes Latitude et Longitude décimales attendues');
            ELSIF (f ->> 'latitude') = ANY (v_used) OR (f ->> 'longitude') = ANY (v_used) THEN
                v_err := v_err || jsonb_build_object('fields[' || i || ']', 'colonne déjà dans le formulaire');
            END IF;
            v_used := v_used || (f ->> 'latitude') || (f ->> 'longitude');
            CONTINUE;
        END IF;
        IF f ->> 'kind' = 'section' THEN
            IF coalesce(f ->> 'title', '') = '' THEN
                v_err := v_err || jsonb_build_object('fields[' || i || '].title', 'titre de section manquant');
            END IF;
            CONTINUE;
        END IF;
        v_col := v_cols -> (f ->> 'column');
        IF v_col IS NULL THEN
            v_err := v_err || jsonb_build_object('fields[' || i || '].column', 'colonne inconnue');
            CONTINUE;
        END IF;
        IF (f ->> 'column') = ANY (v_used) THEN
            v_err := v_err || jsonb_build_object(f ->> 'column', 'colonne déjà dans le formulaire');
        END IF;
        v_used := v_used || (f ->> 'column');
        v_type := v_col ->> 'type';
        v_widget := CASE WHEN f ->> 'kind' = 'position' THEN 'position' ELSE coalesce(f ->> 'widget', '') END;
        IF NOT (
            (v_widget IN ('', 'list') ) OR
            (v_widget IN ('text', 'textarea') AND v_type = 'text') OR
            (v_widget = 'number' AND v_type IN ('integer', 'decimal')) OR
            (v_widget = 'date' AND v_type = 'date') OR
            (v_widget = 'datetime' AND v_type = 'datetime') OR
            (v_widget IN ('checkbox', 'yesno') AND v_type = 'boolean') OR
            (v_widget = 'position' AND v_type = 'text') OR
            (v_widget = 'position' AND v_type = 'decimal' AND f ? 'lon_column')) THEN
            v_err := v_err || jsonb_build_object(f ->> 'column', 'type de champ « ' || v_widget || ' » impossible sur une colonne ' || v_type);
        END IF;
        IF v_widget = 'position' THEN
            IF v_type = 'decimal' AND (f ->> 'lon_column' IS NULL OR (v_cols -> (f ->> 'lon_column') ->> 'type') IS DISTINCT FROM 'decimal') THEN
                v_err := v_err || jsonb_build_object(f ->> 'column', 'position : colonne de longitude (décimal) manquante');
            ELSIF f ->> 'lon_column' IS NOT NULL THEN
                IF (f ->> 'lon_column') = ANY (v_used) THEN
                    v_err := v_err || jsonb_build_object(f ->> 'lon_column', 'colonne déjà dans le formulaire');
                END IF;
                v_used := v_used || (f ->> 'lon_column');
            END IF;
        END IF;
        IF f ? 'format' AND jsonb_typeof(f -> 'format') <> 'null' AND (f ->> 'format') NOT IN ('free', 'email', 'phone', 'url') THEN
            v_err := v_err || jsonb_build_object(f ->> 'column', 'format : free, email, phone ou url');
        END IF;
        IF f ? 'max_length' AND jsonb_typeof(f -> 'max_length') <> 'null'
           AND (jsonb_typeof(f -> 'max_length') <> 'number' OR (f ->> 'max_length')::numeric NOT BETWEEN 1 AND 10000) THEN
            v_err := v_err || jsonb_build_object(f ->> 'column', 'longueur maximale de 1 à 10 000');
        END IF;
        IF f ? 'param' AND jsonb_typeof(f -> 'param') <> 'null' THEN
            IF (f ->> 'param') !~ '^[a-z][a-z0-9_]{0,39}$' THEN
                v_err := v_err || jsonb_build_object(f ->> 'column', 'nom court : lettres minuscules, chiffres et _');
            ELSIF (f ->> 'param') = ANY (v_params) THEN
                v_err := v_err || jsonb_build_object(f ->> 'column', 'nom court déjà pris');
            END IF;
            v_params := v_params || (f ->> 'param');
        END IF;
        IF f ? 'default' AND jsonb_typeof(f -> 'default') = 'object' AND f -> 'default' ? 'token'
           AND (f -> 'default' ->> 'token') NOT IN ('today', 'now', 'year', 'respondent') THEN
            v_err := v_err || jsonb_build_object(f ->> 'column', 'valeur par défaut : today, now, year ou respondent');
        END IF;
        v_list := f -> 'list';
        IF v_list IS NOT NULL AND jsonb_typeof(v_list) = 'object' THEN
            IF v_list ? 'values' THEN
                IF jsonb_typeof(v_list -> 'values') <> 'array' OR jsonb_array_length(v_list -> 'values') NOT BETWEEN 1 AND 1000 THEN
                    v_err := v_err || jsonb_build_object(f ->> 'column', 'liste : de 1 à 1 000 valeurs');
                END IF;
            ELSIF v_list ? 'table_id' THEN
                BEGIN
                    EXECUTE format('SELECT t.name, c.name AS col FROM %I.lattis_meta_tables t JOIN %I.lattis_meta_columns c
                                    ON c.table_id = t.table_id WHERE t.table_id = $1 AND c.column_id = $2', v.schema_name, v.schema_name)
                        INTO v_src USING (v_list ->> 'table_id')::uuid, (v_list ->> 'column')::uuid;
                EXCEPTION WHEN invalid_text_representation THEN
                    v_src := NULL;
                END;
                IF v_src.name IS NULL THEN
                    v_err := v_err || jsonb_build_object(f ->> 'column', 'liste : tableau ou colonne source inconnu');
                END IF;
            ELSE
                v_err := v_err || jsonb_build_object(f ->> 'column', 'liste : values ou table_id attendu');
            END IF;
        END IF;
    END LOOP;
    i := 0;
    FOR s IN SELECT * FROM jsonb_array_elements(coalesce(d -> 'server_values', '[]')) LOOP
        i := i + 1;
        v_col := v_cols -> (s ->> 'column');
        IF v_col IS NULL THEN
            v_err := v_err || jsonb_build_object('server_values[' || i || '].column', 'colonne inconnue');
            CONTINUE;
        END IF;
        IF (s ->> 'column') = ANY (v_used) THEN
            v_err := v_err || jsonb_build_object(s ->> 'column', 'colonne déjà dans le formulaire');
        END IF;
        v_used := v_used || (s ->> 'column');
        IF s -> 'value' ? 'token' THEN
            IF (s -> 'value' ->> 'token') NOT IN ('received_at', 'respondent', 'form_title', 'next_number') THEN
                v_err := v_err || jsonb_build_object(s ->> 'column', 'valeur du serveur : received_at, respondent, form_title ou next_number');
            ELSIF (s -> 'value' ->> 'token') = 'next_number' AND (v_col ->> 'type') NOT IN ('integer', 'decimal') THEN
                v_err := v_err || jsonb_build_object(s ->> 'column', 'numérotation : colonne entière attendue');
            ELSIF (s -> 'value' ->> 'token') = 'received_at' AND (v_col ->> 'type') NOT IN ('datetime', 'date', 'text') THEN
                v_err := v_err || jsonb_build_object(s ->> 'column', 'date de réception : colonne date et heure attendue');
            END IF;
        ELSIF NOT (s -> 'value' ? 'fixed') THEN
            v_err := v_err || jsonb_build_object(s ->> 'column', 'valeur du serveur : token ou fixed attendu');
        END IF;
    END LOOP;
    RETURN v_err;
END
$$;

CREATE OR REPLACE FUNCTION lattis.form_apply_options(p_id uuid, p jsonb) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_key text;
BEGIN
    FOR v_key IN SELECT jsonb_object_keys(p) LOOP
        IF v_key NOT IN ('definition', 'access', 'audience', 'opens_at', 'closes_at', 'max_responses', 'max_per_respondent',
                         'edit', 'daily_limit') THEN
            RAISE EXCEPTION 'champ inconnu : %', v_key USING ERRCODE = 'invalid_parameter_value';
        END IF;
    END LOOP;
    IF p ? 'access' THEN
        PERFORM lattis.form_check_access(p ->> 'access');
    END IF;
    UPDATE lattis.forms SET
        access             = CASE WHEN p ? 'access' THEN p ->> 'access' ELSE access END,
        opens_at           = CASE WHEN p ? 'opens_at' THEN (p ->> 'opens_at')::timestamptz ELSE opens_at END,
        closes_at          = CASE WHEN p ? 'closes_at' THEN (p ->> 'closes_at')::timestamptz ELSE closes_at END,
        max_responses      = CASE WHEN p ? 'max_responses' THEN (p ->> 'max_responses')::integer ELSE max_responses END,
        max_per_respondent = CASE WHEN p ? 'max_per_respondent' THEN (p ->> 'max_per_respondent')::integer ELSE max_per_respondent END,
        edit               = CASE WHEN p ? 'edit' THEN coalesce(p ->> 'edit', 'none') ELSE edit END,
        daily_limit        = CASE WHEN p ? 'daily_limit' THEN coalesce((p ->> 'daily_limit')::integer, 1000) ELSE daily_limit END
    WHERE id = p_id;
    IF p ? 'audience' THEN
        DELETE FROM lattis.form_audience WHERE form_id = p_id;
        -- Identifiants, ou objets {"principal"}.
        PERFORM lattis.form_audience_put(p_id, (CASE WHEN jsonb_typeof(x) = 'string' THEN x #>> '{}' ELSE x ->> 'principal' END)::uuid)
        FROM jsonb_array_elements(coalesce(p -> 'audience', '[]')) x;
    END IF;
END
$$;

CREATE OR REPLACE FUNCTION lattis.create_form(p_workbook_id uuid, p_table_id uuid, p jsonb) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v      lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', false);
    v_id   uuid := gen_random_uuid();
    v_code text;
    v_user lattis.users;
    v_role text;
    v_tab  text;
    v_title text := trim(coalesce(p #>> '{definition,appearance,title}', ''));
    f      lattis.forms;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(p_workbook_id::text, 0));
    IF v.mode <> 'collaborative' OR v.state <> 'published' THEN
        RAISE EXCEPTION 'un formulaire se crée sur un tableau d''un classeur collaboratif publié' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    EXECUTE format('SELECT name FROM %I.lattis_meta_tables WHERE table_id = $1', v.schema_name) INTO v_tab USING p_table_id;
    IF v_tab IS NULL THEN
        RAISE EXCEPTION 'tableau inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    PERFORM lattis.form_require_valid(v, p_table_id, p -> 'definition');
    PERFORM lattis.form_check_access(coalesce(p ->> 'access', 'members'));
    LOOP
        v_code := lattis.form_code();
        EXIT WHEN NOT EXISTS (SELECT 1 FROM lattis.forms WHERE code = v_code);
    END LOOP;
    -- Identité technique : un compte sans fournisseur, jamais connecté en
    -- personne ; lattisd ouvre la connexion pour lui.
    v_user := lattis.create_user('form+' || left(replace(v_id::text, '-', ''), 12) || '@lattis.invalid',
                                 'Formulaire : ' || v_title, NULL, NULL);
    PERFORM lattis.activate_user(v_user.id);
    v_role := 'wb_' || v.slug || '_f_' || left(replace(v_id::text, '-', ''), 8);
    EXECUTE format('CREATE ROLE %I NOLOGIN', v_role);
    EXECUTE format('GRANT %I TO lattis_service WITH INHERIT TRUE', v_role);
    EXECUTE format('GRANT %I TO %I', v_role, v_user.pg_role);
    INSERT INTO lattis.forms (id, code, workbook_id, table_id, identity, pg_role, definition, created_by)
    VALUES (v_id, v_code, p_workbook_id, p_table_id, v_user.id, v_role, p -> 'definition', lattis.actor());
    PERFORM lattis.form_apply_options(v_id, p - 'definition');
    PERFORM lattis.compile_acl(p_workbook_id);
    SELECT * INTO f FROM lattis.forms WHERE id = v_id;
    PERFORM lattis.form_notify(f);
    PERFORM lattis.audit_log('create_form', v_id::text, NULL, jsonb_build_object('workbook_id', p_workbook_id, 'table_id', p_table_id, 'title', v_title));
    RETURN jsonb_build_object('id', v_id, 'code', v_code, 'path', '/f/' || v_code, 'url', lattis.form_url(v_code));
END
$$;

CREATE OR REPLACE FUNCTION lattis.list_forms(p_workbook_id uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_manage boolean := lattis.has_workbook_role(p_workbook_id, '{owner,designer}') OR lattis.is_server_owner();
    v_me     uuid := (SELECT id FROM lattis.users WHERE pg_role = session_user);
BEGIN
    IF NOT v_manage AND cardinality(lattis.my_builtin_roles(p_workbook_id)) = 0 THEN
        RAISE EXCEPTION 'aucun accès à ce classeur' USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN (SELECT coalesce(jsonb_agg(jsonb_build_object(
                'id', f.id, 'code', f.code, 'path', '/f/' || f.code, 'url', lattis.form_url(f.code), 'table_id', f.table_id,
                'title', f.definition #>> '{appearance,title}', 'state', f.state, 'pause_reason', f.pause_reason,
                'access', f.access, 'manageable', v_manage, 'version', CASE WHEN v_manage THEN f.version END,
                'responses', CASE WHEN v_manage THEN (SELECT count(*) FROM lattis.form_submissions s WHERE s.form_id = f.id) END,
                'last_response_at', CASE WHEN v_manage THEN (SELECT max(submitted_at) FROM lattis.form_submissions s WHERE s.form_id = f.id) END,
                'definition', CASE WHEN v_manage THEN f.definition END,
                'max_per_respondent', CASE WHEN v_manage THEN f.max_per_respondent END,
                'audience', CASE WHEN v_manage THEN (SELECT coalesce(jsonb_agg(jsonb_build_object('principal', a.principal,
                                'kind', a.principal_kind, 'name', coalesce(nullif(u.display_name, ''), u.email, t.name))), '[]')
                            FROM lattis.form_audience a LEFT JOIN lattis.users u ON u.id = a.principal
                            LEFT JOIN lattis.teams t ON t.id = a.principal WHERE a.form_id = f.id) END,
                'options', CASE WHEN v_manage THEN jsonb_build_object('opens_at', f.opens_at, 'closes_at', f.closes_at,
                    'max_responses', f.max_responses, 'max_per_respondent', f.max_per_respondent, 'edit', f.edit,
                    'daily_limit', f.daily_limit,
                    'audience', (SELECT coalesce(jsonb_agg(jsonb_build_object('principal', a.principal, 'kind', a.principal_kind,
                                        'label', coalesce(u.email, t.name))), '[]')
                                 FROM lattis.form_audience a LEFT JOIN lattis.users u ON u.id = a.principal
                                 LEFT JOIN lattis.teams t ON t.id = a.principal WHERE a.form_id = f.id)) END)
            ORDER BY f.created_at), '[]')
            FROM lattis.forms f
            WHERE f.workbook_id = p_workbook_id
              AND (v_manage OR (f.state = 'open' AND lattis.form_can_respond(f, v_me) IS NULL)));
END
$$;

CREATE OR REPLACE FUNCTION lattis.forms_structure(v lattis.workbooks, p_schema text, p_table_id uuid, p_action text, p_detail jsonb) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    f      lattis.forms;
    v_col  text := p_detail ->> 'column_id';
    v_def  jsonb;
    v_hit  boolean;
    v_req  boolean;
BEGIN
    IF v.state <> 'published' OR p_schema IS DISTINCT FROM v.schema_name THEN
        RETURN;
    END IF;
    IF p_action = 'drop_table' THEN
        DELETE FROM lattis.forms WHERE workbook_id = v.id AND table_id = p_table_id;
        RETURN;
    END IF;
    FOR f IN SELECT * FROM lattis.forms WHERE workbook_id = v.id AND table_id = p_table_id FOR UPDATE LOOP
        IF p_action = 'drop_column' THEN
            v_hit := false;
            v_req := false;
            SELECT coalesce(bool_or(true), false), coalesce(bool_or(coalesce((x ->> 'required')::boolean, false)), false)
              INTO v_hit, v_req
              FROM jsonb_array_elements(coalesce(f.definition -> 'fields', '[]')) x
             WHERE v_col IN (x ->> 'column', x ->> 'lon_column', x ->> 'latitude', x ->> 'longitude');
            IF EXISTS (SELECT 1 FROM jsonb_array_elements(coalesce(f.definition -> 'server_values', '[]')) s WHERE s ->> 'column' = v_col) THEN
                v_hit := true;
                v_req := true;
            END IF;
            CONTINUE WHEN NOT v_hit;
            v_def := jsonb_set(jsonb_set(f.definition, '{fields}',
                         coalesce((SELECT jsonb_agg(x) FROM jsonb_array_elements(coalesce(f.definition -> 'fields', '[]')) x
                                   WHERE v_col NOT IN (coalesce(x ->> 'column', ''), coalesce(x ->> 'lon_column', ''),
                                                       coalesce(x ->> 'latitude', ''), coalesce(x ->> 'longitude', ''))), '[]')),
                     '{server_values}',
                         coalesce((SELECT jsonb_agg(s) FROM jsonb_array_elements(coalesce(f.definition -> 'server_values', '[]')) s
                                   WHERE s ->> 'column' IS DISTINCT FROM v_col), '[]'));
            UPDATE lattis.forms SET definition = v_def,
                   state = CASE WHEN v_req AND state = 'open' THEN 'paused' ELSE state END,
                   pause_reason = CASE WHEN v_req AND state = 'open' THEN 'column_dropped' ELSE pause_reason END,
                   version = version + 1, updated_at = now()
            WHERE id = f.id RETURNING * INTO f;
            PERFORM lattis.form_notify(f);
        ELSIF p_action = 'alter_column_type' THEN
            IF lattis.form_check(v, p_table_id, f.definition) <> '{}' THEN
                UPDATE lattis.forms SET state = CASE WHEN state = 'open' THEN 'paused' ELSE state END,
                       pause_reason = CASE WHEN state = 'open' THEN 'type_changed' ELSE pause_reason END,
                       version = version + 1, updated_at = now()
                WHERE id = f.id RETURNING * INTO f;
            END IF;
            PERFORM lattis.form_notify(f);
        END IF;
    END LOOP;
END
$$;

REVOKE EXECUTE ON FUNCTION lattis.form_url(text) FROM PUBLIC;
