-- Limite de l'administration : sessions de la démo publique en même temps
-- (0 : sans limite, sous le plafond de l'installation).

INSERT INTO lattis.server_config (key, value) VALUES ('web_demo_max_sessions', '0');
