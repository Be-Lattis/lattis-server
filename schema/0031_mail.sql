-- Envoi d'e-mails (facultatif) : une file d'envoi en base, servie par
-- lattisd quand un serveur SMTP est configuré (réglage mail_enabled, posé
-- par lattisd au démarrage). Sans SMTP, rien n'est mis en file et les liens
-- restent à copier depuis l'administration.
--
-- Ce qui part par e-mail : lien de choix du mot de passe (compte créé,
-- réinitialisation, mot de passe oublié), confirmation d'une adresse,
-- invitation à un classeur, échec d'un pipeline. Le contenu d'un message
-- (lien, titre) est effacé de la file une fois envoyé : il ne reste qu'un
-- journal (type, destinataire, dates, statut).

INSERT INTO lattis.server_config (key, value) VALUES ('mail_enabled', 'false');

CREATE TABLE lattis.mail_outbox (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind        text NOT NULL CHECK (kind IN ('password_link', 'confirm_email', 'invitation', 'pipeline_failed')),
    recipient   text NOT NULL,
    lang        text NOT NULL DEFAULT 'fr',
    data        jsonb NOT NULL DEFAULT '{}',
    status      text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'failed', 'expired')),
    attempts    integer NOT NULL DEFAULT 0,
    last_error  text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    sent_at     timestamptz
);
CREATE INDEX mail_outbox_pending ON lattis.mail_outbox (created_at) WHERE status = 'pending';
CREATE INDEX mail_outbox_recipient ON lattis.mail_outbox (recipient, sent_at);
REVOKE ALL ON lattis.mail_outbox FROM PUBLIC;

