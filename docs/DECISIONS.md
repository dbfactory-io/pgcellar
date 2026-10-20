# Decisions log

One line per decision, newest first. Date, decision, why.

- 2026-10-20 - Name: pgCellar. The barman serves, the cellar keeps; pgcellar.io/.com/.app free at DNS level on that day.
- 2026-10-20 - Licence: FSL-1.1-Apache-2.0. Source available, free internal use, no competing hosted service, Apache-2.0 after two years. Chosen over BSL (four years) for the trust argument, over AGPL dual licensing (CLA, legal fear) and over MIT (nothing to sell).
- 2026-10-20 - Language: Go. Static binary, nobody compiles, contributions stay easy, and the GPL boundary with Barman is enforced by the stack (we can only run the command).
- 2026-10-20 - Barman is driven through its CLI and files only, never imported. GPL-3.0 boundary; the Docker image is a mere aggregation.
- 2026-10-20 - Configuration lives in text files (Barman's INI files + one pgcellar.yaml) with a local git history; SQLite only for run history, audit, sessions, api keys.
- 2026-10-20 - Front end: server-rendered templates + HTMX, no build step, no npm.
- 2026-10-20 - SQLite driver: modernc.org/sqlite (pure Go, no CGO) so cross-compilation stays trivial.
- 2026-10-20 - Pricing to test: Free 2 servers / Pro 490 EUR per year up to 10 servers / Business 1 490 EUR per year unlimited. Offline-verified Ed25519 licence keys.
- 2026-10-20 - Second engine (pgBackRest, MIT) designed for through the `Engine` interface, not built in 0.1.
