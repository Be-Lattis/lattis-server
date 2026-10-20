-- Comptes techniques (pipelines, formulaires : adresse en @lattis.invalid) :
-- ils n'agissent que par lattisd, sous leur identité, et ne se connectent
-- jamais en personne. Garanti par la base, en plus des fonctions :
-- - aucun fournisseur d'identité ne peut leur être relié (pas de connexion
--   OAuth ou mot de passe à leur nom) ;
-- - aucune invitation ne peut leur être adressée (enroll ne relie une
--   identité à un compte existant que par une invitation en cours).

ALTER TABLE lattis.users ADD CONSTRAINT users_technical_no_login
    CHECK (email NOT LIKE '%@lattis.invalid' OR (provider_id IS NULL AND subject IS NULL));

ALTER TABLE lattis.invitations ADD CONSTRAINT invitations_not_technical
    CHECK (email NOT LIKE '%@lattis.invalid');
