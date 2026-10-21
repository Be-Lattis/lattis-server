-- Durée d'une session de la démo publique (secondes) : prévenue avant la
-- fin, la personne est expulsée à l'échéance.

INSERT INTO lattis.server_config (key, value) VALUES ('demo_session_seconds', '600');
