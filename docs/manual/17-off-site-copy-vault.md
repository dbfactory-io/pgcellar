# Off-site copy (Vault)

A backup on the same machine, or in the same room, dies with it: fire, ransomware, a deleted volume, a cloud account closed. The Vault is a second copy of every backup, somewhere else, encrypted before it leaves.

After each successful backup, and every 15 minutes for the WAL files, pgCellar sends the Barman directory of each server to an S3 bucket with [restic](https://restic.net). Any S3-compatible storage works. This chapter walks through OVHcloud Object Storage, the storage dbfactory sells in prepaid packs.

## What is copied, and how

- **One restic repository per server**, at `s3:<endpoint>/<bucket>/<prefix>/<server>`. Inside it, restic keeps snapshots of `<barman_home>/<server>` (base backups, WAL files, Barman's catalogue) and of the server's file in `barman.d`.
- **Encrypted on this host.** restic encrypts everything with a password that pgCellar generates the first time it needs it, in `<data_dir>/vault/restic-password` (mode 0600). The storage provider, dbfactory included, only ever sees encrypted blocks. One password for the whole pgCellar, so one recovery key to keep, valid for the servers you add later too.
- **Deduplicated.** Each copy only sends what is new. The first copy of a server sends everything; the next ones send the new backup, the new WAL files and a few KB of metadata.
- **When.** Right after each successful backup (scheduled or **Backup now**), and every `vault.wal_interval` (15 minutes) for the WAL files. **Copy now** on the Vault page or the server page queues one at once.
- **One at a time.** Copies run in the background, one after the other, through the same audited runner as Barman. A copy waits while a backup, a restore or a drill of the same server runs. It never blocks a backup: if the bucket is unreachable, the backups go on and the copy is tried again.
- **Retention.** Once a day pgCellar runs `restic forget --prune` with `vault.keep` (7 daily and 4 weekly snapshots by default). Barman's own retention keeps applying on this host; the Vault keeps its own history.

What is not copied: `pgcellar.yaml`, the SQLite database, the SSH keys. Keep `pgcellar.yaml` in its git repository (pushed somewhere) and the recovery key in a safe place.

## The recovery key

> **Lose the key, lose the copies.** The copies are encrypted with the restic password of this pgCellar. Without it nobody can read them: not you, not dbfactory, not the storage provider. dbfactory cannot recover it for you.

On the Vault page, an administrator clicks **Download recovery key** (or `GET /api/v1/vault/recovery-key`). The text file holds the password, the repositories, and the commands to read a copy without pgCellar. Every download is audited (`vault.key.download`), and the page says when it was last downloaded and by whom. Do it once, right after the first save, and keep the file offline: password manager, safe, printed copy.

To give the copies to a new pgCellar (the old machine is gone), write the password, alone on one line, into `vault.password_file` (default `<data_dir>/vault/restic-password`, mode 0600, owned by the barman user) **before the first copy**, then configure the same bucket and folder. A wrong password makes every copy fail with "the restic password does not open this repository": pgCellar never replaces an existing password file.

## Setting up OVHcloud Object Storage

The names below are those of the OVHcloud Control Panel in October 2026; check the [OVHcloud Object Storage documentation](https://docs.ovhcloud.com/en/guides/storage-and-backup/object-storage/s3-managing-object-lock.md) if a screen moved.

1. **A Public Cloud project.** In the Control Panel, *Public Cloud*, create a project (or use one). Object Storage is billed to it.
2. **An S3 user.** *Object Storage* > *S3 users* > *Create a user*. Copy its **access key** and **secret key** (the secret is shown once). Give it read and write rights on the bucket only (the *Read/Write* policy of the container is enough).
3. **The bucket, with Object Lock.** *Object Storage* > *Create an object container*. Choose **Standard object storage (S3 API)**, the region (for example *Gravelines*, `gra`), and in the dedicated step, **enable Object Lock**. Object Lock can only be switched on when the bucket is created, never later; it turns versioning on by itself. Name the bucket, for example `acme-pgcellar`.
4. **The default retention, in governance mode.** In the container, *Object Lock*: mode **Governance**, for example 30 days. Or with the AWS CLI:

   ```bash
   aws s3api create-bucket --bucket acme-pgcellar --object-lock-enabled-for-bucket \
     --endpoint-url https://s3.gra.io.cloud.ovh.net --region gra
   aws s3api put-object-lock-configuration --bucket acme-pgcellar \
     --endpoint-url https://s3.gra.io.cloud.ovh.net --region gra \
     --object-lock-configuration '{"ObjectLockEnabled":"Enabled","Rule":{"DefaultRetention":{"Mode":"GOVERNANCE","Days":30}}}'
   ```

5. **The endpoint** is `https://s3.<region>.io.cloud.ovh.net`: `https://s3.gra.io.cloud.ovh.net` for Gravelines, `s3.sbg` for Strasbourg, `s3.rbx` for Roubaix, `s3.de` for Frankfurt. Use the `.io.cloud.ovh.net` endpoint, not the older `.perf.cloud.ovh.net` one.
6. **In pgCellar**, page **Vault** (administrators): tick *Copy the backups to this bucket*, fill in the endpoint, the region (`gra`), the bucket, the access key and the secret key, the storage you bought (`500GB`), **Save**, then **Test connection**, then **Download recovery key**.

The first copies start within a minute (the WAL pass), or after the next backup.

### Why Object Lock, and what it costs

With a default retention, every object restic writes can neither be deleted nor overwritten before its time, not even with your keys. Someone who takes over this machine can delete the local backups and run `restic forget`, but the versions in the bucket stay readable for the retention period. Governance mode (not compliance) lets an account with the `s3:BypassGovernanceRetention` right remove them early, should you ever have to.

pgCellar does not set the lock itself: it reads the rule of the bucket and shows it ("Locked for 30 days (governance)" or "Not locked") on the Vault page, in the test result and on the Status line.

**Deleted versions are still billed during the retention.** When restic prunes, the bucket keeps the old versions until their retention ends, and the provider bills them. pgCellar counts them: the Vault page shows how much of the storage is "kept by versioning or Object Lock", and the quota uses the total. A retention of 30 days with `keep: daily 7, weekly 4` keeps about a month of extra data; size the pack accordingly, or shorten the retention.

## Quota

`vault.quota` is the storage you bought (`100GB`, `500GB`, `2TB`; GB and TB are powers of 1000, as storage is sold). After each copy after a backup, and every 6 hours otherwise, pgCellar lists the objects of each server's folder, older versions included, and adds them up. When the S3 user may not list versions, it counts the current objects only; when it may not list at all, it asks restic (`restic stats --mode raw-data`).

The Vault page shows a gauge; the Status page shows the use in its Vault line. At **80 %** one alert is sent (`vault_quota_warning`), at **100 %** another (`vault_quota_exceeded`, repeated every `alerts.repeat_every` while it lasts). **pgCellar never stops a backup nor a copy because of the quota**, like it never does because of a licence: you get warned, the copies go on, and you buy a bigger pack or keep fewer snapshots.

## Status and alerts

- **Per server**: a card *Off-site copy* on the server page (last copy, last good copy, what it takes in the bucket, the repository, the five latest copies, **Copy now**), an emoji in the server list (✅ fresh, ⚠️ last copy failed, ❌ late or failing, ⏳ no copy yet, 💤 not copied), a line on the Status page.
- **Orange** when no copy succeeded for `vault.max_age` (36 hours by default), or after 3 failed copies in a row. A server that never had a copy is not orange until its first attempt is older than `vault.max_age`.
- **Alerts**: `vault_copy_failed` once per run of failures: at the first failure of a copy that follows a backup or a click, at the second failure in a row of the periodic WAL pass (a network hiccup heals by itself). Late copies are announced as a level change of the server.
- **Metrics**: `pgcellar_vault_last_copy_ok{server}` (1/0), `pgcellar_vault_last_copy_age_seconds{server}` (age of the last good copy), `pgcellar_vault_bytes{server}` (what the server takes in the bucket).
- **API**: `GET /api/v1/vault`, `PUT /api/v1/vault`, `POST /api/v1/vault/test`, `POST /api/v1/vault/copy` (`{"server": "pg-main"}`), `GET /api/v1/vault/copies`, `GET /api/v1/vault/copies/{id}`, `GET /api/v1/vault/recovery-key`. Details in `/api/v1/openapi.json`.

Every restic command is in the audit log (the background WAL passes only when they fail), never the S3 secret key: restic gets it through its environment.

## Restore drill from the Vault

A copy you never restored is a hope. With `drills.source: vault` (Vault page, *Scheduled restore drills restore from*), the scheduled drills restore from the off-site copy instead of the local Barman directory. **Drill from the off-site copy** on the server page does it once, whatever the setting; through the API, `POST /api/v1/servers/{name}/drills` with `{"source": "vault"}`.

Before the usual steps (chapter 7), the drill:

1. finds the latest restic snapshot of the server;
2. restores the backup catalogue only, and picks the latest complete backup (or the one asked for);
3. restores that backup, its parents (incremental backups) and the WAL files into `<recoveries.dir>/<server>/vault-<timestamp>/`, never the other backups;
4. writes a Barman configuration that reads that copy (the paths that point to the live directories and the hooks are left out) and runs `barman rebuild-xlogdb` on it;
5. then runs `barman -c <that configuration> recover` and the rest of the drill as always.

The fetched copy is removed at the end. The PDF report says "restored from the off-site copy", with the restic snapshot and its date. A drill from the Vault never runs while a copy of the same server is in progress.

It downloads one full backup plus its WAL: allow for the bandwidth and the disk under `recoveries.dir`, and raise `drills.timeout` (30 minutes by default) for large servers.

## Settings

```yaml
vault:
  enabled: true
  endpoint: https://s3.gra.io.cloud.ovh.net
  region: gra
  bucket: acme-pgcellar
  # prefix: pgcellar                    # folder in the bucket, one per pgCellar
  access_key: 5f2a9c4e7b1d4e0f8a3c6b9d2e7f1a4c
  # secret_key_file: /var/lib/pgcellar/vault/s3-secret-key   # written by the Vault page
  # password_file: /var/lib/pgcellar/vault/restic-password   # generated, see the recovery key
  quota: 500GB
  # wal_interval: 15m                   # 0 = only after each backup
  # max_age: 36h
  # keep: {daily: 7, weekly: 4}
servers:
  - name: shop-staging
    vault: false                         # this one stays on this host only
drills:
  source: vault                          # scheduled drills restore from the off-site copy
```

Every key is in chapter 10. The S3 secret key is never in `pgcellar.yaml` nor in git: the Vault page writes it into `vault.secret_key_file` (mode 0600).

The Docker image ships restic 0.19.1 (the official binary, checked against its published SHA-256). On a binary install, install restic 0.17 or later (`restic version`); **Test connection** says when it is missing.

## When something goes wrong

| What you see | Why | What to do |
|---|---|---|
| "the secret key does not match the access key" | Wrong secret, or the region does not match the endpoint. | Paste the secret again; check `region`. |
| "the bucket does not exist in this region" | Typo, or the bucket is in another region. | Check `bucket` and the endpoint. |
| "the S3 user is not allowed to do this on the bucket" | The user has no write right on that bucket. | Give it Read/Write on the container. |
| "the restic password does not open this repository" | The password file is not the one the copies were made with (new machine, file replaced). | Put the recovery key back into `vault.password_file`. |
| "the S3 endpoint cannot be reached from this host" | Network, DNS, firewall, proxy. | The copy is tried again at the next WAL pass; check outbound HTTPS. |
| A copy of a few KB every 15 minutes with "nothing new" in it | restic records a snapshot when the metadata of a directory changed, even when no file did. | Nothing: the daily prune forgets them. |
| Storage keeps growing after a prune | Object Lock keeps the deleted versions until their retention ends. | Expected; see "Why Object Lock, and what it costs". |
