#!/usr/bin/env bash
# Run on rasp001. For each relay port (5433-5438) test the TCP connection, then,
# if psql is installed, a real login as the barman role. Exit code is non-zero
# if any port fails. Password: PGPASSWORD, or ~/.pgpass of the calling user.
set -u

HOST="${HOST:-10.8.0.1}"
declare -A NAMES=(
  [5433]=deckhand [5434]=chutag [5435]=sygnet
  [5436]=listmonk [5437]=postiz [5438]=nyx
)

fail=0
have_psql=0
command -v psql >/dev/null 2>&1 && have_psql=1
[ "$have_psql" -eq 0 ] && echo "note: psql not found, only TCP is tested"

for port in 5433 5434 5435 5436 5437 5438; do
  name="${NAMES[$port]}"
  if ! nc -z -w3 "$HOST" "$port" >/dev/null 2>&1; then
    printf '%-9s %s:%s  TCP FAIL\n' "$name" "$HOST" "$port"
    fail=1
    continue
  fi
  if [ "$have_psql" -eq 0 ]; then
    printf '%-9s %s:%s  TCP ok\n' "$name" "$HOST" "$port"
    continue
  fi
  if out=$(PGCONNECT_TIMEOUT=5 psql "host=$HOST port=$port user=barman dbname=postgres" \
           -Atc "select version()" 2>&1); then
    printf '%-9s %s:%s  TCP ok, login ok: %s\n' "$name" "$HOST" "$port" "${out%%,*}"
  else
    printf '%-9s %s:%s  TCP ok, LOGIN FAIL: %s\n' "$name" "$HOST" "$port" "$(echo "$out" | head -1)"
    fail=1
  fi
done

if [ "$fail" -eq 0 ]; then echo "all ports ok"; else echo "some ports failed"; fi
exit "$fail"
