-- Rôle de serveur « créateur de classeurs » : quand la création est réservée
-- (workbook_creators = owners), ses membres créent des classeurs comme les
-- propriétaires du serveur, sans les autres droits d'administration.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'lattis_workbook_creators') THEN
        CREATE ROLE lattis_workbook_creators NOLOGIN;
    END IF;
END
$$;

-- session_user peut-il créer un classeur ?
CREATE FUNCTION lattis.can_create_workbooks() RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT lattis.config('workbook_creators') #>> '{}' = 'everyone'
        OR lattis.is_server_owner()
        OR pg_has_role(session_user, 'lattis_workbook_creators', 'MEMBER')
$$;

-- Donne ou retire le rôle à un utilisateur, même avant sa première connexion.
CREATE FUNCTION lattis.set_workbook_creator(p_user_id uuid, p_on boolean) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_role text;
BEGIN
    PERFORM lattis.require_server_owner();
    SELECT pg_role INTO v_role FROM lattis.users WHERE id = p_user_id;
    IF v_role IS NULL THEN
        RAISE EXCEPTION 'utilisateur inconnu' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF p_on THEN
        EXECUTE format('GRANT lattis_workbook_creators TO %I', v_role);
    ELSIF pg_has_role(v_role, 'lattis_workbook_creators', 'MEMBER') THEN
        EXECUTE format('REVOKE lattis_workbook_creators FROM %I', v_role);
    END IF;
    PERFORM lattis.audit_log(CASE WHEN p_on THEN 'grant_workbook_creator' ELSE 'revoke_workbook_creator' END,
        p_user_id::text, NULL, NULL);
END
$$;

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

    v_slug := lattis.slugify(p_title);
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

CREATE OR REPLACE FUNCTION lattis.login_summary() RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT jsonb_build_object(
        'server', jsonb_build_object(
            'display_name', lattis.config('display_name'),
            'access_mode', lattis.config('access_mode')),
        'user', (SELECT jsonb_build_object('id', u.id, 'email', u.email, 'display_name', u.display_name,
                                           'pg_role', u.pg_role, 'last_login_at', u.last_login_at)
                 FROM lattis.users u WHERE u.pg_role = session_user),
        'server_owner', pg_has_role(session_user, 'lattis_server_owners', 'MEMBER'),
        'workbook_creator', pg_has_role(session_user, 'lattis_workbook_creators', 'MEMBER'),
        'can_create_workbooks', lattis.can_create_workbooks(),
        'workbooks', lattis.my_workbooks())
$$;

GRANT EXECUTE ON FUNCTION lattis.can_create_workbooks(), lattis.set_workbook_creator(uuid, boolean) TO PUBLIC;
