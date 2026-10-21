#!/usr/bin/env bash
# Tests of pgcellar-updater against a fake pgCellar API (fake_api.py) and a
# fake docker (fake-docker). Nothing touches Docker or the network.
#   deploy/updater/test/run.sh
# Assertions are strings evaluated later by check(), hence single quotes.
# shellcheck disable=SC2016,SC2034
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
updater="$here/../pgcellar-updater"
tmp=$(mktemp -d)
state="$tmp/state"
mkdir -p "$state"
api_pid=
# shellcheck disable=SC2329 # called by the EXIT trap
finish() {
  if [ -n "$api_pid" ]; then
    kill "$api_pid" 2>/dev/null || true
    wait "$api_pid" 2>/dev/null || true
  fi
  rm -rf "$tmp"
}
trap finish EXIT

KEY=pgc_test_0123456789abcdef
D_GOOD=sha256:1111111111111111111111111111111111111111111111111111111111111111
D_BAD=sha256:2222222222222222222222222222222222222222222222222222222222222222

python3 "$here/fake_api.py" "$state" &
api_pid=$!
for _ in $(seq 50); do [ -s "$state/port" ] && break; sleep 0.1; done
[ -s "$state/port" ] || { echo "fake API did not start"; exit 1; }
port=$(cat "$state/port")

pass=0 fail=0
ok()  { pass=$((pass + 1)); printf '  ok   %s\n' "$1"; }
bad() { fail=$((fail + 1)); printf '  FAIL %s\n' "$1"; }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }

# reset: a fresh compose directory and API state, version 0.1.0 running.
reset() {
  rm -rf "$tmp/compose" "$tmp/lock"*
  find "$state" -mindepth 1 ! -name port -delete
  mkdir -p "$tmp/compose"
  printf 'services: {}\n' >"$tmp/compose/docker-compose.yml"
  printf '# compose variables\nPGCELLAR_TAG=0.1.0\nOTHER_SECRET=keepme\n' >"$tmp/compose/.env"
  chmod 600 "$tmp/compose/.env"
  printf '%s\n' "$KEY" >"$tmp/key"
  chmod 600 "$tmp/key"
  echo "$KEY" >"$state/key"
  echo 0.1.0 >"$state/running"
  echo 0.1.1 >"$state/latest"
  echo ghcr.io/dbfactory-io/pgcellar:0.1.1 >"$state/image"
  echo "$D_GOOD" >"$state/digest"
  echo "$D_GOOD" >"$state/remote_digest"
  echo 200 >"$state/ready"
  : >"$state/docker.log"
  cat >"$tmp/updater.env" <<CONF
COMPOSE_DIR=$tmp/compose
SERVICE="pgcellar pgcellar-ssh"
API_URL=http://127.0.0.1:$port
API_KEY_FILE=$tmp/key
HEALTH_TIMEOUT=3
DOCKER=$here/fake-docker
LOCK_FILE=$tmp/lock
DRY_RUN=0
CONF
}

# run: start the updater, output in $out, exit code in $rc.
run() {
  rc=0
  out=$(UPDATER_CONFIG="$tmp/updater.env" FAKE_STATE="$state" POLL_INTERVAL=0.2 "$updater" "$@" 2>&1) || rc=$?
  printf '%s\n' "$out" | sed 's/^/       | /'
}
tag()     { sed -n 's/^PGCELLAR_TAG=//p' "$tmp/compose/.env"; }
dlog()    { cat "$state/docker.log"; }
no_key()  { ! printf '%s' "$out" | grep -qF "$KEY"; }
failed="$tmp/compose/.pgcellar-updater.failed"
failed_lines() { [ -f "$failed" ] && awk -v v="$1" '$1 == v' "$failed" | wc -l | tr -d ' ' || echo 0; }

echo "1. up to date"
reset; echo 0.1.1 >"$state/running"
run
check "exit 0" '[ "$rc" = 0 ]'
check "says up to date" 'printf "%s" "$out" | grep -q "up to date (0.1.1)"'
check "docker not called" '[ ! -s "$state/docker.log" ]'

echo "2. busy (409)"
reset; touch "$state/busy"
run
check "exit 0" '[ "$rc" = 0 ]'
check "gives the reason" 'printf "%s" "$out" | grep -q "a backup of chutag is running"'
check "docker not called" '[ ! -s "$state/docker.log" ]'
check "tag unchanged" '[ "$(tag)" = 0.1.0 ]'

echo "2b. outside the window (409), then --now"
reset; echo 409 >"$state/ready"
run
check "exit 0, says outside the window" '[ "$rc" = 0 ] && printf "%s" "$out" | grep -q "outside the update window"'
check "the timer run never sends now=1" '! grep -q now "$state/queries"'
check "docker not called" '[ ! -s "$state/docker.log" ]'
run --now
check "--now: asks /ready?now=1 and updates" '[ "$rc" = 0 ] && grep -qx "now=1" "$state/queries" && [ "$(tag)" = 0.1.1 ]'
reset; touch "$state/busy"
run --now
check "--now while a backup runs: still refused" '[ "$rc" = 0 ] && printf "%s" "$out" | grep -q "a backup of chutag is running" && [ ! -s "$state/docker.log" ]'

