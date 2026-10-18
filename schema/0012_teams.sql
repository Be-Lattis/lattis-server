-- Équipes Lattis (choix de Louis, 18/10) : créées par les propriétaires du
-- serveur, membres d'un rôle de classeur comme une personne, et filtre
-- « mon équipe » (lattis.my_teams()). Les groupes d'annuaire pourront les
-- alimenter plus tard.
--
-- Une équipe est un rôle PostgreSQL NOLOGIN donné à ses membres ; un rôle de
-- classeur donné à l'équipe passe à ses membres par héritage. pg_has_role
-- (MEMBER), qui décide de tous les droits, suit donc les équipes sans autre
-- changement, de même que les règles RLS.

CREATE TABLE lattis.teams (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL CHECK (length(trim(name)) BETWEEN 1 AND 200),
    pg_role    text NOT NULL UNIQUE,
    created_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX teams_name ON lattis.teams (lower(name));
-- Noms d'équipe lisibles par tous : my_teams() tourne dans les règles RLS,
-- sous le rôle de l'utilisateur. Les appartenances sont de toute façon
-- visibles dans pg_auth_members.
REVOKE ALL ON lattis.teams FROM PUBLIC;
GRANT SELECT (id, name, pg_role) ON lattis.teams TO PUBLIC;

CREATE TABLE lattis.team_members (
    team_id  uuid NOT NULL REFERENCES lattis.teams (id) ON DELETE CASCADE,
    user_id  uuid NOT NULL REFERENCES lattis.users (id) ON DELETE CASCADE,
    added_by text NOT NULL,
    added_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, user_id)
);
CREATE INDEX team_members_user ON lattis.team_members (user_id);
REVOKE ALL ON lattis.team_members FROM PUBLIC;

-- Rôles de classeur donnés à une équipe.
CREATE TABLE lattis.team_roles (
    role_id    uuid NOT NULL REFERENCES lattis.roles (id) ON DELETE CASCADE,
    team_id    uuid NOT NULL REFERENCES lattis.teams (id) ON DELETE CASCADE,
    granted_by text NOT NULL,
    granted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (role_id, team_id)
);
CREATE INDEX team_roles_team ON lattis.team_roles (team_id);
REVOKE ALL ON lattis.team_roles FROM PUBLIC;

-- Gestion (propriétaires du serveur) -------------------------------------------

CREATE FUNCTION lattis.team(p_team_id uuid) RETURNS lattis.teams
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v lattis.teams;
BEGIN
    SELECT * INTO v FROM lattis.teams WHERE id = p_team_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'équipe inconnue' USING ERRCODE = 'no_data_found';
    END IF;
    RETURN v;
END
$$;

CREATE FUNCTION lattis.create_team(p_name text) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_id uuid := gen_random_uuid();
    v_pg text := 'lattis_team_' || replace(v_id::text, '-', '');
BEGIN
    PERFORM lattis.require_server_owner();
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

