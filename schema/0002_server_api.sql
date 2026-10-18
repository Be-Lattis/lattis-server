-- API SQL du serveur (spécification §9.1) et inscription des utilisateurs (§7.4).
--
-- Toutes les fonctions sont SECURITY DEFINER (propriétaire lattis_service),
-- avec un search_path fixé, et vérifient les droits de session_user.
-- lattisd appelle les fonctions réservées au service sous lattis_service ;
-- l'interface d'administration les appelle sous l'identité de l'utilisateur.

-- Auteur d'une action : session_user, ou l'utilisateur pour le compte duquel
-- lattisd agit (réglage lattis.actor, pris en compte seulement pour lattis_service).
CREATE FUNCTION lattis.actor() RETURNS text
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT CASE
        WHEN session_user = 'lattis_service'
            THEN coalesce(nullif(current_setting('lattis.actor', true), ''), session_user::text)
        ELSE session_user::text
    END
$$;

CREATE FUNCTION lattis.is_server_owner() RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT session_user = 'lattis_service'
        OR pg_has_role(session_user, 'lattis_server_owners', 'MEMBER')
$$;

CREATE FUNCTION lattis.require_server_owner() RETURNS void
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    IF NOT lattis.is_server_owner() THEN
        RAISE EXCEPTION 'réservé aux propriétaires du serveur'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
END
$$;

CREATE FUNCTION lattis.require_service() RETURNS void
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    IF session_user <> 'lattis_service' THEN
        RAISE EXCEPTION 'réservé à lattisd' USING ERRCODE = 'insufficient_privilege';
    END IF;
END
$$;

CREATE FUNCTION lattis.audit_log(p_action text, p_target text, p_before jsonb, p_after jsonb) RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    INSERT INTO lattis.audit (actor, action, target, before, after)
    VALUES (lattis.actor(), p_action, p_target, p_before, p_after)
$$;

CREATE FUNCTION lattis.config(p_key text) RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT value FROM lattis.server_config WHERE key = p_key
$$;

-- Nom du rôle PostgreSQL d'un utilisateur : son adresse quand elle est
-- utilisable telle quelle (63 octets, préfixe pg_ réservé, nom libre),
-- sinon u_<identifiant>.
CREATE FUNCTION lattis.role_name_for(p_email text, p_user_id uuid) RETURNS text
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT CASE
        WHEN octet_length(p_email) BETWEEN 3 AND 63
             AND p_email NOT LIKE 'pg\_%'
             AND NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = p_email)
            THEN p_email
        ELSE 'u_' || replace(p_user_id::text, '-', '')
    END
$$;

-- Crée l'utilisateur et son rôle PostgreSQL, sans droit de connexion tant que
-- la première connexion n'a pas eu lieu (lattis.activate_user).
CREATE FUNCTION lattis.create_user(p_email text, p_display_name text, p_provider_id uuid, p_subject text)
RETURNS lattis.users
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_id   uuid := gen_random_uuid();
    v_role text;
    v_user lattis.users;
BEGIN
    p_email := lower(trim(p_email));
    IF p_email !~ '^[^@\s]+@[^@\s]+$' THEN
        RAISE EXCEPTION 'adresse invalide : %', p_email USING ERRCODE = 'invalid_parameter_value';
    END IF;
    v_role := lattis.role_name_for(p_email, v_id);
    EXECUTE format('CREATE ROLE %I NOLOGIN', v_role);
    INSERT INTO lattis.users (id, pg_role, email, display_name, provider_id, subject)
    VALUES (v_id, v_role, p_email, coalesce(p_display_name, ''), p_provider_id, p_subject)
    RETURNING * INTO v_user;
    RETURN v_user;
END
$$;

