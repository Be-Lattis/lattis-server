#!/bin/bash
# Point d'entrée du parent be-lattis/server : réglages gérés appliqués à
# chaque démarrage, puis le point d'entrée officiel de PostgreSQL.
set -euo pipefail
/usr/local/bin/lattis-conf
exec docker-entrypoint.sh "$@"
