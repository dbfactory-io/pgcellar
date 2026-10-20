# pgCellar

A self-hosted control room for physical PostgreSQL backups with point-in-time recovery, built on Barman.

Try the demo: https://demo.pgcellar.dbfactory.io

## Why

Most backup tools with a web UI run dumps. A dump brings you back to last night, not to 14:32 before the bad query. The tools that do physical backups with PITR (Barman, pgBackRest, WAL-G) live in the terminal, with no overview, and nobody tests the restore. So you find out it does not work on the worst day.

I wanted the missing layer: one screen that tells the truth, and a restore that is tested every week.

## What it does

- **Status**: one card per server, green when all is well, orange or red with the reason in a sentence. It tells the truth, and stays quiet otherwise.
- **Servers**: add a PostgreSQL server from a form, SSH keys generated, `barman check` explained in plain words, the PostgreSQL settings Barman depends on read and judged.
- **Backups**: list, run now, schedules run by pgCellar itself (no host cron), retention, delete a backup.
- **Point-in-time recovery**: pick a date and a time on a timeline, get the restore done into a directory (and a `tar.gz` to download).
- **Restore drills**: every week, restore the latest backup, start the copy with the PostgreSQL binaries of its version, look inside, stop it, and write a dated PDF report you can hand to an auditor.
- **Storage**: what backups and WAL take, what is left on the disk, and a warning before it is too late.
- **Alerts**: Telegram, mail and a signed webhook. A message when a server changes level, when a backup or a drill fails, and a daily summary if you want one.
- **API**: JSON under `/api/v1`, with API keys. Whatever the interface does, the API does.
- **Metrics**: Prometheus format on `/metrics`.

Screenshots will be added here before the first release.

## Quick start (5 minutes, Docker Compose)

You need Docker (or Podman) with Compose, and a PostgreSQL server that allows a streaming connection from this host. The image contains pgCellar and Barman.

```
git clone https://github.com/stranix79/pgcellar.git
cd pgcellar/deploy
mkdir etc-pgcellar etc-barman.d
cp pgcellar.example.yaml etc-pgcellar/pgcellar.yaml
docker compose build
```

Create the password of the admin user, paste the hash printed by the last command into `users:` in `etc-pgcellar/pgcellar.yaml` (replace the fake `password_hash`), and set `base_url`:

```
read -rs PW; printf '%s\n' "$PW" | docker run -i --rm pgcellar:dev hash-password
```

Remove the example servers from `servers:` (you add yours from the interface), then:

```
docker compose up -d
```

Open http://localhost:8080, sign in as `admin`, and use **Add a server**. The form explains what the PostgreSQL side needs (a backup role, a replication role, `pg_hba.conf`). To try everything without a PostgreSQL server, run the demo on a fake Barman: `make run` (login `admin` / `admin`).

To run the binary without Docker, put `pgcellar` next to a working Barman and start `pgcellar serve --config /etc/pgcellar/pgcellar.yaml`. Archives for linux amd64, linux arm64 and macOS arm64 are built by `make release-dry`.

## Files are the truth

Barman's files stay Barman's (`/etc/barman.conf`, `/etc/barman.d/*.conf`). Everything that is mine lives in one `pgcellar.yaml`. Every change goes through a file, is validated, and is committed in a local git repository of the config directory. The UI and the API are only forms over those files. You can do everything with a text editor and `git log`.

SQLite keeps run history, audit log and sessions. It can be thrown away. Secrets are never written in the YAML or in git.

## Configuration

Start from `deploy/pgcellar.example.yaml`, it is commented line by line. The Barman side is in `deploy/barman.d.example/`. `pgcellar check --config FILE` validates both.

## Alerts

Secrets stay out of the YAML: it only names the environment variables that hold them.

```yaml
alerts:
  max_backup_age: 26h
  repeat_every: 24h          # a problem that lasts is announced again after this; 0 = never
  on_drill_success: false
  daily_summary: "08:30"     # empty = no summary
  telegram:
    token_env: PGCELLAR_TELEGRAM_TOKEN
    chat_id: "-1001234567890"
  mail:
    host: smtp.example.com
    port: 587
    starttls: true
    from: pgcellar@example.com
    to: [dba@example.com]
    user_env: PGCELLAR_SMTP_USER
    password_env: PGCELLAR_SMTP_PASSWORD
  webhook:
    url: https://hooks.example.com/pgcellar
    secret_env: PGCELLAR_WEBHOOK_SECRET   # signs the body: X-PgCellar-Signature: sha256=<hmac>
```

The **Alerts** page (admin) shows which channels are configured, whether their environment variables are set, a button to send a test message, and the latest messages. Nothing is sent during the first two minutes after a start.

## Editions and licence keys

| Edition | Servers | Price |
|---|---|---|
| Free | 2 | 0 |
| Pro | up to 10 | 490 EUR per year |
| Business | unlimited | 1 490 EUR per year |

Early access is free during the beta.

A licence never stops a backup, a restore or a drill. It only limits **adding** a server beyond the quota; what already exists keeps running, and a discreet banner on the Status page says so. After expiry you get 30 days of grace, then the Free rules apply to new servers. The edition and the number of servers are in the footer.

Put the key in `pgcellar.yaml` (`license.key`, or `license.file` for a file that holds it). Keys are signed with Ed25519 and verified offline, nothing phones home. `pgcellar license show --config FILE` tells what the application sees. Get a key at [dbfactory.io/pgcellar](https://dbfactory.io/pgcellar).

## API

Base path `/api/v1`, JSON, `Authorization: Bearer <api key>` (create one with `pgcellar apikey create`). Only the SHA-256 of a key is stored. The full description is served at `/api/v1/openapi.json`. The main routes:

```
GET    /api/v1/status
GET    /api/v1/servers
GET    /api/v1/servers/{name}
GET    /api/v1/servers/{name}/backups
POST   /api/v1/servers/{name}/backup         run a backup now
GET    /api/v1/servers/{name}/timeline
POST   /api/v1/servers/{name}/recoveries     restore to a directory or a point in time
POST   /api/v1/servers/{name}/drills         run a drill now
GET    /api/v1/drills/{id}/report            the PDF
GET    /api/v1/alerts                        channels and journal (admin)
POST   /api/v1/alerts/test                   send a test message (admin)
GET    /api/v1/license
```

## Develop

```
make test       # go test -race ./...
make lint       # gofmt, go vet, golangci-lint
make run        # demo on a fake Barman, login admin / admin
make release-dry
```

Go 1.25 or later. Barman is not needed for the tests: a shell script mimics its command line.

## Licence

Functional Source License 1.1, Apache-2.0 future licence (FSL-1.1-Apache-2.0, see `LICENSE`). Free for internal use. You cannot sell a competing hosted service with it. Each version becomes Apache-2.0 two years after its release.

Barman is GPL-3.0. It is installed unmodified and I only drive it through its command line. It is never imported, linked or patched.

## Status

Version 0.1 is ready to be tagged and not released yet: the repository is private and moves to its organisation first. See `CHANGELOG.md`.

## Author

Gilles Fauvie - [dbfactory.io](https://dbfactory.io) - [stranix.net](https://stranix.net)
