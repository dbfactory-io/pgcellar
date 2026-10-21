# Troubleshooting

Each entry has the symptom you see, the cause, and the fix. They come from real installations, in the order people hit them. Start with the **Runs** page and the output of the failed run: pgCellar keeps every command with its answer.

## Setup and sign-in

### "attempt to write a readonly database" in the logs

**Cause.** A volume or folder was created by a container that ran as root (an older image, or Docker creating a bind mount). pgCellar now runs as the `barman` user and cannot write to it.

**Fix.** Give the folders to the barman uid, then restart:

```
docker compose run --rm --entrypoint id pgcellar barman   # prints the uid, 997 in the current image
sudo chown -R 997:997 etc-pgcellar etc-barman.d /path/to/your/data /path/to/recoveries
docker compose restart pgcellar
```

For named volumes: `docker compose run --rm --no-deps --user root --entrypoint chown pgcellar -R 997:997 /var/lib/pgcellar`.

### "sudo: executable file not found" on every `barman cron`

**Cause.** An older build ran pgCellar as root in a way that made it prefix commands with `sudo -u barman`. The image has no sudo.

**Fix.** Use the current image. pgCellar only prefixes `sudo -u barman` when the current user is not already the barman user.

### Sign-in answers 429

**Cause.** More than 10 attempts a minute, or 8 failures in 10 minutes, from one address. Behind a proxy, everyone may share the proxy's address if `X-Real-IP` is not passed.

**Fix.** Wait a minute. Behind a proxy, pass `X-Real-IP $remote_addr` (chapter 12).

### The page looks broken after an update (no styles, odd layout), mostly on Safari

**Cause.** The browser kept the old stylesheet with the new HTML. Safari is the usual victim: an earlier build served `app.css` with `max-age=3600`.

**Fix.** Current builds link the stylesheet as `/static/app.css?v=<hash of its content>`, so a change is always a new URL. If you see it, reload without the cache (Cmd-Shift-R) once. If a reverse proxy sits in front, make sure it does not add its own caching to HTML.

### After `docker compose up`, the proxy answers 502

**Cause.** A reverse proxy resolved the container's IP address once and keeps it. Compose gave the recreated container a new address.

**Fix.** Reload the proxy (`nginx -s reload`). For a lasting fix, let nginx re-resolve: use the Docker resolver `resolver 127.0.0.11 valid=10s;` and a variable in `proxy_pass` (`set $up http://pgcellar:8080; proxy_pass $up;`).

## The server page

### Everything is red in the first minute after a start

**Cause.** Barman has not started its WAL receivers yet. `barman cron` starts them.

**Fix.** Nothing to do. pgCellar runs `barman cron` once before the first refresh, then every minute, and stays silent on alerts for two minutes. If it stays red, read the failed check.

### "first refresh pending" or HTTP 503 with `Retry-After`

**Cause.** The status cache has no data for the server yet (just added, or just started).

**Fix.** Wait up to `status_interval`.

### "Could not read the server settings"

**Cause.** The `psql` query behind Server setup failed: the server is unreachable from the pgCellar host, or the barman user's `~/.pgpass` has no line for this connection.

**Fix.** Run the test from chapter 3 (`psql "host=... user=barman dbname=postgres" -c "select 1"`) and read the error. The line in `.pgpass` is `host:port:database:user:password`, mode 0600, host and port as in the `conninfo`. It does not change the health of the server: Barman's own checks say whether the server is reachable.

### `archive_command` shows "hidden" or `<insufficient privilege>`

**Cause.** The role pgCellar connects with may not read `archive_command`.

**Fix.** `GRANT pg_read_all_settings TO barman;`.

### "no pg_hba.conf entry for host ..." for a relay or proxy

**Cause.** PostgreSQL sees the address of the relay (HAProxy, a Docker network gateway), not the address of the pgCellar host.

**Fix.** Read the address in the PostgreSQL log line and use that address or its subnet in `pg_hba.conf`. A `host replication` line is separate from `host all`; you need both.

### The Runs page is empty although backups exist

**Cause.** Runs lists only what was started from pgCellar. Backups taken with the `barman` command or from a host cron appear in the Backups list of the server page, which comes from Barman.

**Fix.** Nothing to fix. Let the built-in scheduler run the backups and remove the host cron.

## Backups

### `barman backup --wait` hangs for a quarter of an hour on a quiet database

**Cause.** The end of a backup sits in a WAL segment that is still being written. On a quiet database that segment fills (16 MB) rarely, so `--wait` blocks until a switch happens. One real case: 11 seconds of copy, 15 minutes of waiting.

**Fix.** Do not use `--wait`. Run `barman backup <server>`, then `barman switch-wal --force --archive <server>`. pgCellar does exactly that. If you take backups by hand, do the same; or press **Backup now**.

### "Last filled WAL file" is hours old although streaming is live

**Cause.** On a quiet database a WAL file fills slowly. Streaming data still arrives in a partial file, so you are not at risk, but the *completed* file is old.

**Fix.** It is a note, not an alert, while streaming is live (the server page shows "Streaming: live" and "last data"). For a bounded recovery point also for the archive path, set `archive_timeout = '1h'` on PostgreSQL (chapter 3). Tune `alerts.max_wal_age` if you want a tighter or looser limit.

### Warning: `archive_timeout` is 0

**Cause.** A quiet database never closes a WAL file by itself, so with `archive_command` the recovery point can be very old.

**Fix.** `ALTER SYSTEM SET archive_timeout = '1h'; SELECT pg_reload_conf();`. A warning never turns the server orange.

### Warning: `max_slot_wal_keep_size` is -1

