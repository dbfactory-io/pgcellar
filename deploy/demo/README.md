# pgCellar public demo

An instance of pgCellar with fictional data, read-only, that you can put on the
internet and send to prospects: `pgcellar serve --demo`.

It is **not** a copy of a real installation. Everything it shows is invented
(a company called Northwind Logistics with five PostgreSQL servers), the Barman,
psql and pg_ctl it talks to are small scripts embedded in the binary, and it
holds no secret, no real key and no connection to any real server.

## Run it on your machine

```
make demo                       # builds, then listens on http://127.0.0.1:8080
# or
./pgcellar serve --demo --listen 127.0.0.1:8080
```

Open the address and press **Try the demo** (user `demo`, password `demo`). It
needs `bash` and `git` on the machine (the Docker image has both). Nothing is
written outside a temporary directory, removed when the program stops.

`PGCELLAR_BASE_URL` sets the address used in links (default
`https://demo.pgcellar.dbfactory.io`). There is no configuration file.

## What the visitor can and cannot do

- One user, `demo`, role viewer, no API key. The pages of the administrators
  (SSH key, Alerts, Audit) are readable by that user **in the demo only**: they
  show fictional data.
- Every request that would change something (any POST, PUT, PATCH or DELETE
  except sign-in and sign-out, and the two GET routes that run a command or hand
  out an archive) is answered `403` with "This is the public demo: read-only.
  Run your own pgCellar: https://dbfactory.io/pgcellar". The buttons stay on the
  page, disabled, with a tooltip, so the visitor sees what exists.
- The PDF report of a restore drill can be downloaded (fictional); the
  restored archives cannot.
- 10 sign-in attempts a minute and per address (also in the normal mode).
- Every answer carries `X-Robots-Tag: noindex, nofollow`.
- Nothing is sent: the alert loop does not run, the channels are only displayed.

## The data

Five servers, all dates relative to the moment the instance started, then
kept moving (new nightly backups appear, old ones expire):

| server | PostgreSQL | size | story |
|---|---|---|---|
| `erp-prod` | 16 | 48 GiB | healthy, streaming and `archive_command`, 14 daily backups, drill passed last Sunday |
| `crm-prod` | 17 | 12 GiB | healthy, one setup point to review (`max_slot_wal_keep_size`) |
| `analytics` | 18 | 210 GiB | attention: last backup 31 h old, WAL streaming live |
| `shop-staging` | 16 | 3 GiB | failing: streaming stopped two days ago, last drill failed at "Start PostgreSQL" |
| `legacy-9` | 15 | 6 GiB | schedule off, one old backup, `archive_timeout` note |

Business edition licence "Northwind Logistics", signed by a throw-away key made
at start-up (the private half never leaves memory).

## Put it on the internet

```
docker compose up -d --build           # from this directory
```

The tmpfs on `/tmp` must allow exec (`exec` option): the fake programs are extracted there. The demo refuses to start, and never runs a program outside that directory, otherwise.

Then nginx in front of it, with HTTPS, as in `nginx-vhost.conf`. The compose
file runs the same image as the product with a read-only root file system, a
tmpfs for the temporary directory, 256 MB of memory, no capability, no volume.
Restart the container whenever you like: the data is rebuilt at each start.
