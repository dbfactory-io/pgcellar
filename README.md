# pgCellar

A self-hosted control room for physical PostgreSQL backups with point-in-time recovery, built on Barman.

## Why

Most backup tools with a web UI run dumps. A dump brings you back to last night, not to 14:32 before the bad query. The tools that do physical backups with PITR (Barman, pgBackRest, WAL-G) live in the terminal, with no overview, and nobody tests the restore. So you find out it does not work on the worst day.

I wanted the missing layer: one screen that tells the truth, and a restore that is tested every week.

## What it does

- Servers: add a PostgreSQL server in a few clicks, SSH keys generated, `barman check` explained in plain words.
- Backups: list, run now, schedules and retention.
- Status: one board per server, red when something needs a human, quiet otherwise.
- PITR wizard: pick a date and time, get the recovery done (or the exact command).
- Weekly restore drill: restore the latest backup, start the copy with the PostgreSQL binaries of its version, check what is inside, write a dated PDF report you can give to an auditor.
- Alerts: mail, Telegram, webhook.
- API: JSON, with api keys.
- Metrics: Prometheus format on `/metrics`.

## Files are the truth

Barman's files stay Barman's (`/etc/barman.conf`, `/etc/barman.d/*.conf`). Everything that is mine lives in one `pgcellar.yaml`. Every change goes through a file, is validated, and is committed in a local git repository of the config directory. The UI and the API are only forms over those files. You can do everything with a text editor and `git log`.

SQLite keeps run history, audit log and sessions. It can be thrown away. Secrets are never written in the YAML or in git.

## Install

Pick one. Nothing is published yet, see Status.

- Binary: download `pgcellar` from the releases page, put it next to a working Barman, run `pgcellar serve --config /etc/pgcellar/pgcellar.yaml`.
- Docker image: it contains pgCellar and Barman (PGDG packages, unmodified).
- Compose:

```
cd deploy
docker compose up -d
```

Then open http://localhost:8080.

## Configuration

Start from `deploy/pgcellar.example.yaml`, it is commented line by line. The Barman side is in `deploy/barman.d.example/`.

## API

Base path `/api/v1`, JSON, `Authorization: Bearer <api key>`. Only the SHA-256 of a key is stored. Sketch:

```
GET    /api/v1/servers
GET    /api/v1/servers/{name}
GET    /api/v1/servers/{name}/backups
POST   /api/v1/servers/{name}/backups        run a backup now
GET    /api/v1/servers/{name}/check
POST   /api/v1/servers/{name}/recoveries     PITR
POST   /api/v1/servers/{name}/drills         run a drill now
GET    /api/v1/servers/{name}/drills
GET    /api/v1/drills/{id}/report            the PDF
GET    /api/v1/status
```

This is a sketch, it will move before 0.1.

## Licence

Functional Source License 1.1, Apache-2.0 future licence (FSL-1.1-Apache-2.0, see `LICENSE`). Free for internal use. You cannot sell a competing hosted service with it. Each version becomes Apache-2.0 two years after its release.

Barman is GPL-3.0. It is installed unmodified and I only drive it through its command line. It is never imported, linked or patched.

## Pricing

- Free: 2 servers.
- Pro: 490 EUR per year, up to 10 servers.
- Business: 1 490 EUR per year.

Early access is free during the beta.

## Status

Early development. Nothing to download yet.

## Author

Gilles Fauvie - [dbfactory.io](https://dbfactory.io) - [stranix.net](https://stranix.net)
