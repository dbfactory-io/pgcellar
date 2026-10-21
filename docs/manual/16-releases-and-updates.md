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

### Automatic updates (Docker)

In a container, pgCellar cannot update itself, and that is on purpose. Replacing an image means talking to the Docker daemon, and a container that holds the Docker socket can start a privileged container and take over the host: it is root on the host. pgCellar keeps backup credentials and SSH keys to production databases; it must not also be root on the machine that runs it. So the container has no socket, and the update is done by a small script on the Docker host, `pgcellar-updater` (`deploy/updater/`), started by a systemd timer. pgCellar only answers two questions through its API: is there a new release, and is now a good time.

**What a run does.** Every 15 minutes (plus up to 2 minutes of random delay):

1. `GET /api/v1/updates`. Nothing newer than the running version: "up to date", exit 0.
2. `GET /api/v1/updates/ready`. `409`: the reason is logged ("outside the update window", "a backup of erp-prod is running") and the run ends with exit 0; the next run asks again. The timer never passes `?now=1`: when to update is decided by `updates.window` in `pgcellar.yaml` and by the jobs pgCellar runs, not by the timer.
3. The target is `latest`; the image is `manifest.image` of that release. Anything other than `ghcr.io/dbfactory-io/pgcellar:<that version>` is refused (another registry, a look-alike name, a digest-only reference); the repository is written in the script, not in its configuration.
4. `docker pull`, then the digest the registry serves for that tag (`docker buildx imagetools inspect`, or the repository digests recorded by the pull when buildx is missing) is compared with `manifest.image_digest`. A difference stops the run with exit 1; nothing was changed.
5. The current `PGCELLAR_TAG` is saved in `.pgcellar-updater.prev` next to the compose file, the new tag is written into the compose `.env` file (one line replaced, the rest of the file kept, written to a temporary file and renamed), then `docker compose up -d pgcellar pgcellar-ssh`.
6. It waits up to `HEALTH_TIMEOUT` (180 s) for `/healthz` to answer `200` and for `/api/v1/updates` to report the new version as `current`. Then: "updated from 0.1.1 to 0.1.2", exit 0, and pgCellar itself sends its `update_applied` alert.
7. Otherwise it rolls back: the previous tag goes back into `.env`, `docker compose up -d` again, wait for `/healthz`, "rolled back", exit 2, so the unit is marked failed. If even the previous version does not come back, exit 3 with what to look at.

Two runs never overlap (`flock` on `/run/lock/pgcellar-updater.lock`). The API key is read from a file and handed to `curl` on its standard input: it appears neither in the journal nor in the process list. Everything goes to the journal: `journalctl -u pgcellar-updater`.

**Pin the version.** The compose file says `image: ghcr.io/dbfactory-io/pgcellar:${PGCELLAR_TAG:-latest}` and `deploy/.env.example` sets `PGCELLAR_TAG=0.1.0`. With a pinned tag a restart never brings a new version by surprise and the updater knows exactly where to come back. Without one (`latest`), the updater uses the running version as the way back and pins the tag from then on.

**Install.** On the Docker host, from a checkout of the repository:

```
cd pgcellar/deploy/updater
sudo ./install.sh                # script, units, /etc/pgcellar/updater.env; enables nothing
sudo nano /etc/pgcellar/updater.env
```

| Setting | Default | Meaning |
|---|---|---|
| `COMPOSE_DIR` | (required) | directory of the `docker-compose.yml` that runs pgCellar |
| `SERVICE` | `pgcellar` | services to recreate; the example says `"pgcellar pgcellar-ssh"`, both use the image |
| `ENV_FILE` | `$COMPOSE_DIR/.env` | file compose reads its variables from |
| `TAG_VAR` | `PGCELLAR_TAG` | variable that holds the image tag |
| `API_URL` | `http://127.0.0.1:8080` | where pgCellar answers, seen from the host |
| `API_KEY_FILE` | `/etc/pgcellar/updater.key` | one line, an API key with the operator role, mode 0600 |
| `HEALTH_TIMEOUT` | `180` | seconds before a rollback |
| `DRY_RUN` | `0` | `1` = say what would be done, change nothing |
| `DOCKER` | `docker` | `podman` works too when `podman compose` does |

Then the key, the window, a dry run, and the timer:

```
cd /srv/pgcellar                 # COMPOSE_DIR
sudo install -m 600 /dev/null /etc/pgcellar/updater.key
docker compose exec -T pgcellar pgcellar apikey create --config /etc/pgcellar/pgcellar.yaml \
  --name updater --role operator | sudo tee /etc/pgcellar/updater.key >/dev/null
sudo pgcellar-updater --dry-run
sudo systemctl enable --now pgcellar-updater.timer
```

Set `updates.window` (above) to a quiet slot, away from the nightly backups and drills. `updates.auto` is about binary installs; on Docker the timer is the opt-in. `updates.check: false` also stops automatic Docker updates: the updater relies on what pgCellar's own check found.

