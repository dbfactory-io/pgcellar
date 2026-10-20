# pgCellar test bed kit

Barman on rasp001 (Raspberry Pi 4, arm64) backing up the six PostgreSQL instances of hawking in streaming mode, through the existing `pg-vpn` relay (`10.8.0.1:5433-5437`, plus a new `5438` for nyx). Background, inventory and risks: [docs/TESTBED.md](../../docs/TESTBED.md).

Legend: **safe** = no effect on production. **touches production** = changes something on hawking, needs an explicit go before each step.

| Instance (container) | PG | Relay port | Barman server | Superuser (check, see A0) |
|---|---|---|---|---|
| `deckhand_postgres` | 16.13 | 5433 | `deckhand` | application role |
| `chutag_postgres` | 16.13 | 5434 | `chutag` | application role |
| `sygnet_postgres` | 16.13 | 5435 | `sygnet` | application role |
| `listmonk_db` | 17.9 | 5436 | `listmonk` | application role |
| `postiz-postgres` | 17.9 | 5437 | `postiz` | application role |
| `nyx-postgres-1` | 18.3 | 5438 (to create) | `nyx` | `postgres` |

Files in this kit: `docker-compose.yml`, `.env.example`, `pgpass.example`, `etc/barman.conf`, `etc/barman.d/*.conf`, `etc/pgcellar/pgcellar.yaml`, `check-from-pi.sh`. Nothing in this directory contains a secret; `.env` and `pgpass` are git-ignored.

## Before anything: two things to know

1. **The image must ship the PostgreSQL 18 client.** `deploy/Dockerfile` installs `postgresql-client-16` today. Barman and `pg_basebackup` must be at least as recent as the server, so listmonk and postiz (17) and nyx (18) cannot be backed up with a 16 client. Before the build (B3) change `postgresql-client-16` to `postgresql-client-18` in `deploy/Dockerfile` (client 18 talks to 16, 17 and 18). The server files in `etc/barman.d/` already set `path_prefix = /usr/lib/postgresql/18/bin`.
2. **Passwords.** This kit uses one `barman` and one `streaming_barman` password shared by the six instances (acceptable for a test bed; separate them in production). On the Mac: `openssl rand -hex 24` twice, store them in the password manager and in `.env` (copied from `.env.example`, mode 0600). Hex avoids any escaping problem in `.pgpass`. Never paste a password in a command line: part A reads them from environment variables typed with `read -rs`.

---

## Part A: hawking (production, needs explicit go)

All commands run on hawking, as a user allowed to use Docker. Do the instances one at a time, starting with `chutag` (least critical), and validate (A5) before moving on. Order inside an instance: A1 roles, A2 find the relay address, A3 `pg_hba`, A4 reload, A5 verify.

### A0. Prepare the shell (safe, read-only)

```bash
read -rsp 'barman password: ' BARMAN_PASSWORD; echo
read -rsp 'streaming_barman password: ' STREAMING_BARMAN_PASSWORD; echo
export BARMAN_PASSWORD STREAMING_BARMAN_PASSWORD

# Superuser of an instance = POSTGRES_USER of the container (app role, "postgres" for nyx).
for c in deckhand_postgres chutag_postgres sygnet_postgres listmonk_db postiz-postgres nyx-postgres-1; do
  printf '%-20s %s\n' "$c" "$(docker exec "$c" printenv POSTGRES_USER)"
done
```

If a value is empty, the container uses the default (`postgres`). Local socket connections are `trust`, so no superuser password is needed inside the container.

### A1. Create the roles (touches production: two CREATE ROLE and a few GRANTs, no restart)

The passwords travel as environment variables of the `docker exec` (only the names are on the command line) and are quoted by `psql` itself (`:'var'`), so they appear neither in `ps` nor in the shell history.

