# Servers and WAL methods

A server in pgCellar is two things: a section in a Barman file (`/etc/barman.d/<name>.conf`) and, optionally, an entry in `pgcellar.yaml` for what Barman does not know (label, schedule, drill schedule, tags). The **Add a server** form writes both and commits them to git.

## The form

| Field | What it does | Barman key |
|---|---|---|
| Name | File name and section name. Lowercase letters, digits, `-`, `_`. | `[name]` |
| Label | Free text shown in pgCellar. | (pgcellar.yaml) |
| Description | Barman's description. | `description` |
| Backup method | `postgres` (streaming, no SSH) or `rsync` (over SSH). | `backup_method` |
| WAL method | `streaming`, `archive_command` or `both`. | `streaming_archiver`, `archiver` |
| Connection string (conninfo) | How Barman and pgCellar query the server. | `conninfo` |
| Streaming connection string | The replication connection. | `streaming_conninfo` |
| Replication slot | Slot Barman creates and uses. | `slot_name`, `create_slot = auto` |
| SSH command | rsync method only, for example `ssh postgres@db1`. | `ssh_command` |
| Retention | Recovery window in days, or redundancy, plus a minimum redundancy. | `retention_policy`, `minimum_redundancy` |
| Backup schedule | Schedule and on/off (chapter 5). | (pgcellar.yaml) |

Keys the form does not know are left exactly as they are, in the same order. If you add `backup_compression = gzip`, `path_prefix`, `reuse_backup = link` or anything else by hand, saving the form keeps them.

The form refuses combinations Barman would reject, with the reason in a sentence:

- `postgres` method needs `conninfo` and `streaming_conninfo`, and WAL by streaming or by `archive_command`; a slot is needed when WAL is streamed.
- `rsync` method needs `ssh_command`, `conninfo`, and `archiver = on` (so the WAL method is `archive_command` or `both`). With streaming it also needs `streaming_conninfo`.

You can also write the file by hand and check it. This is `deploy/barman.d.example/pg-main.conf`:

```
[pg-main]
description = "Main application database"

conninfo = host=pg-main.example.com user=barman dbname=postgres port=5432
streaming_conninfo = host=pg-main.example.com user=streaming_barman dbname=postgres port=5432

backup_method = postgres
streaming_archiver = on
archiver = off
slot_name = barman
create_slot = auto

retention_policy = RECOVERY WINDOW OF 14 DAYS
minimum_redundancy = 2

backup_compression = gzip
```

Run `pgcellar check --config /etc/pgcellar/pgcellar.yaml` to validate the configuration and the Barman files together (chapter 15).

## The three WAL methods

The WAL method is a choice over two Barman switches. It is not stored anywhere: it is worked out from the two keys every time the file is read.

| WAL method | `streaming_archiver` | `archiver` | Default for |
|---|---|---|---|
| `streaming` | on | off | `postgres` backups |
| `archive_command` | off | on | `rsync` backups |
| `both` | on | on | nothing; the resilient choice |

**Pick `streaming`** when pgCellar can open a replication connection to PostgreSQL. It is the simplest: no SSH, no package on the database host, almost no data lost on a crash.

**Pick `archive_command`** when a replication connection is impossible (a managed service, a firewall, an old setup), or when you use the `rsync` backup method. PostgreSQL pushes each completed WAL file over SSH to the companion container.

**Pick `both`** for a server you cannot afford to lose WAL from. If one path breaks, the other keeps the chain whole. It costs a slot, a role and an SSH key.

Notes:

- `backup_method = postgres` with `archiver = on` is valid: `pg_basebackup` takes the base backup and `archive_command` carries the WAL. No slot is needed, but `streaming_conninfo` is still required, because `pg_basebackup` uses it.
- `create_slot = auto` is added only when streaming is on.
- pgCellar runs `barman cron` every `barman.cron_interval` (one minute) to start the WAL receivers. You never run it yourself.

## The SSH companion

WAL shipping needs an SSH server in front of Barman's data. The Compose file runs one as a second container, `pgcellar-ssh`, built from the same image with another entry point. Facts:

