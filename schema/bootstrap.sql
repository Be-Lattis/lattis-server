-- Amorçage du Serveur Lattis, exécuté une seule fois par un superutilisateur
-- (script d'initialisation de l'image be-lattis/server, ou à la main sur un
-- PostgreSQL 18 existant). Variables psql : service_password, dbname.
--
-- lattis_service n'est pas superutilisateur : CREATEROLE (délégation
-- PostgreSQL 16+), CREATE sur la base, lecture des statistiques et droit de
-- couper les sessions des utilisateurs désactivés.

SELECT format('CREATE ROLE lattis_service LOGIN CREATEROLE PASSWORD %L', :'service_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'lattis_service')
\gexec

GRANT CREATE, CONNECT ON DATABASE :"dbname" TO lattis_service;
GRANT pg_monitor, pg_signal_backend TO lattis_service;
