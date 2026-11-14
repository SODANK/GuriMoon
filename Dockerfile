# syntax=docker/dockerfile:1
#
# Server image. The database is not in this image: docker-compose.yml runs
# MariaDB beside it.

FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN chmod +x rebuild.sh && CGO_ENABLED=0 ./rebuild.sh -o /out/gurimoon

FROM debian:bookworm-slim
WORKDIR /app
COPY --from=build /out/gurimoon ./gurimoon
# config.docker.json is the image's default; docker-compose.yml bind-mounts the
# checkout's copy over it so an edit needs no rebuild.
COPY config.docker.json ./config.json
# The string table is static and lives outside /app/data so that directory can
# be an empty volume on first start.
COPY data/strings.txt ./strings.txt
RUN mkdir -p /app/data
# 26000 game clients, 26080 admin panel, 80 Galnet / news for the in-game browser.
EXPOSE 26000 26080 80
VOLUME ["/app/data"]
ENTRYPOINT ["./gurimoon", "-config", "config.json", "-strings", "/app/strings.txt"]
