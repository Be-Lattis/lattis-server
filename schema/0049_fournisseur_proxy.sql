-- Fournisseur « reverse proxy » : le proxy placé devant lattisd a déjà
-- authentifié la personne et transmet son adresse dans un en-tête, avec un
-- en-tête de contrôle dont la valeur, secrète, prouve que la requête vient
-- bien de lui. Colonnes : client_id = en-tête de l'adresse e-mail,
-- claim_name = en-tête de contrôle, client_secret_ref = valeur attendue,
-- claim_value = en-tête du nom affiché (facultatif).

ALTER TABLE lattis.identity_providers DROP CONSTRAINT identity_providers_kind_check,
    ADD CONSTRAINT identity_providers_kind_check
    CHECK (kind IN ('password', 'google', 'google_workspace', 'microsoft', 'keycloak', 'oidc', 'proxy'));
