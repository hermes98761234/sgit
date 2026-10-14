# syntax=docker/dockerfile:1
ARG GO_VERSION=1.27
FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sg-server ./cmd/sg-server

FROM alpine:3.20
RUN apk add --no-cache git openssh-client ca-certificates \
 && adduser -D -u 10001 -h /home/sg sg \
 && mkdir -p /data && chown sg:sg /data
COPY --from=build /out/sg-server /usr/local/bin/sg-server
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
USER sg
ENV SG_DATA_DIR=/data SG_LISTEN=:7419
VOLUME /data
EXPOSE 7419
HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=3 CMD ["sg-server", "health"]
ENTRYPOINT ["docker-entrypoint.sh"]