```bash
create_roles () {   # usage: create_roles <container>
  local c="$1" su
  su="$(docker exec "$c" printenv POSTGRES_USER)"; su="${su:-postgres}"
  docker exec -i -e BPW="$BARMAN_PASSWORD" -e SPW="$STREAMING_BARMAN_PASSWORD" -e SU="$su" "$c" \
    sh -c 'psql -U "$SU" -d postgres -v ON_ERROR_STOP=1 -v bpw="$BPW" -v spw="$SPW"' <<'SQL'
-- Create only if absent (re-running is harmless; existing passwords are kept).
SELECT format('CREATE ROLE barman LOGIN PASSWORD %L', :'bpw')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'barman') \gexec
SELECT format('CREATE ROLE streaming_barman LOGIN REPLICATION PASSWORD %L', :'spw')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'streaming_barman') \gexec

-- PostgreSQL 15 and later (all six instances: 16, 17, 18).
GRANT pg_read_all_settings, pg_read_all_stats, pg_checkpoint TO barman;
GRANT EXECUTE ON FUNCTION pg_backup_start(text, boolean) TO barman;
GRANT EXECUTE ON FUNCTION pg_backup_stop(boolean) TO barman;
GRANT EXECUTE ON FUNCTION pg_switch_wal() TO barman;
GRANT EXECUTE ON FUNCTION pg_create_restore_point(text) TO barman;
SQL
}

create_roles chutag_postgres
```

Then, one instance per session and after validation: `create_roles deckhand_postgres`, `create_roles sygnet_postgres`, `create_roles listmonk_db`, `create_roles postiz-postgres`, `create_roles nyx-postgres-1`.

PostgreSQL older than 15 (none in the test bed, useful for customers): `pg_checkpoint` does not exist and the backup functions have other names. Grant `pg_monitor` instead of the three predefined roles, and `GRANT EXECUTE ON FUNCTION pg_start_backup(text, boolean, boolean), pg_stop_backup(), pg_stop_backup(boolean, boolean), pg_switch_wal(), pg_create_restore_point(text) TO barman;`. Do not enable `immediate_checkpoint` there (a plain `CHECKPOINT` needs a superuser before 15).

### A2. Find the address PostgreSQL will see (safe, read-only)

The relay (`pg-vpn`, HAProxy in TCP mode) opens its own connection to PostgreSQL, so the source seen by `pg_hba.conf` is the HAProxy container on the instance's internal network, **not `10.8.0.x`**. Find it before writing any rule:

```bash
# Subnet of the instance's internal network, and pg-vpn's address on it (example: chutag)
docker network inspect chutag_internal \
  -f '{{range .IPAM.Config}}subnet {{.Subnet}}{{end}}  {{range .Containers}}{{.Name}}={{.IPv4Address}} {{end}}'
```

Networks: `deckhand_internal`, `chutag_internal`, `sygnet-api_sygnet_internal`, `listmonk_internal`, `postiz-internal`, `nyx_elastic` (nyx only after A7). The `pg-vpn` address is dynamic: use the **subnet** of the network (the relay is the only thing that reaches the database from outside that network) or pin the address in the `pg-vpn` compose. Set the variable used below:

```bash
HBA_SRC=172.27.0.0/16        # example only: the subnet printed above, never guess
```

Confirm with the real thing: when the Pi attempts a login before A3, PostgreSQL refuses it and logs the exact address it saw.

```bash
# from the Pi:   psql "host=10.8.0.1 port=5434 user=barman dbname=postgres" -c 'select 1'
docker logs --tail 20 chutag_postgres 2>&1 | grep -i 'pg_hba.conf'
#  FATAL:  no pg_hba.conf entry for host "172.27.0.4", user "barman", database "postgres"
```

If the logged address is in `10.8.0.0/24`, use `HBA_SRC=10.8.0.0/24` as in the brief. Behind HAProxy it will not be: `HBA_SRC` is then the relay's subnet, the VPN-only restriction is enforced by the wireguard container (only peers reach `10.8.0.1`), and authentication (SCRAM, 48 hex characters) is the real barrier.

### A3. Add the two `pg_hba.conf` lines (touches production: file edit, no restart)

Never overwrite the file: back it up, append, look. The appended lines come after the existing `host all all all scram-sha-256`; a non-replication login as `barman` is matched by that earlier line (also SCRAM). The `host replication` line is the one that is really needed, because `all` does not match replication connections. For `nyx` see A7 first (its `trust` line comes before anything appended).

```bash
add_hba () {   # usage: add_hba <container>   (uses $HBA_SRC)
  local c="$1" su hba
  su="$(docker exec "$c" printenv POSTGRES_USER)"; su="${su:-postgres}"
  hba="$(docker exec "$c" psql -U "$su" -d postgres -Atc 'show hba_file')"
  echo "hba_file in $c: $hba"
  docker exec "$c" cp -p "$hba" "$hba.bak-$(date +%Y%m%d)"
  docker exec -i "$c" tee -a "$hba" >/dev/null <<EOF
host all barman ${HBA_SRC} scram-sha-256
host replication streaming_barman ${HBA_SRC} scram-sha-256
EOF
  docker exec "$c" tail -n 4 "$hba"
}

add_hba chutag_postgres
```