CREATE FUNCTION lattis.rename_team(p_team_id uuid, p_name text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v lattis.teams := lattis.team(p_team_id);
BEGIN
    PERFORM lattis.require_server_owner();
    p_name := trim(coalesce(p_name, ''));
    IF EXISTS (SELECT 1 FROM lattis.teams WHERE lower(name) = lower(p_name) AND id <> p_team_id) THEN
        RAISE EXCEPTION 'une équipe « % » existe déjà', p_name USING ERRCODE = 'unique_violation';
    END IF;
    UPDATE lattis.teams SET name = p_name WHERE id = p_team_id;
    PERFORM lattis.audit_log('rename_team', p_team_id::text, jsonb_build_object('name', v.name), jsonb_build_object('name', p_name));
END
$$;

CREATE FUNCTION lattis.drop_team(p_team_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v lattis.teams := lattis.team(p_team_id);
BEGIN
    PERFORM lattis.require_server_owner();
    -- Les classeurs qui donnaient un rôle à l'équipe sont prévenus par le
    -- déclencheur de team_roles (suppression en cascade).
    DELETE FROM lattis.teams WHERE id = p_team_id;
    EXECUTE format('DROP ROLE %I', v.pg_role);
    PERFORM lattis.audit_log('drop_team', p_team_id::text, jsonb_build_object('name', v.name), NULL);
END
$$;

CREATE FUNCTION lattis.add_team_member(p_team_id uuid, p_user_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v      lattis.teams := lattis.team(p_team_id);
    v_user text;
BEGIN
    PERFORM lattis.require_server_owner();
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

CREATE FUNCTION lattis.remove_team_member(p_team_id uuid, p_user_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v      lattis.teams := lattis.team(p_team_id);
    v_user text;
BEGIN
    PERFORM lattis.require_server_owner();
    SELECT pg_role INTO v_user FROM lattis.users WHERE id = p_user_id;
    DELETE FROM lattis.team_members WHERE team_id = p_team_id AND user_id = p_user_id;
    IF v_user IS NOT NULL THEN
        EXECUTE format('REVOKE %I FROM %I', v.pg_role, v_user);
    END IF;
    PERFORM lattis.audit_log('remove_team_member', p_team_id::text, jsonb_build_object('user_id', p_user_id), NULL);
END
$$;

-- Équipes du serveur, pour choisir une équipe dans « Partager » (tout
-- utilisateur connecté), avec leurs membres pour les propriétaires du serveur.
CREATE FUNCTION lattis.list_teams() RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT coalesce(jsonb_agg(jsonb_build_object('id', t.id, 'name', t.name,
               'members', CASE WHEN lattis.is_server_owner() THEN
                   (SELECT coalesce(jsonb_agg(jsonb_build_object('user_id', u.id, 'email', u.email,
                                                                 'display_name', u.display_name) ORDER BY u.email), '[]')
                    FROM lattis.team_members m JOIN lattis.users u ON u.id = m.user_id WHERE m.team_id = t.id) END,
               'member_count', (SELECT count(*) FROM lattis.team_members m WHERE m.team_id = t.id))
           ORDER BY lower(t.name)), '[]')
    FROM lattis.teams t
$$;

-- « Mon équipe » dans un filtre : noms des équipes de l'utilisateur courant
-- (current_user : dans une règle RLS, l'utilisateur qui lit, y compris sous
-- « Voir en tant que… »). Forme conseillée, évaluée une fois par requête :
--   "Équipe" IN (SELECT lattis.my_teams())
CREATE FUNCTION lattis.my_teams() RETURNS SETOF text
LANGUAGE sql STABLE SECURITY INVOKER PARALLEL SAFE SET search_path = pg_catalog, pg_temp
AS $$
    SELECT t.name FROM lattis.teams t WHERE pg_has_role(current_user, t.pg_role, 'MEMBER') ORDER BY t.name
$$;

GRANT EXECUTE ON FUNCTION lattis.create_team(text), lattis.rename_team(uuid, text), lattis.drop_team(uuid),
    lattis.add_team_member(uuid, uuid), lattis.remove_team_member(uuid, uuid), lattis.list_teams(),
    lattis.my_teams() TO PUBLIC;

-- Membres d'un rôle : une personne ou une équipe -------------------------------

CREATE OR REPLACE FUNCTION lattis.grant_role(p_role_id uuid, p_principal uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_role lattis.roles;
    v_user text;
    v_team lattis.teams;
BEGIN
    SELECT * INTO v_role FROM lattis.roles WHERE id = p_role_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'rôle inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    PERFORM lattis.require_workbook_role(v_role.workbook_id, '{owner}', true);
    SELECT pg_role INTO v_user FROM lattis.users WHERE id = p_principal;
    IF v_user IS NOT NULL THEN
        INSERT INTO lattis.members (role_id, principal, granted_by) VALUES (p_role_id, p_principal, lattis.actor())
        ON CONFLICT DO NOTHING;
        EXECUTE format('GRANT %I TO %I', v_role.pg_role, v_user);
        PERFORM lattis.audit_log('grant_role', p_role_id::text, NULL, jsonb_build_object('principal', p_principal));
        RETURN;
    END IF;
    SELECT * INTO v_team FROM lattis.teams WHERE id = p_principal;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'utilisateur ou équipe inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    INSERT INTO lattis.team_roles (role_id, team_id, granted_by) VALUES (p_role_id, p_principal, lattis.actor())
    ON CONFLICT DO NOTHING;
    EXECUTE format('GRANT %I TO %I', v_role.pg_role, v_team.pg_role);
    PERFORM lattis.audit_log('grant_role', p_role_id::text, NULL, jsonb_build_object('team', p_principal));
END
$$;

CREATE OR REPLACE FUNCTION lattis.revoke_role(p_role_id uuid, p_principal uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_role lattis.roles;
    v_user text;
    v_team lattis.teams;
BEGIN
    SELECT * INTO v_role FROM lattis.roles WHERE id = p_role_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'rôle inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    PERFORM lattis.require_workbook_role(v_role.workbook_id, '{owner}', true);
    SELECT * INTO v_team FROM lattis.teams WHERE id = p_principal;
    IF FOUND THEN
        DELETE FROM lattis.team_roles WHERE role_id = p_role_id AND team_id = p_principal;
        EXECUTE format('REVOKE %I FROM %I', v_role.pg_role, v_team.pg_role);
        PERFORM lattis.audit_log('revoke_role', p_role_id::text, jsonb_build_object('team', p_principal), NULL);
        RETURN;
    END IF;
    -- Le dernier propriétaire en personne reste : une équipe propriétaire ne
    -- le remplace pas (on peut la vider).
    IF v_role.builtin = 'owner' AND (SELECT count(*) FROM lattis.members WHERE role_id = p_role_id) <= 1
       AND EXISTS (SELECT 1 FROM lattis.members WHERE role_id = p_role_id AND principal = p_principal) THEN
        RAISE EXCEPTION 'un classeur garde au moins un propriétaire' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    SELECT pg_role INTO v_user FROM lattis.users WHERE id = p_principal;
    DELETE FROM lattis.members WHERE role_id = p_role_id AND principal = p_principal;
    IF v_user IS NOT NULL THEN
        EXECUTE format('REVOKE %I FROM %I', v_role.pg_role, v_user);
    END IF;
    PERFORM lattis.audit_log('revoke_role', p_role_id::text, jsonb_build_object('principal', p_principal), NULL);
END
$$;

-- Notifications « *acl » : un rôle donné à une équipe ou retiré
-- (trg_acl_notify le relie au classeur par role_id), et un membre ajouté à
-- une équipe ou retiré (tous les classeurs où l'équipe a un rôle).
CREATE TRIGGER lattis_acl_notify AFTER INSERT OR UPDATE OR DELETE ON lattis.team_roles
    FOR EACH ROW EXECUTE FUNCTION lattis.trg_acl_notify();

CREATE FUNCTION lattis.trg_team_acl_notify() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_team uuid := CASE WHEN TG_OP = 'DELETE' THEN OLD.team_id ELSE NEW.team_id END;
    v_slug text;
BEGIN
    FOR v_slug IN SELECT DISTINCT w.slug FROM lattis.team_roles tr
                  JOIN lattis.roles r ON r.id = tr.role_id JOIN lattis.workbooks w ON w.id = r.workbook_id
                  WHERE tr.team_id = v_team LOOP
        PERFORM pg_notify('wb_' || v_slug, '*acl');
    END LOOP;
    RETURN NULL;
END
$$;

CREATE TRIGGER lattis_team_acl_notify AFTER INSERT OR DELETE ON lattis.team_members
    FOR EACH ROW EXECUTE FUNCTION lattis.trg_team_acl_notify();

-- Lectures : équipes des rôles, membres par équipe --------------------------------

-- workbook_acl : chaque rôle donne aussi ses équipes.
CREATE OR REPLACE FUNCTION lattis.workbook_acl(p_workbook_id uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner}', true);
BEGIN
    RETURN jsonb_build_object(
        'workbook', jsonb_build_object('id', v.id, 'title', v.title, 'mode', v.mode, 'state', v.state,
                                       'version', v.version, 'schema', v.schema_name),
        'tables', (SELECT coalesce(jsonb_agg(jsonb_build_object('table_id', t.table_id, 'name', t.name,
                                                                'sheet_id', t.sheet_id)), '[]')
                   FROM lattis.data_tables(v.schema_name) t),
        'roles', (SELECT coalesce(jsonb_agg(jsonb_build_object(
                     'id', r.id, 'name', r.name, 'builtin', r.builtin,
                     'grants', (SELECT coalesce(jsonb_agg(jsonb_build_object('node_kind', g.node_kind, 'node_id', g.node_id,
                                    'level', g.level, 'read_filter', g.read_filter, 'write_filter', g.write_filter)
                                    ORDER BY g.node_kind DESC, g.node_id), '[]')
                                FROM lattis.role_grants g WHERE g.role_id = r.id),
                     'members', (SELECT coalesce(jsonb_agg(jsonb_build_object('user_id', u.id, 'email', u.email,
                                    'display_name', u.display_name) ORDER BY u.email), '[]')
                                 FROM lattis.members m JOIN lattis.users u ON u.id = m.principal WHERE m.role_id = r.id),
                     'teams', (SELECT coalesce(jsonb_agg(jsonb_build_object('team_id', t.id, 'name', t.name)
                                    ORDER BY lower(t.name)), '[]')
                               FROM lattis.team_roles tr JOIN lattis.teams t ON t.id = tr.team_id WHERE tr.role_id = r.id))
                  ORDER BY array_position(ARRAY['owner', 'designer', 'editor', 'participant', 'reader'], r.builtin),
                           r.created_at, r.name), '[]')
                  FROM lattis.roles r WHERE r.workbook_id = p_workbook_id),
        'restrictions', (SELECT coalesce(jsonb_agg(to_jsonb(x) - 'workbook_id' ORDER BY x.label), '[]') FROM lattis.restrictions x
                         WHERE x.workbook_id = p_workbook_id),
        'invitations', (SELECT coalesce(jsonb_agg(jsonb_build_object('id', i.id, 'email', i.email, 'roles', i.roles,
                                                                     'expires_at', i.expires_at) ORDER BY i.email), '[]')
                        FROM lattis.invitations i WHERE i.workbook_id = p_workbook_id
                          AND i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > now()));
END
$$;

-- workbook_members : les membres par une équipe aussi (« Voir en tant que… »
-- choisit une personne). « teams » : équipes par lesquelles la personne est
-- membre ; « roles » : tous ses rôles, en personne ou par une équipe.
CREATE OR REPLACE FUNCTION lattis.workbook_members(p_workbook_id uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', true);
BEGIN
    RETURN (SELECT coalesce(jsonb_agg(jsonb_build_object('user_id', u.id, 'email', u.email, 'display_name', u.display_name,
                                                         'pg_role', u.pg_role, 'roles', x.roles, 'teams', x.teams)
                                      ORDER BY u.email), '[]')
            FROM (SELECT a.principal,
                         jsonb_agg(DISTINCT coalesce(r.builtin, r.name)) AS roles,
                         coalesce(jsonb_agg(DISTINCT a.team) FILTER (WHERE a.team IS NOT NULL), '[]') AS teams
                  FROM (SELECT m.role_id, m.principal, NULL::text AS team FROM lattis.members m
                        UNION ALL
                        SELECT tr.role_id, tm.user_id, t.name FROM lattis.team_roles tr
                        JOIN lattis.teams t ON t.id = tr.team_id
                        JOIN lattis.team_members tm ON tm.team_id = tr.team_id) a
                  JOIN lattis.roles r ON r.id = a.role_id
                  WHERE r.workbook_id = v.id GROUP BY a.principal) x
            JOIN lattis.users u ON u.id = x.principal);
END
$$;
