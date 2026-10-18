-- Choix du mot de passe : la personne qui tient le lien peut aussi changer
-- l'adresse du compte (celle prévue à la création n'est qu'une suggestion).
-- Le rôle PostgreSQL suit l'adresse quand il en portait le nom ; les
-- invitations en attente suivent aussi. NULL ou même adresse : inchangée.
CREATE FUNCTION lattis.set_password_with_token(p_token text, p_hash text, p_email text) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_user lattis.users;
    v_role text;
BEGIN
    PERFORM lattis.require_service();
    SELECT u.* INTO v_user FROM lattis.local_credentials c JOIN lattis.users u ON u.id = c.user_id
     WHERE c.setup_token_hash = sha256(convert_to(p_token, 'UTF8')) AND c.setup_expires_at > now()
       FOR UPDATE;
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;
    p_email := lower(trim(coalesce(p_email, '')));
    IF p_email <> '' AND p_email <> v_user.email THEN
        IF p_email !~ '^[^@\s]+@[^@\s]+$' THEN
            RAISE EXCEPTION 'adresse invalide : %', p_email USING ERRCODE = 'invalid_parameter_value';
        END IF;
        IF EXISTS (SELECT 1 FROM lattis.users WHERE provider_id = v_user.provider_id AND email = p_email) THEN
            RAISE EXCEPTION 'un compte à mot de passe existe déjà pour %', p_email USING ERRCODE = 'unique_violation';
        END IF;
        v_role := v_user.pg_role;
        IF v_role = v_user.email THEN
            v_role := lattis.role_name_for(p_email, v_user.id);
            EXECUTE format('ALTER ROLE %I RENAME TO %I', v_user.pg_role, v_role);
        END IF;
        UPDATE lattis.users SET email = p_email, pg_role = v_role WHERE id = v_user.id;
        UPDATE lattis.invitations SET email = p_email
         WHERE email = v_user.email AND accepted_at IS NULL AND revoked_at IS NULL;
        UPDATE lattis.refresh_tokens SET revoked_at = now() WHERE user_id = v_user.id AND revoked_at IS NULL;
        PERFORM lattis.audit_log('change_email', v_user.id::text,
            jsonb_build_object('email', v_user.email, 'pg_role', v_user.pg_role),
            jsonb_build_object('email', p_email, 'pg_role', v_role));
    END IF;
    RETURN lattis.set_password_with_token(p_token, p_hash);
END
$$;

REVOKE EXECUTE ON FUNCTION lattis.set_password_with_token(text, text, text) FROM PUBLIC;
