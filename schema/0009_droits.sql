-- Droits (retours de l'application, docs/collaboration/droits.md §4) :
--  * create_table reçoit la feuille du tableau (niveaux par feuille) ;
--  * invite accepte les rôles personnalisés (par leur nom) ;
--  * workbook_members : membres lisibles par les concepteurs (« Voir en tant que… ») ;
--  * my_capabilities : read_filtered, write_filtered (filtres et restrictions) et sheet_id.

-- Tableau rattaché à une feuille : set_level('sheet', …) s'applique à lui.
CREATE FUNCTION lattis.create_table(p_workbook_id uuid, p_name text, p_columns jsonb, p_sheet_id text) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_id  uuid := lattis.create_table(p_workbook_id, p_name, p_columns);
    v     lattis.workbooks := lattis.workbook(p_workbook_id);
BEGIN
    IF nullif(p_sheet_id, '') IS NULL THEN
        RETURN v_id;
    END IF;
    -- Brouillon ou version de travail : le tableau vient d'y être créé.
    EXECUTE format('UPDATE %I.lattis_meta_tables SET sheet_id = $1 WHERE table_id = $2',
                   coalesce(v.staging_schema, v.schema_name))
        USING p_sheet_id, v_id;
    PERFORM lattis.compile_acl(p_workbook_id);
    RETURN v_id;
END
$$;

GRANT EXECUTE ON FUNCTION lattis.create_table(uuid, text, jsonb, text) TO PUBLIC;

-- Rôles d'une invitation : rôles fournis (owner, designer…) ou noms de rôles
-- personnalisés du classeur.
CREATE OR REPLACE FUNCTION lattis.apply_roles(p_user_id uuid, p_workbook_id uuid, p_roles text[]) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_user text;
    v_name text;
    v_role lattis.roles;
BEGIN
    SELECT pg_role INTO STRICT v_user FROM lattis.users WHERE id = p_user_id;
    IF p_workbook_id IS NULL THEN
        IF 'server_owner' = ANY (p_roles) THEN
            EXECUTE format('GRANT lattis_server_owners TO %I', v_user);
        END IF;
        RETURN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM lattis.workbooks WHERE id = p_workbook_id) THEN
        RETURN;
    END IF;
    FOREACH v_name IN ARRAY p_roles LOOP
        IF EXISTS (SELECT 1 FROM lattis.roles WHERE workbook_id = p_workbook_id AND builtin = v_name) THEN
            PERFORM lattis.add_member(p_workbook_id, v_name, p_user_id);
            CONTINUE;
        END IF;
        SELECT * INTO v_role FROM lattis.roles
        WHERE workbook_id = p_workbook_id AND builtin IS NULL AND lower(name) = lower(v_name);
        IF FOUND THEN
            INSERT INTO lattis.members (role_id, principal, granted_by) VALUES (v_role.id, p_user_id, lattis.actor())
            ON CONFLICT DO NOTHING;
            EXECUTE format('GRANT %I TO %I', v_role.pg_role, v_user);
        END IF;
    END LOOP;
END
$$;

-- Invitation : rôles fournis ou personnalisés.
CREATE OR REPLACE FUNCTION lattis.invite(p_email text, p_workbook_id uuid, p_roles text[]) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_id   uuid;
    v_user lattis.users;
    v_bad  text;
