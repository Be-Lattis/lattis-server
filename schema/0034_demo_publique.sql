-- Démo publique (édition web) : /demo/ ouvre Lattis dans le navigateur sans
-- compte, sur un classeur jamais enregistré sur le serveur. Désactivée par
-- défaut ; réglage de l'administration (Serveur › Accès et publication).

INSERT INTO lattis.server_config (key, value) VALUES ('public_demo', 'false');
