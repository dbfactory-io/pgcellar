# pgCellar - product brief

The barman serves, the cellar keeps.

## One sentence
A self-hosted control room for physical PostgreSQL backups with point-in-time recovery, built on Barman: add a server in three clicks, keys generated and deployed, schedules and retention, a status board that never lies, a PITR wizard, and a weekly automatic restore drill with a dated report you can hand to an auditor.

## Why it exists
Every tool with a web UI (pgBackWeb/UFO Backup, Databasus, the SaaS backup services) runs logical dumps: no return to 14:32 before the bad query. The tools that do physical backups with PITR (Barman, pgBackRest, WAL-G) are command-line only, with no overview and no restore testing. Small and mid-sized companies, and the consultants who serve them, are stuck between the two. pgCellar is the missing layer.

## Principles
1. Files are the truth. Barman keeps its own files; pgCellar keeps one YAML. The UI edits files, the API edits files, git keeps the history. A sysadmin, an automation agent or a text editor can do everything the UI does.
2. Never trust a backup you have not restored. The weekly restore drill is a core feature, not an add-on.
3. Barman is upstream, not ours. We run it, we read its JSON, we never fork it.
4. One binary, zero ceremony. Download, run, open the browser. Docker image for those who prefer.
5. Quiet by default. Alerts only when something needs a human.

## Users
- The accidental DBA of a 20-200 people company: has three PostgreSQL servers, nightly dumps, and a vague feeling that it would not come back.
- The freelance consultant or small MSP managing PostgreSQL for several customers: wants one screen per customer and proof for the quarterly report.
- The ops team under NIS2 or ISO 27001 pressure: needs restore evidence.

## Scope of 0.1
Servers, backups, status, PITR wizard, restore drill with report, alerts, API, metrics, licence key. Single Barman host. Streaming and rsync/SSH methods. PostgreSQL 13 to 18.

## Out of scope for 0.1
Multiple Barman hosts behind one UI, pgBackRest engine, barman-cloud/S3 repositories, hosted control plane, SSO. All designed for, none built.

## Licence and business model
Functional Source License 1.1 with Apache-2.0 future licence: source available, free for internal use, no competing hosted service, each version becomes Apache-2.0 two years after release. Revenue: yearly licence per managed server beyond the free tier (Free 2 servers; Pro up to 10 servers with drills and reports; Business unlimited, several Barman hosts, SSO, support), dbfactory services (backup and recovery audit, setup, on-call), later managed operations. Licence keys are Ed25519-signed and verified offline.

## Name
pgCellar. Not "Barman" anything: Barman is EnterpriseDB's trademark; "built on Barman" is descriptive and fine.

## Scaling

One pgCellar instance per Barman host. No page and no GET of the API ever runs Barman: everything is served from the status cache (refreshed every `status_interval`, saved in SQLite so a restart starts with full pages marked stale) and from the files of `barman.d` and `pgcellar.yaml`. Only explicit actions (check now, backup now, switch WAL, all POST) call Barman. The cost of a refresh is about 3 Barman processes per server and per interval (`status`, `list-backups`, `check`, plus `show-backup` for a new backup only), 2 to 4 s each on a small machine.

Next steps for hundreds of servers: read `base/<id>/backup.info` and the WAL catalogue (`xlog.db`) instead of `list-backups` and `status` (Barman's own files, instantaneous); a `check_interval` separate from and longer than `status_interval`; spread the servers over the interval instead of one burst; configurable parallelism (4 today). A multi-host view comes after that.
