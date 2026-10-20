# pgCellar test bed: Barman on rasp001 backing up the PostgreSQL instances of hawking

Status: proposal. Everything below comes from read-only inspection made on 2026-10-20. Nothing was changed on hawking or on rasp001 (see "Not verified" at the end for the limits of the inspection).

## 1. PostgreSQL inventory on hawking

hawking: Debian, 8 cores, 22 GiB RAM, load average about 2.4, `/` is a single 197 GB disk (58 GB free). Docker Compose stacks live in `/opt/stranix-git/docker-compose/hawking/<entity>/<stack>/`.

Six live PostgreSQL instances, five of them already reachable from the VPN through an existing TCP relay (section 1.2).

### 1.1 Live instances

| Container | Stack (compose dir) | Image / version | Databases (size) | Host data directory (du) | Docker network(s) | Published ports |
|---|---|---|---|---|---|---|
| `deckhand_postgres` | `stranix/deckhand` | postgres:16-alpine / 16.13 | `deckhand` (8 MB) | `/var/lib/deckhand/postgres` (47 MB) | `deckhand_internal` | none (5432/tcp internal) |
| `chutag_postgres` | `stranix/chutag` | postgres:16-alpine / 16.13 | `chutag` (8 MB) | `/var/lib/chutag/postgres` (47 MB) | `chutag_internal` | none |
| `sygnet_postgres` | `code79/sygnet` | postgres:16-alpine / 16.13 | `sygnet` (10 MB) | `/var/lib/sygnet/postgres` (66 MB), PGDATA is the `pgdata` subdirectory | `sygnet-api_sygnet_internal` | none |
| `listmonk_db` | `stranix/listmonk` | postgres:17-alpine / 17.9 | `listmonk` (10 MB) | `/var/lib/listmonk/postgres` (65 MB) | `listmonk_internal` | none |
| `postiz-postgres` | `stranix/postiz` | postgres:17-alpine / 17.9 | `postiz` (13 MB), `temporal` (10 MB), `temporal_visibility` (9 MB) | `/var/lib/stranix.net/postiz/postgres` (99 MB) | `postiz-internal` | none |
| `nyx-postgres-1` | `/opt/nyx/docker` (outside the stranix-git tree) | postgres:18.3 (Debian 18.3-1.pgdg13+1) | `nyx` (8 MB) | `/opt/nyx/docker/data/postgres` (47 MB), PGDATA `/var/lib/postgresql/18/docker` | `nyx_elastic` | none |

Total: 6 instances, 8 user databases, well under 500 MB of data directories. Each database also has the empty `postgres` maintenance database (about 7.5 MB).

Settings read with `show` (identical on all six): `wal_level = replica`, `max_wal_senders = 10`, `max_replication_slots = 10`, `archive_mode = off`, `max_connections = 100`. No replication slot exists on any instance. `wal_level` and the sender/slot limits are already sufficient for streaming backup; `archive_mode` can stay off if Barman uses `streaming_archiver` with a slot.

Roles: each instance has a single role (named after the application, `postgres` for nyx) which is superuser with the replication attribute. There is no dedicated backup role yet.

`pg_hba.conf` (all instances, same shape): `trust` for local sockets and 127.0.0.1/::1 (including `replication` on loopback), then `host all all all scram-sha-256`. Important detail: the `all` keyword in the database column does not match replication connections, so a streaming connection from the network is currently refused. A `host replication ...` line must be added. `nyx-postgres-1` is the exception: its last line is `host all all all trust` (no password at all for any client that can reach it; it is only protected by being on an isolated Docker network). To be fixed before it is exposed.

Compose files were not opened, only container labels and `docker inspect` were used, so no secret was read.

### 1.2 Existing VPN relay (important finding)

A stack `shared/pg-vpn` already exists and is running (container `pg-vpn`, `haproxy:3.0-alpine`, up 13 days):

- HAProxy in TCP mode, joined to `wireguard_default` (fixed IP 172.20.0.250) and to the internal network of five of the databases. Backends are resolved by container name through Docker DNS.
- The `wireguard` container DNATs `10.8.0.1:5433-5437` to `172.20.0.250` (init script `40-pg-vpn-dnat.sh` in the wireguard stack, replayed at boot).
- Port map: 5433 deckhand, 5434 chutag, 5435 sygnet, 5436 listmonk, 5437 postiz. Timeouts are 8 h, which suits long backups.
- `nyx-postgres-1` is not behind it (network `nyx_elastic` is not joined).

