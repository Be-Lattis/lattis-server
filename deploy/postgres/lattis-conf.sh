#!/bin/bash
# Réglages PostgreSQL gérés par l'image be-lattis/server : réécrits à chaque
# démarrage (parent et réplicas) dans $PGDATA/lattis.conf, inclus en fin de
# postgresql.conf. Une nouvelle version de l'image applique donc ses
# réglages d'elle-même, sans commande à la main.
#
#   LATTIS_PG_MAX_CONNECTIONS   connexions (400) ; un réplica doit en avoir
#                               au moins autant que son parent
#   LATTIS_PG_STANDBY_DELAY     attente maximale d'un rejeu bloqué par une
#                               lecture sur un réplica (30s)
set -euo pipefail
: "${PGDATA:?}"
[ -s "$PGDATA/PG_VERSION" ] || exit 0
cat > "$PGDATA/lattis.conf" <<CONF
# Géré par l'image be-lattis/server : réécrit à chaque démarrage, ne pas modifier.
# Chaque poste peut garder une connexion ouverte sous son propre rôle (pas de pooler).
max_connections = ${LATTIS_PG_MAX_CONNECTIONS:-400}
# Réplicas : le parent garde les lignes encore lues par un réplica ; un rejeu
# bloqué par une lecture attend au plus ce délai avant de l'annuler.
hot_standby_feedback = on
max_standby_streaming_delay = '${LATTIS_PG_STANDBY_DELAY:-30s}'
CONF
if ! grep -q "^include_if_exists = 'lattis.conf'" "$PGDATA/postgresql.conf"; then
	printf "\n# Réglages gérés par l'image be-lattis/server (en dernier : ils l'emportent).\ninclude_if_exists = 'lattis.conf'\n" >> "$PGDATA/postgresql.conf"
fi
if [ "$(id -u)" = 0 ]; then
	chown postgres:postgres "$PGDATA/lattis.conf"
fi
chmod 0600 "$PGDATA/lattis.conf"
