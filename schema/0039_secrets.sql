-- Secrets saisis dans l'administration (secret d'un fournisseur d'identité,
-- mot de passe d'une source de pipeline), à la place d'un fichier du dossier
-- des secrets : chiffrés par lattisd (clé tirée de sa clé de signature),
-- lisibles du seul compte de service, jamais relus par l'administration.
-- Référence : db-<32 hex>, dans client_secret_ref ou secret_ref.

CREATE TABLE lattis.stored_secrets (
    name       text PRIMARY KEY CHECK (name ~ '^db-[0-9a-f]{32}$'),
    key_id     text NOT NULL,
    sealed     bytea NOT NULL CHECK (octet_length(sealed) BETWEEN 1 AND 65536),
    created_at timestamptz NOT NULL DEFAULT now()
);
REVOKE ALL ON lattis.stored_secrets FROM PUBLIC;

-- Enregistrement (propriétaires du serveur) ; p_old : secret remplacé.
CREATE FUNCTION lattis.store_secret(p_name text, p_key_id text, p_sealed bytea, p_old text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_server_owner();
    INSERT INTO lattis.stored_secrets (name, key_id, sealed) VALUES (p_name, p_key_id, p_sealed);
    IF p_old IS NOT NULL AND p_old <> p_name THEN
        DELETE FROM lattis.stored_secrets WHERE name = p_old;
    END IF;
    PERFORM lattis.audit_log('store_secret', p_name, NULL, NULL);
END
$$;

-- Entretien (lattisd) : secrets que plus rien ne désigne, après une heure
-- (le temps qu'un enregistrement en cours aboutisse).
CREATE FUNCTION lattis.secrets_housekeeping() RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    n integer;
BEGIN
    PERFORM lattis.require_service();
    DELETE FROM lattis.stored_secrets s
    WHERE s.created_at < now() - interval '1 hour'
      AND NOT EXISTS (SELECT 1 FROM lattis.identity_providers p WHERE p.client_secret_ref = s.name)
      AND NOT EXISTS (SELECT 1 FROM lattis.pipelines x WHERE x.secret_ref = s.name);
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END
$$;

REVOKE EXECUTE ON FUNCTION lattis.secrets_housekeeping() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION lattis.store_secret(text, text, bytea, text) TO PUBLIC;
