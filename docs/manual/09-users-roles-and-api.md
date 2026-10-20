# Users, roles, API keys and the API

People sign in to the console with a user. Programs call the API with an API key. Both have one of three roles.

## Roles

| Role | Can do |
|---|---|
| **viewer** | Read everything except secrets and the audit log: Status, Servers, Timeline, Server setup, Runs, History, restores and drills lists, the license. |
| **operator** | Everything a viewer can, plus: run a check, backup now, switch WAL, start a restore, download a restore archive, start a drill, download a drill report. |
| **admin** | Everything an operator can, plus: add and edit servers, delete a backup, delete a restore, test SSH, manage SSH keys, read the Alerts page and send test messages, read the audit log. |

The audit log is admin-only because it contains the exact command lines that were run. A restore archive is a full copy of the database, so downloading it needs the operator role.

At least one user must have the admin role, or pgCellar refuses to load the file.

## Users

Users live in `pgcellar.yaml`. There is no user screen: you edit the file, and pgCellar picks the change up.

```yaml
users:
  - name: admin
    role: admin
    password_hash: "$2a$10$..."
  - name: alice
    role: operator
    password_hash: "$2a$10$..."
  - name: bob
    role: viewer
    password_hash: "$2a$10$..."
```

Create a hash with:

```
read -rs PW; printf '%s\n' "$PW" | docker compose run --rm -T pgcellar hash-password
```

Minimum 8 characters. The hash is bcrypt; the password itself is never stored. Paste the hash into `password_hash`.

To remove a user, delete the entry; their access ends at the next request, because the role is read from the file on every request.

## Sessions and sign-in

The console uses a cookie session valid for 24 hours. The login form is protected against password guessing (chapter 12). Each state-changing request carries a CSRF token. Log out with **Log out** in the top bar.

![Sign in](images/login.png)

## API keys

An API key is a long random string starting with `pgc_`. pgCellar stores only its SHA-256, in `pgcellar.yaml`. The key itself is shown once, when you create it.

```
docker compose exec pgcellar pgcellar apikey create --config /etc/pgcellar/pgcellar.yaml --name grafana --role viewer
```

The command prints the key and nothing else. Save it at once; there is no way to read it again. It writes the hash into `pgcellar.yaml` and commits the change to the configuration history, and the running server picks it up by itself. List the names and roles:

```
docker compose exec pgcellar pgcellar apikey list --config /etc/pgcellar/pgcellar.yaml
```

To revoke a key, delete its entry in `api_keys:`.

Give each program its own key and the smallest role that works: a monitoring tool needs `viewer`, a script that triggers backups needs `operator`.

## Calling the API

The base path is `/api/v1`. Authenticate with a header:

```
curl -H "Authorization: Bearer pgc_..." http://localhost:8080/api/v1/status
```

Every answer is JSON, either `{"data": ...}` or `{"error": "..."}`. A request body is JSON, limited to 64 KB, and unknown fields are refused.

| Code | Meaning |
|---|---|
| 200, 201, 202, 204 | Done; 202 means started in the background, follow `run_id` |
| 401 | Missing or invalid key (`WWW-Authenticate: Bearer`) |
| 403 | The key's role is too low |
| 404 | No such server, run, backup or restore |
| 409 | Conflict: another job is running, Barman refused a delete, the archive does not exist |
| 413 | The archive is larger than `recoveries.max_download_bytes` |
| 422 | The input is invalid; `problems` lists each reason |
| 502 | Barman or the command failed; its message is in `error` |
| 503 | The first status refresh is not done yet; retry after the `Retry-After` delay |

Reading never runs Barman. `GET` routes answer from the status cache (refreshed every `status_interval`) and from Barman's files. Only an explicit `POST` calls Barman.

### The main routes

```
GET    /api/v1/status                           cached state of every server
GET    /api/v1/servers                          list
POST   /api/v1/servers                          create (admin)
GET    /api/v1/servers/{name}                   one server with its state
PUT    /api/v1/servers/{name}                   update (admin)
GET    /api/v1/servers/{name}/setup             Server setup checks
GET    /api/v1/servers/{name}/timeline          what you can restore to
POST   /api/v1/servers/{name}/check             run barman check now (operator)
POST   /api/v1/servers/{name}/backup            run a backup now (operator)
POST   /api/v1/servers/{name}/switch-wal        force a WAL switch (operator)
GET    /api/v1/servers/{name}/backups           list backups
DELETE /api/v1/servers/{name}/backups/{id}      delete a backup (admin)
POST   /api/v1/servers/{name}/recoveries        restore to a directory (operator)
GET    /api/v1/servers/{name}/recoveries        restores of a server
GET    /api/v1/recoveries/{id}                  one restore
GET    /api/v1/recoveries/{id}/download         the .tar.gz (operator)
DELETE /api/v1/recoveries/{id}                  delete a restore (admin)
POST   /api/v1/servers/{name}/drills            run a drill now (operator)
GET    /api/v1/servers/{name}/drills            drills of a server
GET    /api/v1/drills                           drills of all servers (?server=, ?limit=)
GET    /api/v1/drills/{id}                      one drill with its steps
GET    /api/v1/drills/{id}/report               the PDF report (operator)
GET    /api/v1/runs                             recent runs (?server=, ?limit=)
GET    /api/v1/runs/{id}                        one run with its output
GET    /api/v1/history                          git history of the configuration
GET    /api/v1/audit                            audit log (admin)
GET    /api/v1/alerts                           channels and journal (admin)
POST   /api/v1/alerts/test                      send a test message (admin)
GET    /api/v1/license                          edition, servers allowed and used
GET    /api/v1/ssh/authorized-keys              keys allowed to push WAL (admin)
POST   /api/v1/ssh/authorized-keys              authorise a key (admin)
DELETE /api/v1/ssh/authorized-keys              revoke a key (admin)
```

The complete description, with parameters, bodies, schemas and the role each route needs, is served by pgCellar itself, as OpenAPI 3:

```
GET /api/v1/openapi.json
```

That route needs no key. Load it in Swagger UI, Postman or your generator of choice. It is always the description of the version you are running.

### Examples

Is everything fine?

```
curl -s -H "Authorization: Bearer $KEY" http://localhost:8080/api/v1/status | jq '.data.servers[] | {name, level}'
```

Back up now and follow the run:

```
RUN=$(curl -s -X POST -H "Authorization: Bearer $KEY" http://localhost:8080/api/v1/servers/pg-main/backup | jq -r .data.run_id)
curl -s -H "Authorization: Bearer $KEY" http://localhost:8080/api/v1/runs/$RUN | jq '.data | {status, exit_code}'
```

Create a server:

```
curl -s -X POST -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  http://localhost:8080/api/v1/servers -d '{
    "name": "pg-main",
    "backup_method": "postgres",
    "wal_method": "streaming",
    "conninfo": "host=pg-main.example.com user=barman dbname=postgres",
    "streaming_conninfo": "host=pg-main.example.com user=streaming_barman dbname=postgres",
    "slot_name": "barman"
  }'
```

Whatever the console does, the API does: both are thin layers over the same code. An automation tool can configure and operate pgCellar without a browser.