BEGIN
    p_email := lower(trim(p_email));
    p_roles := coalesce(p_roles, '{}');
    IF p_workbook_id IS NULL THEN
        PERFORM lattis.require_server_owner();
        IF EXISTS (SELECT 1 FROM unnest(p_roles) r WHERE r <> 'server_owner') THEN
            RAISE EXCEPTION 'rôle de serveur inconnu (seul server_owner existe)' USING ERRCODE = 'invalid_parameter_value';
        END IF;
    ELSE
        PERFORM lattis.require_workbook_role(p_workbook_id, '{owner}', true);
        IF cardinality(p_roles) = 0 THEN
            RAISE EXCEPTION 'au moins un rôle' USING ERRCODE = 'invalid_parameter_value';
        END IF;
        SELECT r INTO v_bad FROM unnest(p_roles) r
        WHERE NOT EXISTS (SELECT 1 FROM lattis.roles WHERE workbook_id = p_workbook_id
                          AND (builtin = r OR (builtin IS NULL AND lower(name) = lower(r)))) LIMIT 1;
        IF v_bad IS NOT NULL THEN
            RAISE EXCEPTION 'rôle « % » inexistant sur ce classeur', v_bad USING ERRCODE = 'invalid_parameter_value';
        END IF;
        -- Sur invitation, inviter à un classeur vaut invitation au serveur ;
        -- en entrée libre aussi. Rien à vérifier de plus.
    END IF;

    INSERT INTO lattis.invitations (email, workbook_id, roles, invited_by, expires_at)
    VALUES (p_email, p_workbook_id, p_roles, lattis.actor(),
            now() + make_interval(secs => (lattis.config('invitation_ttl') #>> '{}')::double precision))
    RETURNING id INTO v_id;

    IF NOT EXISTS (SELECT 1 FROM lattis.users WHERE email = p_email) THEN
        v_user := lattis.create_user(p_email, '', NULL, NULL);
    END IF;

    SELECT * INTO v_user FROM lattis.users
    WHERE email = p_email AND subject IS NOT NULL AND last_login_at IS NOT NULL AND NOT disabled
    ORDER BY last_login_at DESC LIMIT 1;
    IF FOUND THEN
        PERFORM lattis.apply_roles(v_user.id, p_workbook_id, p_roles);
        UPDATE lattis.invitations SET accepted_at = now() WHERE id = v_id;
    END IF;

    PERFORM lattis.audit_log('invite', v_id::text, NULL,
        jsonb_build_object('email', p_email, 'workbook_id', p_workbook_id, 'roles', p_roles));
    RETURN v_id;
END
$$;

-- Membres d'un classeur, pour ses propriétaires et concepteurs (choix d'un
-- membre dans « Voir en tant que… ») : sans invitations ni restrictions.
CREATE FUNCTION lattis.workbook_members(p_workbook_id uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', true);
BEGIN
    RETURN (SELECT coalesce(jsonb_agg(jsonb_build_object('user_id', u.id, 'email', u.email, 'display_name', u.display_name,
                                                         'pg_role', u.pg_role, 'roles', x.roles) ORDER BY u.email), '[]')
            FROM (SELECT m.principal, jsonb_agg(coalesce(r.builtin, r.name) ORDER BY r.name) AS roles
                  FROM lattis.members m JOIN lattis.roles r ON r.id = m.role_id
                  WHERE r.workbook_id = v.id GROUP BY m.principal) x
            JOIN lattis.users u ON u.id = x.principal);
END
$$;

GRANT EXECUTE ON FUNCTION lattis.workbook_members(uuid) TO PUBLIC;

-- Capacités : read_filtered et write_filtered (filtres de niveau et
-- restrictions qui me visent), feuille du tableau. « filtered » reste.
CREATE OR REPLACE FUNCTION lattis.my_capabilities(p_workbook_id uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v       lattis.workbooks := lattis.workbook(p_workbook_id);
    v_roles text[] := lattis.my_builtin_roles(p_workbook_id);
    v_mine  uuid[];
    v_tab   record;
    v_role  lattis.roles;
    g       lattis.role_grants;
    v_best  text;
    v_rank  integer;
    v_filt  boolean;
    v_rf    boolean;
    v_wf    boolean;
    v_out   jsonb := '{}';
BEGIN
    SELECT coalesce(array_agg(id), '{}') INTO v_mine FROM lattis.roles
    WHERE workbook_id = p_workbook_id AND pg_has_role(session_user, pg_role, 'MEMBER');
    IF cardinality(v_mine) = 0 THEN
        RAISE EXCEPTION 'aucun accès à ce classeur' USING ERRCODE = 'insufficient_privilege';
    END IF;
    FOR v_tab IN SELECT * FROM lattis.data_tables(v.schema_name) LOOP
        v_filt := false;
        v_rf := false;
        v_wf := false;
        IF v.mode = 'frozen' THEN
            v_best := 'reader';
        ELSE
            v_rank := 0;
            v_best := 'none';
            FOR v_role IN SELECT * FROM lattis.roles WHERE id = ANY (v_mine) LOOP
                g := lattis.effective_grant(v_role, v_tab.table_id, v_tab.sheet_id);
                IF array_position(ARRAY['none', 'reader', 'participant', 'editor'], g.level) > v_rank THEN
                    v_rank := array_position(ARRAY['none', 'reader', 'participant', 'editor'], g.level);
                    v_best := g.level;
                END IF;
                v_filt := v_filt OR coalesce(g.read_filter, g.write_filter) IS NOT NULL;
                v_rf := v_rf OR g.read_filter IS NOT NULL;
                v_wf := v_wf OR g.write_filter IS NOT NULL;
            END LOOP;
            -- Restrictions qui me visent (« tous » : hors propriétaires et concepteurs).
            v_rf := v_rf OR EXISTS (SELECT 1 FROM lattis.restrictions r
                WHERE r.workbook_id = p_workbook_id AND r.table_id = v_tab.table_id AND r.command IN ('select', 'all')
                  AND (r.applies_to && v_mine OR (cardinality(r.applies_to) = 0 AND NOT v_roles && '{owner,designer}')));
            v_wf := v_wf OR EXISTS (SELECT 1 FROM lattis.restrictions r
                WHERE r.workbook_id = p_workbook_id AND r.table_id = v_tab.table_id AND r.command <> 'select'
                  AND (r.applies_to && v_mine OR (cardinality(r.applies_to) = 0 AND NOT v_roles && '{owner,designer}')));
        END IF;
        v_out := v_out || jsonb_build_object(v_tab.table_id::text,
            jsonb_build_object('name', v_tab.name, 'sheet_id', v_tab.sheet_id, 'level', v_best, 'filtered', v_filt,
                               'read_filtered', v_rf, 'write_filtered', v_wf));
    END LOOP;
    RETURN jsonb_build_object('workbook_id', v.id, 'title', v.title, 'mode', v.mode, 'version', v.version, 'schema', v.schema_name,
        'channel', 'wb_' || v.slug, 'roles', v_roles,
        'custom_roles', (SELECT coalesce(jsonb_agg(name ORDER BY name), '[]') FROM lattis.roles
                         WHERE id = ANY (v_mine) AND builtin IS NULL),
        'can_design', v_roles && '{owner,designer}', 'can_manage', 'owner' = ANY (v_roles),
        'tables', v_out,
        'restrictions', (SELECT coalesce(jsonb_agg(jsonb_build_object('table_id', r.table_id, 'command', r.command,
                                                                      'label', r.label) ORDER BY r.label), '[]')
                         FROM lattis.restrictions r
                         WHERE r.workbook_id = p_workbook_id AND (cardinality(r.applies_to) = 0 OR r.applies_to && v_mine)));
END
$$;

