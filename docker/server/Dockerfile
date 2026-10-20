# Image be-lattis/server : PostgreSQL 18 officiel + module lattis_oauth +
# script d'initialisation (rôle lattis_service, TLS, pg_hba OAuth).
#   docker build -f docker/server/Dockerfile -t be-lattis/server:dev-pg18 .
ARG PG_MAJOR=18

FROM postgres:${PG_MAJOR} AS build
ARG PG_MAJOR
RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential "postgresql-server-dev-${PG_MAJOR}" libssl-dev libkrb5-dev  \
 && rm -rf /var/lib/apt/lists/*
COPY oauth/ /src/oauth/
RUN make -C /src/oauth \
 && make -C /src/oauth install DESTDIR=/out

FROM postgres:${PG_MAJOR}
COPY --from=build /out/usr/lib/postgresql/ /usr/lib/postgresql/
COPY schema/bootstrap.sql /usr/share/lattis/bootstrap.sql
COPY deploy/postgres/initdb/10-lattis.sh /docker-entrypoint-initdb.d/10-lattis.sh
COPY deploy/postgres/replica.sh /usr/local/bin/lattis-replica
COPY deploy/postgres/lattis-conf.sh /usr/local/bin/lattis-conf
COPY deploy/postgres/entrypoint.sh /usr/local/bin/lattis-entrypoint
RUN for f in /docker-entrypoint-initdb.d/10-lattis.sh /usr/local/bin/lattis-replica /usr/local/bin/lattis-conf /usr/local/bin/lattis-entrypoint; do \
      sed -i 's/\r$//' "$f" && chmod +x "$f"; done
ENV POSTGRES_DB=lattis
# Réglages gérés (lattis.conf) appliqués à chaque démarrage.
ENTRYPOINT ["lattis-entrypoint"]
CMD ["postgres"]
