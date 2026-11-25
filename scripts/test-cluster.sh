#!/usr/bin/env bash
# Run the integration suite against a disposable, loopback-only Valkey cluster.
set -euo pipefail
cd "$(dirname "$0")/.."

container=$(docker run --rm -d -p 127.0.0.1::6379 valkey/valkey:9.1.0-alpine3.23 \
  valkey-server --cluster-enabled yes --cluster-announce-ip 127.0.0.1 --appendonly yes)
trap 'docker rm -f "$container" >/dev/null' EXIT
port=$(docker port "$container" 6379/tcp)
port=${port##*:}
cli() { docker exec "$container" valkey-cli "$@"; }
for ((i=0; i<50; i++)); do
  if [[ $(cli ping 2>/dev/null) == PONG ]]; then break; fi
  sleep 0.2
done
cli config set cluster-announce-port "$port" | grep -qx OK
cli cluster addslotsrange 0 16383 | grep -qx OK
cli acl setuser telemost on '>fixture-password' '~telemost:*' +ping \
  '+cluster|slots' '+cluster|shards' +get +set +del +eval | grep -qx OK
for ((i=0; i<50; i++)); do
  if cli cluster info | grep -q 'cluster_state:ok'; then break; fi
  sleep 0.2
done
TEST_VALKEY_ADDR="127.0.0.1:$port" TEST_VALKEY_USER=telemost \
  TEST_VALKEY_PASSWORD=fixture-password go test -race -count=1 ./...