Reachability from the Pi was tested with a plain TCP connect (no authentication): `10.8.0.1:5433` and `:5436` answered from rasp001. rasp001 routes to 10.8.0.1 through the LAN gateway `10.79.0.254` (the OpenWrt router holds the WireGuard peer 10.8.0.2 whose allowed IPs are `10.8.0.2/32, 10.79.0.0/24`). So rasp001 itself runs no WireGuard client; the tunnel is terminated at the router. Latency measured by ping: about 24 ms.

### 1.3 Old restorations to ignore

Seven containers named `native-restore-20260918161010-d81696-*` (created 2026-09-18, six running and one exited) are leftovers of a native restore test. They have no compose labels, no published port, anonymous images referenced by ID, and anonymous Docker volumes:

| Container | State | Version |
|---|---|---|
| `...-nyx-postgres-1` | running | 18.3 |
| `...-listmonk-db` | running | 17.9 |
| `...-postiz-postgres` | running | 17.x (image ID shared with listmonk) |
| `...-deckhand-postgres` | running | 16.13 |
| `...-chutag-postgres` | running | 16.x (image ID shared with deckhand) |
| `...-sygnet-postgres` | running | 16.x (image ID shared with deckhand) |
| `...-ghost-mariadb` | exited (1) | MariaDB |

They are stale copies, not production, and must not be targeted by Barman. Cleaning them up is a separate decision for Gilles (not part of this test bed).

### 1.4 Out of scope, for the record

- `ghost_mariadb` (`mariadb:latest`, Ghost blog, port 3306 internal): MariaDB, not covered by Barman.
- `sygnet_pgadmin` and `nyx-pgadmin-1` (pgAdmin) are clients, not databases.
- hawking is already saved at file level by restic (`/mnt/usbdata/backups/restic-repos/hawking` exists on rasp001). Barman would add physical backups with point-in-time recovery, it does not replace that.

## 2. rasp001

| Item | Value |
|---|---|
| Model | Raspberry Pi 4 Model B Rev 1.4, 4 cores, aarch64 |
| OS / kernel | Debian 12 bookworm, kernel 6.12.93+rpt-rpi-v8 |
| Root filesystem | `/dev/mmcblk0p2`, 114 GB, 30 GB used, 78 GB free (this is an SD card) |
| Data disk | `/dev/sda1` mounted on `/mnt/usbdata`, ext4 `rw,relatime`, 4.6 TB, 2.2 TB used, 2.2 TB free (51 %) |
| Content of `/mnt/usbdata` | `backups` (8.2 GB: `openwrt-router`, `restic-repos/{hawking,rasp001,rasp002}`), `brols` (455 GB), `videos` (1.8 TB) |
| RAM | 3.7 GiB total, 2.1 GiB used, 1.6 GiB available (page cache included), swap 3.7 GiB with 1.3 GiB used |
| Load | load average 2.4 / 0.7 / 0.6 at inspection time, 4 cores |
| Docker | 29.7.2; `stranix` is not in the `docker` group, commands need `sudo` |
| Barman packages | `apt-cache policy barman`: candidate 3.4.0-1 from `deb.debian.org bookworm/main` (arm64). Only Debian repositories are configured (`docker.list`, `raspi.list`), PGDG is not. `postgresql-client` candidate is 15 |
| Power | `vcgencmd get_throttled` returned `0xe0000`: past under-voltage / throttling / frequency-cap events occurred since boot (current state is clear). Worth checking the power supply since the disk is USB-powered through the same Pi |

Already running (12 containers, do not disturb): Home Assistant, Grafana, Prometheus, Alertmanager, blackbox_exporter, node_exporter, cAdvisor, snmp_exporter, Plex, qBittorrent, certbot-cloudflare, dyndns-cloudflare. Also a `zabbix` agent and a desktop session (`wf-panel`) are present.

Implications: Debian's Barman 3.4.0 is too old for PostgreSQL 17 and 18 (support for new majors came in later 3.x releases), and `pg_basebackup` / `pg_receivewal` must be at least as recent as the server (18 for nyx). The packages must therefore come from PGDG (apt.postgresql.org has bookworm arm64 builds of `barman` and `postgresql-client-18`) or from the pgCellar Docker image, which per CLAUDE.md installs Barman from PGDG. The version actually available from PGDG for arm64 has not been checked from here (no repository was added).

## 3. Architecture proposal

### 3.1 Option A (recommended): Barman on rasp001, streaming, through the existing pg-vpn relay

Flow: rasp001 -> LAN -> OpenWrt WireGuard peer -> hawking `wireguard` netns (10.8.0.1:543x) -> DNAT -> `pg-vpn` HAProxy -> PostgreSQL container on its internal network. Barman uses `backup_method = postgres` (`pg_basebackup` over the replication protocol) and `streaming_archiver = on` with `slot_name` and `create_slot = auto` (WAL through `pg_receivewal`). No access to the host data directories and no SSH to hawking is needed.

