# Decisions log

One line per decision, newest first. Date, decision, why.

- 2026-10-20 - Web/API milestone: one service layer (internal/app) behind both the HTML pages and /api/v1; handlers only translate. Why: what the UI can do the API can do, by construction.
- 2026-10-20 - Sessions: cookie holds a random token, the database holds only its SHA-256; the CSRF token is an HMAC of the session id with a secret kept in the settings table; login form uses a stateless HMAC token. Why: nothing replayable in SQLite, no extra column.
- 2026-10-20 - Config writes reload pgcellar.yaml from disk first, and the file is re-read when its mtime changes (if still valid). Why: files are the truth, a hand edit must not be overwritten.
- 2026-10-20 - Commits go to the repository of the pgcellar.yaml folder, plus a second repository in barman.config_dir when that folder is elsewhere. Why: /etc/barman.d is usually not under /etc/pgcellar.
- 2026-10-20 - Status colours: red = a check fails, no good backup, or last backup older than twice the limit; orange = any other problem (last attempt failed, backup older than alerts.max_backup_age, WAL older than max_wal_age); defaults 26 h backup and 24 h WAL when alerts are not set. Why: red must mean "act now".
- 2026-10-20 - Roles: viewer reads; operator also runs check, backup, switch-wal; admin also edits configuration and reads the audit log. Audit is admin-only because it holds exact command lines.
- 2026-10-20 - Barman cron interval is a serve flag (--cron-interval, 0 disables), not a pgcellar.yaml key, because config.Save would drop unknown keys.
- 2026-10-20 - --dev works on a temporary copy of pgcellar.yaml and barman.d.example (git history is created there, deploy/ stays clean), freezes the clock at the fixtures time and resets admin/admin. Why: the example hash is a placeholder and the fixtures are static.
- 2026-10-20 - Name: pgCellar. The barman serves, the cellar keeps; pgcellar.io/.com/.app free at DNS level on that day.
- 2026-10-20 - Licence: FSL-1.1-Apache-2.0. Source available, free internal use, no competing hosted service, Apache-2.0 after two years. Chosen over BSL (four years) for the trust argument, over AGPL dual licensing (CLA, legal fear) and over MIT (nothing to sell).
- 2026-10-20 - Language: Go. Static binary, nobody compiles, contributions stay easy, and the GPL boundary with Barman is enforced by the stack (we can only run the command).
- 2026-10-20 - Barman is driven through its CLI and files only, never imported. GPL-3.0 boundary; the Docker image is a mere aggregation.
- 2026-10-20 - Configuration lives in text files (Barman's INI files + one pgcellar.yaml) with a local git history; SQLite only for run history, audit, sessions, api keys.
- 2026-10-20 - Front end: server-rendered templates + HTMX, no build step, no npm.
- 2026-10-20 - SQLite driver: modernc.org/sqlite (pure Go, no CGO) so cross-compilation stays trivial.
- 2026-10-20 - Pricing to test: Free 2 servers / Pro 490 EUR per year up to 10 servers / Business 1 490 EUR per year unlimited. Offline-verified Ed25519 licence keys.
- 2026-10-20 - Second engine (pgBackRest, MIT) designed for through the `Engine` interface, not built in 0.1.
- 2026-10-20 - Repository starts as private stranix79/pgcellar; it will be transferred to a dbfactory-io organisation (GitHub keeps redirects) and the module path updated before the first public release.
