# Configuration reference

Everything that belongs to pgCellar lives in one file, `pgcellar.yaml`. Barman's own settings stay in `/etc/barman.conf` and `/etc/barman.d/*.conf`. Secrets never go in this file: use the `*_env` keys, which name an environment variable, and hashes.

The example in `deploy/pgcellar.example.yaml` is commented line by line. Validate any file with:

```
pgcellar check --config /etc/pgcellar/pgcellar.yaml
```

## The rule: an absent key is the default

If a key is not in the file, pgCellar uses its default. Nothing else. This matters for the settings where the default is not the "zero" value of the language, such as `drills.enabled` (default **on**) or `alerts.repeat_every` (default **24h**, where `0` means *never*).

What follows from it:

- Do not write a key just to write its default. A line `drills: {enabled: false}` is a decision (no scheduled drills), not a placeholder.
- When pgCellar saves the file (a new API key, an edited server), it does not write defaults you never chose. An absent key stays absent. A value you wrote, including an explicit `0`, is kept.
- `pgcellar check` warns about a line that looks like a leftover: `drills.enabled: false` with no drill schedule on any server. Files are never rewritten automatically.

This rule exists because of a real bug: a version once wrote `drills: {enabled: false}` from the zero value when it saved the file, and later versions read that `false` as a choice and silently turned off every drill.

**Durations** are written in the Go style: `30s`, `5m`, `26h`, `192h`. There is no day unit; 8 days is `192h`.

**Reload.** pgCellar re-reads the file when its modification time changes, if the new content is valid. A change made in the console reloads the file first, so a hand edit is never overwritten.

## Top level

| Key | Default | Meaning |
|---|---|---|
| `listen` | `":8080"` | Address and port of the console and the API. Plain HTTP; put a proxy in front for TLS (chapter 12). |
| `base_url` | empty | Public address, used in links inside alerts and reports, for example `https://backup.example.com`. |
| `data_dir` | `/var/lib/pgcellar` | SQLite database (`pgcellar.db`) and runtime files. |
| `status_interval` | `1m` | How often the cached status of every server is refreshed. Minimum `30s`. Each refresh runs about three Barman commands per server (2 to 4 seconds each on a small machine), so on a small host with many servers, raise it. |
| `setup_interval` | `30m` | How often the PostgreSQL settings are read again for Server setup. Minimum `1m`. One read-only `psql` query per server. A server whose settings could not be read is tried again after 5 minutes. |
| `ssh_host` | empty | How PostgreSQL servers reach this host over SSH. Used in the advised `archive_command`. Empty means this machine's host name, which in Docker is a container id: set it. |
| `ssh_port` | `2222` | Port of the SSH companion container, written as `--port` in the advised `archive_command`. |
| `audit_retention_days` | `90` | Audit lines older than this are purged daily. `0` keeps everything. |

## `barman`

Where Barman lives and how to call it.

| Key | Default | Meaning |
|---|---|---|
| `binary` | `barman` | On `PATH`, or an absolute path. |
| `config_file` | `/etc/barman.conf` | Barman's global file. |
| `config_dir` | `/etc/barman.d` | One `<server>.conf` per server. A separate git repository tracks this folder when it is not under the pgCellar one. |
| `user` | `barman` | Commands run as this user when pgCellar runs as root. In the image pgCellar already runs as `barman` and no `sudo` is used. |
| `host` | empty | Reserved for a remote Barman host. Leave empty (local Barman). |
| `cron_interval` | `1m` | How often pgCellar runs `barman cron`. `0` = never. `pgcellar serve --cron-interval` overrides it. |

## `scheduler`

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `true` | `false` = pgCellar starts nothing by itself. The schedule fields keep showing. |
| `catch_up` | `6h` | A due time missed while pgCellar was stopped still runs if it is not older than this. `0` = never catch up. |
| `parallel` | `1` | Backups running at once, all servers together. Must be at least 1. |

## `servers`

A list. What pgCellar adds to a Barman section; `name` must match a `[section]` in `barman.d`.

| Key | Default | Meaning |
|---|---|---|
| `name` | required | Lowercase letters, digits, `-` and `_`. Unique. |
| `label` | empty | Free text shown in the console. |
| `schedule.backup` | empty | `daily 02:00`, `weekly sun 03:00`, `hourly`, `every 6 hours`, or a 5-field cron expression. |
| `schedule.enabled` | on when `backup` is set, off otherwise | Pause the schedule without deleting it. |
| `retention.recovery_window_days` | `0` (not set) | Barman `RECOVERY WINDOW OF n DAYS`. |
| `retention.redundancy` | `0` (not set) | Barman `REDUNDANCY n`. Do not combine with a window. |
| `retention.minimum_redundancy` | `0` | Never delete below this number of backups. Cannot exceed `redundancy`. |
| `drills.schedule` | empty | Schedule of the automatic restore drill, same syntax. Empty = none. |
| `tags` | empty | Free labels for your own use. |

The retention values are written to the Barman file when the server form is saved; pgCellar does not apply retention itself.

## `alerts`

