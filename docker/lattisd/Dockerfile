# Image be-lattis/lattisd.
#   docker build -f docker/lattisd/Dockerfile --build-arg VERSION=0.1.0 -t be-lattis/lattisd:dev .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/Be-Lattis/lattis-server/internal/httpserver.Version=${VERSION}" \
      -o /out/lattisd ./cmd/lattisd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/lattisd /usr/local/bin/lattisd
EXPOSE 8080 9464
ENTRYPOINT ["/usr/local/bin/lattisd"]
CMD ["--migrate"]