What would have to change, per stack (nothing is done yet):

Common to every instance:
1. Create a role `barman` (login, password, `pg_monitor`, plus the grants Barman documents for non-superuser backups: `pg_checkpoint`, `EXECUTE` on `pg_backup_start`, `pg_backup_stop`, `pg_switch_wal`, `pg_create_restore_point`) and a role `streaming_barman` with `REPLICATION` (and no other privilege). Strong generated passwords, kept out of git; Barman reads them from `~barman/.pgpass` (0600).
2. Add to `pg_hba.conf` (file is in the bind-mounted data directory), restricted to the relay's address on that instance's internal network (HAProxy connects from there, so the source is the `pg-vpn` IP on the internal network, not 10.8.0.x):
   - `host postgres barman <pg-vpn internal IP>/32 scram-sha-256`
   - `host replication streaming_barman <pg-vpn internal IP>/32 scram-sha-256`
   Then `SELECT pg_reload_conf();`. No restart. Because of the source rewrite by HAProxy, the check "only 10.8.0.0/24" must be enforced in the DNAT/firewall layer on the wireguard container (it already is: only VPN peers can reach 10.8.0.1), not in `pg_hba`.
3. `wal_level = replica`, `max_wal_senders`, `max_replication_slots`: already satisfied (replica / 10 / 10). Barman needs 2 senders per server (basebackup + WAL streaming) and 1 slot. No restart needed.
4. `archive_mode` stays off for streaming-only operation. (Optional hardening: `archive_mode = on` with a `barman-wal-archive` command would need a restart and a Barman client in the container; not needed here.)

Per stack:
- `deckhand`, `chutag`, `sygnet`, `listmonk`, `postiz`: nothing to publish and no compose change, the ports 5433-5437 already exist. Only SQL (roles) and a `pg_hba.conf` edit in the data directory for each. One consideration: pg-vpn has a single static `haproxy.cfg`; reloading it is not required for these five.
- `postiz`: three databases; `pg_basebackup` copies the whole cluster, so all three are covered by the one backup.
- `nyx` (`/opt/nyx/docker`): add a `pg-vpn` listener on 5438 (`server db nyx-postgres-1:5432`), attach `pg-vpn` to the `nyx_elastic` network (edit `shared/pg-vpn/docker-compose.yml`, recreate `pg-vpn`, which briefly interrupts the five existing tunnels), extend the DNAT script of the wireguard stack with port 5438 (a rule in the wireguard netns, no recreation needed if applied by hand and in the init script). Also replace `host all all all trust` in nyx's `pg_hba.conf` by `scram-sha-256` and give the superuser a password, or at minimum add the `barman`/`streaming_barman` lines before the trust line. Nyx can be left out of the first round (it is the only instance needing more than SQL).

Other Barman-side facts:
- One Barman server definition per instance in `/etc/barman.d/<name>.conf`, generated by pgCellar.
- Versions: PostgreSQL 16 (x3), 17 (x2), 18 (x1). The client tools used by Barman must be 18 to back up the 18 cluster. With the pgCellar image (PGDG `barman` plus `postgresql-client-18`) a single image handles all three majors, since pg_basebackup 18 can talk to older servers.
- Restore drills: restoring a base backup into a throwaway container is trivial at these sizes and is exactly what the product demonstrates.

### 3.2 Option B: SSH tunnel or rsync/SSH

- B1, `backup_method = postgres` through an SSH tunnel (`ssh -L`) from the Pi to hawking: it carries the same protocol as A, so it needs the same roles and the same `pg_hba` lines, plus a permanent tunnel service per instance to supervise, SSH keys on both sides and a way to reach container IPs (a tunnel to `172.2x.0.y` which changes on recreation, or a published port on 127.0.0.1 which means editing every compose). It adds failure modes (tunnel down = backup down) and gives nothing over the relay that already exists.
- B2, `backup_method = rsync` with `ssh_command`: Barman then needs to read the PGDATA directories of the host as the postgres user of each container (UIDs 70 for alpine images and 999 for Debian ones, directories under `/var/lib/...` owned by those UIDs), call `pg_backup_start`/`stop` over a SQL connection anyway, and WAL still needs either `archive_command` (restart, a Barman client inside each container, alpine has no `barman-cli`) or streaming. It requires root-equivalent SSH access to hawking from the Pi, a different access path per stack, and tight coupling with the host layout. Compared with A it is more intrusive on production, harder to secure and harder to reproduce for customers, who will mostly have a plain TCP route to PostgreSQL, which is what we want to test.

