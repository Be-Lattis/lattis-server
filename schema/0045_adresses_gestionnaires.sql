-- Adresses autorisées aux gestionnaires d'équipe (choix de Louis, 21/10).
--
-- Les propriétaires du serveur gardent toutes les sources et destinations
-- des pipelines. Un pipeline créé ou modifié par un gestionnaire d'équipe
-- est « restreint » : HTTP, SFTP et PostgreSQL (lecture ou export) et son
-- webhook ne joignent que les hôtes de la liste pipeline_team_hosts, tenue
-- par les propriétaires sur l'écran Serveur (vide : ces sources leur sont
-- fermées). Fichiers envoyés, déposés et tableaux poussés restent permis.
-- lattisd vérifie l'hôte à chaque exécution (y compris celui de la chaîne
-- de connexion PostgreSQL, gardée chiffrée) ; la base le vérifie dès la
-- saisie quand il est en clair.

INSERT INTO lattis.server_config (key, value) VALUES ('pipeline_team_hosts', '[]');

ALTER TABLE lattis.pipelines ADD COLUMN restricted boolean NOT NULL DEFAULT false;

-- Restreint pour de bon dès qu'un gestionnaire d'équipe crée ou modifie le
-- pipeline (lattisd, sous lattis_service, n'y touche pas).
CREATE FUNCTION lattis.trg_pipeline_restricted() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    IF session_user <> 'lattis_service' AND NOT lattis.is_server_owner() THEN
        NEW.restricted := true;
    ELSIF TG_OP = 'UPDATE' THEN
        NEW.restricted := OLD.restricted;
    END IF;
    RETURN NEW;
END
$$;
REVOKE EXECUTE ON FUNCTION lattis.trg_pipeline_restricted() FROM PUBLIC;

CREATE TRIGGER lattis_pipeline_restricted BEFORE INSERT OR UPDATE ON lattis.pipelines
    FOR EACH ROW EXECUTE FUNCTION lattis.trg_pipeline_restricted();

-- Hôte autorisé aux gestionnaires : nom exact, *.domaine, ou bloc d'adresses.
CREATE FUNCTION lattis.team_host_allowed(p_host text) RETURNS boolean
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    a text;
BEGIN
    p_host := lower(rtrim(coalesce(p_host, ''), '.'));
    IF p_host = '' THEN
        RETURN false;
    END IF;
    FOR a IN SELECT lower(trim(x)) FROM jsonb_array_elements_text(coalesce(lattis.config('pipeline_team_hosts'), '[]')) x LOOP
        IF a = p_host OR (a LIKE '*.%' AND right(p_host, length(a) - 1) = substr(a, 2)) THEN
            RETURN true;
        END IF;
        IF a ~ '/' AND p_host ~ '^[0-9a-f.:]+$' THEN
            BEGIN
                IF p_host::inet <<= a::cidr THEN
                    RETURN true;
                END IF;
            EXCEPTION WHEN others THEN
                NULL;
            END;
        END IF;
    END LOOP;
    RETURN false;
END
$$;
REVOKE EXECUTE ON FUNCTION lattis.team_host_allowed(text) FROM PUBLIC;

-- Hôte d'une adresse http(s)://.
CREATE FUNCTION lattis.url_host(p_url text) RETURNS text
LANGUAGE sql IMMUTABLE SET search_path = pg_catalog, pg_temp
AS $$
    SELECT lower(coalesce(substring(p_url FROM '^[a-zA-Z][a-zA-Z0-9+.-]*://(?:[^/@]*@)?\[([^]]+)\]'),
                          substring(p_url FROM '^[a-zA-Z][a-zA-Z0-9+.-]*://(?:[^/@]*@)?([^/:?#]+)')))
$$;
REVOKE EXECUTE ON FUNCTION lattis.url_host(text) FROM PUBLIC;

CREATE OR REPLACE FUNCTION lattis.pipeline_check_manager(p jsonb) RETURNS void
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_host text;
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
    IF p ->> 'source_kind' IN ('http', 'sftp', 'sql') AND jsonb_array_length(coalesce(lattis.config('pipeline_team_hosts'), '[]')) = 0 THEN
        RAISE EXCEPTION 'sources HTTP, SFTP et PostgreSQL : aucune adresse n''est autorisée aux gestionnaires d''équipe (écran Serveur)'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    FOREACH v_host IN ARRAY ARRAY[lattis.url_host(p #>> '{source,url}'), p #>> '{source,host}', lattis.url_host(p ->> 'webhook_url')] LOOP
        IF coalesce(v_host, '') <> '' AND NOT lattis.team_host_allowed(v_host) THEN
            RAISE EXCEPTION 'adresse « % » non autorisée aux gestionnaires d''équipe : un propriétaire du serveur peut l''ajouter (écran Serveur)', v_host
                USING ERRCODE = 'insufficient_privilege';
        END IF;
    END LOOP;
END
$$;

-- Réglage pipeline_team_hosts : liste de noms d'hôte.
CREATE OR REPLACE FUNCTION lattis.set_config(p_key text, p_value jsonb) RETURNS void
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
    ELSIF p_key IN ('web_scripting', 'forms_public') AND jsonb_typeof(p_value) <> 'boolean' THEN
        RAISE EXCEPTION '% : booléen attendu', p_key USING ERRCODE = 'invalid_parameter_value';
    ELSIF p_key = 'workbook_creators' AND (p_value #>> '{}') NOT IN ('everyone', 'owners') THEN
        RAISE EXCEPTION 'workbook_creators : everyone ou owners' USING ERRCODE = 'invalid_parameter_value';
    ELSIF p_key = 'logo_url' AND jsonb_typeof(p_value) <> 'null'
          AND (jsonb_typeof(p_value) <> 'string' OR (p_value #>> '{}') !~ '^https://[^\s"<>]+$') THEN
        RAISE EXCEPTION 'logo_url : adresse https:// ou vide' USING ERRCODE = 'invalid_parameter_value';
    ELSIF p_key LIKE '%\_ttl' OR p_key IN ('tombstone_retention', 'password_max_failures', 'password_lock_seconds',
                                           'republish_grace') THEN
        IF jsonb_typeof(p_value) <> 'number' OR (p_value #>> '{}')::numeric <= 0 THEN
            RAISE EXCEPTION '% : nombre positif attendu', p_key USING ERRCODE = 'invalid_parameter_value';
        END IF;
    ELSIF p_key = 'pipeline_team_hosts' AND (jsonb_typeof(p_value) <> 'array' OR EXISTS (
            SELECT 1 FROM jsonb_array_elements(p_value) x
            WHERE jsonb_typeof(x) <> 'string' OR (x #>> '{}') !~ '^(\*\.)?[A-Za-z0-9._:-]+(/[0-9]{1,3})?$')) THEN
        RAISE EXCEPTION 'adresses autorisées aux gestionnaires : un nom d''hôte (ou *.domaine, ou bloc d''adresses) par ligne'
            USING ERRCODE = 'invalid_parameter_value';
    ELSIF p_key IN ('display_name') AND (jsonb_typeof(p_value) <> 'string' OR p_value #>> '{}' = '') THEN
        RAISE EXCEPTION '% : texte attendu', p_key USING ERRCODE = 'invalid_parameter_value';
    END IF;
    UPDATE lattis.server_config SET value = p_value WHERE key = p_key;
    PERFORM lattis.audit_log('set_config', p_key, v_before, p_value);
END
$$;
