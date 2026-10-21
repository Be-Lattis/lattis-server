-- Même adresse chez plusieurs fournisseurs (décidé par Louis le 21/10/2026) :
-- - Google, Google Workspace et Microsoft, adresse vérifiée par le
--   fournisseur : l'identité est reliée au compte existant de cette adresse
--   (lattis.user_identities), sans en créer un second ;
-- - autres fournisseurs, ou adresse non vérifiée : refus, qui dit avec
--   quel moyen se connecter (statut email_taken).
-- Les comptes techniques (@lattis.invalid) ne sont jamais reliés.

CREATE TABLE lattis.user_identities (
    provider_id uuid NOT NULL REFERENCES lattis.identity_providers (id) ON DELETE CASCADE,
    subject     text NOT NULL,
    user_id     uuid NOT NULL REFERENCES lattis.users (id) ON DELETE CASCADE,
    linked_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider_id, subject)
);
CREATE INDEX user_identities_user ON lattis.user_identities (user_id);
REVOKE ALL ON lattis.user_identities FROM PUBLIC;

CREATE OR REPLACE FUNCTION lattis.enroll(
    p_provider_id uuid, p_subject text, p_email text, p_email_verified boolean,
    p_display_name text, p_tenant_default_role text
) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_user  lattis.users;
    v_mode  text := 'existing';
    v_kind  text;
    v_other lattis.users;
    v_label text;
BEGIN
    PERFORM lattis.require_service();
    p_email := lower(trim(p_email));
    SELECT kind INTO v_kind FROM lattis.identity_providers WHERE id = p_provider_id;

    SELECT * INTO v_user FROM lattis.users WHERE provider_id = p_provider_id AND subject = p_subject FOR UPDATE;
    IF NOT FOUND THEN
        -- Identité déjà reliée à un compte (même adresse, autre fournisseur).
        SELECT u.* INTO v_user FROM lattis.users u
        JOIN lattis.user_identities i ON i.user_id = u.id
        WHERE i.provider_id = p_provider_id AND i.subject = p_subject FOR UPDATE OF u;
    END IF;

    IF v_user.id IS NULL AND p_email_verified THEN
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
        -- Un compte existe déjà pour cette adresse, par un autre fournisseur
        -- (pas un compte d'invitation encore libre : celui-ci se relie par
        -- l'invitation, adresse vérifiée).
        SELECT u.* INTO v_other FROM lattis.users u
        WHERE u.email = p_email AND u.email NOT LIKE '%@lattis.invalid'
          AND u.subject IS NOT NULL AND u.provider_id IS DISTINCT FROM p_provider_id
        ORDER BY u.disabled, u.created_at
        LIMIT 1;
        IF v_other.id IS NOT NULL THEN
            IF p_email_verified AND v_kind IN ('google', 'google_workspace', 'microsoft') THEN
                INSERT INTO lattis.user_identities (provider_id, subject, user_id)
                VALUES (p_provider_id, p_subject, v_other.id);
                PERFORM lattis.audit_log('link_identity', v_other.id::text, NULL,
                    jsonb_build_object('email', p_email, 'provider_id', p_provider_id));
                v_user := v_other;
                v_mode := 'linked';
            ELSE
                SELECT coalesce(p.label, 'son moyen de connexion habituel') INTO v_label
                FROM lattis.users u LEFT JOIN lattis.identity_providers p ON p.id = u.provider_id
                WHERE u.id = v_other.id;
                RETURN jsonb_build_object('status', 'email_taken', 'email', p_email, 'provider', v_label);
            END IF;
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