Recommendation: A. It touches the least (SQL roles, one `pg_hba` line per instance, reload), reuses a relay that is already running and VPN-only, matches the streaming method scoped for 0.1, and needs no filesystem access to hawking.

### 3.3 Common sizing

- Repository: `/mnt/usbdata/barman` on ext4 with 2.2 TB free. Barman home and `barman_home` metadata can live there too, so the SD card only holds the OS, the Docker image and logs.
- Space: today the six data directories total about 370 MB (cluster files, uncompressed). With gzip compression (`compression = gzip`) and `reuse_backup = link`, a daily base backup plus 7 days of retention is about 8 x 0.4 GB, so roughly 3 GB, plus WAL. These applications are small and mostly idle, WAL volume is expected to be a few hundred MB per day at most, so roughly 5 to 10 GB for the whole bed. Order of magnitude only, to be replaced by measured numbers after a week.
- Time of the first backup: at VPN speed the limiting factor is the Pi's uplink/CPU rather than the volume. 50 MB to 100 MB per instance over a link with 24 ms latency takes seconds to a couple of minutes per instance (`pg_basebackup` also waits for a checkpoint; `checkpoint_timeout` default leads to up to a few minutes unless `immediate_checkpoint` is set). All six first backups should finish in well under 20 minutes. Not measured.
- pgCellar runs on the Pi as the Docker image arm64 (Go binary built for linux/arm64 plus PGDG Barman and `postgresql-client-18`), with volumes `/mnt/usbdata/barman` (data), a config directory (git-tracked Barman files plus `pgcellar.yaml`) and the Barman user's `~/.ssh` (not used by option A) and `.pgpass`. Its web UI listens on the LAN only. Memory budget: pgCellar (Go) plus Barman processes are in the order of 100 to 200 MB, to be measured.

## 4. Risks

- Load on hawking: base backups of 50 to 100 MB are negligible, but `pg_basebackup` forces a checkpoint and each instance holds 2 walsender connections. Schedule them off-peak (for example 03:30, after the weekly audit collection at 03:00 on Sundays) and stagger the instances.
- WAL retention by slots: a replication slot retains WAL on hawking while the Pi is down. At this write volume it is small, but a Pi offline for weeks could fill the 58 GB root disk. Mitigation: `max_slot_wal_keep_size` on each instance (reload, no restart), alert on the slot lag in pgCellar.
- 3.7 GiB of RAM on the Pi with 1.6 GiB available and 1.3 GiB of swap in use: Barman and pgCellar fit, but Plex, qBittorrent, Home Assistant and Grafana compete. Run backups with `nice`/`ionice`, one server at a time, set a Docker memory limit on the pgCellar container, and watch Grafana and Home Assistant during the first runs. A restore drill that starts a PostgreSQL on the Pi adds some tens of MB per instance; run drills sequentially.
- SD card vs USB: the root filesystem is a SD card (`mmcblk0p2`). Keep the repository, the SQLite run history, logs and the Docker volumes of the test bed on `/mnt/usbdata` to avoid wearing the card; Docker's own data root is on the SD card, so the image itself will sit there (a few hundred MB). The USB disk is a single device with no redundancy and is also used by Plex and qBittorrent (I/O contention during backups). The throttling flags (`0xe0000`) suggest checking the power supply, as an under-voltage event during a write can corrupt the repository.
- Security: a replication role can read all data of the cluster. Mitigations: tunnel is WireGuard (encrypted), the relay is reachable only from VPN peers, SCRAM passwords of 32+ random characters, `pg_hba` lines restricted to the relay's internal address, role with no superuser. Note that `10.8.0.1:543x` is reachable by every VPN peer (10.8.0.0/24) and by the whole LAN behind the OpenWrt peer (`10.79.0.0/24`), so authentication, not the network, is the real barrier; keep the roles' passwords out of git and out of `pgcellar.yaml`. Also consider PostgreSQL TLS (`sslmode=require`) as a later step.
- Fragile existing pieces: the DNAT rule in the wireguard container and the pg-vpn container are used by Gilles' other clients (DBeaver-style access to application databases). Recreating `pg-vpn` for the nyx listener drops those sessions briefly. A mistake in the wireguard stack must not be allowed to cut the VPN (see the recreate/dependents notes in memory).
- Do not do: no `pg_basebackup` or any Barman access to the `native-restore-*` containers (they are stale copies and would give a false sense of coverage); no manual `docker compose up` that recreates a database container (the nginx 502 issue and the `pg-vpn` name-based resolution make this visible); no use of the application's own superuser credentials for Barman; no restart of production PostgreSQL for this test bed.

