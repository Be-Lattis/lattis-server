-- Formulaires (conception côté application : docs/conception/partager-comme-formulaire.md
-- §10, §14 et §15) : un tableau d'un classeur collaboratif publié se partage
-- comme une page /f/<code> servie par lattisd. Chaque réponse devient une
-- ligne du tableau, écrite par lattisd sous l'identité technique du
-- formulaire (« Formulaire : <titre> »), qui n'a qu'un rôle interne :
-- ajouter des lignes, et modifier celles qu'elle a écrites (modification de
-- sa réponse). Ce rôle n'est pas un rôle du classeur : il n'apparaît ni dans
-- les rôles ni parmi les membres. Une restriction « tous les rôles » le vise
-- aussi.
--
-- Les colonnes sont citées par leur column_id (lattis_meta_columns).

INSERT INTO lattis.server_config (key, value) VALUES ('forms_public', 'false');

CREATE TABLE lattis.forms (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code               text NOT NULL UNIQUE CHECK (code ~ '^[a-z2-9]{10}$'),
    workbook_id        uuid NOT NULL REFERENCES lattis.workbooks (id) ON DELETE CASCADE,
    table_id           uuid NOT NULL,
    identity           uuid NOT NULL REFERENCES lattis.users (id),
    pg_role            text NOT NULL UNIQUE,
    state              text NOT NULL DEFAULT 'draft' CHECK (state IN ('draft', 'open', 'paused', 'closed')),
    pause_reason       text,
    access             text NOT NULL DEFAULT 'members' CHECK (access IN ('members', 'audience', 'server_accounts', 'public')),
    definition         jsonb NOT NULL,
    opens_at           timestamptz,
    closes_at          timestamptz,
    max_responses      integer CHECK (max_responses IS NULL OR max_responses > 0),
    max_per_respondent integer CHECK (max_per_respondent IS NULL OR max_per_respondent > 0),
    edit               text NOT NULL DEFAULT 'none' CHECK (edit ~ '^(none|until_close|[1-9][0-9]{0,3}d)$'),
    daily_limit        integer NOT NULL DEFAULT 1000 CHECK (daily_limit > 0),
    preview_token      text,
    preview_until      timestamptz,
    version            integer NOT NULL DEFAULT 1,
    created_by         text NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX forms_table ON lattis.forms (workbook_id, table_id);
REVOKE ALL ON lattis.forms FROM PUBLIC;

CREATE TABLE lattis.form_audience (
    form_id        uuid NOT NULL REFERENCES lattis.forms (id) ON DELETE CASCADE,
    principal_kind text NOT NULL CHECK (principal_kind IN ('user', 'team')),
    principal      uuid NOT NULL,
    added_by       text NOT NULL DEFAULT lattis.actor(),
    PRIMARY KEY (form_id, principal)
);
REVOKE ALL ON lattis.form_audience FROM PUBLIC;

CREATE TABLE lattis.form_submissions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    form_id         uuid NOT NULL REFERENCES lattis.forms (id) ON DELETE CASCADE,
    row_id          uuid NOT NULL,
    respondent      uuid REFERENCES lattis.users (id) ON DELETE SET NULL,
    edit_token_hash text,
    submitted_at    timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz,
    client_hash     text
);
CREATE INDEX form_submissions_recent ON lattis.form_submissions (form_id, submitted_at DESC);
CREATE INDEX form_submissions_respondent ON lattis.form_submissions (form_id, respondent);
REVOKE ALL ON lattis.form_submissions FROM PUBLIC;