echo "3. successful update (buildx)"
reset
run
check "exit 0" '[ "$rc" = 0 ]'
check "journal: updated from 0.1.0 to 0.1.1" 'printf "%s" "$out" | grep -q "updated from 0.1.0 to 0.1.1"'
check "tag pinned to 0.1.1" '[ "$(tag)" = 0.1.1 ]'
check "previous tag saved" '[ "$(cat "$tmp/compose/.pgcellar-updater.prev")" = 0.1.0 ]'
check "other lines of .env kept" 'grep -qx "OTHER_SECRET=keepme" "$tmp/compose/.env" && grep -qx "# compose variables" "$tmp/compose/.env"'
check "mode 0600 kept" '[ "$(ls -l "$tmp/compose/.env" | cut -c1-10)" = "-rw-------" ]'
check "pull, digest via buildx, compose up both services" 'dlog | grep -q "^docker pull ghcr.io/dbfactory-io/pgcellar:0.1.1$" && dlog | grep -q "buildx imagetools inspect" && dlog | grep -q "up -d pgcellar pgcellar-ssh$"'
check "API key never printed" 'no_key'
check "API key never on a docker command line" '! grep -qF "$KEY" "$state/docker.log"'
run
check "second run: up to date, idempotent" '[ "$rc" = 0 ] && printf "%s" "$out" | grep -q "up to date (0.1.1)"'

echo "3b. successful update without buildx (image inspect), tag not pinned yet"
reset; touch "$state/no-buildx"
printf 'OTHER_SECRET=keepme\n' >"$tmp/compose/.env"
run
check "exit 0" '[ "$rc" = 0 ]'
check "digest via image inspect" 'dlog | grep -q "image inspect" && ! dlog | grep -q imagetools'
check "tag line added" '[ "$(tag)" = 0.1.1 ]'
check "rollback target = running version" '[ "$(cat "$tmp/compose/.pgcellar-updater.prev")" = 0.1.0 ]'

echo "4. digest mismatch"
reset; echo "$D_BAD" >"$state/remote_digest"
run
check "exit 1" '[ "$rc" = 1 ]'
check "says digest mismatch" 'printf "%s" "$out" | grep -q "digest mismatch"'
check "tag unchanged" '[ "$(tag)" = 0.1.0 ]'
check "no compose up" '! dlog | grep -q compose'
check "still running 0.1.0" '[ "$(cat "$state/running")" = 0.1.0 ]'

echo "5. image outside ghcr.io/dbfactory-io/pgcellar"
for img in ghcr.io/evil/pgcellar:0.1.1 docker.io/dbfactory-io/pgcellar:0.1.1 ghcr.io/dbfactory-io/pgcellar-evil:0.1.1 "ghcr.io/dbfactory-io/pgcellar@$D_GOOD"; do
  reset; echo "$img" >"$state/image"
  run
  check "refused: $img" '[ "$rc" = 1 ] && printf "%s" "$out" | grep -q "refusing image"'
  check "nothing pulled, tag unchanged" '[ ! -s "$state/docker.log" ] && [ "$(tag)" = 0.1.0 ]'
done

echo "6. health does not come back: rollback"
reset; echo 0.1.1 >"$state/broken"
run
check "exit 2" '[ "$rc" = 2 ]'
check "says rolled back" 'printf "%s" "$out" | grep -q "rolled back: PGCELLAR_TAG=0.1.0"'
check "old tag restored" '[ "$(tag)" = 0.1.0 ]'
check "old version running" '[ "$(cat "$state/running")" = 0.1.0 ]'
check "compose up twice" '[ "$(dlog | grep -c "compose")" = 2 ]'
check "other lines of .env kept" 'grep -qx "OTHER_SECRET=keepme" "$tmp/compose/.env"'
check "0.1.1 recorded as failed, one line" '[ "$(failed_lines 0.1.1)" = 1 ] && grep -q "^0.1.1 20[0-9-]*T[0-9:]*Z rolled-back$" "$failed"'
check "failed list mode 0644, no temporary file left" '[ "$(ls -l "$failed" | cut -c1-10)" = "-rw-r--r--" ] && [ -z "$(ls -A "$tmp/compose" | grep updater-failed)" ]'

echo "6b. next run: the failed version is not tried again"
: >"$state/docker.log"; rm -f "$state/queries"
run
check "exit 0" '[ "$rc" = 0 ]'
check "says failed, rolled back, not retrying, how to retry" 'printf "%s" "$out" | grep -q "version 0.1.1 failed on .* and was rolled back; not retrying. Remove it from $failed to try again"'
check "no pull, no compose, /ready not asked" '[ ! -s "$state/docker.log" ] && [ ! -s "$state/queries" ]'
check "still on 0.1.0" '[ "$(tag)" = 0.1.0 ] && [ "$(cat "$state/running")" = 0.1.0 ]'
run --now
check "--now does not bypass the list" '[ "$rc" = 0 ] && [ ! -s "$state/docker.log" ]'
run --dry-run
check "--dry-run lists the skipped version" '[ "$rc" = 0 ] && printf "%s" "$out" | grep -q "dry run: not retried (in $failed): 0.1.1, failed on .* (rolled-back)"'