- It runs as the `barman` user, never as root. It shares the Barman volume.
- It listens on port 2222 inside and outside (`ports: "2222:2222"`). Bind it to a private address if you can (`"10.0.0.5:2222:2222"`); do not expose it to the internet.
- Login is by key only. No password, no terminal, no forwarding, no sftp.
- Its host key is generated on first start in `~barman/.ssh/host/` in the volume and kept.
- Each authorised key is restricted to a single command, `barman put-wal <server>`. A stolen key can push WAL of one server and run nothing else.

The configuration is `deploy/sshd/sshd_config`, mounted read-only.

### Authorising a PostgreSQL server

1. On the PostgreSQL host, install `barman-cli` (it provides `barman-wal-archive`) and make an SSH key for the `postgres` user, without a passphrase:

   ```
   sudo -u postgres ssh-keygen -t ed25519 -N "" -f /var/lib/postgresql/.ssh/id_ed25519
   sudo cat /var/lib/postgresql/.ssh/id_ed25519.pub
   ```

   The home of the `postgres` user differs between systems. Use the real path.

2. In pgCellar, open **SSH key** (admin). In the section "PostgreSQL servers allowed to push WAL", choose the server, paste the public key line, and press **Authorise this key**. pgCellar appends this to `~barman/.ssh/authorized_keys`:

   ```
   restrict,command="barman put-wal pg-main" ssh-ed25519 AAAA... pgcellar:pg-main
   ```

   The comment you pasted is dropped. The same key cannot be added twice, for any server. **Remove** revokes it.

3. Let the `postgres` user trust the companion's host key. Connect once by hand and answer `yes`:

   ```
   sudo -u postgres ssh -p 2222 barman@backup.example.com true
   ```

   The command itself does not do anything useful because of the restriction; it is the host key you want recorded in `known_hosts`.

4. Tell PostgreSQL what to run. This is the command Server setup advises (chapter 3):

   ```sql
   ALTER SYSTEM SET archive_mode = 'on';          -- restart needed
   ALTER SYSTEM SET archive_command = 'barman-wal-archive --port 2222 backup.example.com pg-main %p';
   SELECT pg_reload_conf();
   ```

   The arguments are, in order: `--port` of the companion, the name by which PostgreSQL reaches the pgCellar host (`ssh_host`), the server name in Barman, and `%p`, the path of the WAL file.

5. On the server page press **Switch WAL**, then **Run check now**. The WAL archive lines of the check must turn green.

Set `ssh_host` and `ssh_port` in `pgcellar.yaml`. If you leave `ssh_host` empty, pgCellar advises its own host name, which inside Docker is a container id nobody can reach. Always set it:

```yaml
ssh_host: backup.example.com
ssh_port: 2222
```

`barman-wal-archive --test` does not work with a restricted key (the forced command ignores the option). That is a price of the restriction. Use **Switch WAL** and the check instead.

### The rsync method and the SSH key of pgCellar

The `rsync` backup method works the other way round: Barman connects to the PostgreSQL host over SSH, as the user in `ssh_command`. For that, pgCellar makes one ed25519 key pair for the barman user (`~/.ssh/id_ed25519`) the first time you open the **SSH key** page, or run an SSH test. It never replaces an existing key. The page shows the public line; put it in the `authorized_keys` of the user on the PostgreSQL host.

The private key stays in `~barman/.ssh` with mode 0600, in the volume. It is never shown, never written to the API, the audit log or git.

On a server page, **Test SSH connection** (admin) runs `ssh -o BatchMode=yes -o ConnectTimeout=10 ... true` and shows ssh's own answer. `BatchMode` means a host key unknown to `known_hosts` fails; connect once by hand to accept it.

## Editing and removing a server

**Save changes** on the server page rewrites the Barman file and the pgCellar entry, validates, and commits. The **History** page lists the commits; each one shows its patch.

Removing a server is a manual action for now: delete its file in `etc-barman.d` and its entry in `pgcellar.yaml`. Its backups stay on disk under `/var/lib/barman/<name>`; deleting them is your decision. If the server used a slot, drop it on the PostgreSQL side:

```sql
SELECT pg_drop_replication_slot('barman');
```

A forgotten slot keeps WAL forever.