**Cause.** With a replication slot and no limit, PostgreSQL keeps all WAL Barman has not received. If the backup host is down for long, the PostgreSQL disk fills.

**Fix.** `ALTER SYSTEM SET max_slot_wal_keep_size = '2GB'; SELECT pg_reload_conf();` (or more, sized for your longest outage). If the limit is exceeded, the slot is invalidated and the WAL chain is broken: take a new backup.

### Warning: `data_checksums` is off

**Cause.** The cluster was created without checksums. A damaged page goes unnoticed, in the backup as well, and a drill cannot tell.

**Fix.** It is not a setting. New clusters: `initdb --data-checksums`. Existing clusters: `pg_checksums --enable -D <data directory>` on a **stopped** cluster, then start it. On a big cluster this takes a while; plan it.

### A hole in the WAL on the Timeline

**Cause.** A segment is missing between two files of the same timeline: the slot was dropped or invalidated, `archive_command` failed for a while, or WAL was removed by hand.

**Fix.** The backup before the hole cannot be used for a target after the hole. Fix the cause, then take a new backup; the chain restarts from it. The earliest restore point moves forward accordingly.

### A server listed in pgcellar.yaml but missing in barman.d

**Cause.** `pgcellar.yaml` lists a server that has no `[section]` in `barman.d`, or the file does not validate.

**Fix.** `pgcellar check --config ...` names each problem. The server form refuses an invalid combination with the reason.

## Restores and drills

### The drill fails at "Find the PostgreSQL version": "PostgreSQL N binaries are not in this image"

**Cause.** The restored directory is from a major version that the image does not hold. The image ships binaries for the versions in `PG_MAJORS` (default `16 17 18`).

**Fix.** Rebuild with the version added: `--build-arg PG_MAJORS="15 16 17 18"`. Check your servers with `SHOW server_version;`.

### The drill fails at "Start PostgreSQL": "The cluster uses locale X, which this image does not have"

**Cause.** A restored cluster keeps the locale of its source (for example `en_US.utf8` for a `postgres` container). PostgreSQL refuses to start on a locale the system lacks.

**Fix.** Add the locale to the build argument `PG_LOCALES` and rebuild (chapter 7). Find it with `SELECT datcollate, datctype FROM pg_database;`. The run output also contains the last 40 lines of the copy's log.

### The demo refuses to start: "not executable (noexec mount?)"

**Cause.** A `tmpfs` mounted without `exec`. Docker mounts a `tmpfs` as `noexec` by default, and programs written there cannot start. Demo mode extracts its fake `barman`, `psql` and the rest to a temporary directory and refuses to start if they cannot run, rather than fall back to the real ones.

**Fix.** Add `exec` to the mount: `tmpfs: ["/tmp:size=64m,mode=1777,exec"]`.

### A drill fails at "Wait for the end of the recovery"

**Cause.** The copy has too much WAL to replay inside `drills.recovery_timeout`, or the WAL it needs is missing.

**Fix.** Raise `drills.recovery_timeout` (and `drills.timeout`). If WAL is missing, look at the Timeline for a hole.

### A drill fails at the checksums step

**Cause.** `pg_checksums --check` found a damaged page. This is exactly what the step is for.

**Fix.** Treat it as a real problem: the page is damaged in the backup, and probably on the source. Look at the PostgreSQL log, check the disk of the source, and take a new backup after the source is repaired.

### The drills never run

**Cause.** The server has no `drills.schedule`, or `drills.enabled: false` is in the file. A leftover `enabled: false` is the usual one (see the rule "an absent key is the default" in chapter 10).

**Fix.** Set `servers[].drills.schedule` (for example `weekly sun 04:30`) and delete the `enabled: false` line. `pgcellar check` warns about it.

### Restore says "the target is before the end of the backup"

**Cause.** Barman refuses a target time earlier than the end of the chosen backup.

**Fix.** Pick an older backup, or a later time. The Timeline shows the earliest point you can reach.

### "Permission denied" writing a restore, or the restore folder is empty on the host

**Cause.** `recoveries.dir` is inside the container and the folder you mapped is not owned by the barman uid, or it is not mapped at all and the data lives in an anonymous volume.

**Fix.** Map a host folder with a bind mount, and `chown` it to the barman uid (chapter 6).

## WAL shipping (`archive_command`)

### `barman-wal-archive` fails on the PostgreSQL host

**Check, in this order.**

1. The package `barman-cli` is installed on the PostgreSQL host.
2. The command is exactly the advised one, with `--port 2222` and the right host name. Set `ssh_host` and `ssh_port` in `pgcellar.yaml`.
3. The public key of the `postgres` user is authorised for this server on the **SSH key** page.
4. The `postgres` user has accepted the host key: `sudo -u postgres ssh -p 2222 barman@<host> true` once.
5. The `pgcellar-ssh` container is running and the port is reachable. Its log (`docker compose logs pgcellar-ssh`) says why a key was refused.

`barman-wal-archive --test` fails with a restricted key; that is expected. Use **Switch WAL** and **Run check now**.

### "WAL archive" check stays red after a switch

**Cause.** No WAL file arrived. Often `archive_mode` is off (needs a restart) or `archive_command` was not reloaded.

**Fix.** Server setup shows both with the exact commands. After fixing them, press **Switch WAL** and **Run check now**.

## Still stuck

Collect, in this order, before you ask for help:

- the failed run on the Runs page (command, exit code, output);
- `docker compose logs --tail 100 pgcellar`;
- `pgcellar check --config /etc/pgcellar/pgcellar.yaml`;
- `pgcellar version`;
- the Server setup section of the server.

Never paste a `.pgpass`, a key, a token or a licence key into a ticket.