`hba_file` is in the bind-mounted data directory, so the `.bak-<date>` copy lands next to it on the host: deckhand `/var/lib/deckhand/postgres`, chutag `/var/lib/chutag/postgres`, sygnet `/var/lib/sygnet/postgres/pgdata`, listmonk `/var/lib/listmonk/postgres`, postiz `/var/lib/stranix.net/postiz/postgres`, nyx `/opt/nyx/docker/data/postgres` (PGDATA `/var/lib/postgresql/18/docker` inside). Always use the value printed by `show hba_file`, not this list.

Way back: `docker exec <c> cp -p "$hba.bak-<date>" "$hba"`, then A4.

### A4. Reload (touches production: no restart, no dropped connection)

```bash
docker exec chutag_postgres sh -c 'psql -U "${POSTGRES_USER:-postgres}" -d postgres -Atc "SELECT pg_reload_conf()"'
```

### A5. Verify (safe)

On hawking, check that the rules were parsed without error:

```bash
docker exec chutag_postgres sh -c 'psql -U "${POSTGRES_USER:-postgres}" -d postgres -c "
  SELECT line_number, type, database, user_name, address, netmask, auth_method, error
    FROM pg_hba_file_rules
   WHERE error IS NOT NULL OR user_name && ARRAY[\$\$barman\$\$, \$\$streaming_barman\$\$]"'
```

From rasp001, through the relay (type the password when asked, or use `PGPASSWORD` in your shell only):

```bash
psql "host=10.8.0.1 port=5434 user=barman dbname=postgres" -c 'SELECT 1'
pg_basebackup --dry-run -D /tmp/none -X stream -h 10.8.0.1 -p 5434 -U streaming_barman   # --dry-run needs a client 17 or later
```

The most complete check, once part B runs the container: `sudo docker exec -u barman pgcellar barman check chutag`. Everything must be OK except the replication slot (created at TESTBED.md step 11) and the WAL lines until the first segment arrives.

### A6. Optional safety bound for slots (touches production: reload only)

A slot keeps WAL on hawking while the Pi is down. Cap it (2 GB per instance here):

```bash
docker exec chutag_postgres sh -c 'psql -U "${POSTGRES_USER:-postgres}" -d postgres -c "ALTER SYSTEM SET max_slot_wal_keep_size = \$\$2GB\$\$" -c "SELECT pg_reload_conf()"'
```

`ALTER SYSTEM` writes `postgresql.auto.conf` in the data directory; undo with `ALTER SYSTEM RESET max_slot_wal_keep_size`. Watch the slot with `SELECT slot_name, active, wal_status, pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)) FROM pg_replication_slots;`.

### A7. nyx only (touches production, phase 2: briefly drops the five other tunnels)

Nyx is not behind `pg-vpn`, and its `pg_hba.conf` ends with `host all all all trust` (no password for anyone who can reach it). Work in the `shared/pg-vpn` and wireguard stacks under `/opt/stranix-git/docker-compose/hawking/`. The snippets follow the structure described in TESTBED.md; read the real files and mirror their style before editing.

1. **HAProxy listener** (`shared/pg-vpn/haproxy.cfg`), next to the five existing ones:

   ```
   frontend nyx_in
       bind *:5438
       mode tcp
       default_backend nyx_db

   backend nyx_db
       mode tcp
       server db nyx-postgres-1:5432 check
   ```

   Keep the same `timeout client` / `timeout server` (8 h) as the other listeners.

2. **Network attachment** (`shared/pg-vpn/docker-compose.yml`): add `nyx_elastic` to the `pg-vpn` service networks and declare it external:

   ```yaml
   services:
     pg-vpn:
       networks:
         # ... existing networks ...
         - nyx_elastic
   networks:
     nyx_elastic:
       external: true
   ```

   Recreate `pg-vpn` (`docker compose up -d --force-recreate pg-vpn`): the five existing tunnels drop for a few seconds. Do it off-peak and check each application's database connection afterwards.

