-- Gestionnaires d'équipe (demande de Louis, 21/10) : une administration web
-- réduite, sans rien du serveur (réglages, connexion, état, surveillance,
-- journal).
--
-- Un propriétaire du serveur nomme des gestionnaires sur une équipe. Leur
-- périmètre : cette équipe et toutes ses sous-équipes, et les personnes qui
-- en sont membres ou gestionnaires. Dans ce périmètre, un gestionnaire :
--   - gère les équipes (membres, sous-équipes, noms, gestionnaires) ; il ne
--     déplace ni ne supprime l'équipe qu'on lui a confiée, et n'y retire
--     d'autre gestionnaire que lui-même ;
--   - donne ou retire les habilitations jusqu'à la sienne incluse :
--     gestionnaire d'équipe, créateur de classeurs, publieur au Data Lake ;
--     jamais « propriétaire du serveur », et rien sur un propriétaire du
--     serveur ni sur lui-même.
-- Classeurs, Data Lake et formulaires : ceux dont il a déjà les droits
-- (propriétaire du classeur, concepteur pour les formulaires, données qu'il
-- gère) ; les fonctions de ces écrans vérifiaient déjà ces droits.
-- Choix prudent : pas de création de compte, de réinitialisation de mot de
-- passe, de désactivation ni d'invitation au serveur.

CREATE TABLE lattis.team_managers (
    team_id  uuid NOT NULL REFERENCES lattis.teams (id) ON DELETE CASCADE,
    user_id  uuid NOT NULL REFERENCES lattis.users (id) ON DELETE CASCADE,
    added_by text NOT NULL,
    added_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, user_id)
);
CREATE INDEX team_managers_user ON lattis.team_managers (user_id);
REVOKE ALL ON lattis.team_managers FROM PUBLIC;

-- Périmètre --------------------------------------------------------------------

-- Équipes gérées par l'utilisateur courant, sous-équipes comprises.
CREATE FUNCTION lattis.managed_teams() RETURNS SETOF uuid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT DISTINCT c.descendant FROM lattis.team_managers m
    JOIN lattis.users u ON u.id = m.user_id AND u.pg_role = session_user AND NOT u.disabled
    JOIN lattis.team_closure() c ON c.ancestor = m.team_id
$$;

CREATE FUNCTION lattis.is_team_manager() RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT EXISTS (SELECT 1 FROM lattis.team_managers m
                   JOIN lattis.users u ON u.id = m.user_id AND u.pg_role = session_user AND NOT u.disabled)
$$;

CREATE FUNCTION lattis.can_manage_team(p_team_id uuid) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT lattis.is_server_owner() OR p_team_id IN (SELECT lattis.managed_teams())
$$;

-- Vrai si l'utilisateur gère une équipe strictement au-dessus de celle-ci :
-- il peut alors la déplacer, la supprimer ou en retirer les gestionnaires.
CREATE FUNCTION lattis.manages_above(p_team_id uuid) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT lattis.is_server_owner() OR EXISTS (
        SELECT 1 FROM lattis.team_managers m
        JOIN lattis.users u ON u.id = m.user_id AND u.pg_role = session_user AND NOT u.disabled
        JOIN lattis.team_closure() c ON c.ancestor = m.team_id AND c.descendant = p_team_id AND c.depth > 0)
$$;

CREATE FUNCTION lattis.require_team_manager(p_team_id uuid) RETURNS void
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.team(p_team_id);
    IF NOT lattis.can_manage_team(p_team_id) THEN
        RAISE EXCEPTION 'réservé aux propriétaires du serveur et aux gestionnaires de cette équipe'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
END
$$;

-- Personnes du périmètre : membres et gestionnaires des équipes gérées.
CREATE FUNCTION lattis.managed_users() RETURNS SETOF uuid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT tm.user_id FROM lattis.team_members tm WHERE tm.team_id IN (SELECT lattis.managed_teams())
    UNION
    SELECT mg.user_id FROM lattis.team_managers mg WHERE mg.team_id IN (SELECT lattis.managed_teams())
$$;

-- Habilitations d'une personne : un propriétaire du serveur pour tous ; un
-- gestionnaire pour les personnes de son périmètre, sauf lui-même et les
-- propriétaires du serveur.
CREATE FUNCTION lattis.require_user_manager(p_user_id uuid) RETURNS lattis.users
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v lattis.users;
BEGIN
    SELECT * INTO v FROM lattis.users WHERE id = p_user_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'utilisateur inconnu' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF lattis.is_server_owner() THEN
        RETURN v;
    END IF;
    IF v.pg_role = session_user OR pg_has_role(v.pg_role, 'lattis_server_owners', 'MEMBER')
       OR p_user_id NOT IN (SELECT lattis.managed_users()) THEN
        RAISE EXCEPTION 'réservé aux propriétaires du serveur et aux gestionnaires des équipes de cette personne'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN v;
