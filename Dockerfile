# syntax=docker/dockerfile:1
ARG GO_VERSION=1.27
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/sg-server ./cmd/sg ./cmd/sgd

FROM alpine:3.20
RUN apk add --no-cache git openssh-client ca-certificates \
 && adduser -D -u 10001 -h /home/sg sg \
 && mkdir -p /data && chown sg:sg /data
COPY --from=build /out/sg-server /out/sg /out/sgd /usr/local/bin/
COPY --chmod=0755 contrib/sg-git-askpass /usr/local/bin/sg-git-askpass
COPY --chmod=0755 docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
USER sg
ENV SG_DATA_DIR=/data SG_LISTEN=:7419
VOLUME /data
EXPOSE 7419
HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=3 CMD ["sg-server", "health"]
ENTRYPOINT ["docker-entrypoint.sh"]