3. **DNAT on 10.8.0.1:5438**: add the same rule as the five existing ones in the init script `40-pg-vpn-dnat.sh` of the wireguard stack (target `172.20.0.250:5438`) and apply it once by hand in the wireguard network namespace, so `wireguard` is not recreated. Copy the existing rule for 5437 and change the port.

4. **`pg_hba.conf` of nyx**: first find out how the nyx application connects. With `trust` it may carry no password at all; after the fix it needs one or it will fail. Then back up, rebuild the file with the two barman lines before the (replaced) trust line, and write it back in place:

   ```bash
   c=nyx-postgres-1; hba=/var/lib/postgresql/18/docker/pg_hba.conf
   docker exec "$c" cp -p "$hba" "$hba.bak-$(date +%Y%m%d)"
   docker exec "$c" sh -c "grep -v '^host all all all trust' $hba > /tmp/hba.new"
   docker exec -i "$c" sh -c 'cat >> /tmp/hba.new' <<EOF
host all barman ${HBA_SRC} scram-sha-256
host replication streaming_barman ${HBA_SRC} scram-sha-256
host all all all scram-sha-256
EOF
   docker exec "$c" sh -c "cat /tmp/hba.new > $hba && rm /tmp/hba.new"   # keeps owner and mode
   ```

   `HBA_SRC` for nyx is the subnet of `nyx_elastic`. Reload (A4), verify (A5), and test the nyx application right away. Minimum alternative while the application is not ready: only insert the two barman lines before the trust line and leave `trust` for later.

---

## Part B: rasp001 (safe)

Everything here is local to the Pi: files under `/mnt/usbdata/pgcellar`, one image, one container. Docker needs `sudo` there.

### B1. Layout (safe)

```bash
sudo mkdir -p /mnt/usbdata/pgcellar/barman /mnt/usbdata/pgcellar/data /mnt/usbdata/pgcellar/recoveries
mkdir -p ~/pgcellar-testbed
```

Copy the kit from the Mac, without `.env` or `pgpass` (you create them on the Pi): `rsync -a --exclude .env --exclude pgpass deploy/testbed/ rasp001:pgcellar-testbed/`. The commands below run in `~/pgcellar-testbed`.

### B2. Build the arm64 image (safe)

First apply the Dockerfile change described at the top (client 18). Either build on the Pi (slow, needs about 1 GB of temporary space on the SD card), from a checkout of the repository:

```bash
sudo docker build -f deploy/Dockerfile -t pgcellar:testbed .
```

or, preferred, on the Mac, then ship the image:

```bash
podman build --platform linux/arm64 -f deploy/Dockerfile -t pgcellar:testbed .
podman save pgcellar:testbed | ssh rasp001 'sudo docker load'
```

Print the versions (TESTBED.md step 4):

```bash
sudo docker run --rm --entrypoint barman pgcellar:testbed --version
sudo docker run --rm --entrypoint /usr/lib/postgresql/18/bin/pg_basebackup pgcellar:testbed --version
```

### B3. Ownership of the volumes (safe)

```bash
read BUID BGID < <(sudo docker run --rm --entrypoint sh pgcellar:testbed -c 'echo $(id -u barman) $(id -g barman)')
sudo chown -R "$BUID:$BGID" /mnt/usbdata/pgcellar/barman /mnt/usbdata/pgcellar/data /mnt/usbdata/pgcellar/recoveries etc/barman.d etc/pgcellar
```

`etc/barman.conf` stays root-owned and is mounted read-only. pgCellar writes `etc/barman.d` and `etc/pgcellar` (and commits them in a local git repository).

### B4. Secrets: `.env` and `.pgpass` (safe)

```bash
cp .env.example .env && chmod 600 .env && $EDITOR .env      # same passwords as in part A
set -a; . ./.env; set +a
sed -e "s/:barman:CHANGE_ME/:barman:${BARMAN_PASSWORD}/" \
    -e "s/:streaming_barman:CHANGE_ME/:streaming_barman:${STREAMING_BARMAN_PASSWORD}/" \
    pgpass.example | grep -v '^#' > pgpass
sudo install -m 0600 -o "$BUID" -g "$BGID" pgpass /mnt/usbdata/pgcellar/barman/.pgpass
shred -u pgpass 2>/dev/null || rm -f pgpass
```

