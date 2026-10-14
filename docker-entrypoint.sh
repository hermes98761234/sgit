#!/bin/sh
set -eu
DATA="${SG_DATA_DIR:-/data}"

if [ ! -f "$DATA/sg.db" ]; then
  sg-server init --data "$DATA"
fi

if [ -z "${SG_TLS_CERT:-}" ] && [ "${SG_TLS_SELF_SIGNED:-0}" = "1" ]; then
  mkdir -p "$DATA/tls"
  if [ ! -f "$DATA/tls/cert.pem" ]; then
    sg-server gen-cert --hosts "${SG_TLS_HOSTS:-localhost,127.0.0.1}" \
      --out-cert "$DATA/tls/cert.pem" --out-key "$DATA/tls/key.pem"
  fi
  export SG_TLS_CERT="$DATA/tls/cert.pem" SG_TLS_KEY="$DATA/tls/key.pem"
fi

exec sg-server serve "$@"
