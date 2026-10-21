-- Recherche de personnes et invitations (décidé par Louis le 21/10/2026) :
-- tout champ qui vise une personne cherche par nom ou adresse parmi les
-- comptes du serveur, avec en dernier recours « Inviter à rejoindre » et les
-- droits à donner, préenregistrés jusqu'à l'inscription. L'e-mail part si le
-- serveur sait en envoyer ; sinon le lien d'inscription est à transmettre.

-- Invitation à une équipe (en plus des invitations à un classeur).
ALTER TABLE lattis.invitations ADD COLUMN team_id uuid REFERENCES lattis.teams (id) ON DELETE CASCADE;

-- Recherche (tout utilisateur) : comptes actifs déjà connectés une fois.
CREATE FUNCTION lattis.search_people(p_query text, p_limit integer DEFAULT 10) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_q text := lower(trim(coalesce(p_query, '')));
BEGIN
    IF NOT EXISTS (SELECT 1 FROM lattis.users WHERE pg_role = session_user AND NOT disabled) AND NOT lattis.is_server_owner() THEN
        RAISE EXCEPTION 'réservé aux utilisateurs Lattis' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF length(v_q) < 2 THEN
        RETURN '[]';
    END IF;
    v_q := replace(replace(replace(v_q, '\', '\\'), '%', '\%'), '_', '\_');
    RETURN (SELECT coalesce(jsonb_agg(jsonb_build_object('id', id, 'display_name', display_name, 'email', email)
                                      ORDER BY lower(display_name), email), '[]')
            FROM (SELECT u.id, u.display_name, u.email FROM lattis.users u
                  WHERE NOT u.disabled AND u.subject IS NOT NULL AND u.email NOT LIKE '%@lattis.invalid'
                    AND (u.email LIKE '%' || v_q || '%' OR lower(u.display_name) LIKE '%' || v_q || '%')
                  ORDER BY lower(u.display_name), u.email
                  LIMIT least(greatest(coalesce(p_limit, 10), 1), 20)) x);
END
$$;

-- Lien d'inscription et envoi : réponse commune des invitations.
CREATE FUNCTION lattis.invitation_result(p_id uuid, p_link text) RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT jsonb_build_object('invitation_id', i.id,
        'status', CASE WHEN i.accepted_at IS NOT NULL THEN 'granted' ELSE 'invited' END,
        'mailed', i.accepted_at IS NULL AND coalesce((lattis.config('mail_enabled') #>> '{}')::boolean, false),
        'link_path', p_link)
    FROM lattis.invitations i WHERE i.id = p_id
$$;

-- Invitation à un classeur avec ses rôles (propriétaires du classeur) :
-- compte existant, rôles donnés tout de suite ; sinon invitation en attente.
CREATE FUNCTION lattis.invite_person(p_email text, p_workbook_id uuid, p_roles text[]) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_id uuid;
BEGIN
    IF p_workbook_id IS NULL THEN
        RAISE EXCEPTION 'classeur manquant' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    v_id := lattis.invite(p_email, p_workbook_id, p_roles);
    RETURN lattis.invitation_result(v_id, '/w/' || p_workbook_id);
END
$$;

-- Invitation à une équipe (gestionnaires de l'équipe, propriétaires) : la
-- personne y est inscrite dès maintenant (compte d'invitation), et la
-- retrouve à sa première connexion.
CREATE FUNCTION lattis.invite_to_team(p_email text, p_team_id uuid) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_team lattis.teams := lattis.team(p_team_id);
    v_user lattis.users;
    v_id   uuid;
BEGIN
    PERFORM lattis.require_team_manager(p_team_id);
    p_email := lower(trim(coalesce(p_email, '')));
    IF p_email !~ '^[^@\s]+@[^@\s]+\.[^@\s]+$' OR p_email LIKE '%@lattis.invalid' THEN
        RAISE EXCEPTION 'adresse e-mail invalide' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    SELECT * INTO v_user FROM lattis.users
    WHERE email = p_email AND subject IS NOT NULL AND NOT disabled ORDER BY last_login_at DESC NULLS LAST LIMIT 1;
    INSERT INTO lattis.invitations (email, team_id, roles, invited_by, expires_at, accepted_at)
    VALUES (p_email, p_team_id, '{}', lattis.actor(),
            now() + make_interval(secs => (lattis.config('invitation_ttl') #>> '{}')::double precision),
            CASE WHEN v_user.id IS NOT NULL THEN now() END)
    RETURNING id INTO v_id;
    IF v_user.id IS NULL THEN
        SELECT * INTO v_user FROM lattis.users WHERE email = p_email AND subject IS NULL ORDER BY created_at LIMIT 1;
        IF v_user.id IS NULL THEN
            v_user := lattis.create_user(p_email, '', NULL, NULL);
        END IF;
    END IF;
    PERFORM lattis.add_team_member(p_team_id, v_user.id);
    PERFORM lattis.audit_log('invite_to_team', v_id::text, NULL, jsonb_build_object('email', p_email, 'team_id', p_team_id));
    RETURN lattis.invitation_result(v_id, '/');
END
$$;

-- Invitations en attente d'un classeur (propriétaires du classeur).
CREATE FUNCTION lattis.workbook_invitations(p_workbook_id uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_workbook_role(p_workbook_id, '{owner}', true);
    RETURN (SELECT coalesce(jsonb_agg(jsonb_build_object('id', id, 'email', email, 'roles', roles, 'invited_by', invited_by,
                                                         'created_at', created_at, 'expires_at', expires_at) ORDER BY created_at DESC), '[]')
            FROM lattis.invitations
            WHERE workbook_id = p_workbook_id AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now());
END
$$;

-- Invitations en attente d'une équipe (gestionnaires de l'équipe).
CREATE FUNCTION lattis.team_invitations(p_team_id uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_team_manager(p_team_id);
    RETURN (SELECT coalesce(jsonb_agg(jsonb_build_object('id', id, 'email', email, 'invited_by', invited_by,
                                                         'created_at', created_at, 'expires_at', expires_at) ORDER BY created_at DESC), '[]')
            FROM lattis.invitations
            WHERE team_id = p_team_id AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now());
END
$$;

-- Annulation : propriétaires du classeur, gestionnaires de l'équipe, ou
-- propriétaires du serveur pour une invitation au serveur. Le compte
-- d'invitation sans autre invitation en attente disparaît (avec ses
-- inscriptions aux équipes).
CREATE OR REPLACE FUNCTION lattis.revoke_invitation(p_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_inv  lattis.invitations;
    v_user lattis.users;
BEGIN
    SELECT * INTO v_inv FROM lattis.invitations WHERE id = p_id AND accepted_at IS NULL AND revoked_at IS NULL;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'invitation inconnue ou déjà acceptée' USING ERRCODE = 'no_data_found';
    END IF;
    IF v_inv.workbook_id IS NOT NULL THEN
        PERFORM lattis.require_workbook_role(v_inv.workbook_id, '{owner}', true);
    ELSIF v_inv.team_id IS NOT NULL THEN
        PERFORM lattis.require_team_manager(v_inv.team_id);
    ELSE
        PERFORM lattis.require_server_owner();
    END IF;
    UPDATE lattis.invitations SET revoked_at = now() WHERE id = p_id;
    IF v_inv.team_id IS NOT NULL THEN
        DELETE FROM lattis.team_members m USING lattis.users u
        WHERE m.team_id = v_inv.team_id AND m.user_id = u.id AND u.email = v_inv.email AND u.subject IS NULL;
    END IF;
    FOR v_user IN
        SELECT * FROM lattis.users
        WHERE email = v_inv.email AND subject IS NULL AND last_login_at IS NULL
          AND NOT EXISTS (SELECT 1 FROM lattis.invitations
                          WHERE email = v_inv.email AND accepted_at IS NULL AND revoked_at IS NULL)
    LOOP
        DELETE FROM lattis.users WHERE id = v_user.id;
        EXECUTE format('DROP ROLE IF EXISTS %I', v_user.pg_role);
    END LOOP;
    PERFORM lattis.audit_log('revoke_invitation', p_id::text, to_jsonb(v_inv), NULL);
END
$$;

-- E-mail d'invitation : classeur ou équipe.
CREATE OR REPLACE FUNCTION lattis.trg_invitation_mail() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_title text;
BEGIN
    IF NEW.accepted_at IS NOT NULL THEN
        RETURN NEW;
    END IF;
    IF NEW.workbook_id IS NOT NULL THEN
        SELECT title INTO v_title FROM lattis.workbooks WHERE id = NEW.workbook_id AND kind = 'workbook';
        IF v_title IS NOT NULL THEN
            PERFORM lattis.mail_enqueue('invitation', NEW.email, jsonb_build_object(
                'workbook_id', NEW.workbook_id, 'title', v_title, 'by', NEW.invited_by,
                'server', lattis.config('display_name') #>> '{}'));
        END IF;
    ELSIF NEW.team_id IS NOT NULL THEN
        SELECT name INTO v_title FROM lattis.teams WHERE id = NEW.team_id;
        PERFORM lattis.mail_enqueue('invitation', NEW.email, jsonb_build_object(
            'team', v_title, 'by', NEW.invited_by, 'server', lattis.config('display_name') #>> '{}'));
    END IF;
    RETURN NEW;
END
$$;

REVOKE EXECUTE ON FUNCTION lattis.invitation_result(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION lattis.search_people(text, integer), lattis.invite_person(text, uuid, text[]),
    lattis.invite_to_team(text, uuid), lattis.workbook_invitations(uuid), lattis.team_invitations(uuid) TO PUBLIC;
