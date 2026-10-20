# Security model

pgCellar holds the keys to your backups, and a backup is a complete copy of your data. This chapter says what it does to protect them, and what you must do around it.

## Principles

- **Least surprise.** pgCellar never writes to a production PostgreSQL server. It reads `pg_settings`, and Barman runs the backup. It tells you what to change; you change it.
- **One door to commands.** Every command pgCellar runs on a server goes through one function that logs the exact command, its exit code and its duration to the audit log. There is no second code path, not even for `psql`.
- **No secret in files that travel.** Not in `pgcellar.yaml`, not in git, not in the API.
- **Small surface.** Server-rendered HTML, a few kilobytes of vendored JavaScript for HTMX, no framework, no build step. A system administrator can read the code that matters.

## Roles

Three roles: viewer, operator, admin (chapter 9). The check happens on the server, on every route, for the console and the API. The buttons you do not have are simply absent.

The audit log is admin-only because it contains exact command lines. Downloading a restore archive is operator-only: it is a full copy of the database.

## Passwords and sessions

- Passwords are stored as bcrypt hashes in `pgcellar.yaml`. Minimum length at hash time: 8 characters.
- After sign-in the browser holds a random token in a cookie. The database stores only the SHA-256 of the token, so a copy of the database cannot be replayed as a session.
- The cookie is `HttpOnly` and `SameSite=Lax`, with `Secure` set when the request arrives over HTTPS. A session lasts 24 hours.
- Every state-changing request carries a CSRF token, derived with HMAC from the session id and a secret kept in the database. The sign-in form uses a stateless signed token valid for an hour.
- HTML pages are sent with `Cache-Control: no-store`, `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin` and a Content Security Policy that allows only the site itself (`default-src 'self'`, `form-action 'self'`, `frame-ancestors 'none'`).
- Static files are versioned by content (`app.css?v=<hash>`) and served `immutable`; the HTML is never cached. After an update, a browser never mixes new HTML with old CSS.

## Sign-in limiter

Guessing passwords is slowed down in two ways, for every mode:

- At most **10 sign-in attempts a minute** per address, successful or not. Above that: HTTP 429 with `Retry-After`.
- At most **8 failures in 10 minutes** per address.

The address is the connection's. If pgCellar runs behind a reverse proxy, the proxy's address would be the same for everybody, so pgCellar believes the `X-Real-IP` header **only** when the connection comes from a loopback or private address (the proxy). A stranger on the internet cannot choose his own address. Make sure your proxy sets `X-Real-IP` and does not pass the client's own value through.

## API keys

An API key is 32 random bytes, shown once. Only the SHA-256 is stored. The comparison is against the hash. Keys carry a role, are revoked by deleting their entry, and every call made with one is audited under the key's name.

## Secrets: where they live

| Secret | Lives in | Never in |
|---|---|---|
| PostgreSQL passwords | `~/.pgpass` of the barman user (mode 0600) | `pgcellar.yaml`, git, a command line |
| SSH private key | `~barman/.ssh/` (mode 0600) | The database, git, the API, the audit log |
| Alert tokens, SMTP login, webhook secret | Environment variables of the container, named by `*_env` | `pgcellar.yaml`, git |
| Password hashes, API key hashes | `pgcellar.yaml` | (only hashes, never the secret) |
| Session tokens | Browser cookie; SHA-256 in the database | |
| Licence key | `pgcellar.yaml` or a file | (it is not a secret key; it is signed and verified offline) |

A `password=` in a connection string is removed before `psql` runs, so it cannot show up in `ps`. The Telegram token is removed from error texts. A secret printed once (a new API key) is printed to your terminal and nowhere else.

## SSH, restricted

The SSH companion that receives WAL files:

- runs as the `barman` user, not as root; `PermitRootLogin no`, `AllowUsers barman`, `UsePAM no`;
- accepts keys only (no password, no keyboard-interactive);
- gives no terminal, no X11, no TCP or agent forwarding, no sftp;
- writes each authorised key with `restrict,command="barman put-wal <server>"`, so a stolen key of a database host pushes WAL files of its own server and can do nothing else;
- keeps its host key in the volume.

Even so: do not publish port 2222 on the internet. Bind it to a private address, or a VPN.

For the rsync method, pgCellar's own key pair lets the barman user log into your PostgreSQL hosts. Give it the account you would give Barman by hand, nothing more.

## Container

- The container runs as the `barman` user, uid 997 in the current image. There is no root process, no `sudo` binary and no SSH daemon in the application container.
- Bind-mounted folders must be owned by that uid. A volume created by an older root container must be fixed with `chown` (chapter 13).
- Only port 8080 (and 2222 on the companion) are exposed. You can go further with `cap_drop: [ALL]`, `security_opt: [no-new-privileges:true]`, memory and process limits; the demo's Compose file in `deploy/demo/` does exactly that.

## Network

pgCellar speaks plain HTTP. Put it behind a reverse proxy that terminates TLS, and:

- restrict who can reach it: a VPN, an allow-list, or single sign-on in the proxy, in addition to pgCellar's own sign-in;
- block `/metrics` and `/healthz` from the internet. They need no authentication (they expose server names and ages, nothing more);
- let the proxy set `X-Real-IP`, as above, and pass `X-Forwarded-Proto` so the cookie gets the `Secure` flag.

A sketch for nginx:

```
location / {
    proxy_pass         http://127.0.0.1:8080;
    proxy_set_header   Host $host;
    proxy_set_header   X-Real-IP $remote_addr;
    proxy_set_header   X-Forwarded-Proto $scheme;
}
location = /metrics { allow 10.0.0.0/8; deny all; proxy_pass http://127.0.0.1:8080; }
```

## Demo mode

`pgcellar serve --demo` is a public, read-only instance on fictional data (Northwind Logistics, five servers). It exists so that a visitor can click through the product without being one bug away from real backups.

- No configuration file is read. The Barman, `psql`, `pg_ctl` and `ssh` it runs are fakes embedded in the binary, extracted to a temporary directory at start. If they cannot run, pgCellar refuses to start rather than call the real ones.
- Every `POST`, `PUT`, `PATCH` and `DELETE`, except sign-in and sign-out, answers 403 before routing, before authentication and before CSRF. So do the two `GET` routes that start a command or hand out an archive. A test requests every mutating route registered in the code, so a new route cannot be forgotten.
- The user `demo` has the viewer role in effect and no API key. The scheduler and the alert loop are off. Nothing is sent anywhere.
- Disabled buttons in the page are cosmetic. The server is what refuses.
- Never put real data in a demo instance. Run it as a separate container, read-only, with no volume of your own (`deploy/demo/`).

## Your side of the bargain

- Use a strong admin password, and an `operator` or `viewer` account for daily work.
- Keep the `pgcellar-data` and `barman-data` volumes, and the backups, on encrypted storage if your data needs it. pgCellar does not encrypt backups.
- Copy the backups to a second place that a compromise of this one cannot reach.
- Restore drills prove that backups restore. They do not protect against someone deleting them. Limit who is `admin`.
