#!/bin/sh
# Runs the backend's end-to-end tests: the real server binary against a
# throwaway Postgres and SeaweedFS in Docker, removed afterwards.
#   backend/e2e/run.sh [go test flags...]   e.g. -run TestLiveSession -v
# To use services of your own instead, set TEST_DATABASE_URL and S3_TEST_*
# and run `go test ./e2e/` yourself.
set -eu
cd "$(dirname "$0")/.."

name="polycode-e2e-$$"
config="$(mktemp -d)"
cleanup() {
  docker rm -f "$name-db" "$name-s3" >/dev/null 2>&1 || true
  rm -rf "$config"
}
trap cleanup EXIT INT TERM

cat >"$config/s3.json" <<'JSON'
{"identities":[{"name":"e2e","credentials":[{"accessKey":"e2e","secretKey":"e2e-secret"}],"actions":["Admin","Read","List","Tagging","Write"]}]}
JSON

docker run -d --name "$name-db" -p 127.0.0.1::5432 \
  -e POSTGRES_USER=e2e -e POSTGRES_PASSWORD=e2e -e POSTGRES_DB=e2e \
  postgres:17-alpine >/dev/null
docker run -d --name "$name-s3" -p 127.0.0.1::8333 -v "$config/s3.json:/etc/s3.json:ro" \
  chrislusf/seaweedfs:4.05 server -dir=/data -s3 -s3.port=8333 -s3.config=/etc/s3.json \
  -volume.max=20 -master.volumeSizeLimitMB=64 >/dev/null

db_port="$(docker port "$name-db" 5432/tcp | head -n1 | sed 's/.*://')"
s3_port="$(docker port "$name-s3" 8333/tcp | head -n1 | sed 's/.*://')"

printf 'waiting for postgres'
until docker exec "$name-db" pg_isready -h 127.0.0.1 -U e2e -d e2e >/dev/null 2>&1; do printf .; sleep 1; done
printf '\nwaiting for s3'
until curl -s -o /dev/null "http://127.0.0.1:$s3_port/"; do printf .; sleep 1; done
echo

export TEST_DATABASE_URL="postgres://e2e:e2e@127.0.0.1:$db_port/e2e?sslmode=disable"
export S3_TEST_ENDPOINT="127.0.0.1:$s3_port"
export S3_TEST_ACCESS_KEY=e2e S3_TEST_SECRET_KEY=e2e-secret S3_TEST_BUCKET=e2e
export E2E_MAKE_BUCKET=1
go test -count=1 -race "$@" ./e2e/