`/var/lib/barman` is the home of the `barman` user in the container, so the file is `~barman/.pgpass`. PostgreSQL clients ignore it unless it is mode `0600` and owned by the user that runs them (hence `install -m 0600 -o <barman uid>`). The `*` in the database column also matches the `replication` pseudo-database.

### B5. Admin password hash (safe)

pgCellar expects a **bcrypt** hash in `users[].password_hash`. Either:

```bash
htpasswd -nbBC 10 admin 'the-password' | cut -d: -f2        # apache2-utils; prints $2y$10$...
python3 -c 'import bcrypt,getpass; print(bcrypt.hashpw(getpass.getpass().encode(), bcrypt.gensalt(10)).decode())'
```

Put the result, between double quotes, in `etc/pgcellar/pgcellar.yaml` instead of `CHANGE_ME_BCRYPT_HASH`, and set `alerts.telegram.chat_id`. Prefer the Python form on a shared machine: `htpasswd -b` puts the password on the command line.

### B6. Start and check (safe, until the replication slot is created)

```bash
./check-from-pi.sh                 # TCP + login on 5433-5438; needs psql and the barman password (PGPASSWORD or ~/.pgpass of your user)
sudo docker compose up -d
sudo docker exec pgcellar pgcellar check --config /etc/pgcellar/pgcellar.yaml
sudo docker exec -u barman pgcellar barman check chutag
```

`check-from-pi.sh` fails on 5438 until A7 is done, and on any port whose role does not exist yet; read its output port by port (`HOST` can be overridden). Web UI: **https://pgcellar.stranix.net** from any WireGuard peer (tunnel DNS resolves it to 10.8.0.1, ghost_nginx on hawking terminates TLS and relays to the Pi through the `pgcellar-vpn` socat in the wireguard netns; everything else gets 403). Fallbacks: `http://127.0.0.1:8082` on the Pi (8080 is taken by cadvisor there, 8081 by qbittorrent), or `ssh -L 8082:127.0.0.1:8082 rasp001` then `http://localhost:8082`.

On the first `barman check chutag` expect PostgreSQL, `pg_basebackup` compatibility and connection lines OK; the replication slot missing; and the minimum redundancy failing until the first backup. `create_slot = auto` makes Barman create the slot at the first `barman cron`, which is the first persistent object on production (TESTBED.md step 11, needs its own go): until then keep `streaming_archiver = off` in the server file, or leave the server out of `etc/barman.d` and of `servers:`.

### B7. First backup (needs the go for TESTBED.md steps 11-12)

```bash
sudo docker exec -u barman pgcellar barman backup chutag      # off-peak
sudo docker exec -u barman pgcellar barman list-backup chutag
```

Until milestone 4 (scheduling) ships, pgCellar does not run the schedules itself: install `cron.d/pgcellar-testbed` on the Pi (`sudo install -m 644 -o root -g root cron.d/pgcellar-testbed /etc/cron.d/`), which takes one base backup per server from 03:00, ten minutes apart, and logs to `/var/log/pgcellar-testbed.log`. Remove it when pgCellar takes over.

The cron lines call the API (`POST /api/v1/servers/<name>/backup`) instead of `docker exec ... barman backup`, so every nightly backup appears on the Runs page and in the audit log, and the forced WAL switch is chained by pgCellar itself. The header is `Authorization: Bearer <key>`; the answer is `202` with `{"data":{"run_id":...,"status":"running"}}` and the backup keeps running inside pgCellar. Create the key once (role `operator` is enough) and put it where the cron reads it:

```bash
sudo docker exec -u barman pgcellar pgcellar apikey create --config /etc/pgcellar/pgcellar.yaml --name testbed-cron --role operator \
  | sudo tee /etc/pgcellar-testbed.key >/dev/null
sudo chown root:root /etc/pgcellar-testbed.key && sudo chmod 600 /etc/pgcellar-testbed.key
```

The key is printed once, only its SHA-256 is stored in `pgcellar.yaml` (a running pgCellar picks it up within a second, no restart). `pgcellar apikey list --config ...` shows names and roles. Adjust the container name and config path to your compose file. Check with `sudo bash -c 'curl -fsS -H "Authorization: Bearer $(cat /etc/pgcellar-testbed.key)" http://127.0.0.1:8082/api/v1/runs'`.