The service runs as root. The updater runs `docker compose` and rewrites the `.env` file, which usually belongs to root and holds secrets; membership of the `docker` group is worth root anyway, so a dedicated user in that group would not be safer. To run it as another user all the same, add a drop-in with `User=` and `Group=docker`, and give that user the compose directory, the `.env` file and the key file. With rootless Podman, install the units as user units of the account that owns the containers and set `DOCKER=podman`.

The script needs `bash`, `curl` 7.76 or later, `jq` and `flock` (util-linux) on the host.

**Update now, by hand.** `sudo pgcellar-updater --now` skips `updates.window` (it sends `?now=1`); pgCellar still says no while a backup, a restore or a drill runs. The Updates page of a pgCellar in a container shows this command.

**Disable.** `sudo systemctl disable --now pgcellar-updater.timer`. The tag in `.env` stays where it is.

**Without the updater.** The same steps by hand:

```
cd /srv/pgcellar
docker pull ghcr.io/dbfactory-io/pgcellar:0.1.2
docker buildx imagetools inspect ghcr.io/dbfactory-io/pgcellar:0.1.2 | head -3   # Digest = image_digest of manifest.json
sed -i 's/^PGCELLAR_TAG=.*/PGCELLAR_TAG=0.1.2/' .env
docker compose up -d
```

To go back, put the previous tag in `.env` (the updater left it in `.pgcellar-updater.prev`) and run `docker compose up -d` again. The SQLite migrations of a newer version are not undone by going back; back up `/var/lib/pgcellar` before a jump of several versions ([Operations](11-operations.md)).
### Automatic updates (binary install)

A pgCellar installed as a binary under systemd can install new releases by itself. It is off unless you ask for it, and it never runs in a container (the image is updated from the host, see Automatic updates (Docker) above).

**Prerequisites.**

- `updates.auto: true` in `pgcellar.yaml` (the update check must be on too: `updates.check` absent or `true`).
- A release build: `pgcellar version` prints a version number, not `dev`.
- Not in a container: `PGCELLAR_IN_DOCKER=1`, `/.dockerenv` or `/run/.containerenv` turns it off.
- The binary and its directory are writable by the user of the service, because the new binary is written next to the old one and renamed over it. The unit in `deploy/systemd/pgcellar.service` runs `/opt/pgcellar/bin/pgcellar` as `barman` (the only user Barman agrees to run as) and lists `/opt/pgcellar/bin` in `ReadWritePaths`:

  ```sh
  install -d -o barman -g barman -m 0755 /opt/pgcellar/bin
  install -o barman -g barman -m 0755 pgcellar /opt/pgcellar/bin/pgcellar
  ```

- `Restart=on-failure` in the unit: a new version that crashes is started again by systemd, and those starts are what triggers the rollback.

When one of these is missing, nothing happens and the Updates page says why next to "Automatic updates" (the same text is `self_update.refused_reason` in `GET /api/v1/updates`).

**What happens.** Every 10 minutes pgCellar looks at the latest release found by the daily check. It goes on only when that release is newer than the running version, has a binary for this platform (`linux_amd64`, `linux_arm64`, `darwin_arm64`), never failed here before, the clock is inside `updates.window`, and no backup, restore or restore drill is running. Then:

1. It downloads the archive named in `manifest.json` (HTTPS only, 200 MB at most) into a temporary file in the directory of the binary, and compares its SHA-256 with the manifest.
2. It takes the `pgcellar` file out of the archive (an archive with an absolute path or a `..` in any entry is refused) and makes it executable.
3. It runs `<new binary> version` (10 seconds at most) and checks that it prints the expected version.
4. It writes `pgcellar.update.json` next to the binary (`from`, `to`, `started_at`, `attempts`), renames the running binary to `pgcellar.prev`, renames the new one to `pgcellar`, and replaces its own process with the new binary (same PID, same arguments, systemd sees no restart).
5. The new version counts its start in `pgcellar.update.json`. After 120 seconds during which `/healthz` answers on the local address, it removes the file: the update is confirmed. `pgcellar.prev` stays, for a return by hand.

Any failure before step 4 leaves the running binary untouched and removes what was downloaded; the error is shown on the Updates page and the next attempt comes 10 minutes later. If the process cannot be replaced at step 4, the old binary is put back and pgCellar goes on with it.

**Rollback.** A new version that crashes before it is confirmed is started again by systemd (`Restart=on-failure`, 5 seconds later). At its third start pgCellar gives up on it: `pgcellar.prev` is renamed back to `pgcellar`, the version is written to `pgcellar.update.failed.json`, and the old binary is started in place of the new one. A version listed in that file is never installed automatically again; the Updates page lists it. Delete the file to allow it again.

**Turning it off.** Remove `updates.auto` from `pgcellar.yaml` or set it to `false`. The change is read at the next check, no restart needed.

**Going back by hand.** The previous binary is kept as `pgcellar.prev` next to the binary:

```sh
cd /opt/pgcellar/bin
mv pgcellar.prev pgcellar && systemctl restart pgcellar
```

Then, to keep it from coming back at the next window, either set `updates.auto: false` or add the version to `pgcellar.update.failed.json`.
