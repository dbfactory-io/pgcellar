# Changelog

All notable changes to pgCellar are written here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Changed

- `deploy/docker-compose.yml` uses the published image `ghcr.io/dbfactory-io/pgcellar:${PGCELLAR_TAG:-latest}` (the `build:` section is kept for a local build); `deploy/.env.example` pins `PGCELLAR_TAG=0.1.0`.
- The repository moved to the dbfactory-io organisation; the Go module path is now `github.com/dbfactory-io/pgcellar` (GitHub redirects the old URL).

### Added

- `pgcellar serve --demo`: a public, read-only demonstration instance with fictional data (Northwind Logistics), meant to be put on the internet; compose file, nginx vhost and README in `deploy/demo/`, `make demo` to try it locally.
- Sign-in attempts are limited to 10 a minute per address (the address comes from `X-Real-IP` when the request reaches pgCellar through a proxy on a private or loopback address).
- Release pipeline: a `v*` tag builds a GitHub Release with the binaries (linux amd64 and arm64, macOS arm64) and `checksums.txt`, the multi-arch image `ghcr.io/dbfactory-io/pgcellar` (linux/amd64, linux/arm64) and a `manifest.json` for update checks; notes come from this file (`make release-notes`). Manual chapter 16, Releases and updates.
- Update check: once a day pgCellar reads the latest GitHub release and its `manifest.json` (nothing sent but its version, in the User-Agent) and says when a newer version exists: an Updates page with "Check now", a line on the Status page, one alert per version (`update_available`) and one when the running version changed (`update_applied`), the `pgcellar_update_available` metric, `GET /api/v1/updates`, `POST /api/v1/updates/check`, and `GET /api/v1/updates/ready` for updaters that run outside pgCellar. New `updates:` block in `pgcellar.yaml` (check, interval, channel, auto, window, source).
- `pgcellar-updater` (`deploy/updater/`): updates a pgCellar run by Docker Compose from the host, through a systemd timer every 15 minutes. It asks `/api/v1/updates` and `/api/v1/updates/ready` (so it waits for `updates.window` and for running jobs), accepts only images of `ghcr.io/dbfactory-io/pgcellar`, checks the pulled digest against `manifest.json`, pins the new tag in the compose `.env`, and puts the previous tag back when the new version does not come up healthy. `--now` and `--dry-run` for a run by hand; `install.sh` installs it without enabling anything; tests with a fake API and a fake docker (`deploy/updater/test/run.sh`). Manual chapter 16, "Automatic updates (Docker)".

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