The six schedules in `pgcellar.yaml` are `daily 03:00` to `03:50`, ten minutes apart, all enabled. Set `enabled: false` on the servers whose part A is not done yet, otherwise pgCellar alerts on them. Turn on `drills.enabled` after the first manual restore (TESTBED.md step 13).

### B8. Restore to a point in time (safe)

pgCellar can write a restored PostgreSQL data directory, and optionally a `.tar.gz` of it, without touching hawking: Barman runs `barman recover` locally, inside the container, into `/var/lib/pgcellar/recoveries`. The folder is mapped on the Pi as `/mnt/usbdata/pgcellar/recoveries` (compose volume, `recoveries.dir` in `pgcellar.yaml`).

1. **Once**, give the folder to the barman user (B3 does it with the other volumes; to do it alone, `997` is the uid and gid of `barman` in this image, check with B3's first line):

   ```bash
   sudo mkdir -p /mnt/usbdata/pgcellar/recoveries
   sudo chown -R 997:997 /mnt/usbdata/pgcellar/recoveries
   ```

   Without it the restore fails at once with `Permission denied` on the first line of the run output. Check the free space first (`df -h /mnt/usbdata`): a restore is as big as the database plus its WAL, a second time if you tick the archive.
2. In the web interface open a server, **Restore...**, pick the backup, the target (latest possible, end of the backup, or a point in time) and, if you want a file to download, tick the `.tar.gz`. The same thing through the API: `POST /api/v1/servers/<name>/recoveries` with `{"backup_id":"20261020T020001","target":"time","target_time":"2026-10-20 14:30:00+02:00","archive":true}` (operator key), then follow `GET /api/v1/runs/<run_id>`.
3. Where to find the result on the Pi: `/mnt/usbdata/pgcellar/recoveries/<server>/<backup id>-<timestamp>/` (the data directory) and, next to it, `<same name>.tar.gz`. The run page shows the full path (as seen inside the container: replace `/var/lib/pgcellar/recoveries` by `/mnt/usbdata/pgcellar/recoveries`) and a **Download .tar.gz** button when the archive is under `recoveries.max_download_bytes` (2 GiB). Above it, copy the file with `scp` from the Pi.
4. The directory is a ready-to-start data directory: start a PostgreSQL of the **same major version** on it (for example `docker run --rm -v /mnt/usbdata/pgcellar/recoveries/chutag/<name>:/var/lib/postgresql/data postgres:16`, mind the ownership: the postgres user of that image must be able to read it).
5. Restores older than `recoveries.keep_days` (7) are removed every day by pgCellar. Administrators can delete one earlier from the Recoveries section of the server page.

### B9. Notes

- Memory: `mem_limit: 768m` on a Pi with 3.7 GiB shared with Home Assistant, Grafana and Plex. Watch them during the first runs. On this Pi the kernel runs without the memory cgroup (`Your kernel does not support memory limit capabilities` at `compose up`), so the limit is ignored; enabling it means `cgroup_enable=memory cgroup_memory=1` in `/boot/firmware/cmdline.txt` and a reboot.
- The container runs as `barman` (uid 997 in this image). If `docker logs pgcellar` shows `attempt to write a readonly database`, a volume was created by an older root container: `sudo chown -R 997:997 /mnt/usbdata/pgcellar/data` and restart.
- `alerts.max_wal_age` is 25 h here, not 1 h: these databases are quiet, a WAL segment closes only at 16 MB, at the nightly backup (forced switch) or every `archive_timeout` if PostgreSQL has one. A tighter recovery point means setting `archive_timeout` (for example `1h`) on each PostgreSQL instance, which is a hawking change.
- Do not run `barman backup --wait` by hand on a quiet server: it waits for the current WAL segment, which closes only after 16 MB or a switch. Run `barman backup <server>` then `barman switch-wal --force --archive <server>` (pgCellar does exactly that).
- nyx keeps its `host all all all trust` line for now (the application's credentials were not verified): the barman lines sit after it, the replication one is the one that matters. Replacing `trust` by `scram-sha-256` is a separate change, to be tested with the nyx application.
- All the test bed data stays on `/mnt/usbdata/pgcellar`; only the image sits on the SD card.
- Tear-down: `sudo docker compose down`, remove `/mnt/usbdata/pgcellar`. On hawking drop the slots (`SELECT pg_drop_replication_slot('barman_chutag')`) and the two roles; the `pg_hba.conf.bak-<date>` files hold the original state.