-- Mise en file (rien si l'envoi n'est pas configuré).
CREATE FUNCTION lattis.mail_enqueue(p_kind text, p_recipient text, p_data jsonb) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_id bigint;
BEGIN
    IF NOT coalesce((lattis.config('mail_enabled') #>> '{}')::boolean, false)
       OR coalesce(p_recipient, '') !~ '^[^@\s]+@[^@\s]+$' OR p_recipient LIKE '%@lattis.invalid' THEN
        RETURN NULL;
    END IF;
    INSERT INTO lattis.mail_outbox (kind, recipient, data) VALUES (p_kind, lower(trim(p_recipient)), coalesce(p_data, '{}'))
    RETURNING id INTO v_id;
    PERFORM pg_notify('lattis_mail', v_id::text);
    RETURN v_id;
END
$$;

-- Lien de choix du mot de passe, par l'administration (propriétaires du
-- serveur) : p_token vient de create_password_account ou reset_password.
CREATE FUNCTION lattis.mail_password_link(p_email text, p_token text, p_reason text) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_server_owner();
    RETURN lattis.mail_enqueue('password_link', p_email, jsonb_build_object('token', p_token,
        'reason', CASE WHEN p_reason = 'reset' THEN 'reset' ELSE 'new' END,
        'server', lattis.config('display_name') #>> '{}')) IS NOT NULL;
END
$$;

-- Mot de passe oublié (lattisd, sans session) : un lien neuf, l'ancien mot
-- de passe restant valable jusqu'à son usage. Rien ne dit si l'adresse a un
-- compte (même réponse dans tous les cas).
CREATE FUNCTION lattis.request_password_reset(p_email text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_user lattis.users;
BEGIN
    PERFORM lattis.require_service();
    SELECT u.* INTO v_user FROM lattis.users u
    JOIN lattis.identity_providers p ON p.id = u.provider_id AND p.kind = 'password'
    WHERE u.email = lower(trim(p_email)) AND NOT u.disabled;
    IF v_user.id IS NULL OR NOT coalesce((lattis.config('mail_enabled') #>> '{}')::boolean, false) THEN
        RETURN;
    END IF;
    -- Pas plus d'une demande en attente par adresse et par quart d'heure.
    IF EXISTS (SELECT 1 FROM lattis.mail_outbox WHERE recipient = v_user.email AND kind = 'password_link'
               AND created_at > now() - interval '15 minutes') THEN
        RETURN;
    END IF;
    PERFORM lattis.mail_enqueue('password_link', v_user.email, jsonb_build_object(
        'token', lattis.new_setup_token(v_user.id), 'reason', 'forgot', 'server', lattis.config('display_name') #>> '{}'));
    PERFORM lattis.audit_log('request_password_reset', v_user.id::text, NULL, NULL);
END
$$;

-- Confirmation d'adresse : jeton à usage unique, 7 jours.
CREATE TABLE lattis.email_confirmations (
    token_hash bytea PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES lattis.users (id) ON DELETE CASCADE,
    email      text NOT NULL,
    expires_at timestamptz NOT NULL
);
REVOKE ALL ON lattis.email_confirmations FROM PUBLIC;

-- Demande de confirmation (lattisd, après un choix d'adresse).
CREATE FUNCTION lattis.request_email_confirmation(p_user_id uuid) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_user  lattis.users;
    v_token text := replace(gen_random_uuid()::text || gen_random_uuid()::text, '-', '');
BEGIN
    PERFORM lattis.require_service();
    SELECT * INTO v_user FROM lattis.users WHERE id = p_user_id AND NOT disabled AND NOT email_verified;
    IF v_user.id IS NULL OR NOT coalesce((lattis.config('mail_enabled') #>> '{}')::boolean, false) THEN
        RETURN false;
    END IF;
    DELETE FROM lattis.email_confirmations WHERE user_id = p_user_id;
    INSERT INTO lattis.email_confirmations (token_hash, user_id, email, expires_at)
    VALUES (sha256(convert_to(v_token, 'UTF8')), p_user_id, v_user.email, now() + interval '7 days');
    RETURN lattis.mail_enqueue('confirm_email', v_user.email, jsonb_build_object('token', v_token,
        'server', lattis.config('display_name') #>> '{}')) IS NOT NULL;
END
$$;

-- Lien de confirmation suivi (lattisd) : adresse vérifiée, invitations
-- adressées à cette adresse appliquées. Renvoie l'adresse, ou NULL.
CREATE FUNCTION lattis.confirm_email(p_token text) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    c lattis.email_confirmations;
BEGIN
    PERFORM lattis.require_service();
    DELETE FROM lattis.email_confirmations WHERE token_hash = sha256(convert_to(p_token, 'UTF8')) AND expires_at > now()
    RETURNING * INTO c;
    IF c.user_id IS NULL THEN
        RETURN NULL;
    END IF;
    -- L'adresse a pu changer depuis la demande : seule celle confirmée compte.
    UPDATE lattis.users SET email_verified = true WHERE id = c.user_id AND email = c.email;
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;
    PERFORM lattis.accept_invitations(c.user_id);
    PERFORM lattis.audit_log('confirm_email', c.user_id::text, NULL, jsonb_build_object('email', c.email));
    RETURN c.email;
END
$$;

-- Invitation à un classeur : un e-mail à la personne invitée.
CREATE FUNCTION lattis.trg_invitation_mail() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_title text;
BEGIN
    IF NEW.workbook_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT title INTO v_title FROM lattis.workbooks WHERE id = NEW.workbook_id AND kind = 'workbook';
    IF v_title IS NOT NULL THEN
        PERFORM lattis.mail_enqueue('invitation', NEW.email, jsonb_build_object(
            'workbook_id', NEW.workbook_id, 'title', v_title, 'by', NEW.invited_by,
            'server', lattis.config('display_name') #>> '{}'));
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER invitation_mail AFTER INSERT ON lattis.invitations FOR EACH ROW EXECUTE FUNCTION lattis.trg_invitation_mail();

-- Alerte d'échec d'un pipeline : adresse(s) de destination.
ALTER TABLE lattis.pipelines ADD COLUMN alert_email text
    CHECK (alert_email IS NULL OR alert_email ~ '^[^@\s,]+@[^@\s,]+(\s*,\s*[^@\s,]+@[^@\s,]+){0,4}$');

CREATE FUNCTION lattis.set_pipeline_alert_email(p_id uuid, p_email text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_server_owner();
    UPDATE lattis.pipelines SET alert_email = nullif(lower(trim(p_email)), ''), updated_at = now() WHERE id = p_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'pipeline inconnu' USING ERRCODE = 'no_data_found';
    END IF;
END
$$;

-- Journal et état de l'envoi (propriétaires du serveur).
CREATE FUNCTION lattis.mail_status() RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_server_owner();
    RETURN jsonb_build_object(
        'enabled', coalesce((lattis.config('mail_enabled') #>> '{}')::boolean, false),
        'pending', (SELECT count(*) FROM lattis.mail_outbox WHERE status = 'pending'),
        'sent_24h', (SELECT count(*) FROM lattis.mail_outbox WHERE status = 'sent' AND sent_at > now() - interval '1 day'),
        'failed_24h', (SELECT count(*) FROM lattis.mail_outbox WHERE status IN ('failed', 'expired') AND created_at > now() - interval '1 day'),
        'recent', (SELECT coalesce(jsonb_agg(jsonb_build_object('kind', kind, 'recipient', recipient, 'status', status,
                        'created_at', created_at, 'sent_at', sent_at, 'error', last_error) ORDER BY id DESC), '[]')
                   FROM (SELECT * FROM lattis.mail_outbox ORDER BY id DESC LIMIT 20) m));
END
$$;

REVOKE EXECUTE ON FUNCTION lattis.mail_enqueue(text, text, jsonb), lattis.request_password_reset(text),
    lattis.request_email_confirmation(uuid), lattis.confirm_email(text), lattis.trg_invitation_mail() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION lattis.mail_password_link(text, text, text), lattis.set_pipeline_alert_email(uuid, text),
    lattis.mail_status() TO PUBLIC;
