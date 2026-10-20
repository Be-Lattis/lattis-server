#!/bin/bash
# Initialisation de l'image be-lattis/server : rôle lattis_service, réglages
# OAuth et pg_hba (TLS seulement). Les migrations du schéma lattis sont
# appliquées ensuite par `lattisd --migrate`.
set -euo pipefail

: "${LATTIS_SERVICE_PASSWORD:?LATTIS_SERVICE_PASSWORD manquant}"
: "${LATTIS_ISSUER:?LATTIS_ISSUER manquant (URL publique de lattisd)}"
LATTIS_PUBLIC_KEYS_FILE="${LATTIS_PUBLIC_KEYS_FILE:-/etc/lattis/keys/public.pem}"
LATTIS_TLS_DIR="${LATTIS_TLS_DIR:-/etc/lattis/tls}"

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
     -v service_password="$LATTIS_SERVICE_PASSWORD" -v dbname="$POSTGRES_DB" \
     -f /usr/share/lattis/bootstrap.sql

# Certificat du serveur : copié dans PGDATA pour en être propriétaire (0600).
install -m 0644 "$LATTIS_TLS_DIR/server.crt" "$PGDATA/server.crt"
install -m 0600 "$LATTIS_TLS_DIR/server.key" "$PGDATA/server.key"

cat >> "$PGDATA/postgresql.conf" <<CONF

# --- Serveur Lattis ---
ssl = on
ssl_cert_file = 'server.crt'
ssl_key_file = 'server.key'
oauth_validator_libraries = 'lattis_oauth'
lattis_oauth.issuer = '${LATTIS_ISSUER}'
lattis_oauth.public_keys_file = '${LATTIS_PUBLIC_KEYS_FILE}'
shared_preload_libraries = 'pg_stat_statements'
hot_standby = on
wal_keep_size = '1GB'
CONF
# Connexions et réplicas : lattis.conf, réécrit à chaque démarrage.
/usr/local/bin/lattis-conf

cat > "$PGDATA/pg_hba.conf" <<HBA
# Serveur Lattis : TLS obligatoire. Utilisateurs par jeton lattisd,
# comptes techniques (lattis_service, DBA) par mot de passe.
local      all  all                         peer
hostssl    all  lattis_service  0.0.0.0/0   scram-sha-256
hostssl    all  lattis_service  ::/0        scram-sha-256
hostssl    replication  all     0.0.0.0/0   scram-sha-256
hostssl    all  +lattis_technical 0.0.0.0/0 scram-sha-256
hostssl    all  all  0.0.0.0/0  oauth issuer="${LATTIS_ISSUER}" scope="openid lattis" validator=lattis_oauth delegate_ident_mapping=1
hostssl    all  all  ::/0       oauth issuer="${LATTIS_ISSUER}" scope="openid lattis" validator=lattis_oauth delegate_ident_mapping=1
HBA

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<SQL
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'lattis_technical') THEN
    CREATE ROLE lattis_technical NOLOGIN;
  END IF;
END \$\$;
SQL

# Réplication en flux (§12) : rôle des réplicas, si un mot de passe est fourni.
if [ -n "${LATTIS_REPLICATION_PASSWORD:-}" ]; then
	psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
	     -v pw="$LATTIS_REPLICATION_PASSWORD" <<'SQL'
SELECT format('CREATE ROLE lattis_replicator LOGIN REPLICATION PASSWORD %L', :'pw')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'lattis_replicator') \gexec
SQL
fi
