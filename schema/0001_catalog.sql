-- Catalogue commun du Serveur Lattis (spécification §5.1).
--
-- Exécuté par lattisd --migrate sous le rôle lattis_service, propriétaire du
-- schéma lattis. Les tables d'identité ne sont lisibles que par lattis_service
-- et par les propriétaires de serveur (groupe lattis_server_owners).

CREATE SCHEMA IF NOT EXISTS lattis;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'lattis_server_owners') THEN
        CREATE ROLE lattis_server_owners NOLOGIN;
    END IF;
END
$$;

GRANT USAGE ON SCHEMA lattis TO PUBLIC;

CREATE TABLE lattis.server_config (
    key   text PRIMARY KEY,
    value jsonb NOT NULL
);

INSERT INTO lattis.server_config (key, value) VALUES
    ('display_name',          '"Serveur Lattis"'),
    ('logo_url',              'null'),
    ('access_mode',           '"invitation"'),
    ('default_role',          '"none"'),
    ('web_scripting',         'false'),
    ('access_token_ttl',      '900'),
    ('refresh_token_ttl',     '2592000'),
    ('invitation_ttl',        '1209600'),
    ('tombstone_retention',   '2592000'),
    ('password_max_failures', '5'),
    ('password_lock_seconds', '900');

CREATE TABLE lattis.identity_providers (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind              text NOT NULL CHECK (kind IN ('password', 'google', 'google_workspace', 'microsoft', 'keycloak', 'oidc')),
    label             text NOT NULL,
    issuer            text,
    client_id         text,
    client_secret_ref text,
    scopes            text[] NOT NULL DEFAULT '{openid,email,profile}',
    claim_name        text,
    claim_value       text,
    enabled           boolean NOT NULL DEFAULT true,
    position          integer NOT NULL DEFAULT 0,
    created_at        timestamptz NOT NULL DEFAULT now(),
    CHECK (kind = 'password' OR client_id IS NOT NULL),
    CHECK (kind NOT IN ('keycloak', 'oidc') OR issuer IS NOT NULL)
);

-- Un seul fournisseur « mot de passe » par serveur.
CREATE UNIQUE INDEX identity_providers_one_password ON lattis.identity_providers (kind) WHERE kind = 'password';

INSERT INTO lattis.identity_providers (kind, label, scopes, position)
VALUES ('password', 'Compte Lattis', '{}', 0);

CREATE TABLE lattis.allowed_tenants (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id  uuid NOT NULL REFERENCES lattis.identity_providers (id) ON DELETE CASCADE,
    kind         text NOT NULL CHECK (kind IN ('domain', 'tid', 'realm', 'email', 'any')),
    value        text NOT NULL DEFAULT '',
    default_role text,
    UNIQUE (provider_id, kind, value)
);

CREATE TABLE lattis.users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    pg_role       text NOT NULL UNIQUE,
    email         text NOT NULL,
    display_name  text NOT NULL DEFAULT '',
    provider_id   uuid REFERENCES lattis.identity_providers (id) ON DELETE SET NULL,
    subject       text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz,
    disabled      boolean NOT NULL DEFAULT false,
    UNIQUE (provider_id, subject),
    CHECK (email = lower(email))
);

CREATE INDEX users_email ON lattis.users (email);

CREATE TABLE lattis.local_credentials (
    user_id           uuid PRIMARY KEY REFERENCES lattis.users (id) ON DELETE CASCADE,
    password_hash     text,
    changed_at        timestamptz,
    failed_attempts   integer NOT NULL DEFAULT 0,
    locked_until      timestamptz,
    setup_token_hash  bytea UNIQUE,
    setup_expires_at  timestamptz
);

CREATE TABLE lattis.invitations (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email       text NOT NULL CHECK (email = lower(email)),
    workbook_id uuid,
    roles       text[] NOT NULL DEFAULT '{}',
    invited_by  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    accepted_at timestamptz,
    revoked_at  timestamptz
);

CREATE INDEX invitations_email ON lattis.invitations (email) WHERE accepted_at IS NULL AND revoked_at IS NULL;

CREATE TABLE lattis.audit (
    id     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at     timestamptz NOT NULL DEFAULT now(),
    actor  text NOT NULL,
    action text NOT NULL,
    target text,
    before jsonb,
    after  jsonb
);

CREATE INDEX audit_at ON lattis.audit (at);

-- Codes d'autorisation et jetons de rafraîchissement de lattisd (§7.1, §18).
-- Seuls les hachages SHA-256 sont conservés.
CREATE TABLE lattis.oauth_codes (
    code_hash      bytea PRIMARY KEY,
    user_id        uuid NOT NULL REFERENCES lattis.users (id) ON DELETE CASCADE,
    client_id      text NOT NULL,
    redirect_uri   text NOT NULL,
    code_challenge text NOT NULL,
    scope          text NOT NULL,
    nonce          text,
    expires_at     timestamptz NOT NULL,
    used_at        timestamptz
);

CREATE TABLE lattis.refresh_tokens (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash bytea NOT NULL UNIQUE,
    family     uuid NOT NULL,
    user_id    uuid NOT NULL REFERENCES lattis.users (id) ON DELETE CASCADE,
    client_id  text NOT NULL,
    scope      text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    revoked_at timestamptz
);

CREATE INDEX refresh_tokens_family ON lattis.refresh_tokens (family);
CREATE INDEX refresh_tokens_user ON lattis.refresh_tokens (user_id);

-- Droits : rien n'est lisible par PUBLIC ; les propriétaires de serveur lisent
-- le catalogue, les fonctions (0002) font le reste.
REVOKE ALL ON ALL TABLES IN SCHEMA lattis FROM PUBLIC;
GRANT SELECT ON lattis.server_config, lattis.identity_providers, lattis.allowed_tenants,
                lattis.users, lattis.invitations, lattis.audit
    TO lattis_server_owners;
