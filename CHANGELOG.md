# Changelog

All notable changes to pgCellar are written here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.2.1] - 2026-10-23

### Fixed

- The Docker image runs pgCellar under `tini`. Processes started by Barman and left behind by their parent were re-parented to pgCellar, which did not reap them: about 1 300 zombies a day, until the container reached its pids limit after a few days and no backup could start. Upgrade, or add `init: true` to the `pgcellar` service of an older compose file.

## [0.2.0] - 2026-10-23

### Added

- Releases are built by GitHub Actions from a `v*` tag: binaries for linux amd64, linux arm64 and macOS arm64 with `checksums.txt`, the multi-arch image `ghcr.io/dbfactory-io/pgcellar:<version>` (linux/amd64, linux/arm64), and a `manifest.json` that update checks read. The release notes come from this file (`make release-notes`). Manual, chapter 16.
- Update check: once a day pgCellar reads the latest release and its `manifest.json` (it sends nothing but its version, in the User-Agent) and tells you when a newer version exists: an Updates page with **Check now**, a line on the Status page, one alert per new version (`update_available`), one alert when the running version changed (`update_applied`), the `pgcellar_update_available` metric, and `GET /api/v1/updates`, `POST /api/v1/updates/check`, `GET /api/v1/updates/ready`. Configured by the new `updates:` block of `pgcellar.yaml` (`check`, `interval`, `channel`, `auto`, `window`, `source`).
- Automatic updates of a binary install (`updates.auto: true`): inside the update window and when no backup, restore or drill runs, pgCellar downloads the new release, checks its SHA-256 against `manifest.json` and that the new binary runs, puts it in place (the old one stays as `pgcellar.prev`) and restarts into it. A version that does not stay up is rolled back at its third start and not installed automatically again. Never in a container.
- Automatic updates of a Docker install: `pgcellar-updater` (`deploy/updater/`), a script and a systemd timer on the Docker host. It waits for the update window and for running jobs, accepts only `ghcr.io/dbfactory-io/pgcellar` images whose digest matches `manifest.json`, pins the new tag in the compose `.env`, and puts the previous tag back when the new version does not come up healthy. A version rolled back this way is not tried again (`$COMPOSE_DIR/.pgcellar-updater.failed`) unless you run it with `--retry-failed`. `--dry-run` and `--now` for a run by hand; `install.sh` installs it without enabling anything.
- `pgcellar serve --demo`: a public, read-only demonstration with fictional data (Northwind Logistics); compose file, nginx vhost and README in `deploy/demo/`, `make demo` to try it locally.
- Sign-in attempts are limited to 10 a minute per address (the address comes from `X-Real-IP` when the request reaches pgCellar through a proxy on a private or loopback address).

### Changed

- `deploy/docker-compose.yml` runs the published image `ghcr.io/dbfactory-io/pgcellar:${PGCELLAR_TAG:-latest}`, and `deploy/.env.example` pins `PGCELLAR_TAG`. Building the image from the checkout (`docker compose build`, `make docker`) still works.
- The systemd unit moved to `deploy/systemd/pgcellar.service`. It runs `/opt/pgcellar/bin/pgcellar` as `barman`, from a directory owned by that user so that automatic updates can replace the binary, with `Restart=on-failure`.
- The repository moved to the dbfactory-io organisation; the Go module path is now `github.com/dbfactory-io/pgcellar` (GitHub redirects the old URL).
- The manual covers the published image, the binary install under systemd, and the `updates:` block of the configuration.

### Fixed

- Dates read from Barman follow one rule everywhere: a date that carries a time zone (`backup.info`, `show-backup`) is taken as written, a date without one (`list-backups`, backup ids) is the local time of the Barman server, which is the machine pgCellar runs on. A zone abbreviation Go does not know (`CEST` in a process set to UTC) no longer moves a backup by hours.
- Hour marks of the backup timeline fall on whole hours in time zones with a half-hour offset (India, +05:30), instead of every mark showing `:30`.
- `pgcellar serve --dev` freezes its clock on the instant of the fixtures (20 October 2026, 02:30 in Brussels) instead of 02:30 on the wall clock of the machine, so the ages it shows are right in every time zone.

