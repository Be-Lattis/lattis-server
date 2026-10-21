-- Réparation : l'administration effaçait l'émetteur des fournisseurs Google
-- à chaque enregistrement (le champ n'est pas affiché pour eux), et la
-- connexion répondait « le fournisseur ne répond pas ».

UPDATE lattis.identity_providers SET issuer = 'https://accounts.google.com'
WHERE kind IN ('google', 'google_workspace') AND coalesce(issuer, '') = '';
