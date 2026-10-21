-- Logo du serveur envoyé depuis l'administration (image PNG ou JPEG, réduite
-- par lattisd), servi par lattisd sur /logo. Sans image envoyée : l'adresse
-- logo_url si elle est réglée, sinon l'icône Lattis.

CREATE TABLE lattis.server_logo (
    singleton    boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    content_type text NOT NULL CHECK (content_type IN ('image/png', 'image/jpeg')),
    data         bytea NOT NULL CHECK (octet_length(data) BETWEEN 1 AND 1048576),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
REVOKE ALL ON lattis.server_logo FROM PUBLIC;

CREATE FUNCTION lattis.set_server_logo(p_type text, p_data bytea) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_server_owner();
    INSERT INTO lattis.server_logo (content_type, data) VALUES (p_type, p_data)
    ON CONFLICT (singleton) DO UPDATE SET content_type = excluded.content_type, data = excluded.data, updated_at = now();
    PERFORM lattis.audit_log('set_server_logo', NULL, NULL, jsonb_build_object('type', p_type, 'size', octet_length(p_data)));
END
$$;

CREATE FUNCTION lattis.clear_server_logo() RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_server_owner();
    DELETE FROM lattis.server_logo;
    PERFORM lattis.audit_log('clear_server_logo', NULL, NULL, NULL);
END
$$;

GRANT EXECUTE ON FUNCTION lattis.set_server_logo(text, bytea), lattis.clear_server_logo() TO PUBLIC;
