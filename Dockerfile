# syntax=docker/dockerfile:1
FROM golang:1.26.5-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/wallet-service ./cmd/wallet-service

# alpine, not distroless: the entrypoint below is a shell script that
# sources the IAM credentials file the provisioning service generates.
FROM alpine:3.21
RUN apk add --no-cache ca-certificates

# Fixed, non-root UID/GID - matched by APP_RUNTIME_UID/APP_RUNTIME_GID in
# docker-compose.yml, which provision.sh uses to chown the app's
# credentials file so this user can read it despite its 0600 mode. A
# compromise of the app process gets neither root in the container nor
# access to any credential but its own.
RUN addgroup -g 10001 app && adduser -D -u 10001 -G app app

COPY --from=build /out/wallet-service /usr/local/bin/wallet-service
COPY deploy/docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

USER app

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["serve"]
