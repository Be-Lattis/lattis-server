# Module de validation OAuth lattis_oauth (PostgreSQL 18).
#   make PG_CONFIG=/usr/lib/postgresql/18/bin/pg_config && make install
MODULE_big = lattis_oauth
OBJS = lattis_oauth.o
PGFILEDESC = "lattis_oauth - validation des jetons lattisd"
SHLIB_LINK = -lcrypto

PG_CONFIG ?= pg_config
PGXS := $(shell $(PG_CONFIG) --pgxs)
include $(PGXS)
