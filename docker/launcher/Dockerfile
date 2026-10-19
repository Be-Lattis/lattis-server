# Image be-lattis/lattis-launcher : mini lanceur des moteurs de l'édition web
# en mode Docker (LATTIS_WEB_LAUNCHER=docker). Il reçoit le socket Docker :
# il tourne en root (lecture du socket) et ne fait que démarrer, surveiller
# et arrêter l'image du moteur.
#   docker build -f docker/launcher/Dockerfile -t be-lattis/lattis-launcher:dev .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/lattis-launcher ./cmd/lattis-launcher

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/lattis-launcher /usr/local/bin/lattis-launcher
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/lattis-launcher"]