-- Logo et bannière de la personnalisation (rien d'autre : ni photo ni fichier).
CREATE TABLE lattis.form_images (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    form_id      uuid NOT NULL REFERENCES lattis.forms (id) ON DELETE CASCADE,
    kind         text NOT NULL CHECK (kind IN ('logo', 'banner')),
    content_type text NOT NULL CHECK (content_type IN ('image/png', 'image/jpeg')),
    data         bytea NOT NULL CHECK (length(data) <= 1048576),
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (form_id, kind)
);
REVOKE ALL ON lattis.form_images FROM PUBLIC;

-- Réglages -----------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION lattis.set_config(p_key text, p_value jsonb) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_before jsonb;
BEGIN
    PERFORM lattis.require_server_owner();
    SELECT value INTO v_before FROM lattis.server_config WHERE key = p_key FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'réglage inconnu : %', p_key USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF p_key = 'access_mode' AND (p_value #>> '{}') NOT IN ('invitation', 'open') THEN
        RAISE EXCEPTION 'access_mode : invitation ou open' USING ERRCODE = 'invalid_parameter_value';
    ELSIF p_key = 'default_role' AND (p_value #>> '{}') NOT IN ('none', 'reader', 'participant', 'editor') THEN
        RAISE EXCEPTION 'default_role : none, reader, participant ou editor' USING ERRCODE = 'invalid_parameter_value';
    ELSIF p_key IN ('web_scripting', 'forms_public') AND jsonb_typeof(p_value) <> 'boolean' THEN
        RAISE EXCEPTION '% : booléen attendu', p_key USING ERRCODE = 'invalid_parameter_value';
    ELSIF p_key = 'workbook_creators' AND (p_value #>> '{}') NOT IN ('everyone', 'owners') THEN
        RAISE EXCEPTION 'workbook_creators : everyone ou owners' USING ERRCODE = 'invalid_parameter_value';
    ELSIF p_key = 'logo_url' AND jsonb_typeof(p_value) <> 'null'
          AND (jsonb_typeof(p_value) <> 'string' OR (p_value #>> '{}') !~ '^https://[^\s"<>]+$') THEN
        RAISE EXCEPTION 'logo_url : adresse https:// ou vide' USING ERRCODE = 'invalid_parameter_value';
    ELSIF p_key LIKE '%\_ttl' OR p_key IN ('tombstone_retention', 'password_max_failures', 'password_lock_seconds',
                                           'republish_grace') THEN
        IF jsonb_typeof(p_value) <> 'number' OR (p_value #>> '{}')::numeric <= 0 THEN
            RAISE EXCEPTION '% : nombre positif attendu', p_key USING ERRCODE = 'invalid_parameter_value';
        END IF;
    ELSIF p_key IN ('display_name') AND (jsonb_typeof(p_value) <> 'string' OR p_value #>> '{}' = '') THEN
        RAISE EXCEPTION '% : texte attendu', p_key USING ERRCODE = 'invalid_parameter_value';
    END IF;
    UPDATE lattis.server_config SET value = p_value WHERE key = p_key;
    PERFORM lattis.audit_log('set_config', p_key, v_before, p_value);
END
$$;

-- Ce que l'application doit savoir avant de proposer un formulaire.
CREATE FUNCTION lattis.forms_settings() RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT jsonb_build_object('forms', true, 'forms_public', coalesce((lattis.config('forms_public') #>> '{}')::boolean, false))
$$;

-- Définition ---------------------------------------------------------------------------

-- Colonnes d'un tableau publié : {"<column_id>": {"name", "type"}}.
CREATE FUNCTION lattis.form_columns(p_schema text, p_table_id uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_out jsonb;
BEGIN
    EXECUTE format('SELECT coalesce(jsonb_object_agg(column_id::text, jsonb_build_object(''name'', name, ''type'', type, ''position'', position)), ''{}'')
                    FROM %I.lattis_meta_columns WHERE table_id = $1', p_schema) INTO v_out USING p_table_id;
    RETURN v_out;
END
$$;

-- Erreurs d'une définition : {"<chemin>": "raison"} (vide : valide).
CREATE FUNCTION lattis.form_check(v lattis.workbooks, p_table_id uuid, d jsonb) RETURNS jsonb
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
       AND (a ->> 'accent') NOT IN ('green', 'teal', 'blue', 'indigo', 'purple', 'red', 'orange', 'gray') THEN
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
        v_widget := coalesce(f ->> 'widget', '');
        IF NOT (
            (v_widget IN ('', 'list') ) OR
            (v_widget IN ('text', 'textarea') AND v_type = 'text') OR
            (v_widget = 'number' AND v_type IN ('integer', 'decimal')) OR
            (v_widget = 'date' AND v_type = 'date') OR
            (v_widget = 'datetime' AND v_type = 'datetime') OR
            (v_widget IN ('checkbox', 'yesno') AND v_type = 'boolean') OR
            (v_widget = 'position' AND v_type IN ('text', 'decimal'))) THEN
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

CREATE FUNCTION lattis.form_require_valid(v lattis.workbooks, p_table_id uuid, d jsonb) RETURNS void
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_err jsonb := lattis.form_check(v, p_table_id, d);
    v_first record;
BEGIN
    IF v_err <> '{}' THEN
        SELECT key, value #>> '{}' AS reason INTO v_first FROM jsonb_each(v_err) LIMIT 1;
        RAISE EXCEPTION 'formulaire invalide (%) : %', v_first.key, v_first.reason
            USING ERRCODE = 'invalid_parameter_value', DETAIL = jsonb_build_object('fields', v_err)::text;
    END IF;
END
$$;

-- Droits et notifications ------------------------------------------------------------

CREATE FUNCTION lattis.form_manage(p_id uuid) RETURNS lattis.forms
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    f lattis.forms;
BEGIN
    SELECT * INTO f FROM lattis.forms WHERE id = p_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'formulaire inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    PERFORM lattis.require_workbook_role(f.workbook_id, '{owner,designer}', true);
    RETURN f;
END
$$;

CREATE FUNCTION lattis.form_notify(f lattis.forms) RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT pg_notify('wb_' || w.slug, '*form:' || f.id) FROM lattis.workbooks w WHERE w.id = f.workbook_id
$$;

CREATE FUNCTION lattis.form_check_access(p_access text) RETURNS void
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    IF p_access = 'public' AND NOT coalesce((lattis.config('forms_public') #>> '{}')::boolean, false) THEN
        RAISE EXCEPTION 'les formulaires à lien public ne sont pas permis sur ce serveur' USING ERRCODE = 'insufficient_privilege';
    END IF;
END
$$;

-- Code de 10 caractères (base 32 sans caractères ambigus) tiré d'un aléa fort.
CREATE FUNCTION lattis.form_code() RETURNS text
LANGUAGE plpgsql VOLATILE SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_alpha text := 'abcdefghijkmnpqrstuvwxyz23456789';
    v_b     bytea := uuid_send(gen_random_uuid()) || uuid_send(gen_random_uuid());
    v_out   text := '';
    i       integer;
BEGIN
    FOREACH i IN ARRAY ARRAY[0, 1, 2, 3, 4, 5, 9, 10, 11, 12] LOOP
        v_out := v_out || substr(v_alpha, (get_byte(v_b, i) % 32) + 1, 1);
    END LOOP;
    RETURN v_out;
END
$$;

-- Options communes (création et modification).
CREATE FUNCTION lattis.form_apply_options(p_id uuid, p jsonb) RETURNS void
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
        PERFORM lattis.form_audience_put(p_id, (x ->> 'principal')::uuid)
        FROM jsonb_array_elements(coalesce(p -> 'audience', '[]')) x;
    END IF;
END
$$;

CREATE FUNCTION lattis.form_audience_put(p_id uuid, p_principal uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_kind text;
BEGIN
    IF EXISTS (SELECT 1 FROM lattis.users WHERE id = p_principal) THEN
        v_kind := 'user';
    ELSIF EXISTS (SELECT 1 FROM lattis.teams WHERE id = p_principal) THEN
        v_kind := 'team';
    ELSE
        RAISE EXCEPTION 'utilisateur ou équipe inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    INSERT INTO lattis.form_audience (form_id, principal_kind, principal) VALUES (p_id, v_kind, p_principal)
    ON CONFLICT DO NOTHING;
END
$$;

-- Gestion (propriétaires et concepteurs du classeur) -------------------------------

-- p : definition, access (members par défaut), audience [{"principal"}],
-- opens_at, closes_at, max_responses, max_per_respondent, edit, daily_limit.
-- Renvoie {"id", "code", "path"} ; le formulaire naît en brouillon.
CREATE FUNCTION lattis.create_form(p_workbook_id uuid, p_table_id uuid, p jsonb) RETURNS jsonb
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
    RETURN jsonb_build_object('id', v_id, 'code', v_code, 'path', '/f/' || v_code);
END
$$;

-- p : champs facultatifs (ceux de create_form) ; p_version : version lue.
CREATE FUNCTION lattis.update_form(p_id uuid, p jsonb, p_version integer) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    f   lattis.forms := lattis.form_manage(p_id);
    v   lattis.workbooks := lattis.workbook(f.workbook_id);
BEGIN
    SELECT * INTO f FROM lattis.forms WHERE id = p_id FOR UPDATE;
    IF p_version IS DISTINCT FROM f.version THEN
        RAISE EXCEPTION 'formulaire modifié entre-temps : rechargez-le' USING ERRCODE = 'serialization_failure';
    END IF;
    IF p ? 'definition' THEN
        PERFORM lattis.form_require_valid(v, f.table_id, p -> 'definition');
        UPDATE lattis.forms SET definition = p -> 'definition' WHERE id = p_id;
        UPDATE lattis.users SET display_name = 'Formulaire : ' || trim(p #>> '{definition,appearance,title}') WHERE id = f.identity;
    END IF;
    PERFORM lattis.form_apply_options(p_id, p - 'definition');
    UPDATE lattis.forms SET version = version + 1, updated_at = now() WHERE id = p_id RETURNING * INTO f;
    PERFORM lattis.form_notify(f);
    PERFORM lattis.audit_log('update_form', p_id::text, NULL, p);
    RETURN jsonb_build_object('version', f.version);
END
$$;

-- open revérifie la définition sur la structure actuelle et le mode d'accès.
CREATE FUNCTION lattis.set_form_state(p_id uuid, p_state text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    f lattis.forms := lattis.form_manage(p_id);
BEGIN
    IF p_state NOT IN ('open', 'paused', 'closed') THEN
        RAISE EXCEPTION 'état : open, paused ou closed' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF p_state = 'open' THEN
        PERFORM lattis.form_require_valid(lattis.workbook(f.workbook_id), f.table_id, f.definition);
        PERFORM lattis.form_check_access(f.access);
    END IF;
    UPDATE lattis.forms SET state = p_state, pause_reason = CASE WHEN p_state = 'paused' THEN 'manual' END,
           version = version + 1, updated_at = now()
    WHERE id = p_id RETURNING * INTO f;
    PERFORM lattis.form_notify(f);
    PERFORM lattis.audit_log('set_form_state', p_id::text, NULL, jsonb_build_object('state', p_state));
END
$$;

-- Fin d'un formulaire : rôle interne supprimé, identité désactivée (elle
-- reste nommée dans lattis_author), lignes gardées. Appelée aussi par le
-- déclencheur quand le classeur ou le tableau disparaît.
CREATE FUNCTION lattis.form_retire(f lattis.forms) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_login text;
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = f.pg_role) THEN
        -- Droits et règles RLS du rôle retirés avec lui.
        EXECUTE format('DROP OWNED BY %I', f.pg_role);
        EXECUTE format('DROP ROLE %I', f.pg_role);
    END IF;
    UPDATE lattis.users SET disabled = true WHERE id = f.identity RETURNING pg_role INTO v_login;
    IF v_login IS NOT NULL AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_login) THEN
        EXECUTE format('ALTER ROLE %I NOLOGIN', v_login);
    END IF;
END
$$;

CREATE FUNCTION lattis.trg_form_deleted() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.form_retire(OLD);
    PERFORM lattis.form_notify(OLD);
    RETURN OLD;
END
$$;
CREATE TRIGGER form_deleted AFTER DELETE ON lattis.forms FOR EACH ROW EXECUTE FUNCTION lattis.trg_form_deleted();

CREATE FUNCTION lattis.drop_form(p_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    f lattis.forms := lattis.form_manage(p_id);
BEGIN
    DELETE FROM lattis.forms WHERE id = p_id;
    PERFORM lattis.compile_acl(f.workbook_id);
    PERFORM lattis.audit_log('drop_form', p_id::text, to_jsonb(f) - 'definition', NULL);
END
$$;

-- Qui peut répondre (NULL : oui ; sinon la raison). p_user NULL : anonyme.
CREATE FUNCTION lattis.form_can_respond(f lattis.forms, p_user uuid) RETURNS text
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    u lattis.users;
BEGIN
    IF f.access = 'public' THEN
        RETURN CASE WHEN coalesce((lattis.config('forms_public') #>> '{}')::boolean, false) THEN NULL ELSE 'public_disabled' END;
    END IF;
    SELECT * INTO u FROM lattis.users WHERE id = p_user AND NOT disabled;
    IF u.id IS NULL THEN
        RETURN 'login';
    END IF;
    IF f.access = 'server_accounts' THEN
        RETURN CASE WHEN u.email LIKE '%@lattis.invalid' THEN 'forbidden' END;
    ELSIF f.access = 'members' THEN
        RETURN CASE WHEN EXISTS (SELECT 1 FROM lattis.roles r WHERE r.workbook_id = f.workbook_id
                                 AND pg_has_role(u.pg_role, r.pg_role, 'MEMBER')) THEN NULL ELSE 'forbidden' END;
    END IF;
    RETURN CASE WHEN EXISTS (
        SELECT 1 FROM lattis.form_audience a
        LEFT JOIN lattis.teams t ON a.principal_kind = 'team' AND t.id = a.principal
        WHERE a.form_id = f.id AND ((a.principal_kind = 'user' AND a.principal = u.id)
                                    OR (t.pg_role IS NOT NULL AND pg_has_role(u.pg_role, t.pg_role, 'MEMBER'))))
        THEN NULL ELSE 'forbidden' END;
END
$$;

-- Formulaires d'un classeur : tout pour ses propriétaires et concepteurs ;
-- pour les autres membres, les formulaires ouverts auxquels ils peuvent
-- répondre, sans les chiffres.
CREATE FUNCTION lattis.list_forms(p_workbook_id uuid) RETURNS jsonb
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
                'id', f.id, 'code', f.code, 'path', '/f/' || f.code, 'table_id', f.table_id,
                'title', f.definition #>> '{appearance,title}', 'state', f.state, 'pause_reason', f.pause_reason,
                'access', f.access, 'manageable', v_manage, 'version', CASE WHEN v_manage THEN f.version END,
                'responses', CASE WHEN v_manage THEN (SELECT count(*) FROM lattis.form_submissions s WHERE s.form_id = f.id) END,
                'last_response_at', CASE WHEN v_manage THEN (SELECT max(submitted_at) FROM lattis.form_submissions s WHERE s.form_id = f.id) END,
                'definition', CASE WHEN v_manage THEN f.definition END,
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

CREATE FUNCTION lattis.form_stats(p_id uuid, p_days integer DEFAULT 30) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    f lattis.forms := lattis.form_manage(p_id);
BEGIN
    RETURN jsonb_build_object(
        'total', (SELECT count(*) FROM lattis.form_submissions WHERE form_id = p_id),
        'last_at', (SELECT max(submitted_at) FROM lattis.form_submissions WHERE form_id = p_id),
        'throttled', f.pause_reason = 'spike',
        'by_day', (SELECT coalesce(jsonb_agg(jsonb_build_object('day', d.day, 'n', d.n) ORDER BY d.day), '[]')
                   FROM (SELECT (submitted_at AT TIME ZONE 'UTC')::date AS day, count(*) AS n FROM lattis.form_submissions
                         WHERE form_id = p_id AND submitted_at >= now() - make_interval(days => greatest(1, least(p_days, 366)))
                         GROUP BY 1) d));
END
$$;

-- Lien d'aperçu, valable 10 minutes ; ses envois ne sont pas enregistrés.
CREATE FUNCTION lattis.form_preview_url(p_id uuid) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    f   lattis.forms := lattis.form_manage(p_id);
    v_t text := replace(gen_random_uuid()::text || gen_random_uuid()::text, '-', '');
BEGIN
    UPDATE lattis.forms SET preview_token = v_t, preview_until = now() + interval '10 minutes' WHERE id = p_id;
    RETURN '/f/' || f.code || '?p=' || v_t;
END
$$;

CREATE FUNCTION lattis.form_audience_add(p_id uuid, p_principal uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    f lattis.forms := lattis.form_manage(p_id);
BEGIN
    PERFORM lattis.form_audience_put(p_id, p_principal);
    PERFORM lattis.form_notify(f);
END
$$;

CREATE FUNCTION lattis.form_audience_add_by_email(p_id uuid, p_email text) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    f   lattis.forms := lattis.form_manage(p_id);
    v_u uuid;
BEGIN
    SELECT id INTO v_u FROM lattis.users WHERE email = lower(trim(p_email)) AND NOT disabled;
    IF v_u IS NULL THEN
        RAISE EXCEPTION 'aucun compte pour « % »' , p_email USING ERRCODE = 'no_data_found';
    END IF;
    PERFORM lattis.form_audience_put(p_id, v_u);
    PERFORM lattis.form_notify(f);
    RETURN v_u;
END
$$;

CREATE FUNCTION lattis.form_audience_remove(p_id uuid, p_principal uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    f lattis.forms := lattis.form_manage(p_id);
BEGIN
    DELETE FROM lattis.form_audience WHERE form_id = p_id AND principal = p_principal;
    PERFORM lattis.form_notify(f);
END
$$;

-- Logo ou bannière (déjà réduit et sans métadonnées par lattisd).
CREATE FUNCTION lattis.form_set_image(p_id uuid, p_kind text, p_content_type text, p_data bytea) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    f    lattis.forms := lattis.form_manage(p_id);
    v_id uuid;
BEGIN
    DELETE FROM lattis.form_images WHERE form_id = p_id AND kind = p_kind;
    INSERT INTO lattis.form_images (form_id, kind, content_type, data) VALUES (p_id, p_kind, p_content_type, p_data)
    RETURNING id INTO v_id;
    PERFORM lattis.audit_log('form_set_image', p_id::text, NULL, jsonb_build_object('kind', p_kind, 'bytes', length(p_data)));
    RETURN v_id;
END
$$;

-- lattisd ----------------------------------------------------------------------------

-- Page d'un formulaire (connexion de service) : définition, colonnes, listes
-- résolues, état. p_preview : jeton d'aperçu (ou NULL). Inconnu : P0002.
CREATE FUNCTION lattis.form_page(p_code text, p_preview text) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    f       lattis.forms;
    v       lattis.workbooks;
    v_lists jsonb := '{}';
    x       jsonb;
    v_src   record;
    v_vals  jsonb;
    v_count integer;
BEGIN
    PERFORM lattis.require_service();
    SELECT * INTO f FROM lattis.forms WHERE code = p_code;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'formulaire inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    SELECT * INTO v FROM lattis.workbooks WHERE id = f.workbook_id;
    FOR x IN SELECT * FROM jsonb_array_elements(coalesce(f.definition -> 'fields', '[]')) LOOP
        IF x -> 'list' ? 'table_id' THEN
            EXECUTE format('SELECT t.name, c.name AS col FROM %I.lattis_meta_tables t JOIN %I.lattis_meta_columns c
                            ON c.table_id = t.table_id WHERE t.table_id = $1 AND c.column_id = $2', v.schema_name, v.schema_name)
                INTO v_src USING (x -> 'list' ->> 'table_id')::uuid, (x -> 'list' ->> 'column')::uuid;
            IF v_src.name IS NOT NULL THEN
                EXECUTE format('SELECT coalesce(jsonb_agg(val ORDER BY val), ''[]'') FROM (SELECT DISTINCT %I::text AS val FROM %I.%I
                                WHERE %I IS NOT NULL ORDER BY 1 LIMIT 1000) s', v_src.col, v.schema_name, v_src.name, v_src.col)
                    INTO v_vals;
                v_lists := v_lists || jsonb_build_object(x ->> 'column', v_vals);
            END IF;
        ELSIF x -> 'list' ? 'values' THEN
            v_lists := v_lists || jsonb_build_object(x ->> 'column', x -> 'list' -> 'values');
        END IF;
    END LOOP;
    SELECT count(*) INTO v_count FROM lattis.form_submissions WHERE form_id = f.id;
    RETURN jsonb_build_object(
        'id', f.id, 'code', f.code, 'state', f.state, 'pause_reason', f.pause_reason, 'access', f.access,
        'definition', f.definition, 'columns', lattis.form_columns(v.schema_name, f.table_id), 'lists', v_lists,
        'opens_at', f.opens_at, 'closes_at', f.closes_at, 'max_responses', f.max_responses,
        'max_per_respondent', f.max_per_respondent, 'edit', f.edit, 'responses', v_count,
        'identity', f.identity, 'schema', v.schema_name, 'workbook', v.title,
        'table', (SELECT name FROM lattis.data_tables(v.schema_name) t WHERE t.table_id = f.table_id),
        'preview', p_preview IS NOT NULL AND p_preview = f.preview_token AND f.preview_until > now(),
        'images', (SELECT coalesce(jsonb_object_agg(kind, id), '{}') FROM lattis.form_images WHERE form_id = f.id),
        'server_name', lattis.config('display_name') #>> '{}');
END
$$;

-- Qui répond (connexion de service) : NULL si la personne peut répondre,
-- sinon login, forbidden, not_open, closed, full, already.
CREATE FUNCTION lattis.form_gate(p_id uuid, p_user uuid) RETURNS text
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    f      lattis.forms;
    v_why  text;
    v_mine integer;
BEGIN
    PERFORM lattis.require_service();
    SELECT * INTO f FROM lattis.forms WHERE id = p_id;
    IF NOT FOUND THEN
        RETURN 'unknown';
    END IF;
    v_why := lattis.form_can_respond(f, p_user);
    IF v_why IS NOT NULL THEN
        RETURN v_why;
    END IF;
    IF f.state = 'draft' OR f.opens_at > now() THEN
        RETURN 'not_open';
    ELSIF f.state <> 'open' OR f.closes_at <= now() THEN
        RETURN 'closed';
    ELSIF f.max_responses IS NOT NULL AND (SELECT count(*) FROM lattis.form_submissions WHERE form_id = p_id) >= f.max_responses THEN
        RETURN 'full';
    END IF;
    IF p_user IS NOT NULL AND f.max_per_respondent IS NOT NULL THEN
        SELECT count(*) INTO v_mine FROM lattis.form_submissions WHERE form_id = p_id AND respondent = p_user;
        IF v_mine >= f.max_per_respondent THEN
            RETURN 'already';
        END IF;
    END IF;
    RETURN NULL;
END
$$;

-- Début d'un envoi, sous l'identité du formulaire, dans la transaction de
-- l'insertion : verrou sur le tableau, contrôles faisant foi (état, dates,
-- plafond, limite quotidienne), position de la ligne et numéros.
CREATE FUNCTION lattis.form_begin(p_id uuid) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    f       lattis.forms;
    v       lattis.workbooks;
    v_tab   text;
    v_pos   text;
    v_nums  jsonb := '{}';
    s       jsonb;
    v_col   text;
    v_n     numeric;
BEGIN
    SELECT * INTO f FROM lattis.forms WHERE id = p_id;
    IF NOT FOUND OR session_user <> (SELECT pg_role FROM lattis.users WHERE id = f.identity) THEN
        RAISE EXCEPTION 'réservé à l''identité du formulaire' USING ERRCODE = 'insufficient_privilege';
    END IF;
    SELECT * INTO v FROM lattis.workbooks WHERE id = f.workbook_id;
    PERFORM pg_advisory_xact_lock(hashtextextended('form:' || f.table_id::text, 0));
    IF f.state <> 'open' OR f.opens_at > now() OR f.closes_at <= now() THEN
        RAISE EXCEPTION 'formulaire fermé' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF f.max_responses IS NOT NULL AND (SELECT count(*) FROM lattis.form_submissions WHERE form_id = p_id) >= f.max_responses THEN
        RAISE EXCEPTION 'formulaire complet' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF (SELECT count(*) FROM lattis.form_submissions WHERE form_id = p_id AND submitted_at > now() - interval '1 day') >= f.daily_limit THEN
        RAISE EXCEPTION 'limite quotidienne de réponses atteinte' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    SELECT name INTO v_tab FROM lattis.data_tables(v.schema_name) t WHERE t.table_id = f.table_id;
    EXECUTE format('SELECT max(lattis_pos COLLATE "C") FROM %I.%I', v.schema_name, v_tab) INTO v_pos;
    FOR s IN SELECT * FROM jsonb_array_elements(coalesce(f.definition -> 'server_values', '[]')) LOOP
        IF s -> 'value' ->> 'token' = 'next_number' THEN
            v_col := lattis.form_columns(v.schema_name, f.table_id) -> (s ->> 'column') ->> 'name';
            EXECUTE format('SELECT coalesce(max(%I), 0) + 1 FROM %I.%I', v_col, v.schema_name, v_tab) INTO v_n;
            v_nums := v_nums || jsonb_build_object(s ->> 'column', v_n);
        END IF;
    END LOOP;
    RETURN jsonb_build_object('schema', v.schema_name, 'table', v_tab, 'max_pos', v_pos, 'numbers', v_nums,
                              'columns', lattis.form_columns(v.schema_name, f.table_id));
END
$$;

-- Fin d'un envoi, même transaction : nombre de réponses par personne,
-- journal des réponses, pause automatique sur un pic.
CREATE FUNCTION lattis.form_record(p_id uuid, p_row_id uuid, p_respondent uuid, p_client_hash text, p_edit_token_hash text) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    f      lattis.forms;
    v_id   uuid;
    v_10   integer;
    v_base numeric;
BEGIN
    SELECT * INTO f FROM lattis.forms WHERE id = p_id;
    IF NOT FOUND OR session_user <> (SELECT pg_role FROM lattis.users WHERE id = f.identity) THEN
        RAISE EXCEPTION 'réservé à l''identité du formulaire' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF p_respondent IS NOT NULL AND f.max_per_respondent IS NOT NULL
       AND (SELECT count(*) FROM lattis.form_submissions WHERE form_id = p_id AND respondent = p_respondent) >= f.max_per_respondent THEN
        RAISE EXCEPTION 'vous avez déjà répondu à ce formulaire' USING ERRCODE = 'raise_exception';
    END IF;
    INSERT INTO lattis.form_submissions (form_id, row_id, respondent, client_hash, edit_token_hash)
    VALUES (p_id, p_row_id, p_respondent, p_client_hash, p_edit_token_hash) RETURNING id INTO v_id;
    -- Pic : plus de 5 fois le débit habituel sur 10 minutes (et plus de 20 réponses).
    SELECT count(*) INTO v_10 FROM lattis.form_submissions WHERE form_id = p_id AND submitted_at > now() - interval '10 minutes';
    IF v_10 > 20 THEN
        SELECT count(*)::numeric / 1008 INTO v_base FROM lattis.form_submissions
        WHERE form_id = p_id AND submitted_at BETWEEN now() - interval '7 days' AND now() - interval '10 minutes';
        IF v_10 > 5 * greatest(v_base, 4) THEN
            UPDATE lattis.forms SET state = 'paused', pause_reason = 'spike', version = version + 1, updated_at = now()
            WHERE id = p_id RETURNING * INTO f;
            PERFORM lattis.form_notify(f);
            PERFORM lattis.audit_log('form_paused', p_id::text, NULL, jsonb_build_object('reason', 'spike', 'last_10_min', v_10));
        END IF;
    END IF;
    RETURN v_id;
END
$$;

-- Tâche de fond (lattisd) : fermeture à closes_at, purge des empreintes
-- d'adresse après 30 jours.
CREATE FUNCTION lattis.forms_housekeeping() RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    f lattis.forms;
BEGIN
    PERFORM lattis.require_service();
    FOR f IN UPDATE lattis.forms SET state = 'closed', version = version + 1, updated_at = now()
             WHERE state IN ('open', 'paused') AND closes_at <= now() RETURNING * LOOP
        PERFORM lattis.form_notify(f);
    END LOOP;
    UPDATE lattis.form_submissions SET client_hash = NULL
    WHERE client_hash IS NOT NULL AND submitted_at < now() - interval '30 days';
END
$$;

-- Suivi de la structure --------------------------------------------------------------

-- Après une opération de structure sur un tableau publié : colonnes
-- disparues retirées des définitions (pause si le champ était obligatoire
-- ou posé par le serveur), définition revérifiée après une conversion,
-- formulaires supprimés avec le tableau.
CREATE FUNCTION lattis.forms_structure(v lattis.workbooks, p_schema text, p_table_id uuid, p_action text, p_detail jsonb) RETURNS void
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
             WHERE x ->> 'column' = v_col OR x ->> 'lon_column' = v_col;
            IF EXISTS (SELECT 1 FROM jsonb_array_elements(coalesce(f.definition -> 'server_values', '[]')) s WHERE s ->> 'column' = v_col) THEN
                v_hit := true;
                v_req := true;
            END IF;
            CONTINUE WHEN NOT v_hit;
            v_def := jsonb_set(jsonb_set(f.definition, '{fields}',
                         coalesce((SELECT jsonb_agg(x) FROM jsonb_array_elements(coalesce(f.definition -> 'fields', '[]')) x
                                   WHERE x ->> 'column' IS DISTINCT FROM v_col AND x ->> 'lon_column' IS DISTINCT FROM v_col), '[]')),
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

CREATE OR REPLACE FUNCTION lattis.structure_after(v lattis.workbooks, p_schema text, p_table_id uuid, p_action text, p_detail jsonb) RETURNS void
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
    PERFORM lattis.forms_structure(v, p_schema, p_table_id, p_action, p_detail);
    PERFORM lattis.audit_log(p_action, p_table_id::text, NULL,
        jsonb_build_object('workbook_id', v.id, 'schema', p_schema) || coalesce(p_detail, '{}'));
END
$$;

CREATE OR REPLACE FUNCTION lattis.compile_table(v lattis.workbooks, p_schema text, p_table text, p_table_id uuid, p_sheet_id text)
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
    v_res  lattis.restrictions;
    v_to   text;
    v_form lattis.forms;
    v_k    integer := 0;
BEGIN
    EXECUTE format('ALTER TABLE %I.%I ENABLE ROW LEVEL SECURITY', p_schema, p_table);
    EXECUTE format('ALTER TABLE %I.%I FORCE ROW LEVEL SECURITY', p_schema, p_table);
    FOR v_pol IN SELECT policyname FROM pg_policies WHERE schemaname = p_schema AND tablename = p_table
                 AND policyname LIKE 'lattis\_%' LOOP
        EXECUTE format('DROP POLICY %I ON %I.%I', v_pol.policyname, p_schema, p_table);
    END LOOP;
    EXECUTE format('CREATE POLICY lattis_owner ON %I.%I TO %I USING (true) WITH CHECK (true)',
                   p_schema, p_table, 'wb_' || v.slug || '_owner');

    SELECT string_agg(quote_ident(attname), ', ' ORDER BY attnum) INTO v_cols
    FROM pg_attribute
    WHERE attrelid = format('%I.%I', p_schema, p_table)::regclass AND attnum > 0 AND NOT attisdropped
      AND attname NOT IN ('lattis_id', 'lattis_author');

    FOR v_role IN SELECT * FROM lattis.roles WHERE workbook_id = v.id ORDER BY created_at, pg_role LOOP
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

    -- Formulaires : leur rôle interne ajoute des lignes et ne lit ou ne
    -- modifie que celles qu'il a écrites (modification de sa réponse).
    FOR v_form IN SELECT * FROM lattis.forms WHERE workbook_id = v.id AND table_id = p_table_id ORDER BY created_at LOOP
        CONTINUE WHEN NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = v_form.pg_role);
        v_k := v_k + 1;
        EXECUTE format('GRANT USAGE ON SCHEMA %I TO %I', p_schema, v_form.pg_role);
        EXECUTE format('REVOKE ALL ON %I.%I FROM %I', p_schema, p_table, v_form.pg_role);
        EXECUTE format('GRANT SELECT, INSERT ON %I.%I TO %I', p_schema, p_table, v_form.pg_role);
        EXECUTE format('GRANT UPDATE (%s) ON %I.%I TO %I', v_cols, p_schema, p_table, v_form.pg_role);
        EXECUTE format('CREATE POLICY %I ON %I.%I FOR INSERT TO %I WITH CHECK (lattis_author = current_user)',
                       'lattis_f' || v_k || '_insert', p_schema, p_table, v_form.pg_role);
        EXECUTE format('CREATE POLICY %I ON %I.%I FOR SELECT TO %I USING (lattis_author = current_user)',
                       'lattis_f' || v_k || '_select', p_schema, p_table, v_form.pg_role);
        EXECUTE format('CREATE POLICY %I ON %I.%I FOR UPDATE TO %I USING (lattis_author = current_user AND lattis_updated_by = current_user)
                        WITH CHECK (lattis_author = current_user)', 'lattis_f' || v_k || '_update', p_schema, p_table, v_form.pg_role);
    END LOOP;

    -- Restrictions : AS RESTRICTIVE, combinées en ET (§8.2).
    v_n := 0;
    FOR v_res IN SELECT * FROM lattis.restrictions WHERE workbook_id = v.id AND table_id = p_table_id ORDER BY id LOOP
        v_n := v_n + 1;
        SELECT string_agg(quote_ident(r), ', ') INTO v_to FROM (
            SELECT pg_role AS r FROM lattis.roles
            WHERE workbook_id = v.id
              AND (id = ANY (v_res.applies_to)
                   -- « Tous » : sauf propriétaires et concepteurs, qui gardent la main.
                   OR (cardinality(v_res.applies_to) = 0 AND builtin IS DISTINCT FROM 'owner' AND builtin IS DISTINCT FROM 'designer'))
            UNION ALL
            -- « Tous » vise aussi les formulaires du tableau.
            SELECT f.pg_role FROM lattis.forms f
            WHERE cardinality(v_res.applies_to) = 0 AND f.workbook_id = v.id AND f.table_id = p_table_id
              AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname = f.pg_role)) x;
        IF v_to IS NULL THEN
            CONTINUE;
        END IF;
        IF v_res.command IN ('select', 'delete') THEN
            EXECUTE format('CREATE POLICY %I ON %I.%I AS RESTRICTIVE FOR %s TO %s USING (%s)',
                           'lattis_x' || v_n, p_schema, p_table, upper(v_res.command), v_to, v_res.predicate);
        ELSIF v_res.command = 'insert' THEN
            EXECUTE format('CREATE POLICY %I ON %I.%I AS RESTRICTIVE FOR INSERT TO %s WITH CHECK (%s)',
                           'lattis_x' || v_n, p_schema, p_table, v_to, v_res.predicate);
        ELSE
            EXECUTE format('CREATE POLICY %I ON %I.%I AS RESTRICTIVE FOR %s TO %s USING (%s) WITH CHECK (%s)',
                           'lattis_x' || v_n, p_schema, p_table, upper(v_res.command), v_to, v_res.predicate, v_res.predicate);
        END IF;
    END LOOP;

    INSERT INTO lattis.acl_fingerprints (workbook_id, table_id, fingerprint)
    VALUES (v.id, p_table_id, lattis.table_fingerprint(p_schema, p_table))
    ON CONFLICT (workbook_id, table_id) DO UPDATE SET fingerprint = excluded.fingerprint, compiled_at = now();
END
$$;

-- Les droits des formulaires ne sont pas des écarts.
CREATE OR REPLACE FUNCTION lattis.foreign_grants(p_schema text, p_table text) RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT coalesce(jsonb_agg(DISTINCT CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE a.grantee::regrole::text END), '[]')
    FROM pg_class c
    CROSS JOIN LATERAL (SELECT relacl AS acl UNION ALL
                        SELECT attacl FROM pg_attribute WHERE attrelid = c.oid AND attacl IS NOT NULL) l,
         aclexplode(l.acl) a
    WHERE c.oid = format('%I.%I', p_schema, p_table)::regclass
      AND a.grantee <> c.relowner
      AND (a.grantee = 0 OR (NOT EXISTS (SELECT 1 FROM lattis.roles r WHERE r.pg_role = a.grantee::regrole::text)
                             AND NOT EXISTS (SELECT 1 FROM lattis.forms f WHERE f.pg_role = a.grantee::regrole::text)))
$$;

REVOKE EXECUTE ON FUNCTION lattis.form_columns(text, uuid), lattis.form_check(lattis.workbooks, uuid, jsonb),
    lattis.form_require_valid(lattis.workbooks, uuid, jsonb), lattis.form_manage(uuid), lattis.form_notify(lattis.forms),
    lattis.form_check_access(text), lattis.form_code(), lattis.form_apply_options(uuid, jsonb),
    lattis.form_audience_put(uuid, uuid), lattis.form_retire(lattis.forms), lattis.trg_form_deleted(),
    lattis.form_can_respond(lattis.forms, uuid), lattis.form_page(text, text), lattis.form_gate(uuid, uuid),
    lattis.forms_housekeeping(), lattis.forms_structure(lattis.workbooks, text, uuid, text, jsonb) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION lattis.forms_settings(), lattis.create_form(uuid, uuid, jsonb), lattis.update_form(uuid, jsonb, integer),
    lattis.set_form_state(uuid, text), lattis.drop_form(uuid), lattis.list_forms(uuid), lattis.form_stats(uuid, integer),
    lattis.form_preview_url(uuid), lattis.form_audience_add(uuid, uuid), lattis.form_audience_add_by_email(uuid, text),
    lattis.form_audience_remove(uuid, uuid), lattis.form_set_image(uuid, text, text, bytea),
    lattis.form_begin(uuid), lattis.form_record(uuid, uuid, uuid, text, text) TO PUBLIC;
