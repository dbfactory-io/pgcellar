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

<!-- updater -->
