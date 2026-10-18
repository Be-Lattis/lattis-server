-- Noms techniques d'un classeur : le slug se termine par le début de
-- l'identifiant du classeur (wb_ventes_2026_3f9a2c), pour qu'un titre
-- repris ne se heurte pas à un autre classeur, présent ou supprimé. Les
-- classeurs existants gardent leur nom.

CREATE FUNCTION lattis.slugify(p_title text, p_id uuid) RETURNS text
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_base text;
    v_slug text;
    v_n    integer := 1;
BEGIN
    v_base := lower(translate(coalesce(p_title, ''),
        'àâäáãåçéèêëíìîïñóòôöõúùûüýÿÀÂÄÁÃÅÇÉÈÊËÍÌÎÏÑÓÒÔÖÕÚÙÛÜÝœŒæÆ',
        'aaaaaaceeeeiiiinooooouuuuyyAAAAAACEEEEIIIINOOOOOUUUUYoOaA'));
    v_base := trim(BOTH '_' FROM regexp_replace(v_base, '[^a-z0-9]+', '_', 'g'));
    v_base := left(coalesce(nullif(v_base, ''), 'classeur'), 30) || '_' || left(replace(p_id::text, '-', ''), 6);
    v_slug := v_base;
    -- Filet : six caractères d'identifiant identiques et même titre.
    WHILE EXISTS (SELECT 1 FROM lattis.workbooks WHERE slug = v_slug)
       OR EXISTS (SELECT 1 FROM pg_namespace WHERE nspname LIKE 'wb\_' || v_slug || '%')
       OR EXISTS (SELECT 1 FROM pg_roles WHERE rolname LIKE 'wb\_' || v_slug || '\_%') LOOP
        v_n := v_n + 1;
        v_slug := v_base || '_' || v_n;
    END LOOP;
    RETURN v_slug;
END
$$;

REVOKE EXECUTE ON FUNCTION lattis.slugify(text, uuid) FROM PUBLIC;

CREATE OR REPLACE FUNCTION lattis.create_workbook(p_title text, p_mode text) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_id    uuid := gen_random_uuid();
    v_slug  text;
    v_owner text;
    v_user  lattis.users;
    v_role  record;
    v_rid   uuid;
BEGIN
    IF p_mode NOT IN ('frozen', 'collaborative') THEN
        RAISE EXCEPTION 'mode : frozen ou collaborative' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF coalesce(trim(p_title), '') = '' THEN
        RAISE EXCEPTION 'titre manquant' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    SELECT * INTO v_user FROM lattis.users WHERE pg_role = session_user AND NOT disabled;
    IF v_user.id IS NULL THEN
        RAISE EXCEPTION 'seul un utilisateur Lattis crée un classeur' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NOT lattis.can_create_workbooks() THEN
        RAISE EXCEPTION 'la création de classeurs est réservée aux propriétaires du serveur et aux créateurs désignés'
            USING ERRCODE = 'insufficient_privilege';
    END IF;

    v_slug := lattis.slugify(p_title, v_id);
    v_owner := 'wb_' || v_slug || '_owner';
    EXECUTE format('CREATE ROLE %I NOLOGIN', v_owner);
    -- lattis_service agit comme propriétaire (DDL, ALTER … OWNER).
    EXECUTE format('GRANT %I TO lattis_service WITH INHERIT TRUE, SET TRUE', v_owner);
    PERFORM lattis.create_workbook_schema('wb_' || v_slug, v_owner);

    INSERT INTO lattis.workbooks (id, slug, schema_name, title, mode, created_by)
    VALUES (v_id, v_slug, 'wb_' || v_slug, trim(p_title), p_mode, session_user);

    FOR v_role IN
        SELECT * FROM (VALUES ('owner', 'Propriétaire', 1), ('designer', 'Concepteur', 2), ('editor', 'Éditeur', 3),
                              ('participant', 'Participant', 4), ('reader', 'Lecteur', 5)) AS r(builtin, label, ord)
        WHERE p_mode = 'collaborative' OR builtin IN ('owner', 'reader')
        ORDER BY ord
    LOOP
        INSERT INTO lattis.roles (workbook_id, name, builtin, pg_role)
        VALUES (v_id, v_role.label, v_role.builtin, 'wb_' || v_slug || '_r_' || v_role.builtin)
        RETURNING id INTO v_rid;
        EXECUTE format('CREATE ROLE %I NOLOGIN', 'wb_' || v_slug || '_r_' || v_role.builtin);
        EXECUTE format('GRANT USAGE ON SCHEMA %I TO %I', 'wb_' || v_slug, 'wb_' || v_slug || '_r_' || v_role.builtin);
        -- Niveau par défaut sur tout le classeur.
        INSERT INTO lattis.role_grants (role_id, node_kind, node_id, level)
        VALUES (v_rid, 'workbook', v_id::text, CASE v_role.builtin
            WHEN 'reader' THEN 'reader' WHEN 'participant' THEN 'participant' ELSE 'editor' END);
    END LOOP;

    PERFORM lattis.add_member(v_id, 'owner', v_user.id);
    PERFORM lattis.audit_log('create_workbook', v_id::text, NULL,
        jsonb_build_object('title', p_title, 'mode', p_mode, 'schema', 'wb_' || v_slug));
    RETURN v_id;
END
$$;
