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
#   LATTIS_PG_IDLE_TX_TIMEOUT   session fermée si elle reste plus longtemps
#                               « idle in transaction » (5min ; 0 : jamais)
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
# Une transaction oubliée ouverte retient le point de reprise de tous les postes
# (sync_point : plus petit xid en cours) : chaque notification leur fait relire
# tout ce qui a changé depuis. Les sauvegardes (pg_dump) ne sont pas « idle ».
idle_in_transaction_session_timeout = '${LATTIS_PG_IDLE_TX_TIMEOUT:-5min}'
CONF
if ! grep -q "^include_if_exists = 'lattis.conf'" "$PGDATA/postgresql.conf"; then
	printf "\n# Réglages gérés par l'image be-lattis/server (en dernier : ils l'emportent).\ninclude_if_exists = 'lattis.conf'\n" >> "$PGDATA/postgresql.conf"
fi
if [ "$(id -u)" = 0 ]; then
	chown postgres:postgres "$PGDATA/lattis.conf"
fi
chmod 0600 "$PGDATA/lattis.conf"

# Certificat TLS : recopié à chaque démarrage depuis /etc/lattis/tls (un
# certificat renouvelé est pris au redémarrage), clé remise en 0600 (un
# rattachement de volume peut l'avoir rendue lisible par le groupe, ce que
# PostgreSQL refuse).
tls="${LATTIS_TLS_DIR:-/etc/lattis/tls}"
if [ -s "$tls/server.crt" ] && [ -s "$tls/server.key" ]; then
	install -m 0644 "$tls/server.crt" "$PGDATA/server.crt"
	install -m 0600 "$tls/server.key" "$PGDATA/server.key"
fi
if [ -f "$PGDATA/server.key" ]; then
	chmod 0600 "$PGDATA/server.key"
	if [ "$(id -u)" = 0 ]; then
		chown postgres:postgres "$PGDATA/server.key" "$PGDATA/server.crt" 2>/dev/null || true
	fi
fi
