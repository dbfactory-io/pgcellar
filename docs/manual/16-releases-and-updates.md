# Releases and updates

## How a release is built

A release starts from one annotated tag. Nothing is built by hand and nothing is uploaded from a laptop.

1. The section of the version is written in `CHANGELOG.md` (`## [0.1.1] - 2026-10-28`).
2. That change is committed on `main`, then tagged: `git tag -a v0.1.1 -m "pgCellar 0.1.1"`.
3. `git push origin main --tags`.

The tag starts the `release` workflow on GitHub Actions (`.github/workflows/release.yml`). It runs three jobs.

**binaries.** `go test ./...`, then GoReleaser builds the static binary for linux amd64, linux arm64 and macOS arm64, packs each one with `LICENSE` and `README.md`, and writes `checksums.txt` (SHA-256). It creates the GitHub Release as a draft. The notes of the release are the section of `CHANGELOG.md` for that version, extracted by `make release-notes VERSION=0.1.1`; a tag without a section in the changelog fails here.

**image.** Docker Buildx builds `deploy/Dockerfile` for linux/amd64 and linux/arm64 (arm64 under QEMU emulation, about 15 minutes because of the PGDG packages) with `--build-arg VERSION=0.1.1`, and pushes `ghcr.io/dbfactory-io/pgcellar:0.1.1` and `:latest`. The image is pushed without attestation manifests, so its digest is the one `docker pull` shows.

**manifest.** When both jobs succeeded, a small `manifest.json` is attached to the release, then the draft is published. Until that moment the release is invisible: `releases/latest` never points at a release without its image or its manifest.

What a release contains:

| Asset | Content |
|---|---|
| `pgcellar_0.1.1_linux_amd64.tar.gz` | binary, LICENSE, README.md |
| `pgcellar_0.1.1_linux_arm64.tar.gz` | same, arm64 |
| `pgcellar_0.1.1_darwin_arm64.tar.gz` | same, macOS on Apple silicon |
| `checksums.txt` | SHA-256 of the three archives |
| `manifest.json` | what an update needs to know, below |

```json
{
  "version": "0.1.1",
  "published_at": "2026-10-28T09:12:00Z",
  "image": "ghcr.io/dbfactory-io/pgcellar:0.1.1",
  "image_digest": "sha256:…",
  "binaries": {
    "linux_amd64":  { "url": "https://github.com/dbfactory-io/pgcellar/releases/download/v0.1.1/pgcellar_0.1.1_linux_amd64.tar.gz",  "sha256": "…" },
    "linux_arm64":  { "url": "…", "sha256": "…" },
    "darwin_arm64": { "url": "…", "sha256": "…" }
  },
  "notes_url": "https://github.com/dbfactory-io/pgcellar/releases/tag/v0.1.1"
}
```

A tag with a suffix (`v0.2.0-rc.1`) gives a pre-release: same assets, marked as pre-release on GitHub, and `:latest` and `releases/latest` do not move.

Trust rests on HTTPS and on the official repository: the archives are checked against the SHA-256 in `manifest.json` and `checksums.txt`, the image is pulled by digest. Neither the manifest nor the archives are signed yet.

To see what a release would contain without publishing anything: `make release-dry` (archives and checksums in `dist/`) and `make -s release-notes VERSION=0.1.1`.

## Updating pgCellar

### Checking for updates

pgCellar looks for a new release by itself, once a day, and tells you. It does not install anything at this step: installing is done by the self-update (binary installs) or by the updater on the Docker host.

**What is sent.** One `GET` on `updates.source` (by default `https://api.github.com/repos/dbfactory-io/pgcellar/releases/latest`) with `User-Agent: pgcellar/0.1.1` and `Accept: application/vnd.github+json`, then one `GET` on the `manifest.json` of that release. Nothing else: no identifier, no number of servers, no configuration, no cookie. The `ETag` of the last answer is kept in memory and sent back as `If-None-Match`, so an unchanged release costs a `304`. A check gives up after 15 seconds.

**What is compared.** The `tag_name` of the release (without its `v`) against the running version, as semantic versions (`0.1.10` is newer than `0.1.9`, `0.2.0-rc.1` older than `0.2.0`). A development build (`pgcellar version` prints `dev`) is never told an update exists, and does not check by itself; "Check now" still shows the latest release.

**Configuration** (`pgcellar.yaml`, every key optional):

```yaml
updates:
  check: true                # look for a new release every interval; absent = true
  interval: 24h              # at least 1h; absent = 24h
  channel: stable            # only "stable" exists for now
  auto: false                # install new releases by itself, in the window (binary installs); absent = false
  window: "02:00-05:00"      # when an update may be applied, local time of the server; may cross midnight ("23:00-01:00")
  source: https://api.github.com/repos/dbfactory-io/pgcellar/releases/latest   # a mirror answering in the same format
```

`check: false` stops the daily request; nothing leaves the machine any more unless somebody presses "Check now". After a failed check (no network, GitHub rate limit) pgCellar tries again one hour later, not one day later.

**Where you see it.**

- The **Updates** page (`/updates`, any signed-in user; the version in the footer links to it): running version, latest release, when it was published, last and next check, the error of the last check if any, the update window, and how to update this instance. The **Check now** button needs the operator role and is audited as `updates.check`.
- A discreet line on the **Status** page, "pgCellar 0.1.2 is available", with a link to the Updates page, as long as a newer release is known.
- An **alert** through the configured channels (Telegram, mail, webhook), kind `update_available`, sent once per version: a restart does not repeat it, the next version is announced again. When the running version changed since the previous start, one message of kind `update_applied` ("pgCellar updated from 0.1.1 to 0.1.2") with the link to the notes.
- **Metrics**: `pgcellar_update_available` (1 when a newer release is known) next to `pgcellar_build_info{version="…"}`.

What pgCellar remembers (latest release seen, last check, last error, last version announced, last version started) is one row of the `settings` table in SQLite. Losing it costs at most one repeated message.

**API.**

| Request | Role | Answer |
|---|---|---|
| `GET /api/v1/updates` | viewer | `{"data": {"current", "latest", "available", "checked_at", "published_at", "notes_url", "manifest", "error", "check", "interval", "auto", "window", "in_window", "docker", "next_check_at"}}` |
| `POST /api/v1/updates/check` | operator | the same after a check now; `502` with both `error` and `data` when the check failed |
| `GET /api/v1/updates/ready` | operator | `200 {"ready": true, "window": "02:00-05:00"}` or `409 {"ready": false, "reason": "...", "window": "..."}` |

`/updates/ready` is what an external updater asks before it replaces pgCellar: it answers `ready` only when no backup, restore or restore drill is running and the clock of the pgCellar server is inside `updates.window`. `?now=1` skips the window, for an update started by hand; a running backup still says no. It needs an API key with the operator role, like every other request that leads to an action (create one with `pgcellar apikey create`, see [Users, roles and the API](09-users-roles-and-api.md)). Its answer is not wrapped in `data`, so that a shell script can read it with `grep '"ready":true'`.

In a container (`PGCELLAR_IN_DOCKER=1`, or the `/.dockerenv` or `/run/.containerenv` file), the Updates page shows the command to run on the Docker host instead of the self-update setting.

<!-- updater -->
