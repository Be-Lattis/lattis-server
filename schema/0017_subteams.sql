-- Sous-équipes (demande de Louis, 18/10) : une équipe peut appartenir à une
-- équipe parente, sur autant de niveaux qu'on veut.
--
-- Le rôle PostgreSQL d'une sous-équipe est membre de celui de sa parente
-- (GRANT parente TO sous-équipe) : un rôle de classeur donné à une équipe
-- vaut donc pour toutes ses sous-équipes, et pg_has_role (MEMBER) suit la
-- chaîne. lattis.my_teams() rend ainsi aussi les équipes parentes, sans
-- changement. Les cycles sont refusés.

ALTER TABLE lattis.teams ADD COLUMN parent_id uuid REFERENCES lattis.teams (id) ON DELETE RESTRICT;
CREATE INDEX teams_parent ON lattis.teams (parent_id);
GRANT SELECT (parent_id) ON lattis.teams TO PUBLIC;

-- Fermeture : (ancêtre, descendant, profondeur), l'équipe elle-même comprise.
CREATE FUNCTION lattis.team_closure() RETURNS TABLE (ancestor uuid, descendant uuid, depth integer)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    WITH RECURSIVE c(ancestor, descendant, depth) AS (
        SELECT id, id, 0 FROM lattis.teams
        UNION ALL
        SELECT c.ancestor, t.id, c.depth + 1 FROM c JOIN lattis.teams t ON t.parent_id = c.descendant
    )
    SELECT ancestor, descendant, depth FROM c
$$;
REVOKE EXECUTE ON FUNCTION lattis.team_closure() FROM PUBLIC;

-- Place une équipe sous une autre (NULL : au premier niveau).
CREATE FUNCTION lattis.set_team_parent(p_team_id uuid, p_parent_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v     lattis.teams := lattis.team(p_team_id);
    v_new lattis.teams;
    v_old lattis.teams;
BEGIN
    PERFORM lattis.require_server_owner();
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

CREATE FUNCTION lattis.create_team(p_name text, p_parent_id uuid) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_id uuid := lattis.create_team(p_name);
BEGIN
    IF p_parent_id IS NOT NULL THEN
        PERFORM lattis.set_team_parent(v_id, p_parent_id);
    END IF;
    RETURN v_id;
END
$$;

-- Une équipe qui a des sous-équipes ne se supprime pas : les déplacer d'abord.
CREATE OR REPLACE FUNCTION lattis.drop_team(p_team_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v lattis.teams := lattis.team(p_team_id);
BEGIN
    PERFORM lattis.require_server_owner();
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

-- « *acl » vers les classeurs où l'équipe ou l'un de ses ancêtres a un rôle.
CREATE FUNCTION lattis.notify_team_workbooks(p_team_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_slug text;
BEGIN
    FOR v_slug IN SELECT DISTINCT w.slug FROM lattis.team_closure() c
                  JOIN lattis.team_roles tr ON tr.team_id = c.ancestor
                  JOIN lattis.roles r ON r.id = tr.role_id JOIN lattis.workbooks w ON w.id = r.workbook_id
                  WHERE c.descendant = p_team_id LOOP
        PERFORM pg_notify('wb_' || v_slug, '*acl');
    END LOOP;
END
$$;
REVOKE EXECUTE ON FUNCTION lattis.notify_team_workbooks(uuid) FROM PUBLIC;

CREATE OR REPLACE FUNCTION lattis.trg_team_acl_notify() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.notify_team_workbooks(CASE WHEN TG_OP = 'DELETE' THEN OLD.team_id ELSE NEW.team_id END);
    RETURN NULL;
END
$$;

-- Équipes avec leur parente et leur profondeur (pour l'arborescence), dans
-- l'ordre de l'arbre ; membres directs pour les propriétaires du serveur.
CREATE OR REPLACE FUNCTION lattis.list_teams() RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    WITH RECURSIVE tree(id, depth, path) AS (
        SELECT id, 0, ARRAY[lower(name) || '/' || id::text] FROM lattis.teams WHERE parent_id IS NULL
        UNION ALL
        SELECT t.id, tree.depth + 1, tree.path || (lower(t.name) || '/' || t.id::text)
        FROM tree JOIN lattis.teams t ON t.parent_id = tree.id
    )
    SELECT coalesce(jsonb_agg(jsonb_build_object('id', t.id, 'name', t.name, 'parent_id', t.parent_id, 'depth', tree.depth,
               'path', (SELECT string_agg(a.name, ' › ' ORDER BY c.depth DESC) FROM lattis.team_closure() c
                        JOIN lattis.teams a ON a.id = c.ancestor WHERE c.descendant = t.id),
               'members', CASE WHEN lattis.is_server_owner() THEN
                   (SELECT coalesce(jsonb_agg(jsonb_build_object('user_id', u.id, 'email', u.email,
                                                                 'display_name', u.display_name) ORDER BY u.email), '[]')
                    FROM lattis.team_members m JOIN lattis.users u ON u.id = m.user_id WHERE m.team_id = t.id) END,
               'member_count', (SELECT count(*) FROM lattis.team_members m WHERE m.team_id = t.id),
               'subteam_count', (SELECT count(*) FROM lattis.teams s WHERE s.parent_id = t.id))
           ORDER BY tree.path), '[]')
    FROM tree JOIN lattis.teams t ON t.id = tree.id
$$;

-- workbook_members : une personne est membre d'un rôle donné à une équipe si
-- elle est dans cette équipe ou dans l'une de ses sous-équipes.
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
                        JOIN lattis.team_closure() c ON c.ancestor = tr.team_id
                        JOIN lattis.team_members tm ON tm.team_id = c.descendant) a
                  JOIN lattis.roles r ON r.id = a.role_id
                  WHERE r.workbook_id = v.id GROUP BY a.principal) x
            JOIN lattis.users u ON u.id = x.principal);
END
$$;

GRANT EXECUTE ON FUNCTION lattis.set_team_parent(uuid, uuid), lattis.create_team(text, uuid) TO PUBLIC;
