# Operations

Day-two work: updating, backing up pgCellar itself, watching it, and keeping an eye on the disk.

## Updating the image

The state is in volumes and bind mounts, not in the image. Updating is a rebuild and a restart:

```
cd pgcellar/deploy
git pull
docker compose build
docker compose up -d
```

Compose recreates both containers (`pgcellar` and `pgcellar-ssh`) with the new image. Backups in progress are interrupted: a backup that was running is recorded as interrupted, and the scheduler catches up a missed due time if it is younger than `scheduler.catch_up` (6 hours by default). Choose a quiet moment.

After the start:

- `pgcellar version` inside the container, the startup log line and the metric `pgcellar_build_info` tell the version.
- The pages are full right away: the status cache is kept in SQLite and shown marked as stale ("as of 08:12, refreshing") until the first refresh completes.
- Alerts are silent for the first two minutes.
- `docker compose logs --tail 50 pgcellar` shows `pgcellar <version> listening on ...`.

If you run the binary without Docker, replace it and restart the service (`deploy/systemd/pgcellar.service` is a systemd unit that runs as the `barman` user).

**Pin what you can.** The image takes Barman from the PGDG repository at build time. Rebuilding on another day may bring a newer Barman. Read its release notes before a rebuild on a production backup host, and keep the previous image (`docker tag pgcellar:dev pgcellar:previous`) so you can go back.

Migrations of the SQLite database are applied automatically at start. Back up before updating anyway (below).

## Backing up pgCellar itself

pgCellar is a control room, and its state is small. Back up three things, plus Barman's own data.

| What | Where (in the container) | Why |
|---|---|---|
| Configuration | `/etc/pgcellar` (host: `etc-pgcellar/`) | `pgcellar.yaml` **and** its `.git` folder, the history of every change |
| Barman files | `/etc/barman.d` (host: `etc-barman.d/`), and `/etc/barman.conf` if you changed it | One file per server, and its own `.git` history |
| Database | `/var/lib/pgcellar/pgcellar.db` | Run history, audit log, alert journal, drill records. Not critical: pgCellar starts without it. |
| SSH keys and `.pgpass` | `/var/lib/barman/.ssh/`, `/var/lib/barman/.pgpass` | The key pair, the authorised WAL keys, the host key of the companion, the passwords. **Secrets.** Store them encrypted. |
| The backups | `/var/lib/barman` (volume `barman-data`) | The reason pgCellar exists |

Notes:

- `pgcellar.yaml` and `etc-barman.d` are small text files with a git history. A copy of the folder is a complete backup of the configuration. Because every change is a commit, you can also push the repositories to a private remote.
- For a clean copy of the SQLite file, stop the container first (`docker compose stop pgcellar`), or copy `pgcellar.db` together with any `-wal` and `-shm` file next to it. If you lose it, you lose history, not backups.
- **Back up the backups too.** If the volume or the machine goes, the backups go with it. Copy `/var/lib/barman` to another machine (rsync, a snapshot of the volume, object storage is outside the scope of this version). A backup system on a single disk is a single disk.
- `~/.pgpass` and the private key are what let Barman talk to your servers. Treat them like any credential.

To restore pgCellar on a new host: install Docker, put back the configuration folders, the volumes (`.ssh`, `.pgpass`, `barman-data`), build the image, `docker compose up -d`. The status, the schedule and the servers are all back. Drill and run history come back only if you restored `pgcellar.db`.

## Metrics

Prometheus text format on `/metrics`. A scrape never runs Barman: it reads the status cache. Example scrape configuration:

```yaml
scrape_configs:
  - job_name: pgcellar
    metrics_path: /metrics
    static_configs:
      - targets: ["backup.example.com:8080"]
```

`/metrics` and `/healthz` need no authentication. Do not publish them to the internet; restrict them in your proxy (chapter 12).

Per server (label `server`):

