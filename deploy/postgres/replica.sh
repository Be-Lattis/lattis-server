#!/bin/bash
# Point d'entrée d'un réplica be-lattis/server (§12) : au premier démarrage,
# copie le parent (pg_basebackup, réplication en flux, TLS) puis démarre en
# hot_standby. La configuration (TLS, OAuth, pg_hba) vient du parent.
#
#   LATTIS_PRIMARY_HOST, LATTIS_PRIMARY_PORT (5432)
#   LATTIS_REPLICATION_PASSWORD   mot de passe de lattis_replicator
#   LATTIS_REPLICATION_SLOT       facultatif : emplacement de réplication
#                                 (créé au premier essai, repris ensuite) ;
#                                 « auto » : nommé d'après le nom d'hôte
set -euo pipefail
: "${LATTIS_PRIMARY_HOST:?LATTIS_PRIMARY_HOST manquant}"
: "${LATTIS_REPLICATION_PASSWORD:?LATTIS_REPLICATION_PASSWORD manquant}"
: "${PGDATA:?}"
port="${LATTIS_PRIMARY_PORT:-5432}"

if [ ! -s "$PGDATA/PG_VERSION" ]; then
	mkdir -p "$PGDATA"
	chown postgres:postgres "$PGDATA"
	chmod 0700 "$PGDATA"
	conn="host=$LATTIS_PRIMARY_HOST port=$port user=lattis_replicator sslmode=require application_name=$(hostname)"
	slot=()
	if [ "${LATTIS_REPLICATION_SLOT:-}" = auto ]; then
		LATTIS_REPLICATION_SLOT="$(hostname | tr -c 'a-zA-Z0-9\n' '_' | tr 'A-Z' 'a-z')"
	fi
	if [ -n "${LATTIS_REPLICATION_SLOT:-}" ]; then
		slot=(-S "$LATTIS_REPLICATION_SLOT" -C)
	fi
	echo "réplica : copie du parent $LATTIS_PRIMARY_HOST:$port"
	until PGPASSWORD="$LATTIS_REPLICATION_PASSWORD" gosu postgres pg_basebackup -d "$conn" -D "$PGDATA" \
		-R -X stream --checkpoint=fast "${slot[@]}"; do
		echo "réplica : parent indisponible, nouvel essai dans 5 s"
		rm -rf "${PGDATA:?}"/*
		# L'emplacement existe déjà (réplica recréé à vide) : on le reprend.
		[ "${#slot[@]}" -eq 3 ] && slot=(-S "$LATTIS_REPLICATION_SLOT")
		sleep 5
	done
fi
/usr/local/bin/lattis-conf
exec docker-entrypoint.sh postgres