## [0.1.0] - 2026-10-21

First version. A control room for physical PostgreSQL backups with point-in-time recovery, built on top of Barman.

### Added

- One Go binary (no CGO, pure-Go SQLite): web interface, JSON API v1, Prometheus metrics on `/metrics`, health on `/healthz`. Linux amd64 and arm64, macOS arm64.
- Docker image with Barman from the PGDG packages (unmodified) and the PostgreSQL server binaries of the chosen major versions, a Compose file, and an SSH companion container that receives WAL files (`barman put-wal`, one restricted key per PostgreSQL server).
- Files are the truth: Barman's own files stay Barman's, everything pgCellar owns is one `pgcellar.yaml`, every change goes through a file, is validated and is committed in a local git repository of the configuration directory.
- Servers: add and edit a server from a form that writes `barman.d`, WAL method (streaming, `archive_command`, both), `barman check` explained in plain words, a Server setup section read from `pg_settings` (read-only `psql`), SSH key pair for the barman user with a connection test.
- Backups: list, run now, delete a complete backup, schedules run by pgCellar itself (daily, weekly, every N hours, cron), catch-up of a missed run after a restart, retention written to `barman.d`.
- Status: one card per server, green, orange or red with the reason in a sentence; the status cache is kept in SQLite so pages are full right after a restart; no page runs Barman.
- Timeline: an SVG picture of the backups and the WAL available, the earliest and latest point in time you can restore to, WAL gaps.
- Point-in-time recovery: a Restore page (latest, end of backup, or a date and time) that runs `barman recover` into a mapped directory, with an optional downloadable `tar.gz`.
- Restore drills: a real restore of the latest backup, started with the PostgreSQL binaries of its version, checked from the inside (databases, tables, LSN, checksums), then stopped and removed; scheduled per server; a dated PDF report written without any dependency.
- Storage: space taken by backups and WAL per server, free space of the file system, thresholds that turn a server orange or red.
- Alerts to Telegram, mail (SMTP with STARTTLS) and a signed webhook (`X-PgCellar-Signature: sha256=...`): a server changes level (and a "resolved" message when it is healthy again), a backup fails, a restore drill fails, a scheduled run is skipped, and an optional daily summary. A condition that lasts is repeated only after `alerts.repeat_every`; nothing is sent during the first two minutes after a start. Page `/alerts` with a test button per channel and the journal of the latest messages, `GET /api/v1/alerts`, `POST /api/v1/alerts/test`, metric `pgcellar_alerts_sent_total`. Secrets are read from environment variables named in the YAML, never written in it.
- Licence keys: `PGC1.<payload>.<signature>`, Ed25519, verified offline against a public key built into the binary. Free (2 servers), Pro and Business editions. `pgcellar license keygen|issue|verify|show`, `GET /api/v1/license`, metric `pgcellar_license_servers_max`. A licence never blocks a backup, a restore or a drill: it only limits adding servers beyond the quota, with 30 days of grace after expiry.
- Roles (viewer, operator, admin), sessions with CSRF protection, API keys stored as SHA-256 only (`pgcellar apikey create|list`), `pgcellar hash-password`, an audit log of every command run for a user, an original mascot, warm light and dark themes.
- `pgcellar check` validates the configuration and the Barman server files; `pgcellar serve --dev` runs a demo on a fake Barman with temporary data.

### Known limits

- One pgCellar instance per Barman host. The Barman engine is behind an interface, a second engine (pgBackRest) is designed for but not built.
- The repository is private, under the dbfactory-io organisation; the module path is `github.com/dbfactory-io/pgcellar`.
- The interface is in English only; every string goes through one catalogue so other languages can follow.