| Metric | Meaning |
|---|---|
| `pgcellar_last_backup_age_seconds` | Seconds since the last successful backup ended |
| `pgcellar_last_wal_age_seconds` | Age of the last completed WAL file |
| `pgcellar_last_wal_received_age_seconds` | Age of the last data received by the WAL receiver |
| `pgcellar_wal_streaming_live` | 1 if the Barman WAL receiver runs |
| `pgcellar_backups_total` | Number of backups |
| `pgcellar_check_ok` | 1 if the cached `barman check` is fully OK |
| `pgcellar_setup_level` | Server setup: 0 ok, 1 warning, 2 error |
| `pgcellar_backup_bytes`, `pgcellar_wal_bytes`, `pgcellar_recoveries_bytes` | Space taken by backups, WAL and restores |
| `pgcellar_last_drill_ok` | 1 or 0, absent until a drill ran |
| `pgcellar_last_drill_age_seconds` | Age of the last drill, whatever its result |
| `pgcellar_recoveries_total` | Successful restores |

Global:

| Metric | Meaning |
|---|---|
| `pgcellar_fs_free_bytes`, `pgcellar_fs_size_bytes` (label `path`) | File system holding backups and restores |
| `pgcellar_alerts_sent_total` | Messages sent, by channel and outcome |
| `pgcellar_license_servers_max`, `pgcellar_license_servers_used` | Licence quota and use |
| `pgcellar_build_info` | Version |

Useful alerts in Prometheus: `pgcellar_last_backup_age_seconds > 90000`, `pgcellar_check_ok == 0`, `pgcellar_last_drill_ok == 0`, `pgcellar_setup_level == 2`.

`GET /healthz` answers `ok` when the database is reachable, `503` otherwise. Use it as the container health check.

## Logs

pgCellar logs to standard output:

```
docker compose logs -f pgcellar
docker compose logs --since 1h pgcellar-ssh
```

You see the version at start, each failed alert send, config reload problems, and errors from API calls that ended in a 502. Barman's own log file is the one named by `log_file` in `barman.conf`. The SSH companion logs at VERBOSE, so a refused key shows up there.

For the commands pgCellar ran, you do not need the logs: the **Runs** page keeps the output, and the audit log keeps the command line.

## Audit

The **Audit** page (admin) and `GET /api/v1/audit` list every command pgCellar ran on behalf of a person or an API key: backup, check, switch-wal, restore, delete, psql, ssh, configuration writes. Each line has the time, the actor, the exact command, the exit code and the duration. Background commands (`barman cron`, the status refresh, actor `system`) are written only when they fail, and an identical failing command is logged at most once an hour, otherwise there would be a line per minute.

Audit lines older than `audit_retention_days` (90 by default) are purged daily. `0` keeps everything.

Every run goes through one function that writes this line. There is no other path from pgCellar to a command on your servers.

## History

The **History** page shows the git log of the configuration folders, with the patch of each commit and the author. It answers "who changed the retention, and when". `git log -p` in `etc-pgcellar/` shows the same. A hand-written configuration with no commit gets one "Import the existing configuration" commit at the first start.

## Storage and projection

The **Storage** block of a server page and the Status page tell what is on the disk and what is left. Everything is measured at each status refresh, from Barman's own files and the file system, never by running a command.

Per server:

- **Backups**: the sum of the sizes of the complete backups (on disk, with Barman's deduplication where it applies).
- **WAL**: the sum of the sizes in the WAL catalogue.
- **Restores**: what the restores of the server take under `recoveries.dir`.
- **Total**: the three added.

For the file system: free space, total, percent used, the path. The folder of the server is shown when it is a mount of its own, otherwise Barman's home. If `recoveries.dir` is on another device, it is shown too.

**Thresholds.** Under `alerts.min_free_percent` (10 by default) or `alerts.min_free_bytes`, every server on that file system turns orange; under half of the threshold, red. Alerts follow (chapter 8).

**Projection.** pgCellar computes how much is added per day: the WAL archived plus the backups finished in the last 7 days (backups at their size on disk), divided by the days covered. The window is cut to the age of the oldest data, between 1 and 7 days, so a young server is not divided by 7. When the file system would be full in under 90 days, the page says "At this pace the disk is full in N days".

The projection is a ceiling for a server in steady state. It does not predict that retention will delete old backups, nor that compression will change. Read it as "if nothing changes, and nothing is deleted". Not counted: Barman's `streaming` and `incoming` directories and files kept outside `base/` and `wals/`.

## A short weekly routine

- Open Status. Everything green? Done.
- Look at the last drill of each server. Did it pass this week?
- Look at the Storage line: how many days left?
- Once a quarter, restore something by hand and read the result. A drill is a good guard; it is not you.