END
$$;

-- Administration web : propriétaire du serveur ou gestionnaire d'équipe.
CREATE FUNCTION lattis.require_admin() RETURNS void
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    IF NOT (lattis.is_server_owner() OR lattis.is_team_manager()) THEN
        RAISE EXCEPTION 'réservé aux propriétaires du serveur et aux gestionnaires d''équipe'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
END
$$;

-- Accès de l'utilisateur courant à l'administration web : owner,
-- team_manager ou NULL.
CREATE FUNCTION lattis.admin_access() RETURNS text
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT CASE WHEN lattis.is_server_owner() THEN 'owner' WHEN lattis.is_team_manager() THEN 'team_manager' END
$$;

-- Compte d'une adresse (le plus récemment connecté, adresse vérifiée
-- d'abord), pour ajouter quelqu'un par son adresse : {id, verified} ou NULL.
CREATE FUNCTION lattis.find_user(p_email text) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_admin();
    RETURN (SELECT jsonb_build_object('id', id, 'verified', email_verified) FROM lattis.users
            WHERE email = lower(trim(p_email))
            ORDER BY email_verified DESC, last_login_at DESC NULLS LAST LIMIT 1);
END
$$;

-- Équipes ----------------------------------------------------------------------

-- Création sans contrôle (appelée après vérification des droits).
CREATE FUNCTION lattis.create_team_unchecked(p_name text) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_id uuid := gen_random_uuid();
    v_pg text := 'lattis_team_' || replace(v_id::text, '-', '');
BEGIN
    p_name := trim(coalesce(p_name, ''));
    IF EXISTS (SELECT 1 FROM lattis.teams WHERE lower(name) = lower(p_name)) THEN
        RAISE EXCEPTION 'une équipe « % » existe déjà', p_name USING ERRCODE = 'unique_violation';
    END IF;
    EXECUTE format('CREATE ROLE %I NOLOGIN', v_pg);
    INSERT INTO lattis.teams (id, name, pg_role, created_by) VALUES (v_id, p_name, v_pg, lattis.actor());
    PERFORM lattis.audit_log('create_team', v_id::text, NULL, jsonb_build_object('name', p_name));
    RETURN v_id;
END
$$;

