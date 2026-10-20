# pgCellar - working notes for every session and every agent

Read this before touching the repo. The product brief is docs/BRIEF.md, the running list of decisions is docs/DECISIONS.md.

## What this is
A control room for PostgreSQL backups, built on top of Barman (EnterpriseDB, GPL-3.0): one Go binary (web UI + JSON API) that reads and writes Barman's own configuration files, runs Barman's CLI, shows the state of every server and backup, drives point-in-time recovery, and runs automatic restore drills. Shipped as a binary and as a Docker image that also contains Barman.

## Non-negotiable decisions
- Go 1.25+, single module `github.com/stranix79/pgcellar`, single binary `pgcellar`, no CGO (SQLite through `modernc.org/sqlite`). Cross-compiles linux/amd64, linux/arm64, darwin/arm64.
- Standard library first: `net/http` with the 1.22+ pattern-based mux, `html/template`, `embed`. No web framework. Front end = server-rendered HTML + HTMX (vendored in `web/static`, no build step, no npm). Readable by a sysadmin with a text editor.
- **Barman is never imported, modified or linked.** We run its command line (`barman ... --format json` whenever the command supports it) and read/write its files. The image installs Barman from the PGDG packages unchanged. This keeps our code out of the GPL and is a product rule, not a preference.
- **Files are the truth.** Barman's config stays in `/etc/barman.conf` and `/etc/barman.d/<server>.conf` (INI). Everything that is ours lives in one YAML file (`pgcellar.yaml`). The web UI is a form over those files; the API is the same form over HTTP. Every write through pgCellar = write file, validate, record in the config directory's local git repository (one commit per change, author = the pgCellar user). Nothing lives only in the database: SQLite holds run history, audit log and sessions, all of it reconstructible or discardable.
- Secrets (SSH private keys, passwords, API keys) are never written into `pgcellar.yaml` nor committed to the config git repo. Private keys stay in the Barman user's `~/.ssh` with 0600; API keys and session secrets are stored hashed in SQLite.
- Executing anything on a production PostgreSQL (backup, recover, switch-wal) goes through one function in `internal/barman/run.go` that logs the exact command, its exit code and duration to the audit log. No other place shells out.
- Tests: `go test ./...` green at every milestone. Barman itself is not required for tests: `internal/barman/testdata/fake-barman` is a shell script that mimics the real CLI (same flags, JSON fixtures) and is put on PATH by the tests.
- Quality: gofmt, go vet, golangci-lint clean (config in .golangci.yml). Conventional commits in English (`feat:`, `fix:`, `docs:`...). No mention of AI tooling anywhere: commits, code, comments, docs.
- Licence: Functional Source License 1.1, Apache-2.0 future licence (file LICENSE). Headers are not required in source files.
- Language: code, comments, commits and docs in English. The UI ships in English first; strings go through one place (`internal/web/i18n.go`) so French can follow.
- Undecided points: pick the simplest option, log it in docs/DECISIONS.md with the date and one line of why, keep going. Ask only if the milestone is blocked.
- Agents working in parallel own one directory each and do not touch `go.mod`, `go.sum` or files outside their directory; the orchestrator integrates and runs `go mod tidy`.

## Layout
cmd/pgcellar (main: `serve`, `check`, `apikey`, `license`, `hash-password`, `version`) · internal/alerts (Telegram, mail, signed webhook channels; the rules that decide what is worth a message are in internal/app/alerts.go) · internal/license (Ed25519 licence keys, embedded public key) · internal/report (PDF writer for the drill report) · internal/barman (INI config read/write, CLI runner, JSON parsing, fake binary for tests) · internal/config (pgcellar.yaml, validation, git history of the config dir) · internal/store (SQLite: runs, audit, sessions, api keys) · internal/web (handlers, templates, HTMX pages, auth) · internal/api (JSON API v1, api keys) · internal/sshkeys (ed25519 keypair, deploy public key, test connection) · web/ (templates, static) · deploy/ (Dockerfile, docker-compose.yml, pgcellar.example.yaml, systemd unit) · docs/ · internal/barman/testdata

## Demo mode
`pgcellar serve --demo` (`make demo`, `internal/demo`, `deploy/demo/`): public read-only instance with fictional data (Northwind Logistics), fakes embedded in the binary, history rebuilt from the clock, every mutation refused with 403 in `internal/web/demo.go`; see docs/DECISIONS.md.

## Commands (Makefile)
- `make build`  -> ./pgcellar
- `make test`   -> go test ./... (race on)
- `make lint`   -> gofmt check + go vet + golangci-lint
- `make demo`   -> ./pgcellar serve --demo (fictional data, demo / demo)
- `make run`    -> ./pgcellar serve --config deploy/pgcellar.example.yaml --dev (fake barman on PATH)
- `make docker` -> docker build deploy/
- `make release-dry` -> goreleaser snapshot (linux amd64/arm64, darwin arm64, checksums in dist/), nothing published; `make version-check` -> the injected version is printed

## Milestones
1. Foundations: barman package (parse/write barman.d, run CLI, parse JSON of check/list-backups/show-backup/status), config package (yaml + git history), store, fake barman, deploy skeleton. DONE when `go test ./...` is green and `pgcellar check --config deploy/pgcellar.example.yaml` lists the servers from a fixture barman.d.
2. Web + API: login, Servers page (list, add/edit form writing barman.d, live `barman check` with explanations), Backups page (list per server, run now), Status dashboard, JSON API v1 with api keys, `/metrics`.
3. SSH keys: generate ed25519 pair for the barman user, show the public key, optional deploy with a one-time password, test connection.
4. Scheduling and retention: DONE (2026-10-20). pgCellar runs the `schedule.backup` of each server itself (`internal/app/scheduler.go`, `scheduler:` keys enabled/catch_up/parallel); Barman applies the `retention_policy` written to barman.d from the server form.
5. PITR wizard and restore drills: DONE (2026-10-20). The wizard is the Restore page; the drill (`internal/app/drills*.go`) restores a backup, starts the copy with the PostgreSQL binaries of its major version (`/usr/lib/postgresql/<major>/bin`, installed in the image by `PG_MAJORS`), checks it, stops it, and writes a dated PDF (`internal/report`, no dependency). Per-server `drills.schedule` run by the scheduler; global `drills:` block (enabled, timeout, recovery_timeout, keep_last, verify_tables, max_age, bin_dir).
6. Alerts, licence key, release 0.1: DONE (2026-10-20). Alerts: Telegram, mail and signed webhook (`internal/alerts`, rules in `internal/app/alerts.go`, page `/alerts`, API `/api/v1/alerts`). Licence: `internal/license`, `pgcellar license keygen|issue|verify|show`; a licence never blocks a backup, a restore or a drill, it only limits adding a server (Free = 2). Release 0.1 is prepared (CHANGELOG.md, README.md, goreleaser, CI) and NOT tagged: the owner tags after the repository has moved to its GitHub organisation (module path then changes).
