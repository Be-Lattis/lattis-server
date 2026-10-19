-- Corrections de l'audit du 19/10 (critiques).
--
-- C1. Adresse choisie au premier mot de passe : marquée non vérifiée. Elle
--     ne reçoit plus les invitations, rôles ni audiences adressés ensuite à
--     cette adresse (qui pourrait être celle de quelqu'un d'autre) ; seules
--     les invitations reçues avec le lien la suivent. Une adresse déjà portée
--     par un compte ou déjà invitée est refusée.
-- C2. Data Lake : un dossier appartient à son créateur. Poser ou retirer son
--     audience, ou le déplacer (ce qui change l'audience héritée), demande en
--     plus de pouvoir gérer toutes les données qu'il contient ; les
--     propriétaires du serveur le peuvent toujours.
-- C4. Le dernier fournisseur de connexion actif ne peut être ni désactivé ni
--     supprimé.

ALTER TABLE lattis.users ADD COLUMN email_verified boolean NOT NULL DEFAULT true;
ALTER TABLE lattis.users ADD COLUMN email_changed_at timestamptz;

CREATE OR REPLACE FUNCTION lattis.set_password_with_token(p_token text, p_hash text, p_email text) RETURNS uuid
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
        IF EXISTS (SELECT 1 FROM lattis.users WHERE email = p_email AND id <> v_user.id) THEN
            RAISE EXCEPTION 'l''adresse % est déjà celle d''un autre compte', p_email USING ERRCODE = 'unique_violation';
        END IF;
        IF EXISTS (SELECT 1 FROM lattis.invitations WHERE email = p_email AND accepted_at IS NULL AND revoked_at IS NULL) THEN
            RAISE EXCEPTION 'l''adresse % a déjà des invitations en attente : utilisez le lien envoyé à cette adresse', p_email
                USING ERRCODE = 'unique_violation';
        END IF;
        v_role := v_user.pg_role;
        IF v_role = v_user.email THEN
            v_role := lattis.role_name_for(p_email, v_user.id);
            EXECUTE format('ALTER ROLE %I RENAME TO %I', v_user.pg_role, v_role);
        END IF;
        UPDATE lattis.users SET email = p_email, pg_role = v_role, email_verified = false, email_changed_at = now()
         WHERE id = v_user.id;
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

CREATE OR REPLACE FUNCTION lattis.accept_invitations(p_user_id uuid) RETURNS integer
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
          -- Adresse non vérifiée : seulement les invitations reçues avec le lien.
          AND (u.email_verified OR i.created_at <= u.email_changed_at)
        FOR UPDATE OF i
    LOOP
        PERFORM lattis.apply_roles(p_user_id, v_inv.workbook_id, v_inv.roles);
        UPDATE lattis.invitations SET accepted_at = now() WHERE id = v_inv.id;
        v_count := v_count + 1;
    END LOOP;
    RETURN v_count;
END
$$;

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
    WHERE email = p_email AND subject IS NOT NULL AND last_login_at IS NOT NULL AND NOT disabled AND email_verified
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

CREATE OR REPLACE FUNCTION lattis.add_data_audience_by_email(p_dataset_id uuid, p_folder_id uuid, p_email text, p_predicate text) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_user uuid;
BEGIN
    IF p_dataset_id IS NOT NULL THEN
        PERFORM lattis.require_manage_dataset(p_dataset_id);
    ELSE
        PERFORM lattis.require_data_publisher();
    END IF;
    SELECT id INTO v_user FROM lattis.users WHERE email = lower(trim(coalesce(p_email, ''))) AND NOT disabled
     ORDER BY last_login_at DESC NULLS LAST LIMIT 1;
    IF v_user IS NOT NULL AND NOT (SELECT email_verified FROM lattis.users WHERE id = v_user) THEN
        RAISE EXCEPTION 'l''adresse % n''a pas été vérifiée : ajoutez la personne depuis une équipe ou par son compte',
            lower(trim(coalesce(p_email, ''))) USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF v_user IS NULL THEN
        RAISE EXCEPTION 'aucun compte pour %', lower(trim(coalesce(p_email, ''))) USING ERRCODE = 'no_data_found';
    END IF;
    RETURN lattis.add_data_audience(p_dataset_id, p_folder_id, v_user, p_predicate);
END
$$;

-- Gérer un dossier : propriétaire du serveur, ou publieur qui l'a créé.
-- p_contents : il faut en plus pouvoir gérer toutes les données qu'il
-- contient (sous-dossiers compris), car l'opération change leur audience.
CREATE FUNCTION lattis.require_folder_admin(p_folder_id uuid, p_contents boolean) RETURNS void
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_owner text;
BEGIN
    PERFORM lattis.require_data_publisher();
    SELECT created_by INTO v_owner FROM lattis.data_folders WHERE id = p_folder_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'dossier inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    IF lattis.is_server_owner() THEN
        RETURN;
    END IF;
    IF v_owner IS DISTINCT FROM session_user::text THEN
        RAISE EXCEPTION 'seuls son créateur et les propriétaires du serveur gèrent ce dossier' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF p_contents AND EXISTS (SELECT 1 FROM lattis.datasets d JOIN lattis.data_folder_closure() c ON c.descendant = d.folder_id
                              WHERE c.ancestor = p_folder_id AND NOT lattis.can_manage_dataset(d)) THEN
        RAISE EXCEPTION 'ce dossier contient des données que vous ne gérez pas : leur audience ne peut pas changer par le dossier'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
END
$$;
REVOKE EXECUTE ON FUNCTION lattis.require_folder_admin(uuid, boolean) FROM PUBLIC;

CREATE OR REPLACE FUNCTION lattis.rename_data_folder(p_id uuid, p_name text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_parent uuid;
BEGIN
    PERFORM lattis.require_folder_admin(p_id, false);
    p_name := trim(coalesce(p_name, ''));
    SELECT parent_id INTO v_parent FROM lattis.data_folders WHERE id = p_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'dossier inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    IF EXISTS (SELECT 1 FROM lattis.data_folders WHERE parent_id IS NOT DISTINCT FROM v_parent AND lower(name) = lower(p_name) AND id <> p_id) THEN
        RAISE EXCEPTION 'un dossier « % » existe déjà à cet endroit', p_name USING ERRCODE = 'unique_violation';
    END IF;
    UPDATE lattis.data_folders SET name = p_name WHERE id = p_id;
    PERFORM lattis.audit_log('rename_data_folder', p_id::text, NULL, jsonb_build_object('name', p_name));
    PERFORM pg_notify('lattis_data', '*catalog');
END
$$;

CREATE OR REPLACE FUNCTION lattis.move_data_folder(p_id uuid, p_parent_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_folder_admin(p_id, true);
    IF p_parent_id IS NOT NULL THEN
        PERFORM lattis.require_folder_admin(p_parent_id, false);
    END IF;
    IF p_parent_id IS NOT NULL AND EXISTS (SELECT 1 FROM lattis.data_folder_closure() WHERE ancestor = p_id AND descendant = p_parent_id) THEN
        RAISE EXCEPTION 'un dossier ne peut pas être rangé dans lui-même ni dans l''un de ses sous-dossiers'
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    UPDATE lattis.data_folders SET parent_id = p_parent_id WHERE id = p_id;
    PERFORM lattis.compile_folder_datasets(p_id);
    PERFORM lattis.audit_log('move_data_folder', p_id::text, NULL, jsonb_build_object('parent_id', p_parent_id));
END
$$;

CREATE OR REPLACE FUNCTION lattis.drop_data_folder(p_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_folder_admin(p_id, false);
    IF EXISTS (SELECT 1 FROM lattis.data_folders WHERE parent_id = p_id) OR EXISTS (SELECT 1 FROM lattis.datasets WHERE folder_id = p_id) THEN
        RAISE EXCEPTION 'le dossier n''est pas vide' USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    DELETE FROM lattis.data_folders WHERE id = p_id;
    PERFORM lattis.audit_log('drop_data_folder', p_id::text, NULL, NULL);
    PERFORM pg_notify('lattis_data', '*catalog');
END
$$;

CREATE OR REPLACE FUNCTION lattis.add_data_audience(p_dataset_id uuid, p_folder_id uuid, p_principal uuid, p_predicate text) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    d      lattis.datasets;
    v      lattis.workbooks;
    v_kind text;
    v_id   uuid;
BEGIN
    IF (p_dataset_id IS NULL) = (p_folder_id IS NULL) THEN
        RAISE EXCEPTION 'une donnée ou un dossier' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF p_dataset_id IS NOT NULL THEN
        d := lattis.require_manage_dataset(p_dataset_id);
    ELSE
        PERFORM lattis.require_folder_admin(p_folder_id, true);
    END IF;
    IF EXISTS (SELECT 1 FROM lattis.users WHERE id = p_principal) THEN
        v_kind := 'user';
    ELSIF EXISTS (SELECT 1 FROM lattis.teams WHERE id = p_principal) THEN
        v_kind := 'team';
    ELSE
        RAISE EXCEPTION 'utilisateur ou équipe inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    p_predicate := nullif(trim(coalesce(p_predicate, '')), '');
    -- Filtre vérifié sur le tableau source quand on le connaît ; sur un
    -- dossier, il l'est à la compilation de chaque donnée (ignoré s'il ne s'y
    -- applique pas).
    IF p_predicate IS NOT NULL AND d.id IS NOT NULL AND d.workbook_id IS NOT NULL AND d.table_name IS NOT NULL THEN
        SELECT * INTO v FROM lattis.workbooks WHERE id = d.workbook_id;
        p_predicate := lattis.check_predicate(v.schema_name, d.table_name, p_predicate);
    END IF;
    INSERT INTO lattis.data_audience (dataset_id, folder_id, principal, principal_kind, predicate, created_by)
    VALUES (p_dataset_id, p_folder_id, p_principal, v_kind, p_predicate, lattis.actor()) RETURNING id INTO v_id;
    IF p_dataset_id IS NOT NULL THEN
        PERFORM lattis.compile_dataset(p_dataset_id);
    ELSE
        PERFORM lattis.compile_folder_datasets(p_folder_id);
    END IF;
    PERFORM lattis.audit_log('add_data_audience', v_id::text, NULL,
        jsonb_build_object('dataset_id', p_dataset_id, 'folder_id', p_folder_id, 'principal', p_principal, 'predicate', p_predicate));
    RETURN v_id;
END
$$;

CREATE OR REPLACE FUNCTION lattis.remove_data_audience(p_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    a lattis.data_audience;
BEGIN
    SELECT * INTO a FROM lattis.data_audience WHERE id = p_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'entrée inconnue' USING ERRCODE = 'no_data_found';
    END IF;
    IF a.dataset_id IS NOT NULL THEN
        PERFORM lattis.require_manage_dataset(a.dataset_id);
    ELSE
        PERFORM lattis.require_folder_admin(a.folder_id, true);
    END IF;
    DELETE FROM lattis.data_audience WHERE id = p_id;
    IF a.dataset_id IS NOT NULL THEN
        PERFORM lattis.compile_dataset(a.dataset_id);
    ELSE
        PERFORM lattis.compile_folder_datasets(a.folder_id);
    END IF;
    PERFORM lattis.audit_log('remove_data_audience', p_id::text, to_jsonb(a), NULL);
END
$$;

CREATE FUNCTION lattis.trg_keep_one_provider() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    IF (TG_OP = 'DELETE' AND OLD.enabled) OR (TG_OP = 'UPDATE' AND OLD.enabled AND NOT NEW.enabled) THEN
        IF NOT EXISTS (SELECT 1 FROM lattis.identity_providers WHERE enabled AND id <> OLD.id) THEN
            RAISE EXCEPTION 'c''est le dernier moyen de connexion actif : activez-en un autre d''abord, sinon plus personne ne pourra se connecter'
                USING ERRCODE = 'object_not_in_prerequisite_state';
        END IF;
    END IF;
    RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
END
$$;
CREATE TRIGGER lattis_keep_one_provider BEFORE UPDATE OF enabled OR DELETE ON lattis.identity_providers
    FOR EACH ROW EXECUTE FUNCTION lattis.trg_keep_one_provider();
