# The plain HTTPS git host of the Docker end-to-end test.
ARG GO_VERSION=1.27
FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY test/e2e/githttpd/ ./
RUN go mod init githttpd >/dev/null 2>&1 && CGO_ENABLED=0 go build -o /out/githttpd .

FROM alpine:3.20
RUN apk add --no-cache git git-daemon && mkdir -p /gitca
COPY --from=build /out/githttpd /usr/local/bin/githttpd
COPY --chmod=0755 test/e2e/git-seed.sh /usr/local/bin/git-seed.sh
HEALTHCHECK --interval=2s --timeout=3s --retries=30 CMD ["test", "-s", "/gitca/cert.pem"]
ENTRYPOINT ["git-seed.sh"]
