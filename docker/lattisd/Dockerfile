# Image be-lattis/lattisd.
#   docker build -f docker/lattisd/Dockerfile --build-arg VERSION=0.1.0 -t be-lattis/lattisd:dev .
# Édition web (avec le moteur lattis-engine, image du dépôt lattis-app) :
#   docker build -f docker/lattisd/Dockerfile --target web #     --build-arg ENGINE_IMAGE=ghcr.io/be-lattis/lattis-engine:main -t be-lattis/lattisd:web .
ARG ENGINE_IMAGE=ghcr.io/be-lattis/lattis-engine:main

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath       -ldflags "-s -w -X github.com/Be-Lattis/lattis-server/internal/httpserver.Version=${VERSION}"       -o /out/lattisd ./cmd/lattisd
# Répertoire des sessions web, à l'utilisateur nonroot (volume initialisé avec).
RUN mkdir -p /out/web /out/pipelines

FROM ${ENGINE_IMAGE} AS engine

# Édition web : lattisd et lattis-engine.
FROM gcr.io/distroless/static-debian12:nonroot AS web
COPY --from=build /out/lattisd /usr/local/bin/lattisd
COPY --from=engine /usr/local/bin/lattis-engine /usr/local/bin/lattis-engine
COPY --from=build --chown=65532:65532 /out/web /var/lib/lattis/web
COPY --from=build --chown=65532:65532 /out/pipelines /var/lib/lattis/pipelines
ENV LATTIS_EDITION=web LATTIS_WEB_ENGINE=/usr/local/bin/lattis-engine LATTIS_WEB_CACHE_DIR=/var/lib/lattis/web
EXPOSE 8080 9464
ENTRYPOINT ["/usr/local/bin/lattisd"]
CMD ["--migrate"]

# Par défaut : édition « gestion des rôles », sans moteur.
FROM gcr.io/distroless/static-debian12:nonroot AS admin
COPY --from=build /out/lattisd /usr/local/bin/lattisd
COPY --from=build --chown=65532:65532 /out/pipelines /var/lib/lattis/pipelines
EXPOSE 8080 9464
ENTRYPOINT ["/usr/local/bin/lattisd"]
CMD ["--migrate"]