CREATE FUNCTION lattis.activate_user(p_user_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_role text;
BEGIN
    SELECT pg_role INTO v_role FROM lattis.users WHERE id = p_user_id AND NOT disabled;
    IF v_role IS NULL THEN
        RETURN;
    END IF;
    IF NOT (SELECT rolcanlogin FROM pg_roles WHERE rolname = v_role) THEN
        EXECUTE format('ALTER ROLE %I LOGIN', v_role);
    END IF;
    UPDATE lattis.users SET last_login_at = now() WHERE id = p_user_id;
END
$$;

-- Applique les rôles d'une invitation à un utilisateur. Seul « server_owner »
-- existe au niveau du serveur ; les rôles de classeur arrivent avec les
-- classeurs (jalon 2).
CREATE FUNCTION lattis.apply_roles(p_user_id uuid, p_workbook_id uuid, p_roles text[]) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_role text;
BEGIN
    SELECT pg_role INTO STRICT v_role FROM lattis.users WHERE id = p_user_id;
    IF p_workbook_id IS NULL AND 'server_owner' = ANY (p_roles) THEN
        EXECUTE format('GRANT lattis_server_owners TO %I', v_role);
    END IF;
END
$$;

-- Accepte les invitations en attente d'une adresse vérifiée.
CREATE FUNCTION lattis.accept_invitations(p_user_id uuid) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_inv   lattis.invitations;
    v_count integer := 0;
BEGIN
    FOR v_inv IN
        SELECT i.* FROM lattis.invitations i
        JOIN lattis.users u ON u.email = i.email
        WHERE u.id = p_user_id AND i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > now()
        FOR UPDATE OF i
    LOOP
        PERFORM lattis.apply_roles(p_user_id, v_inv.workbook_id, v_inv.roles);
        UPDATE lattis.invitations SET accepted_at = now() WHERE id = v_inv.id;
        v_count := v_count + 1;
    END LOOP;
    RETURN v_count;
END
$$;

-- Inscription à la connexion (§7.4). Appelée par lattisd une fois l'identité
-- vérifiée auprès du fournisseur et le tenant contrôlé.
-- Renvoie {status: ok|disabled|not_invited, user_id, pg_role, mode}.
CREATE FUNCTION lattis.enroll(
    p_provider_id uuid, p_subject text, p_email text, p_email_verified boolean,
    p_display_name text, p_tenant_default_role text
) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_user lattis.users;
    v_mode text := 'existing';
BEGIN
    PERFORM lattis.require_service();
    p_email := lower(trim(p_email));

    SELECT * INTO v_user FROM lattis.users WHERE provider_id = p_provider_id AND subject = p_subject FOR UPDATE;

    IF NOT FOUND AND p_email_verified THEN
        -- Compte créé à l'invitation, pas encore relié à une identité.
        SELECT u.* INTO v_user FROM lattis.users u
        WHERE u.email = p_email AND u.subject IS NULL AND NOT u.disabled
          AND EXISTS (SELECT 1 FROM lattis.invitations i
                      WHERE i.email = u.email AND i.accepted_at IS NULL
                        AND i.revoked_at IS NULL AND i.expires_at > now())
        ORDER BY u.created_at
        LIMIT 1
        FOR UPDATE;
        IF FOUND THEN
            UPDATE lattis.users SET provider_id = p_provider_id, subject = p_subject,
                   display_name = CASE WHEN display_name = '' THEN coalesce(p_display_name, '') ELSE display_name END
            WHERE id = v_user.id RETURNING * INTO v_user;
            v_mode := 'invitation';
        END IF;
    END IF;

    IF v_user.id IS NULL THEN
        IF p_email_verified AND EXISTS (
            SELECT 1 FROM lattis.invitations
            WHERE email = p_email AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()
        ) THEN
            v_mode := 'invitation';
        ELSIF lattis.config('access_mode') #>> '{}' = 'open' THEN
            v_mode := 'open';
        ELSE
            RETURN jsonb_build_object('status', 'not_invited');
        END IF;
        v_user := lattis.create_user(p_email, p_display_name, p_provider_id, p_subject);
        PERFORM lattis.audit_log('enroll', v_user.id::text, NULL,
            jsonb_build_object('email', p_email, 'mode', v_mode, 'provider_id', p_provider_id,
                               'arrival_role', coalesce(p_tenant_default_role, lattis.config('default_role') #>> '{}')));
    END IF;

    IF v_user.disabled THEN
        RETURN jsonb_build_object('status', 'disabled', 'user_id', v_user.id);
    END IF;

    IF p_email_verified THEN
        PERFORM lattis.accept_invitations(v_user.id);
    END IF;
    PERFORM lattis.activate_user(v_user.id);

    RETURN jsonb_build_object('status', 'ok', 'user_id', v_user.id, 'pg_role', v_user.pg_role,
                              'email', v_user.email, 'display_name', v_user.display_name, 'mode', v_mode);
END
$$;

-- §9.1 Serveur ---------------------------------------------------------------

CREATE FUNCTION lattis.set_config(p_key text, p_value jsonb) RETURNS void
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
    ELSIF p_key = 'web_scripting' AND jsonb_typeof(p_value) <> 'boolean' THEN
        RAISE EXCEPTION 'web_scripting : booléen attendu' USING ERRCODE = 'invalid_parameter_value';
    ELSIF p_key LIKE '%\_ttl' OR p_key IN ('tombstone_retention', 'password_max_failures', 'password_lock_seconds') THEN
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

CREATE FUNCTION lattis.add_provider(
    p_kind text, p_label text, p_issuer text, p_client_id text, p_secret_ref text, p_scopes text[]
) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_id uuid;
BEGIN
    PERFORM lattis.require_server_owner();
    p_issuer := coalesce(nullif(p_issuer, ''), CASE p_kind
        WHEN 'google' THEN 'https://accounts.google.com'
        WHEN 'google_workspace' THEN 'https://accounts.google.com'
        WHEN 'microsoft' THEN 'https://login.microsoftonline.com/common/v2.0'
    END);
    INSERT INTO lattis.identity_providers (kind, label, issuer, client_id, client_secret_ref, scopes, position)
    VALUES (p_kind, p_label, p_issuer, p_client_id, p_secret_ref,
            coalesce(p_scopes, '{openid,email,profile}'),
            (SELECT coalesce(max(position), 0) + 1 FROM lattis.identity_providers))
    RETURNING id INTO v_id;
    PERFORM lattis.audit_log('add_provider', v_id::text, NULL,
        jsonb_build_object('kind', p_kind, 'label', p_label, 'issuer', p_issuer, 'client_id', p_client_id));
    RETURN v_id;
END
$$;

-- p_changes : champs à modifier parmi label, issuer, client_id,
-- client_secret_ref, scopes, claim_name, claim_value, enabled, position.
CREATE FUNCTION lattis.update_provider(p_id uuid, p_changes jsonb) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_before lattis.identity_providers;
    v_key    text;
BEGIN
    PERFORM lattis.require_server_owner();
    SELECT * INTO v_before FROM lattis.identity_providers WHERE id = p_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'fournisseur inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    FOR v_key IN SELECT jsonb_object_keys(p_changes) LOOP
        IF v_key NOT IN ('label', 'issuer', 'client_id', 'client_secret_ref', 'scopes',
                         'claim_name', 'claim_value', 'enabled', 'position') THEN
            RAISE EXCEPTION 'champ non modifiable : %', v_key USING ERRCODE = 'invalid_parameter_value';
        END IF;
    END LOOP;
    UPDATE lattis.identity_providers SET
        label             = coalesce(p_changes ->> 'label', label),
        issuer            = CASE WHEN p_changes ? 'issuer' THEN p_changes ->> 'issuer' ELSE issuer END,
        client_id         = CASE WHEN p_changes ? 'client_id' THEN p_changes ->> 'client_id' ELSE client_id END,
        client_secret_ref = CASE WHEN p_changes ? 'client_secret_ref' THEN p_changes ->> 'client_secret_ref' ELSE client_secret_ref END,
        scopes            = CASE WHEN p_changes ? 'scopes'
                                 THEN ARRAY(SELECT jsonb_array_elements_text(p_changes -> 'scopes')) ELSE scopes END,
        claim_name        = CASE WHEN p_changes ? 'claim_name' THEN p_changes ->> 'claim_name' ELSE claim_name END,
        claim_value       = CASE WHEN p_changes ? 'claim_value' THEN p_changes ->> 'claim_value' ELSE claim_value END,
        enabled           = coalesce((p_changes ->> 'enabled')::boolean, enabled),
        position          = coalesce((p_changes ->> 'position')::integer, position)
    WHERE id = p_id;
    PERFORM lattis.audit_log('update_provider', p_id::text, to_jsonb(v_before) - 'client_secret_ref', p_changes - 'client_secret_ref');
END
$$;

CREATE FUNCTION lattis.remove_provider(p_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_before lattis.identity_providers;
BEGIN
    PERFORM lattis.require_server_owner();
    DELETE FROM lattis.identity_providers WHERE id = p_id RETURNING * INTO v_before;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'fournisseur inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    IF v_before.kind = 'password' THEN
        RAISE EXCEPTION 'le fournisseur « mot de passe » se désactive, il ne se supprime pas'
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    PERFORM lattis.audit_log('remove_provider', p_id::text, to_jsonb(v_before) - 'client_secret_ref', NULL);
END
$$;

CREATE FUNCTION lattis.allow_tenant(p_provider_id uuid, p_kind text, p_value text, p_default_role text) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_id uuid;
BEGIN
    PERFORM lattis.require_server_owner();
    IF p_default_role IS NOT NULL AND p_default_role NOT IN ('none', 'reader', 'participant', 'editor') THEN
        RAISE EXCEPTION 'rôle par défaut : none, reader, participant ou editor' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    p_value := CASE WHEN p_kind IN ('domain', 'email') THEN lower(trim(coalesce(p_value, ''))) ELSE trim(coalesce(p_value, '')) END;
    IF p_kind <> 'any' AND p_value = '' THEN
        RAISE EXCEPTION 'valeur du tenant manquante' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    INSERT INTO lattis.allowed_tenants (provider_id, kind, value, default_role)
    VALUES (p_provider_id, p_kind, p_value, p_default_role)
    ON CONFLICT (provider_id, kind, value) DO UPDATE SET default_role = excluded.default_role
    RETURNING id INTO v_id;
    PERFORM lattis.audit_log('allow_tenant', v_id::text, NULL,
        jsonb_build_object('provider_id', p_provider_id, 'kind', p_kind, 'value', p_value, 'default_role', p_default_role));
    RETURN v_id;
END
$$;

CREATE FUNCTION lattis.disallow_tenant(p_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_before lattis.allowed_tenants;
BEGIN
    PERFORM lattis.require_server_owner();
    DELETE FROM lattis.allowed_tenants WHERE id = p_id RETURNING * INTO v_before;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'tenant inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    PERFORM lattis.audit_log('disallow_tenant', p_id::text, to_jsonb(v_before), NULL);
END
$$;

-- Jeton à usage unique pour choisir son mot de passe (/setup?token=…).
-- Seul son hachage est conservé ; le jeton n'est rendu qu'une fois.
CREATE FUNCTION lattis.new_setup_token(p_user_id uuid) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_token text := replace(gen_random_uuid()::text || gen_random_uuid()::text, '-', '');
BEGIN
    INSERT INTO lattis.local_credentials (user_id, setup_token_hash, setup_expires_at)
    VALUES (p_user_id, sha256(convert_to(v_token, 'UTF8')), now() + interval '7 days')
    ON CONFLICT (user_id) DO UPDATE
        SET setup_token_hash = excluded.setup_token_hash,
            setup_expires_at = excluded.setup_expires_at,
            failed_attempts = 0, locked_until = NULL;
    RETURN v_token;
END
$$;

CREATE FUNCTION lattis.create_password_account(p_email text, p_display_name text) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_provider uuid;
    v_user     lattis.users;
BEGIN
    PERFORM lattis.require_server_owner();
    SELECT id INTO STRICT v_provider FROM lattis.identity_providers WHERE kind = 'password';
    p_email := lower(trim(p_email));
    IF EXISTS (SELECT 1 FROM lattis.users WHERE provider_id = v_provider AND email = p_email) THEN
        RAISE EXCEPTION 'un compte à mot de passe existe déjà pour %', p_email USING ERRCODE = 'unique_violation';
    END IF;
    v_user := lattis.create_user(p_email, p_display_name, v_provider, NULL);
    UPDATE lattis.users SET subject = id::text WHERE id = v_user.id;
    PERFORM lattis.audit_log('create_password_account', v_user.id::text, NULL,
        jsonb_build_object('email', p_email, 'display_name', p_display_name));
    RETURN lattis.new_setup_token(v_user.id);
END
$$;

CREATE FUNCTION lattis.reset_password(p_user_id uuid) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_server_owner();
    IF NOT EXISTS (SELECT 1 FROM lattis.users u JOIN lattis.identity_providers p ON p.id = u.provider_id
                   WHERE u.id = p_user_id AND p.kind = 'password') THEN
        RAISE EXCEPTION 'pas un compte à mot de passe' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    UPDATE lattis.local_credentials SET password_hash = NULL WHERE user_id = p_user_id;
    UPDATE lattis.refresh_tokens SET revoked_at = now() WHERE user_id = p_user_id AND revoked_at IS NULL;
    PERFORM lattis.audit_log('reset_password', p_user_id::text, NULL, NULL);
    RETURN lattis.new_setup_token(p_user_id);
END
$$;

-- Utilisé par lattisd : consomme le jeton et enregistre le hachage Argon2id
-- calculé par lattisd. Renvoie l'utilisateur, ou NULL si le jeton est invalide.
CREATE FUNCTION lattis.set_password_with_token(p_token text, p_hash text) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_user uuid;
BEGIN
    PERFORM lattis.require_service();
    UPDATE lattis.local_credentials
       SET password_hash = p_hash, changed_at = now(), setup_token_hash = NULL, setup_expires_at = NULL,
           failed_attempts = 0, locked_until = NULL
     WHERE setup_token_hash = sha256(convert_to(p_token, 'UTF8')) AND setup_expires_at > now()
    RETURNING user_id INTO v_user;
    IF v_user IS NOT NULL THEN
        PERFORM lattis.audit_log('set_password', v_user::text, NULL, NULL);
    END IF;
    RETURN v_user;
END
$$;

CREATE FUNCTION lattis.disable_user(p_user_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_role text;
BEGIN
    PERFORM lattis.require_server_owner();
    UPDATE lattis.users SET disabled = true WHERE id = p_user_id RETURNING pg_role INTO v_role;
    IF v_role IS NULL THEN
        RAISE EXCEPTION 'utilisateur inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    EXECUTE format('ALTER ROLE %I NOLOGIN', v_role);
    UPDATE lattis.refresh_tokens SET revoked_at = now() WHERE user_id = p_user_id AND revoked_at IS NULL;
    -- Coupe les sessions ouvertes (lattis_service a pg_signal_backend).
    BEGIN
        PERFORM pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = v_role;
    EXCEPTION WHEN insufficient_privilege THEN
        NULL;
    END;
    PERFORM lattis.audit_log('disable_user', p_user_id::text, NULL, jsonb_build_object('pg_role', v_role));
END
$$;

CREATE FUNCTION lattis.enable_user(p_user_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_role    text;
    v_subject text;
BEGIN
    PERFORM lattis.require_server_owner();
    UPDATE lattis.users SET disabled = false WHERE id = p_user_id RETURNING pg_role, subject INTO v_role, v_subject;
    IF v_role IS NULL THEN
        RAISE EXCEPTION 'utilisateur inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    -- Un compte jamais connecté reste sans droit de connexion jusqu'à sa première connexion.
    IF v_subject IS NOT NULL AND EXISTS (SELECT 1 FROM lattis.users WHERE id = p_user_id AND last_login_at IS NOT NULL) THEN
        EXECUTE format('ALTER ROLE %I LOGIN', v_role);
    END IF;
    PERFORM lattis.audit_log('enable_user', p_user_id::text, NULL, jsonb_build_object('pg_role', v_role));
END
$$;

-- §9.2 (partie serveur) ------------------------------------------------------

CREATE FUNCTION lattis.invite(p_email text, p_workbook_id uuid, p_roles text[]) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_id   uuid;
    v_user lattis.users;
BEGIN
    IF p_workbook_id IS NOT NULL THEN
        RAISE EXCEPTION 'les invitations à un classeur arrivent avec les classeurs'
            USING ERRCODE = 'feature_not_supported';
    END IF;
    PERFORM lattis.require_server_owner();
    p_email := lower(trim(p_email));
    p_roles := coalesce(p_roles, '{}');
    IF EXISTS (SELECT 1 FROM unnest(p_roles) r WHERE r <> 'server_owner') THEN
        RAISE EXCEPTION 'rôle de serveur inconnu (seul server_owner existe)' USING ERRCODE = 'invalid_parameter_value';
    END IF;

    INSERT INTO lattis.invitations (email, workbook_id, roles, invited_by, expires_at)
    VALUES (p_email, p_workbook_id, p_roles, lattis.actor(),
            now() + make_interval(secs => (lattis.config('invitation_ttl') #>> '{}')::double precision))
    RETURNING id INTO v_id;

    -- Le compte PostgreSQL est créé dès l'invitation (sans droit de connexion).
    IF NOT EXISTS (SELECT 1 FROM lattis.users WHERE email = p_email) THEN
        v_user := lattis.create_user(p_email, '', NULL, NULL);
    END IF;

    -- Utilisateur déjà actif : les rôles s'appliquent tout de suite.
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

CREATE FUNCTION lattis.revoke_invitation(p_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_inv lattis.invitations;
    v_user lattis.users;
BEGIN
    PERFORM lattis.require_server_owner();
    UPDATE lattis.invitations SET revoked_at = now()
    WHERE id = p_id AND accepted_at IS NULL AND revoked_at IS NULL
    RETURNING * INTO v_inv;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'invitation inconnue ou déjà acceptée' USING ERRCODE = 'no_data_found';
    END IF;
    -- Compte créé par l'invitation et jamais utilisé : on le retire.
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

-- Résumé de la session pour le client (menu Compte, §9.2).
CREATE FUNCTION lattis.login_summary() RETURNS jsonb
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
        'workbooks', '[]'::jsonb)
$$;

-- Fonctions internes : réservées à lattis_service (propriétaire).
REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA lattis FROM PUBLIC;
GRANT EXECUTE ON FUNCTION
    lattis.actor(), lattis.is_server_owner(), lattis.login_summary(),
    lattis.set_config(text, jsonb),
    lattis.add_provider(text, text, text, text, text, text[]),
    lattis.update_provider(uuid, jsonb), lattis.remove_provider(uuid),
    lattis.allow_tenant(uuid, text, text, text), lattis.disallow_tenant(uuid),
    lattis.create_password_account(text, text), lattis.reset_password(uuid),
    lattis.disable_user(uuid), lattis.enable_user(uuid),
    lattis.invite(text, uuid, text[]), lattis.revoke_invitation(uuid)
TO PUBLIC;
