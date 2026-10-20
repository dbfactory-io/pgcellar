# CLI reference

One binary, `pgcellar`, with a few subcommands. In the Docker image it is the entry point, so the arguments go straight after the image name or the service name:

```
docker compose run --rm pgcellar check --config /etc/pgcellar/pgcellar.yaml
docker compose exec pgcellar pgcellar apikey list --config /etc/pgcellar/pgcellar.yaml
docker run -i --rm pgcellar:dev hash-password
```

`run` starts a new throwaway container; `exec` runs in the one that is up. The default command of the image is `serve --config /etc/pgcellar/pgcellar.yaml`.

```
pgcellar serve ...
pgcellar check ...
pgcellar apikey create|list ...
pgcellar hash-password
pgcellar license keygen|issue|verify|show ...
pgcellar version
pgcellar help
```

Exit codes: `0` success, `1` the command ran and failed (or `check` found problems), `2` wrong usage (a missing or unknown argument).

## serve

Runs the web console, the API, the scheduler, the alert loop and the background refreshes.

```
pgcellar serve --config FILE [--listen ADDR] [--cron-interval DURATION] [--dev]
pgcellar serve --demo [--listen ADDR]
```

| Flag | Default | Meaning |
|---|---|---|
| `--config` | `/etc/pgcellar/pgcellar.yaml` | Path of `pgcellar.yaml`. |
| `--listen` | `listen:` of the file | Address to listen on, for example `127.0.0.1:8080` or `:8080`. Overrides the file. |
| `--cron-interval` | `barman.cron_interval` of the file | How often to run `barman cron`, for example `30s`. `0` disables it. |
| `--dev` | off | Development mode: a fake Barman, temporary data, the clock frozen at the fixtures time, login `admin` / `admin`. Works on a temporary copy of the configuration. Never use it on real data. |
| `--demo` | off | Public demonstration: fictional servers and data, read-only, login `demo` / `demo`, no configuration file. Cannot be combined with `--dev`. Links use `PGCELLAR_BASE_URL` (default `https://demo.pgcellar.dbfactory.io`). |

On start it prints the version and the address:

```
pgcellar 0.1.0 listening on http://localhost:8080
```

`SIGINT` and `SIGTERM` stop it cleanly: the HTTP server gets 10 seconds to finish its requests.

## check

Validates `pgcellar.yaml` and the Barman server files, and lists the servers.

```
pgcellar check --config FILE [--barman-dir DIR]
```

| Flag | Default | Meaning |
|---|---|---|
| `--config` | `/etc/pgcellar/pgcellar.yaml` | The configuration to validate. |
| `--barman-dir` | `barman.config_dir` of the file | Read the Barman files from this folder instead. |

Typical output:

```
configuration deploy/pgcellar.example.yaml
  ok

servers in deploy/barman.d.example: 2
  ok       pg-main              postgres         pg-main.conf
  ok       pg-legacy            rsync            pg-legacy.conf

all good
```

A problem line starts with `PROBLEM`, a warning with `WARNING` (for example `drills.enabled: false` with no drill schedule). The command lists a server that `pgcellar.yaml` mentions and `barman.d` does not. Exit code 1 if anything is a problem; warnings alone do not fail it.

If the `barman.d` folder named in the file does not exist (a copy of the example, run on a laptop), `check` falls back to a `barman.d.example` folder next to the configuration and says so.

`check` does not talk to PostgreSQL or Barman. It checks files only.

## apikey

Manages API keys. Keys live in `pgcellar.yaml` as a SHA-256 digest; a running server picks up a change by itself.

```
pgcellar apikey create --config FILE --name NAME --role viewer|operator|admin
pgcellar apikey list   --config FILE
```

`create` prints the new key (`pgc_...`) on standard output, once, and nothing else. It validates the configuration, writes the digest into `pgcellar.yaml`, and records the change in the configuration's git history under the name `cli`. `list` prints one line per key, `name<TAB>role`.

Both flags `--name` and `--role` are required for `create`.

## hash-password

Reads one line (the password) from standard input and prints its bcrypt hash, for the `users:` list.

```
read -rs PW; printf '%s\n' "$PW" | pgcellar hash-password
```

The password needs at least 8 characters; at most 1024 bytes are read. A password on the command line would end up in your shell history, so the command takes none. With Docker, give `-i` so that standard input stays open.

## license

```
pgcellar license keygen --out DIR
pgcellar license issue  --key FILE --edition free|pro|business --licensee NAME \
                        --servers N --expires YYYY-MM-DD [--issued YYYY-MM-DD] [--id ID]
pgcellar license verify KEY
pgcellar license show   --config FILE
```

`show` and `verify` are for everybody; `keygen` and `issue` are for the vendor (they need the private signing key, which is never in the repository).

| Subcommand | What it does |
|---|---|
| `show` | Reads the key from `license.key` or `license.file` in the configuration and prints state, edition, licensee, id, servers allowed, expiry and grace end, and the footer line. No key means the Free edition. |
| `verify KEY` | Checks the signature of a key against the public key built into the binary and prints its payload and state. Exit code 1 for an expired key. A bad signature prints `verdict: INVALID` with the reason. |
| `keygen --out DIR` | Makes an Ed25519 signing key pair in `DIR` (`license_signing.key`, mode 0600, and its `.pub`). Refuses to overwrite an existing private key. |
| `issue ...` | Signs a licence key. `--servers 0` means unlimited. `--id` defaults to a random `lic-xxxxxxxxxx`. `--issued` defaults to today (UTC). |

Chapter 14 explains editions and the grace period.

## version

```
$ pgcellar version
pgcellar 0.1.0
```

`--version` and `-v` do the same. The version is set at build time (`-X main.Version=...`); a plain `go build` prints `dev`. The Docker image takes it from the build argument `VERSION`.

## help

`pgcellar help`, `--help` and `-h` print the usage text and exit with 0. A missing or unknown subcommand prints it to standard error and exits with 2.

## Build from source

```
make build            # ./pgcellar
make test             # go test -race ./...
make lint             # gofmt, go vet, golangci-lint
make run              # the --dev mode, on the example configuration
make demo             # the --demo mode on 127.0.0.1:8080
make docker           # build the image as pgcellar:dev
make release-dry      # linux amd64/arm64 and macOS arm64 archives with checksums in dist/
make docs             # this manual, as HTML, in site/docs/
make docs-pdf         # this manual, as site/docs/pgcellar-manual.pdf
```

Go 1.25 or later. Barman is not needed to run the tests: a shell script that mimics its command line stands in for it.
