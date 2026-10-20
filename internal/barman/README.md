# internal/barman

Drives an installed Barman through its command line and its INI files. Nothing from Barman is imported (GPL boundary, see CLAUDE.md).

| File | Role |
|---|---|
| `types.go` | Shared types and the `Runner` / `Engine` interfaces |
| `config.go` | Read and write `/etc/barman.conf` and `/etc/barman.d/*.conf`, `Validate` |
| `run.go` | `CLIRunner`: the only place that starts Barman, audits every call |
| `json.go` | Tolerant parsers for `--format json` output |
| `engine.go` | `CLIEngine`: `Engine` on top of a `Runner` |
| `explain.go` | Standard `barman check` lines: what they mean, how to fix |
| `testdata/fake-barman` | Bash stand-in for the CLI, fixtures in `testdata/fixtures` |

## Config files

Barman reads them with Python's configparser. `SaveServer` edits lines in place: it rewrites a line only when its value changed, drops keys that were emptied, appends new keys at the end of the section, and leaves comments, blank lines, key order, unknown keys, continuation lines and the other sections of the file as they are. Writes are atomic (temp file in the same directory, rename) with mode 0640. A new server goes through `SaveNewServer(dir, server)`, which creates `<name>.conf` and refuses to overwrite.

## What the real Barman prints (checked against Barman 3.20.1)

Source read: `barman/output.py` (`JsonOutputWriter`), `barman/cli.py`, `barman/server.py`, `barman/backup.py`, `barman/wal_archiver.py` from the 3.20.1 wheel. No real Barman was run: the fixtures are written from that code, not captured from a live server.

Global option: `-f json` or `--format json`, before the sub-command (`barman --format json check pg-main`). Output is one JSON object written at the end of the command (`json.dump(..., sort_keys=True)`), so keys come out alphabetically sorted.

Messages are collected in top-level lists: `_INFO`, `_WARNING`, `_ERROR`, `_EXCEPTION`, `_DEBUG`. For example an unknown server gives `{"_ERROR": ["Unknown server 'x'"]}` and exit status 1.

- `list-servers`: no dedicated JSON. The console writer's text goes through `info()`, so the result is `{"_INFO": ["pg-main - Main database", "pg-rsync"]}`. With `--minimal`: `{"_INFO": ["pg-main", "pg-rsync"]}`. The description suffix can carry ` (inactive)`, ` (WARNING: disabled)` or ` (Passive)`.
- `check <server>`: `{"<server>": {"<check_key>": {"status": "OK" | "FAILED", "hint": "..."}}}`. The key is the printed check name lower-cased, spaces and `-` turned into `_`, dots removed: `WAL archive` -> `wal_archive`, `receive-wal running` -> `receive_wal_running`, `superuser or standard user with backup privileges` -> `superuser_or_standard_user_with_backup_privileges`. Some names carry a suffix in parentheses (`wal_level (standby)`). Exit status is 1 when a check fails, and the JSON is still complete.
- `list-backups <server>`: `{"<server>": [ {...}, ... ]}`. Each entry: `backup_id`, `backup_type`, optional `backup_name`, `status`; completed backups (DONE and other copy-done states) add `end_time` (Python `ctime()`, e.g. `"Tue Oct 20 02:10:30 2026"`, server local time), `end_time_timestamp` (epoch, as a string), `size_bytes`, `wal_size_bytes`, `size`, `wal_size` (pretty text), `retention_status`. A failed or running backup has only id, type and status. With `--minimal` the list holds bare ids. No begin time: pgCellar derives it from the id (`YYYYMMDDTHHMMSS`, server local time).
- `show-backup <server> <id>`: `{"<server>": {...}}` with `backup_id`, `status`, `backup_type`, `system_id`, `postgresql_version` (integer such as 160004, shown as `16.4` by pgCellar), `pgdata_directory`, `cluster_size_bytes`, `tablespaces`, `server_information`, `base_backup_information` (`backup_size_bytes`, `wal_size_bytes`, `begin_time` as `isoformat(" ")` with offset, `begin_time_timestamp`, `end_time`, `end_time_timestamp`, `begin_wal`, `end_wal`, `timeline`, `compression`, `copy_time_seconds`, `throughput`...), `catalog_information` (`retention_policy`, `previous_backup`, `next_backup`), `wal_information`. A backup that is not DONE has only id, status and `error`. The id may be `latest`, `first`, `oldest`, `last`.
- `status <server>`: `{"<server>": {"<code>": {"description": "...", "message": "..."}}}`. Every value is text, including booleans (`"True"`) and numbers (`"3"`). Codes seen: `description`, `active`, `disabled`, `active_model`, `pg_version`, `is_in_recovery`, `current_size`, `data_directory`, `current_xlog`, `archive_command`, `last_archived_wal` (`"<segment>, at <ctime>"` or `"No WAL segment shipped yet"`), `failed_count`, `passive_node`, `retention_policies`, `backups_number`, `first_backup`, `last_backup` (`"None"` when empty), `minimum_redundancy`. A server that is not active only gets the first few.

Parsers ignore unknown keys, accept numbers as numbers or strings, read dates from the epoch key first and the text key second, strip any text before the first `{`, and keep the full object in `Backup.Raw`.

## Not verified

- Actual output of a running Barman. The fixtures follow the code of 3.20.1; older 3.x versions lack some keys (`backup_type`, `cluster_size_bytes`), which the parsers tolerate.
- `summarize_wal`, snapshot and incremental-backup keys are passed through in `Raw` but not interpreted.
- Dates without a zone (`ctime`) are read in the local zone of the pgCellar host; this is right only when pgCellar and Barman share a zone. The epoch keys are preferred for that reason.
