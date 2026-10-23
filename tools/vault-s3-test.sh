#!/usr/bin/env bash
# Runs the off-site copy against a real S3 service and the real restic:
# starts a throwaway MinIO in a container (Podman, else Docker), runs
# TestVaultIntegration (internal/app/vault_integration_test.go), removes the
# container. The test creates its own bucket with Object Lock (governance,
# one day).
#
#   tools/vault-s3-test.sh                 # restic on PATH
#   PGCELLAR_TEST_RESTIC=/path/to/restic tools/vault-s3-test.sh
#   MINIO_IMAGE=... PORT=19000 tools/vault-s3-test.sh
#
# The image defaults to docker.io/pgsty/minio (a community build of MinIO:
# MinIO no longer publishes its own images on Docker Hub).
set -euo pipefail
cd "$(dirname "$0")/.."

runtime="${CONTAINER_RUNTIME:-}"
if [ -z "$runtime" ]; then
  if command -v podman >/dev/null 2>&1; then runtime=podman
  elif command -v docker >/dev/null 2>&1; then runtime=docker
  else echo "vault-s3-test: neither podman nor docker is installed" >&2; exit 1; fi
fi
image="${MINIO_IMAGE:-docker.io/pgsty/minio:latest}"
port="${PORT:-19000}"
name="pgcellar-vault-minio-$$"
user=pgcellar
secret="pgcellar-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')"

restic="${PGCELLAR_TEST_RESTIC:-restic}"
command -v "$restic" >/dev/null 2>&1 || { echo "vault-s3-test: restic not found (set PGCELLAR_TEST_RESTIC)" >&2; exit 1; }

cleanup() { "$runtime" rm -f "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT

"$runtime" run -d --name "$name" -p "127.0.0.1:$port:9000" \
  -e MINIO_ROOT_USER="$user" -e MINIO_ROOT_PASSWORD="$secret" \
  "$image" server /data >/dev/null
for _ in $(seq 1 30); do
  curl -fsS "http://127.0.0.1:$port/minio/health/live" >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS "http://127.0.0.1:$port/minio/health/live" >/dev/null || { "$runtime" logs "$name" >&2; exit 1; }

PGCELLAR_TEST_S3_ENDPOINT="http://127.0.0.1:$port" \
PGCELLAR_TEST_S3_ACCESS_KEY="$user" \
PGCELLAR_TEST_S3_SECRET_KEY="$secret" \
PGCELLAR_TEST_RESTIC="$restic" \
  go test ./internal/app/ -run TestVaultIntegration -count=1 -v
