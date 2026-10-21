# pgCellar manual

pgCellar is a control room for physical PostgreSQL backups with point-in-time recovery. It runs on top of Barman: one Go binary with a web console and a JSON API, shipped as a Docker image that also contains Barman. It tells you the truth about every backup, runs the schedules, restores to a point in time, and restores your backups on its own every week to prove they work.

This manual follows the order in which you meet things: install it, understand the few concepts behind it, prepare PostgreSQL, then use each part. The last chapters are reference, and fixes for the traps people actually fall into.

Three words you will see everywhere. A **server** is one PostgreSQL instance that Barman backs up. A **drill** is an automatic restore test. **Files are the truth**: Barman's own files stay Barman's, everything else is one `pgcellar.yaml`, and git keeps the history.

## Chapters

1. [Getting started](01-getting-started.md) - Docker Compose in five minutes: first admin, first server, first backup.
2. [Concepts](02-concepts.md) - Barman, base backups and WAL, streaming or archive_command, slots, point-in-time recovery.
3. [Preparing a PostgreSQL server](03-preparing-postgresql.md) - Roles, pg_hba.conf and the settings that Server setup checks, with the exact commands.
4. [Servers and WAL methods](04-servers-and-wal-methods.md) - The server form, barman.d, the three WAL methods and the SSH companion.
5. [Backups, schedules and retention](05-backups-schedules-retention.md) - Schedule syntax, the built-in scheduler, Barman retention, deleting a backup.
6. [Restores and point-in-time recovery](06-restores-and-pitr.md) - The Restore page, targets, mapped folder, tar.gz, limits.
7. [Restore drills and reports](07-restore-drills-and-reports.md) - What a drill does step by step, the image requirements, the PDF report and its fingerprint.
8. [Alerts](08-alerts.md) - Telegram, mail, webhook, anti-noise rules, daily summary, sample messages.
9. [Users, roles, API keys and the API](09-users-roles-and-api.md) - Roles, API keys, Bearer authentication, the main routes.
10. [Configuration reference](10-configuration-reference.md) - Every key of pgcellar.yaml with its default.
11. [Operations](11-operations.md) - Updating, backing up pgCellar itself, metrics, logs, audit, storage and projection.
12. [Security model](12-security-model.md) - Roles, sessions, CSRF, login limiter, secrets, restricted SSH, non-root container, demo mode.
13. [Troubleshooting](13-troubleshooting.md) - The real traps, with the symptom, the cause and the fix.
14. [Licensing and editions](14-licensing-and-editions.md) - FSL-1.1, what a licence blocks and what it never blocks.
15. [CLI reference](15-cli-reference.md) - serve, check, apikey, hash-password, license, version.
16. [Releases and updates](16-releases-and-updates.md) - How a release is built, what it contains, and how to update.
