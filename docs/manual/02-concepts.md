# Concepts

Six ideas explain nearly everything in pgCellar. Read this chapter once; the rest of the manual gets easier.

## Barman does the backups, pgCellar drives it

Barman (by EnterpriseDB) is the tool that actually takes physical backups of PostgreSQL, keeps the WAL, applies retention and restores. It is excellent and it is a command line. pgCellar adds what the command line lacks: one screen that tells the truth, schedules, a restore form, alerts, and restore tests.

pgCellar never imports, patches or links Barman. It runs the `barman` command, reads its JSON output and its files, and writes its configuration files. The Docker image installs Barman unchanged from the PostgreSQL project's own packages. Barman is GPL-3.0; pgCellar only drives it through its command line, and that boundary is a rule of the project.

Consequence: everything pgCellar does you can do by hand with the `barman` command, and anything you do by hand shows up in pgCellar at the next refresh.

## A physical backup is not a dump

A dump (`pg_dump`) writes the content of the database as SQL. It brings you back to the moment the dump started, and nowhere else.

A physical backup copies the files of the PostgreSQL cluster. Combined with the WAL, the write-ahead log that records every change, it lets you stop at any moment you choose. That is what lets you answer "bring the database back as it was at 14:31, one minute before the bad query".

Two things make that work:

- **Base backups.** A full copy of the data directory, taken while the server runs. It is consistent only together with the WAL written during the copy.
- **WAL files.** PostgreSQL writes its changes to 16 MB segments. Barman keeps every segment, from the first backup on, in an archive.

A restore is "start from a base backup, then replay WAL up to the point you want". Without the WAL between two backups you can restore to the backups themselves, nothing in between. So a backup system is only as good as its WAL chain, and pgCellar's Timeline page draws that chain and shows holes.

## Two ways for the WAL to reach Barman

**Streaming.** Barman connects to PostgreSQL like a replica and receives WAL as it is written, through `pg_receivewal`. No SSH, no setup on the PostgreSQL host beyond a role and a `pg_hba.conf` line. The data is nearly current: a crash loses almost nothing. Barman keeps a **replication slot** on the server so PostgreSQL does not throw away WAL that Barman has not received yet.

**archive_command.** PostgreSQL pushes each completed WAL file to Barman by running a command, here `barman-wal-archive` over SSH. It needs the `barman-cli` package and an SSH key on the PostgreSQL host, and the recovery point is the last *completed* 16 MB file (or `archive_timeout`). It works where a replication connection is impossible, and it is what the rsync backup method requires.

**Both** is allowed and is the most resilient: WAL arrives twice, and either path alone is enough to keep the chain whole.

Chapter 4 shows how to choose, and the exact settings.

## Slots, and why they are both a gift and a risk

A replication slot is a promise from PostgreSQL: "I will not delete WAL that this consumer has not read". It makes streaming reliable. The cost: if the backup host is down for a long time, PostgreSQL keeps all that WAL and its disk can fill up. The setting `max_slot_wal_keep_size` caps that risk. pgCellar's Server setup warns when it is not set (chapter 3).

## Point-in-time recovery

PITR is "base backup plus WAL replay up to a target". The target can be:

- the end of the backup (the moment the copy became consistent),
- a date and time,
- the latest moment Barman holds (replay everything).

The earliest target is the end of the oldest backup whose WAL chain is unbroken. The latest is the last data received (streaming) or the last completed WAL file. The Timeline page works both out for you and shows them in a sentence.

## What pgCellar adds, and what it never touches

pgCellar adds:

- **Status**, one card per server, green, orange or red with the reason in a sentence.
- **Server forms** that write Barman's configuration, and **Server setup**, which reads the PostgreSQL settings Barman depends on and tells you what to change.
- **A scheduler** (no host cron), **retention** through Barman, **backup deletion**.
- **Restore to a directory** with a target, and a `tar.gz` if you want.
- **Restore drills**: every week, a real restore, started and checked, with a PDF report.
- **Alerts**, a **JSON API**, **Prometheus metrics**, **audit**, and a git history of the configuration.

pgCellar never:

- changes anything on your PostgreSQL server. Server setup only *says* what to change; you run the command. The only queries it sends are a read-only `SELECT` on `pg_settings` and what Barman itself runs;
- stores a secret in `pgcellar.yaml` or in git (passwords live in `~/.pgpass`, alert secrets in environment variables, SSH private keys in the barman user's `~/.ssh`);
- keeps the truth in its own database. SQLite holds run history, audit and sessions. You can delete it and lose history, not backups.

## Files are the truth

Barman's configuration stays in `/etc/barman.conf` and `/etc/barman.d/<server>.conf`. Everything pgCellar owns is one YAML file, `pgcellar.yaml`. The console and the API are forms over those files. Every change is validated, written, and committed to a local git repository of the configuration folder, with the name of the user as author.

You can edit the files with a text editor; pgCellar re-reads `pgcellar.yaml` when its modification time changes. Read `git log` in the configuration folder to see who changed what, and the **History** page shows the same thing.

## Vocabulary

| Word | Meaning |
|---|---|
| Server | One PostgreSQL instance backed up by Barman. A `[section]` in `barman.d`. |
| Base backup | A full physical copy of the data directory. |
| WAL | Write-ahead log, the stream of changes. Archived in 16 MB files. |
| Slot | A replication slot on the PostgreSQL side, holding WAL for Barman. |
| Recovery window | Retention rule: keep enough backups to restore to any point in the last N days. |
| Run | One command started from pgCellar (backup, restore, drill), with its output. |
| Drill | An automatic restore of a backup into a throwaway PostgreSQL, checked, then removed. |