-- Déplacement sans contrôle des droits (cycles toujours refusés).
CREATE FUNCTION lattis.move_team_unchecked(p_team_id uuid, p_parent_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v     lattis.teams := lattis.team(p_team_id);
    v_new lattis.teams;
    v_old lattis.teams;
BEGIN
    IF v.parent_id IS NOT DISTINCT FROM p_parent_id THEN
        RETURN;
    END IF;
    IF p_parent_id IS NOT NULL THEN
        v_new := lattis.team(p_parent_id);
        IF EXISTS (SELECT 1 FROM lattis.team_closure() WHERE ancestor = p_team_id AND descendant = p_parent_id) THEN
            RAISE EXCEPTION 'une équipe ne peut pas être placée sous elle-même ni sous l''une de ses sous-équipes'
                USING ERRCODE = 'invalid_parameter_value';
        END IF;
    END IF;
    -- Classeurs touchés avant et après : ceux des anciens et des nouveaux ancêtres.
    PERFORM lattis.notify_team_workbooks(p_team_id);
    IF v.parent_id IS NOT NULL THEN
        SELECT * INTO v_old FROM lattis.teams WHERE id = v.parent_id;
        EXECUTE format('REVOKE %I FROM %I', v_old.pg_role, v.pg_role);
    END IF;
    UPDATE lattis.teams SET parent_id = p_parent_id WHERE id = p_team_id;
    IF p_parent_id IS NOT NULL THEN
        EXECUTE format('GRANT %I TO %I', v_new.pg_role, v.pg_role);
    END IF;
    PERFORM lattis.notify_team_workbooks(p_team_id);
    PERFORM lattis.audit_log('set_team_parent', p_team_id::text,
        jsonb_build_object('parent_id', v.parent_id), jsonb_build_object('parent_id', p_parent_id));
END
$$;

-- Une équipe de premier niveau : propriétaires du serveur ; une sous-équipe :
-- gestionnaires de l'équipe parente aussi.
CREATE OR REPLACE FUNCTION lattis.create_team(p_name text, p_parent_id uuid) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_id uuid;
BEGIN
    IF p_parent_id IS NULL THEN
        PERFORM lattis.require_server_owner();
    ELSE
        PERFORM lattis.require_team_manager(p_parent_id);
    END IF;
    v_id := lattis.create_team_unchecked(p_name);
    IF p_parent_id IS NOT NULL THEN
        PERFORM lattis.move_team_unchecked(v_id, p_parent_id);
    END IF;
    RETURN v_id;
END
$$;

CREATE OR REPLACE FUNCTION lattis.create_team(p_name text) RETURNS uuid
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT lattis.create_team(p_name, NULL::uuid)
$$;

-- Déplacer : propriétaires du serveur ; gestionnaire au-dessus de l'équipe,
-- vers une équipe de son périmètre.
CREATE OR REPLACE FUNCTION lattis.set_team_parent(p_team_id uuid, p_parent_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.team(p_team_id);
    IF NOT lattis.is_server_owner() THEN
        IF NOT lattis.manages_above(p_team_id) THEN
            RAISE EXCEPTION 'seuls les propriétaires du serveur et les gestionnaires d''une équipe parente déplacent cette équipe'
                USING ERRCODE = 'insufficient_privilege';
        END IF;
        IF p_parent_id IS NULL THEN
            RAISE EXCEPTION 'seuls les propriétaires du serveur placent une équipe au premier niveau'
                USING ERRCODE = 'insufficient_privilege';
        END IF;
        PERFORM lattis.require_team_manager(p_parent_id);
    END IF;
    PERFORM lattis.move_team_unchecked(p_team_id, p_parent_id);
END
$$;

CREATE OR REPLACE FUNCTION lattis.rename_team(p_team_id uuid, p_name text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v lattis.teams := lattis.team(p_team_id);
BEGIN
    PERFORM lattis.require_team_manager(p_team_id);
    p_name := trim(coalesce(p_name, ''));
    IF EXISTS (SELECT 1 FROM lattis.teams WHERE lower(name) = lower(p_name) AND id <> p_team_id) THEN
        RAISE EXCEPTION 'une équipe « % » existe déjà', p_name USING ERRCODE = 'unique_violation';
    END IF;
    UPDATE lattis.teams SET name = p_name WHERE id = p_team_id;
    PERFORM lattis.audit_log('rename_team', p_team_id::text, jsonb_build_object('name', v.name), jsonb_build_object('name', p_name));
END
$$;

CREATE OR REPLACE FUNCTION lattis.drop_team(p_team_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v lattis.teams := lattis.team(p_team_id);
BEGIN
    IF NOT lattis.manages_above(p_team_id) THEN
        RAISE EXCEPTION 'seuls les propriétaires du serveur et les gestionnaires d''une équipe parente suppriment cette équipe'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF EXISTS (SELECT 1 FROM lattis.teams WHERE parent_id = p_team_id) THEN
        RAISE EXCEPTION 'l''équipe « % » a des sous-équipes : déplacez-les ou supprimez-les d''abord', v.name
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    PERFORM lattis.notify_team_workbooks(p_team_id);
    DELETE FROM lattis.teams WHERE id = p_team_id;
    EXECUTE format('DROP ROLE %I', v.pg_role);
    PERFORM lattis.audit_log('drop_team', p_team_id::text, jsonb_build_object('name', v.name), NULL);
END
$$;

CREATE OR REPLACE FUNCTION lattis.add_team_member(p_team_id uuid, p_user_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v      lattis.teams := lattis.team(p_team_id);
    v_user text;
BEGIN
    PERFORM lattis.require_team_manager(p_team_id);
    SELECT pg_role INTO v_user FROM lattis.users WHERE id = p_user_id;
    IF v_user IS NULL THEN
        RAISE EXCEPTION 'utilisateur inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    INSERT INTO lattis.team_members (team_id, user_id, added_by) VALUES (p_team_id, p_user_id, lattis.actor())
    ON CONFLICT DO NOTHING;
    EXECUTE format('GRANT %I TO %I', v.pg_role, v_user);
    PERFORM lattis.audit_log('add_team_member', p_team_id::text, NULL, jsonb_build_object('user_id', p_user_id));
END
$$;

CREATE OR REPLACE FUNCTION lattis.remove_team_member(p_team_id uuid, p_user_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v      lattis.teams := lattis.team(p_team_id);
    v_user text;
BEGIN
    PERFORM lattis.require_team_manager(p_team_id);
    SELECT pg_role INTO v_user FROM lattis.users WHERE id = p_user_id;
    DELETE FROM lattis.team_members WHERE team_id = p_team_id AND user_id = p_user_id;
    IF v_user IS NOT NULL THEN
        EXECUTE format('REVOKE %I FROM %I', v.pg_role, v_user);
    END IF;
    PERFORM lattis.audit_log('remove_team_member', p_team_id::text, jsonb_build_object('user_id', p_user_id), NULL);
END
$$;

-- Nomme ou retire un gestionnaire. Sur l'équipe qu'on lui a confiée, un
-- gestionnaire en ajoute d'autres mais ne retire que lui-même.
CREATE FUNCTION lattis.set_team_manager(p_team_id uuid, p_user_id uuid, p_on boolean) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_user lattis.users;
BEGIN
    PERFORM lattis.require_team_manager(p_team_id);
    SELECT * INTO v_user FROM lattis.users WHERE id = p_user_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'utilisateur inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    IF p_on THEN
        IF v_user.disabled THEN
            RAISE EXCEPTION 'compte désactivé' USING ERRCODE = 'object_not_in_prerequisite_state';
        END IF;
        INSERT INTO lattis.team_managers (team_id, user_id, added_by) VALUES (p_team_id, p_user_id, lattis.actor())
        ON CONFLICT DO NOTHING;
    ELSE
        IF v_user.pg_role <> session_user AND NOT lattis.manages_above(p_team_id) THEN
            RAISE EXCEPTION 'seuls les propriétaires du serveur et les gestionnaires d''une équipe parente retirent un gestionnaire de cette équipe'
                USING ERRCODE = 'insufficient_privilege';
        END IF;
        DELETE FROM lattis.team_managers WHERE team_id = p_team_id AND user_id = p_user_id;
    END IF;
    PERFORM lattis.audit_log(CASE WHEN p_on THEN 'grant_team_manager' ELSE 'revoke_team_manager' END,
        p_team_id::text, NULL, jsonb_build_object('user_id', p_user_id));
END
$$;

-- Équipes avec leur parente et leur profondeur ; membres directs et
-- gestionnaires pour qui peut gérer l'équipe (« manageable »).
CREATE OR REPLACE FUNCTION lattis.list_teams() RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    WITH RECURSIVE tree(id, depth, path) AS (
        SELECT id, 0, ARRAY[lower(name) || '/' || id::text] FROM lattis.teams WHERE parent_id IS NULL
        UNION ALL
        SELECT t.id, tree.depth + 1, tree.path || (lower(t.name) || '/' || t.id::text)
        FROM tree JOIN lattis.teams t ON t.parent_id = tree.id
    ), mine AS (
        SELECT t.id, lattis.can_manage_team(t.id) AS manageable, lattis.manages_above(t.id) AS above FROM lattis.teams t
    )
    SELECT coalesce(jsonb_agg(jsonb_build_object('id', t.id, 'name', t.name, 'parent_id', t.parent_id, 'depth', tree.depth,
               'path', (SELECT string_agg(a.name, ' › ' ORDER BY c.depth DESC) FROM lattis.team_closure() c
                        JOIN lattis.teams a ON a.id = c.ancestor WHERE c.descendant = t.id),
               'manageable', mine.manageable, 'movable', mine.above,
               'members', CASE WHEN mine.manageable THEN
                   (SELECT coalesce(jsonb_agg(jsonb_build_object('user_id', u.id, 'email', u.email,
                                                                 'display_name', u.display_name) ORDER BY u.email), '[]')
                    FROM lattis.team_members m JOIN lattis.users u ON u.id = m.user_id WHERE m.team_id = t.id) END,
               'managers', CASE WHEN mine.manageable THEN
                   (SELECT coalesce(jsonb_agg(jsonb_build_object('user_id', u.id, 'email', u.email,
                                                                 'display_name', u.display_name, 'me', u.pg_role = session_user)
                                              ORDER BY u.email), '[]')
                    FROM lattis.team_managers m JOIN lattis.users u ON u.id = m.user_id WHERE m.team_id = t.id) END,
               'member_count', (SELECT count(*) FROM lattis.team_members m WHERE m.team_id = t.id),
               'subteam_count', (SELECT count(*) FROM lattis.teams s WHERE s.parent_id = t.id))
           ORDER BY tree.path), '[]')
    FROM tree JOIN lattis.teams t ON t.id = tree.id JOIN mine ON mine.id = t.id
$$;

-- Habilitations -------------------------------------------------------------------

CREATE OR REPLACE FUNCTION lattis.set_workbook_creator(p_user_id uuid, p_on boolean) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_role text := (lattis.require_user_manager(p_user_id)).pg_role;
BEGIN
    IF p_on THEN
        EXECUTE format('GRANT lattis_workbook_creators TO %I', v_role);
    ELSIF pg_has_role(v_role, 'lattis_workbook_creators', 'MEMBER') THEN
        EXECUTE format('REVOKE lattis_workbook_creators FROM %I', v_role);
    END IF;
    PERFORM lattis.audit_log(CASE WHEN p_on THEN 'grant_workbook_creator' ELSE 'revoke_workbook_creator' END,
        p_user_id::text, NULL, NULL);
END
$$;

CREATE OR REPLACE FUNCTION lattis.set_data_publisher(p_user_id uuid, p_on boolean) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_role text := (lattis.require_user_manager(p_user_id)).pg_role;
BEGIN
    IF p_on THEN
        EXECUTE format('GRANT lattis_data_publishers TO %I', v_role);
    ELSIF pg_has_role(v_role, 'lattis_data_publishers', 'MEMBER') THEN
        EXECUTE format('REVOKE lattis_data_publishers FROM %I', v_role);
    END IF;
    PERFORM lattis.audit_log(CASE WHEN p_on THEN 'grant_data_publisher' ELSE 'revoke_data_publisher' END,
        p_user_id::text, NULL, NULL);
END
$$;

-- Écran Utilisateurs : tous les comptes pour les propriétaires du serveur,
-- les personnes du périmètre pour un gestionnaire (« manageable » : ses
-- habilitations peuvent changer). Invitations au serveur : propriétaires.
CREATE FUNCTION lattis.admin_users(p_q text) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_owner boolean := lattis.is_server_owner();
BEGIN
    PERFORM lattis.require_admin();
    p_q := trim(coalesce(p_q, ''));
    RETURN jsonb_build_object(
        'users', (SELECT coalesce(jsonb_agg(jsonb_build_object(
            'id', u.id, 'email', u.email, 'display_name', u.display_name, 'provider', p.label, 'kind', p.kind,
            'created_at', u.created_at, 'last_login_at', u.last_login_at, 'disabled', u.disabled,
            'pending', u.subject IS NULL AND u.last_login_at IS NULL,
            'server_owner', pg_has_role(u.pg_role, 'lattis_server_owners', 'MEMBER'),
            'workbook_creator', pg_has_role(u.pg_role, 'lattis_workbook_creators', 'MEMBER'),
            'data_publisher', pg_has_role(u.pg_role, 'lattis_data_publishers', 'MEMBER'),
            'team_manager', (SELECT coalesce(jsonb_agg(t.name ORDER BY lower(t.name)), '[]') FROM lattis.team_managers m
                             JOIN lattis.teams t ON t.id = m.team_id WHERE m.user_id = u.id),
            'manageable', v_owner OR (u.pg_role <> session_user AND NOT pg_has_role(u.pg_role, 'lattis_server_owners', 'MEMBER')),
            'email_verified', u.email_verified,
            'link_expires_at', lattis.setup_link_expiry(u.id),
            'link_expired', lattis.setup_link_expiry(u.id) < now())
            ORDER BY u.email), '[]')
            FROM (SELECT * FROM lattis.users x
                  WHERE (p_q = '' OR x.email ILIKE '%' || p_q || '%' OR x.display_name ILIKE '%' || p_q || '%')
                    AND (v_owner OR x.id IN (SELECT lattis.managed_users()))
                  ORDER BY x.email LIMIT 500) u
            LEFT JOIN lattis.identity_providers p ON p.id = u.provider_id),
        'invitations', CASE WHEN v_owner THEN (SELECT coalesce(jsonb_agg(jsonb_build_object('id', i.id, 'email', i.email, 'roles', i.roles,
            'workbook', w.title, 'invited_by', i.invited_by, 'expires_at', i.expires_at) ORDER BY i.created_at DESC), '[]')
            FROM lattis.invitations i LEFT JOIN lattis.workbooks w ON w.id = i.workbook_id
            WHERE i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > now()) ELSE '[]' END,
        'creators', coalesce(lattis.config('workbook_creators') #>> '{}', 'everyone'));
END
$$;

-- Classeurs et formulaires : dans le périmètre de l'administrateur ------------------

-- Tous les classeurs pour les propriétaires du serveur ; ceux dont on est
-- propriétaire pour un gestionnaire d'équipe.
CREATE OR REPLACE FUNCTION lattis.admin_workbooks() RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_admin();
    RETURN (SELECT coalesce(jsonb_agg(jsonb_build_object(
            'id', w.id, 'title', w.title, 'kind', w.kind, 'mode', w.mode, 'state', w.state, 'version', w.version,
            'schema', w.schema_name, 'created_by', w.created_by, 'created_at', w.created_at,
            'published_at', w.published_at,
            'last_edit', lattis.workbook_last_edit(w.id),
            'last_read', (SELECT jsonb_build_object('email', u.email, 'at', r.read_at)
                          FROM lattis.workbook_reads r JOIN lattis.users u ON u.id = r.user_id
                          WHERE r.workbook_id = w.id ORDER BY r.read_at DESC LIMIT 1),
            'owners', (SELECT coalesce(jsonb_agg(u.email ORDER BY u.email), '[]')
                       FROM lattis.roles r JOIN lattis.members m ON m.role_id = r.id
                       JOIN lattis.users u ON u.id = m.principal
                       WHERE r.workbook_id = w.id AND r.builtin = 'owner'),
            'members', (SELECT count(DISTINCT m.principal) FROM lattis.roles r
                        JOIN lattis.members m ON m.role_id = r.id WHERE r.workbook_id = w.id),
            'size', (SELECT coalesce(sum(pg_total_relation_size(c.oid)), 0) FROM pg_class c
                     JOIN pg_namespace n ON n.oid = c.relnamespace
                     WHERE n.nspname IN (w.schema_name, coalesce(w.staging_schema, '')) AND c.relkind IN ('r', 'p', 'm')))
        ORDER BY w.title), '[]') FROM lattis.workbooks w
        WHERE lattis.is_server_owner() OR lattis.has_workbook_role(w.id, '{owner}'));
END
$$;

-- Formulaires : tous pour les propriétaires du serveur ; ceux des classeurs
-- dont on est propriétaire ou concepteur (lattis.form_manage) sinon.
CREATE OR REPLACE FUNCTION lattis.admin_forms() RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_admin();
    RETURN (SELECT coalesce(jsonb_agg(jsonb_build_object(
                'id', f.id, 'code', f.code, 'url', lattis.form_url(f.code), 'title', f.definition #>> '{appearance,title}',
                'workbook_id', f.workbook_id, 'workbook', w.title,
                'table', (SELECT t.name FROM lattis.data_tables(w.schema_name) t WHERE t.table_id = f.table_id),
                'state', f.state, 'pause_reason', f.pause_reason, 'access', f.access, 'created_by', f.created_by,
                'responses', (SELECT count(*) FROM lattis.form_submissions s WHERE s.form_id = f.id),
                'last_day', (SELECT count(*) FROM lattis.form_submissions s WHERE s.form_id = f.id AND s.submitted_at > now() - interval '1 day'),
                'last_response_at', (SELECT max(submitted_at) FROM lattis.form_submissions s WHERE s.form_id = f.id),
                'daily_limit', f.daily_limit)
            ORDER BY w.title, f.created_at), '[]')
            FROM lattis.forms f JOIN lattis.workbooks w ON w.id = f.workbook_id
            WHERE lattis.is_server_owner() OR lattis.has_workbook_role(f.workbook_id, '{owner,designer}'));
END
$$;

REVOKE EXECUTE ON FUNCTION lattis.managed_teams(), lattis.is_team_manager(), lattis.can_manage_team(uuid),
    lattis.manages_above(uuid), lattis.require_team_manager(uuid), lattis.managed_users(),
    lattis.require_user_manager(uuid), lattis.require_admin(), lattis.create_team_unchecked(text),
    lattis.move_team_unchecked(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION lattis.admin_access(), lattis.find_user(text), lattis.set_team_manager(uuid, uuid, boolean),
    lattis.admin_users(text) TO PUBLIC;

-- Pipelines : ceux des classeurs dont on est propriétaire ---------------------------

-- Pipeline administré par l'utilisateur courant : tous pour les
-- propriétaires du serveur ; pour un gestionnaire d'équipe, ceux qui
-- écrivent dans un classeur dont il est propriétaire.
CREATE FUNCTION lattis.can_manage_pipeline(p_id uuid) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT lattis.is_server_owner() OR (lattis.is_team_manager() AND EXISTS (
        SELECT 1 FROM lattis.pipelines x WHERE x.id = p_id AND lattis.has_workbook_role(x.workbook_id, '{owner}')))
$$;

CREATE FUNCTION lattis.require_pipeline_admin(p_id uuid) RETURNS lattis.pipelines
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    x lattis.pipelines;
BEGIN
    SELECT * INTO x FROM lattis.pipelines WHERE id = p_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'pipeline inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    IF NOT lattis.can_manage_pipeline(p_id) THEN
        RAISE EXCEPTION 'réservé aux propriétaires du serveur et aux gestionnaires d''équipe propriétaires du classeur'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN x;
END
$$;

-- Ce qu'un gestionnaire d'équipe ne peut pas faire lire au serveur : un
-- fichier du dossier des secrets (seulement un secret saisi en valeur), et
-- un fichier hors du dossier de dépôt du pipeline.
CREATE FUNCTION lattis.pipeline_check_manager(p jsonb) RETURNS void
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    IF lattis.is_server_owner() THEN
        RETURN;
    END IF;
    IF coalesce(p ->> 'secret_ref', '') <> '' AND (p ->> 'secret_ref') !~ '^db-[0-9a-f]{32}$' THEN
        RAISE EXCEPTION 'un fichier du dossier des secrets est réservé aux propriétaires du serveur : saisissez le secret en valeur'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF coalesce(p #>> '{source,pattern}', '') ~ '[/\\]|\.\.' THEN
        RAISE EXCEPTION 'motif de fichier : un nom simple, sans dossier' USING ERRCODE = 'invalid_parameter_value';
    END IF;
END
$$;

CREATE OR REPLACE FUNCTION lattis.create_pipeline(p jsonb) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_id   uuid := gen_random_uuid();
    v      lattis.workbooks;
    v_tab  record;
    v_user lattis.users;
    v_role uuid;
BEGIN
    PERFORM lattis.require_admin();
    v := lattis.require_workbook_role((p ->> 'workbook_id')::uuid, '{owner}', true);
    PERFORM lattis.pipeline_check_manager(p);
    IF v.mode <> 'collaborative' OR v.state <> 'published' THEN
        RAISE EXCEPTION 'un pipeline écrit dans un classeur collaboratif publié' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    EXECUTE format('SELECT table_id, name, local_id FROM %I.lattis_meta_tables WHERE table_id = $1', v.schema_name)
        INTO v_tab USING (p ->> 'table_id')::uuid;
    IF v_tab.table_id IS NULL THEN
        RAISE EXCEPTION 'tableau inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    PERFORM lattis.pipeline_check(p, v, v_tab.name);
    SELECT id INTO v_role FROM lattis.roles WHERE workbook_id = v.id AND builtin = coalesce(nullif(p ->> 'role', ''), 'editor');
    IF v_role IS NULL THEN
        RAISE EXCEPTION 'rôle « % » inexistant sur ce classeur', p ->> 'role' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    -- Identité technique : un compte sans fournisseur, jamais connecté en personne.
    v_user := lattis.create_user('pipeline+' || left(replace(v_id::text, '-', ''), 12) || '@lattis.invalid',
                                 'Pipeline : ' || trim(p ->> 'name'), NULL, NULL);
    PERFORM lattis.activate_user(v_user.id);
    INSERT INTO lattis.pipelines (id, name, workbook_id, table_local_id, table_id, table_name, source_kind, source, secret_ref,
                                  format, mode, key_columns, column_map, schedule_kind, schedule_minutes, schedule_time,
                                  webhook_url, user_id, created_by)
    VALUES (v_id, trim(p ->> 'name'), v.id, v_tab.local_id, v_tab.table_id, v_tab.name, p ->> 'source_kind',
            coalesce(p -> 'source', '{}'), nullif(p ->> 'secret_ref', ''), coalesce(nullif(p ->> 'format', ''), 'csv'), p ->> 'mode',
            ARRAY(SELECT jsonb_array_elements_text(coalesce(p -> 'key_columns', '[]'))), coalesce(p -> 'column_map', '{}'),
            coalesce(nullif(p ->> 'schedule_kind', ''), 'manual'), (nullif(p ->> 'schedule_minutes', ''))::integer,
            nullif(p ->> 'schedule_time', ''), nullif(p ->> 'webhook_url', ''), v_user.id, lattis.actor());
    PERFORM lattis.grant_role(v_role, v_user.id);
    PERFORM lattis.audit_log('create_pipeline', v_id::text, NULL, p);
    RETURN v_id;
END
$$;

CREATE OR REPLACE FUNCTION lattis.update_pipeline(p_id uuid, p jsonb) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    x     lattis.pipelines;
    v     lattis.workbooks;
    v_key text;
BEGIN
    PERFORM lattis.require_pipeline_admin(p_id);
    PERFORM lattis.pipeline_check_manager(p);
    SELECT * INTO x FROM lattis.pipelines WHERE id = p_id FOR UPDATE;
    FOR v_key IN SELECT jsonb_object_keys(p) LOOP
        IF v_key NOT IN ('name', 'source_kind', 'source', 'secret_ref', 'format', 'mode', 'key_columns', 'column_map',
                         'schedule_kind', 'schedule_minutes', 'schedule_time', 'webhook_url', 'enabled') THEN
            RAISE EXCEPTION 'champ inconnu : %', v_key USING ERRCODE = 'invalid_parameter_value';
        END IF;
    END LOOP;
    v := lattis.workbook(x.workbook_id);
    PERFORM lattis.pipeline_check(p, v, x.table_name);
    UPDATE lattis.pipelines SET
        name             = CASE WHEN p ? 'name' THEN trim(p ->> 'name') ELSE name END,
        source_kind      = CASE WHEN p ? 'source_kind' THEN p ->> 'source_kind' ELSE source_kind END,
        source           = CASE WHEN p ? 'source' THEN coalesce(p -> 'source', '{}') ELSE source END,
        secret_ref       = CASE WHEN p ? 'secret_ref' THEN nullif(p ->> 'secret_ref', '') ELSE secret_ref END,
        format           = CASE WHEN p ? 'format' THEN p ->> 'format' ELSE format END,
        mode             = CASE WHEN p ? 'mode' THEN p ->> 'mode' ELSE mode END,
        key_columns      = CASE WHEN p ? 'key_columns' THEN ARRAY(SELECT jsonb_array_elements_text(p -> 'key_columns')) ELSE key_columns END,
        column_map       = CASE WHEN p ? 'column_map' THEN coalesce(p -> 'column_map', '{}') ELSE column_map END,
        schedule_kind    = CASE WHEN p ? 'schedule_kind' THEN p ->> 'schedule_kind' ELSE schedule_kind END,
        schedule_minutes = CASE WHEN p ? 'schedule_minutes' THEN (nullif(p ->> 'schedule_minutes', ''))::integer ELSE schedule_minutes END,
        schedule_time    = CASE WHEN p ? 'schedule_time' THEN nullif(p ->> 'schedule_time', '') ELSE schedule_time END,
        webhook_url      = CASE WHEN p ? 'webhook_url' THEN nullif(p ->> 'webhook_url', '') ELSE webhook_url END,
        enabled          = CASE WHEN p ? 'enabled' THEN (p ->> 'enabled')::boolean ELSE enabled END,
        next_run_at      = NULL, -- recalculé par lattisd
        updated_at       = now()
    WHERE id = p_id;
    PERFORM lattis.audit_log('update_pipeline', p_id::text, to_jsonb(x), p);
END
$$;

CREATE OR REPLACE FUNCTION lattis.drop_pipeline(p_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_pipeline_admin(p_id);
    PERFORM lattis.drop_pipeline_as(p_id);
END
$$;

CREATE OR REPLACE FUNCTION lattis.run_pipeline(p_id uuid, p_file text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_pipeline_admin(p_id);
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

CREATE OR REPLACE FUNCTION lattis.set_pipeline_alert_email(p_id uuid, p_email text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_pipeline_admin(p_id);
    UPDATE lattis.pipelines SET alert_email = nullif(lower(trim(p_email)), ''), updated_at = now() WHERE id = p_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'pipeline inconnu' USING ERRCODE = 'no_data_found';
    END IF;
END
$$;

CREATE OR REPLACE FUNCTION lattis.admin_pipelines() RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_admin();
    RETURN (SELECT coalesce(jsonb_agg(to_jsonb(x) || jsonb_build_object(
                'workbook', w.title, 'workbook_kind', w.kind, 'identity', u.email,
                'datasets', (SELECT coalesce(jsonb_agg(jsonb_build_object('id', d.id, 'name', d.name, 'sql_name', d.sql_name)), '[]')
                             FROM lattis.datasets d WHERE d.workbook_id = w.id AND d.table_id = x.table_id),
                'runs', (SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY r.started_at DESC), '[]')
                         FROM (SELECT * FROM lattis.pipeline_runs WHERE pipeline_id = x.id ORDER BY started_at DESC LIMIT 10) r))
            ORDER BY lower(x.name)), '[]')
            FROM lattis.pipelines x JOIN lattis.workbooks w ON w.id = x.workbook_id JOIN lattis.users u ON u.id = x.user_id
            WHERE lattis.is_server_owner() OR lattis.has_workbook_role(x.workbook_id, '{owner}'));
END
$$;

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
    PERFORM lattis.require_admin();
    FOR w IN SELECT * FROM lattis.workbooks WHERE mode = 'collaborative' AND state = 'published' AND kind IN ('workbook', 'source')
               AND (lattis.is_server_owner() OR lattis.has_workbook_role(id, '{owner}'))
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

CREATE OR REPLACE FUNCTION lattis.store_secret(p_name text, p_key_id text, p_sealed bytea, p_old text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_admin();
    INSERT INTO lattis.stored_secrets (name, key_id, sealed) VALUES (p_name, p_key_id, p_sealed);
    -- Gestionnaire d'équipe : il ne remplace que le secret d'un de ses pipelines.
    IF p_old IS NOT NULL AND p_old <> p_name AND (lattis.is_server_owner() OR EXISTS (
            SELECT 1 FROM lattis.pipelines x WHERE x.secret_ref = p_old AND lattis.has_workbook_role(x.workbook_id, '{owner}'))) THEN
        DELETE FROM lattis.stored_secrets WHERE name = p_old;
    END IF;
    PERFORM lattis.audit_log('store_secret', p_name, NULL, NULL);
END
$$;

CREATE OR REPLACE FUNCTION lattis.create_datalake_target(p jsonb) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_name text := trim(coalesce(p ->> 'name', ''));
    v_cols jsonb;
    v_wb   uuid;
    v_tid  uuid;
    v_ds   uuid;
BEGIN
    -- Gestionnaire d'équipe : s'il peut créer des classeurs et publier au Data Lake.
    IF NOT (lattis.is_server_owner() OR (lattis.is_team_manager() AND lattis.can_create_workbooks() AND lattis.can_publish_data())) THEN
        RAISE EXCEPTION 'réservé aux propriétaires du serveur et aux gestionnaires d''équipe créateurs de classeurs et publieurs au Data Lake'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
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

REVOKE EXECUTE ON FUNCTION lattis.require_pipeline_admin(uuid), lattis.pipeline_check_manager(jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION lattis.can_manage_pipeline(uuid) TO PUBLIC;
