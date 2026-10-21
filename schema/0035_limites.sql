-- Limites réglables dans l'administration (0 : sans limite).
-- - max_connected_users : utilisateurs connectés en même temps (bureau et
--   web) ; une nouvelle connexion au-delà est refusée, les propriétaires du
--   serveur entrent toujours.
-- - web_max_sessions : sessions de l'application web (sous le plafond
--   LATTIS_WEB_MAX_ENGINES).
-- - web_max_memory_mb : mémoire totale des moteurs web ; au-delà, plus de
--   nouvelle session.
-- - web_engine_memory_mb : mémoire allouée à chaque moteur web.

INSERT INTO lattis.server_config (key, value) VALUES
    ('max_connected_users', '0'),
    ('web_max_sessions', '0'),
    ('web_max_memory_mb', '0'),
    ('web_engine_memory_mb', '0');