echo "6c. a newer release is tried normally"
echo 0.1.2 >"$state/latest"; echo ghcr.io/dbfactory-io/pgcellar:0.1.2 >"$state/image"
run
check "exit 0, updated to 0.1.2" '[ "$rc" = 0 ] && printf "%s" "$out" | grep -q "updated from 0.1.0 to 0.1.2" && [ "$(tag)" = 0.1.2 ]'
check "0.1.1 stays on the list" '[ "$(failed_lines 0.1.1)" = 1 ]'

echo "6d. --retry-failed"
reset; echo 0.1.1 >"$state/broken"
run
check "first failure: exit 2, listed" '[ "$rc" = 2 ] && [ "$(failed_lines 0.1.1)" = 1 ]'
: >"$state/docker.log"
run --retry-failed
check "retried and failed again: exit 2, pulled again" '[ "$rc" = 2 ] && dlog | grep -q "^docker pull ghcr.io/dbfactory-io/pgcellar:0.1.1$"'
check "still listed once" '[ "$(failed_lines 0.1.1)" = 1 ]'
rm -f "$state/broken"
run --retry-failed
check "retried, release fixed: exit 0, on 0.1.1" '[ "$rc" = 0 ] && printf "%s" "$out" | grep -q "trying it again" && [ "$(tag)" = 0.1.1 ]'
check "removed from the list" '[ "$(failed_lines 0.1.1)" = 0 ] && printf "%s" "$out" | grep -q "0.1.1 removed from"'
reset; echo 0.1.1 >"$state/broken"
run
rm -f "$state/broken"
rc=0; out=$(RETRY_FAILED=1 UPDATER_CONFIG="$tmp/updater.env" FAKE_STATE="$state" POLL_INTERVAL=0.2 "$updater" 2>&1) || rc=$?
check "RETRY_FAILED=1 works like --retry-failed" '[ "$rc" = 0 ] && [ "$(tag)" = 0.1.1 ] && [ "$(failed_lines 0.1.1)" = 0 ]'

echo "6e. rollback fails too: exit 3, recorded"
reset; printf '0.1.1\n0.1.0\n' >"$state/broken"
run
check "exit 3" '[ "$rc" = 3 ] && printf "%s" "$out" | grep -q "ROLLBACK FAILED"'
check "recorded as rollback-failed" 'grep -q "^0.1.1 .* rollback-failed$" "$failed"'
rm -f "$state/broken"; : >"$state/docker.log"
run
check "next run: not retried, says the rollback failed" '[ "$rc" = 0 ] && printf "%s" "$out" | grep -q "its rollback failed too; not retrying" && [ ! -s "$state/docker.log" ]'

echo "7. extra: wrong key, dry run, lock"
reset; echo pgc_other >"$state/key"
run
check "wrong key: exit 1 with a hint, key not printed" '[ "$rc" = 1 ] && printf "%s" "$out" | grep -q "check the API key" && no_key'
reset; echo "DRY_RUN=1" >>"$tmp/updater.env"
run
check "dry run (file): exit 0, nothing done" '[ "$rc" = 0 ] && printf "%s" "$out" | grep -q "dry run" && [ ! -s "$state/docker.log" ] && [ "$(tag)" = 0.1.0 ]'
reset
rc=0; out=$(DRY_RUN=1 UPDATER_CONFIG="$tmp/updater.env" FAKE_STATE="$state" "$updater" 2>&1) || rc=$?
check "dry run (environment wins over DRY_RUN=0 in the file)" '[ "$rc" = 0 ] && printf "%s" "$out" | grep -q "dry run" && [ ! -s "$state/docker.log" ]'
reset
run --dry-run
check "dry run (--dry-run)" '[ "$rc" = 0 ] && printf "%s" "$out" | grep -q "dry run" && [ ! -s "$state/docker.log" ] && [ "$(tag)" = 0.1.0 ]'
check "dry run: says the failed list is empty" 'printf "%s" "$out" | grep -q "no version on the failed list"'
run --bogus
check "unknown option: exit 1" '[ "$rc" = 1 ]'
reset
if command -v flock >/dev/null 2>&1; then
  flock "$tmp/lock" sleep 3 & holder=$!; sleep 0.3
else
  mkdir "$tmp/lock.d"; holder=
fi
run
[ -n "$holder" ] && wait "$holder" || true
check "lock held: exit 0, skipped" '[ "$rc" = 0 ] && printf "%s" "$out" | grep -q "another run is in progress"'
check "lock held: nothing done" '[ ! -s "$state/docker.log" ]'

echo
echo "$pass passed, $fail failed"
[ "$fail" = 0 ]
