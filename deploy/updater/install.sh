#!/usr/bin/env bash
# install.sh: install pgcellar-updater on the Docker host that runs pgCellar.
#
#   sudo ./install.sh            install files, enable nothing
#   sudo ./install.sh --enable   same, then enable and start the timer
#
# Installs the script in /usr/local/bin, the systemd units in
# /etc/systemd/system, and /etc/pgcellar/updater.env from the example when it
# does not exist yet (an existing one is never overwritten). Can be run again
# after a git pull to update the script and the units.
set -euo pipefail
cd "$(dirname "$0")"

enable=0
case "${1:-}" in
  "") ;;
  --enable) enable=1 ;;
  -h | --help) sed -n '2,10p' "$0"; exit 0 ;;
  *) echo "usage: $0 [--enable]" >&2; exit 2 ;;
esac

[ "$(id -u)" = 0 ] || { echo "run as root (sudo $0)" >&2; exit 1; }
for tool in curl jq systemctl; do
  command -v "$tool" >/dev/null 2>&1 || { echo "$tool is missing (Debian/Ubuntu: apt install $tool)" >&2; exit 1; }
done
command -v flock >/dev/null 2>&1 || echo "warning: flock not found (util-linux); a lock directory is used instead"

install -m 0755 pgcellar-updater /usr/local/bin/pgcellar-updater
install -m 0644 pgcellar-updater.service /etc/systemd/system/pgcellar-updater.service
install -m 0644 pgcellar-updater.timer /etc/systemd/system/pgcellar-updater.timer
install -d -m 0755 /etc/pgcellar
if [ -e /etc/pgcellar/updater.env ]; then
  echo "kept existing /etc/pgcellar/updater.env"
else
  install -m 0600 updater.env.example /etc/pgcellar/updater.env
  echo "created /etc/pgcellar/updater.env from the example"
fi
systemctl daemon-reload
echo "installed /usr/local/bin/pgcellar-updater and pgcellar-updater.{service,timer}"

if [ "$enable" = 1 ]; then
  systemctl enable --now pgcellar-updater.timer
  echo "timer enabled: systemctl list-timers pgcellar-updater.timer"
fi

cat <<'NEXT'

Remaining steps:
 1. Edit /etc/pgcellar/updater.env: COMPOSE_DIR (directory of docker-compose.yml),
    SERVICE, API_URL.
 2. Pin the running version in the compose .env file, e.g. PGCELLAR_TAG=0.1.0,
    and make sure the compose file uses
    image: ghcr.io/dbfactory-io/pgcellar:${PGCELLAR_TAG:-latest}
 3. Create an API key with the operator role and store it, without printing it:
      cd COMPOSE_DIR
      (umask 077; docker compose exec -T pgcellar pgcellar apikey create \
         --config /etc/pgcellar/pgcellar.yaml --name updater --role operator \
         > /etc/pgcellar/updater.key)
 4. Optional: set updates.window in pgcellar.yaml (when updates may happen).
 5. Try it without changing anything:
      sudo pgcellar-updater --dry-run
    (sudo pgcellar-updater --now updates at once, outside updates.window)
 6. Enable the timer (if not done with --enable):
      systemctl enable --now pgcellar-updater.timer
    Logs: journalctl -u pgcellar-updater
NEXT