## 5. Steps

Legend: "safe" = no effect on production; "PROD" = touches hawking (explicit go required before each).

1. Safe. Decide the scope of the first round: the five instances already behind pg-vpn (deckhand, chutag, sygnet, listmonk, postiz); nyx later.
2. Safe. On rasp001, prepare the layout: create `/mnt/usbdata/barman` (owner of the future container user), a config directory, and a dedicated Linux user or UID. Create no Docker network or container yet.
3. Safe. Build the pgCellar arm64 image on the Mac (PGDG Barman, `postgresql-client-18`, the Go binary linux/arm64) and transfer it to the Pi (`docker save | ssh ... docker load`); do not start it.
4. Safe. On the Pi, check that PGDG provides `barman` and `postgresql-client-18` for arm64 bookworm (inside the image build log) and print the resulting versions with `barman --version` and `pg_basebackup --version`.
5. Safe. Generate the passwords (two per instance) locally, store them in the password manager and in a 0600 `.pgpass` that will be mounted on the Pi. No secret in git.
6. PROD (go explicit). For each of the five instances: create the `barman` and `streaming_barman` roles with the minimal grants (SQL through `docker exec ... psql`). Start with `chutag`, the least critical, and validate before continuing.
7. PROD (go explicit). For each instance: add the two `pg_hba.conf` lines (back up the file first, restricted to the pg-vpn internal address), then `pg_reload_conf()`. No restart. Verify the new lines with `pg_hba_file_rules`.
8. PROD (go explicit). Optionally set `max_slot_wal_keep_size` (reload only) on each instance as a safety bound.
9. Safe. From the Pi, test connectivity and authentication without writing anything: `psql "host=10.8.0.1 port=5434 dbname=postgres user=barman"` with a `SELECT 1`, then `pg_receivewal --dry-run`-style check through `barman check` once configured (step 10).
10. Safe. Start pgCellar on the Pi (Docker, `/mnt/usbdata/barman` mounted), add the first server `chutag` (port 5434) with `backup_method = postgres`, `streaming_archiver = on`, `slot_name`, `create_slot = auto` left off until step 11, and run `barman check`. Every item must be green except the slot.
11. PROD (go explicit). Create the replication slot (this is the first object created on a production instance that persists WAL) and start WAL streaming for `chutag`. Check the slot lag on hawking.
12. PROD (go explicit). Run the first manual `barman backup chutag` off-peak and measure duration, size, hawking load (`uptime`, `docker stats`) and Pi load.
13. Safe. Run a restore into a throwaway container on the Pi and compare row counts: this is the first restore drill.
14. PROD (go explicit). Repeat steps 6, 7, 11 and 12 for `deckhand`, `sygnet`, `listmonk` and `postiz`, one instance per evening, never two at the same time.
15. Safe. Enable the schedule, retention (7 days) and the weekly restore drill in pgCellar; check Grafana and Home Assistant on the Pi for a week.
16. PROD (go explicit), optional phase 2 for nyx: add the 5438 listener to `pg-vpn` and recreate it, extend the DNAT script, replace the `trust` line in nyx's `pg_hba.conf`, then repeat steps 6 to 13.
17. Safe. After a week, replace the sizing estimates of section 3.3 with measured values in this document.

## Not verified

- Compose files of the stacks were not read (only container labels, `docker inspect`, and the pg-vpn compose/haproxy files); the exact environment variable names and any data directory permissions beyond ownership by the image user are therefore not recorded.
- The PGDG arm64 availability and version of `barman` and `postgresql-client-18` for bookworm was not checked (no repository added on the Pi). The claim that Debian's Barman 3.4.0 does not support PostgreSQL 17 and 18 is from memory of Barman's release history.
- The first-backup time and the WAL volume are estimates, nothing was measured. Network throughput through the router's WireGuard peer was not measured, only latency by ping and a TCP connect on ports 5433 and 5436 (ports 5434, 5435 and 5437 not individually tested from the Pi).
- The exact grants Barman needs per PostgreSQL major (16, 17, 18) must be checked against the Barman documentation of the version installed.
- The `pg-vpn` internal IP on each instance's network is dynamic (172.27.0.4 on `chutag_internal`, as seen); it is not pinned, so the `pg_hba` lines should use the Docker subnet of each internal network, or the IP must be pinned in the pg-vpn compose.
- Two read-only `psql` queries failed on the first attempt (shell quoting) and were rerun; no write was made. One temporary file `/tmp/q_ro.sh` was copied to hawking by mistake and removed immediately.