| Key | Default | Meaning |
|---|---|---|
| `max_backup_age` | `26h` | Orange when the last good backup is older than this; red at twice the limit. |
| `max_wal_age` | `24h` | Orange when WAL has not been received for this long. |
| `min_free_percent` | `10` | Orange when the file system holding the backups has less free space (percent); red under half of it. `0` = off. |
| `min_free_bytes` | `0` (off) | Same, in bytes. Either threshold alone is enough. |
| `repeat_every` | `24h` | A lasting problem is announced again after this delay. `0` = never repeat. |
| `on_drill_success` | `false` | Also send a message when a drill passes. |
| `daily_summary` | empty | Time of day, `"08:30"`. Empty = no summary. |
| `telegram.token_env` | | Name of the environment variable holding the bot token. |
| `telegram.chat_id` | | Chat or channel id, as a string. |
| `mail.host`, `mail.port` | | SMTP server. `host`, `from` and `to` are required; the port must be valid. |
| `mail.starttls` | `true` | Upgrade the connection with STARTTLS. `false` sends in clear. |
| `mail.from`, `mail.to` | | Sender, and a list of recipients. |
| `mail.user_env`, `mail.password_env` | | Names of the variables for the SMTP login. Optional. |
| `webhook.url` | | `http://` or `https://`. |
| `webhook.secret_env` | | Name of the variable holding the secret that signs the body. Optional. |

No channel configured means no message is ever sent.

## `license`

| Key | Default | Meaning |
|---|---|---|
| `key` | empty | A licence key, `PGC1.xxxx.yyyy`. |
| `file` | empty | A file that holds the key. |

Neither set means the Free edition (chapter 14).

## `users` and `api_keys`

```yaml
users:
  - name: admin
    role: admin                 # admin, operator or viewer
    password_hash: "$2a$10$..." # bcrypt, from "pgcellar hash-password"

api_keys:
  - name: grafana
    role: viewer
    key_hash: "9f86d0..."       # SHA-256 hex, written by "pgcellar apikey create"
```

At least one user must be `admin`. Names are unique inside each list.

## `recoveries`

| Key | Default | Meaning |
|---|---|---|
| `dir` | `/var/lib/pgcellar/recoveries` | Where restores go: `<dir>/<server>/<backup id>-<timestamp>`. Absolute path, not `/`. Map it on the Docker host. |
| `max_download_bytes` | `2147483648` (2 GiB) | An archive above this has no download link, only its path. `0` = no limit. |
| `keep_days` | `7` | Restores older than this are removed daily. `0` = keep forever. |

## `drills`

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `true` | `false` stops the scheduled drills; manual ones stay possible. |
| `timeout` | `30m` | A whole drill, restore included. |
| `recovery_timeout` | `10m` | Wait for the copy to finish replaying WAL. |
| `keep_last` | `true` | Keep the data directory of the last good drill of each server. |
| `verify_tables` | `false` | `SELECT count(*)` on every table. Slow on big databases. |
| `max_age` | `192h` (8 days) | A server with a drill schedule turns orange when its last good drill is older. |
| `bin_dir` | `/usr/lib/postgresql` | Holds one directory per PostgreSQL major version, each with a `bin/`. Absolute path. |

## `updates`

The check for new releases and the automatic updates (chapter 16). Every key is optional; an absent block means: check once a day, install nothing.

| Key | Default | Meaning |
|---|---|---|
| `check` | `true` | Look for a new release every `interval`. `false` = nothing leaves the machine unless someone presses **Check now**; it also stops automatic updates, which act on what the check found (the host updater included). Only a release build checks: a `dev` build, `--dev` and `--demo` never do. |
| `interval` | `24h` | Time between two checks. At least `1h`. After a failed check the next one comes one hour later. |
| `channel` | `stable` | The only channel for now. Any other value is refused. |
| `auto` | `false` | Binary installs: download, verify and install new releases by themselves, inside `window` (chapter 16 lists the prerequisites). Ignored in a container: there the host updater does the work. Read again at each attempt, no restart needed. |
| `window` | `"02:00-05:00"` | When an update may be applied, `HH:MM-HH:MM` in the local time of the server (`TZ`). It may cross midnight (`"23:00-01:00"`); the start is included, the end is not. Start and end must differ. Applies to `auto` and to `/api/v1/updates/ready`, which the host updater asks. Put it away from the nightly backups and drills. |
| `source` | `https://api.github.com/repos/dbfactory-io/pgcellar/releases/latest` | Where the latest release is read, in the GitHub API format: a mirror that answers the same way. Must be an `http://` or `https://` URL. Read again at each check. |

```yaml
updates:
  auto: true
  window: "03:00-04:30"
```

Validation messages:

```
PROBLEM  updates.interval must be at least 1h0m0s (for example "24h"); leave it out for the default
PROBLEM  updates.channel "beta" is not known: only "stable" exists for now
PROBLEM  updates.window: "3h-4h" is not a time range (use "02:00-05:00")
PROBLEM  updates.window: "03:00-03:00" starts and ends at the same time
PROBLEM  updates.source "ftp://mirror" must be an http:// or https:// URL
```

## Environment variables

| Variable | Used for |
|---|---|
| `TZ` | Time zone of schedules, the daily summary and displayed times. |
| The ones you name in `*_env` | Alert secrets (chapter 8). |
| `PGCELLAR_BASE_URL` | Only in `--demo` mode: base URL of links. |
| `PGCELLAR_IN_DOCKER` | `1` tells pgCellar it runs in a container even without `/.dockerenv` or `/run/.containerenv`: no self-update, the Updates page shows the host updater command instead (chapter 16). |

## Validation errors

`pgcellar check` and the console say what is wrong in a sentence, for example:

```
PROBLEM  server "pg-main": set either recovery_window_days or redundancy, not both
PROBLEM  status_interval must be at least 30s (for example "1m" or "5m"); leave it out for the default
PROBLEM  there must be at least one user with the admin role
PROBLEM  alerts.daily_summary "8h30" is not a time of day (use "08:30", or leave it empty for no summary)
```

Fix the line and save the file again; pgCellar picks the change up.
