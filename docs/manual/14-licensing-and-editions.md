# Licensing and editions

Two separate things: the licence of the **source code** (what you may do with pgCellar), and the **licence key** (how many servers you may add). This chapter explains both. It is a summary: the text of the file `LICENSE` is what counts.

## The source licence: FSL-1.1-Apache-2.0

pgCellar is released under the Functional Source License, version 1.1, with the Apache License 2.0 as its future licence (SPDX: `FSL-1.1-ALv2`). In plain words:

- The source is public. You can read it, build it, modify it and redistribute it.
- You can use it for any purpose except a **Competing Use**. Using it to back up your own databases, or your employer's, or your customers' as part of a professional service, is a permitted purpose. So are non-commercial education and research.
- A Competing Use is making the software available to others in a commercial product or service that substitutes for pgCellar or offers the same or substantially similar functionality. In practice: you cannot take pgCellar and sell a hosted "backup console" service built on it.
- **Each version becomes Apache-2.0 two years after its release.** After that date, that version is under a permissive licence with no such restriction.

If you are in doubt about a specific use (a managed-service offering, an integration in a product), ask before you build it: the author prefers a conversation to a surprise.

Barman is a separate program under GPL-3.0. The image contains it unmodified; pgCellar runs its command line and never imports, links or patches it. The two programs keep their own licences. The Docker image is an aggregation of them.

## Editions

| Edition | Servers | Price |
|---|---|---|
| Free | up to 2 | 0 |
| Pro | up to 10 | 490 EUR per year |
| Business | unlimited | 1 490 EUR per year |

Prices are the ones on the project page at the time of writing; see https://dbfactory.io/pgcellar for the current ones. Early access is free during the beta.

Free is not a trial. There is no expiry and no feature locked away: the Free edition simply holds two servers.

## What a licence blocks, and what it never blocks

**A licence never blocks a backup, a restore or a drill.** That is a product rule, in the code, tested. Whatever the state of your licence:

- scheduled and manual backups keep running;
- restores, point-in-time recovery and downloads keep working;
- restore drills keep running and reports keep being written;
- alerts, the API, metrics and the console keep working.

What a licence limits is **adding a server** beyond the quota. The check is made in one place only, where a server is created, so the form and the API behave the same. Beyond the quota the answer is a message that says what keeps working and where to get a licence.

A server that already exists is never removed or paused because of the licence.

### Expiry and grace

| State | Meaning | Quota |
|---|---|---|
| `free` | No key | 2 servers |
| `valid` | Signed key, not expired | What the key says |
| `grace` | Expired for less than 30 days | Still what the key says |
| `expired` | Expired for 30 days or more | Free rules: 2 servers, for new additions |
| `invalid` | The key does not verify | Free rules, and the reason in a banner |

A key is valid through the end of its expiry day (UTC). After expiry you get 30 days of grace. After that, the Free rules apply to **additions** only; the servers you already have keep running and nothing is deleted.

A key of edition "free" never raises the quota.

The footer of every page shows the edition and the count ("Free edition · 2 of 2 servers"). The Status page shows a banner only when you are over quota, in grace, or expired.

## Putting a key in place

A key looks like `PGC1.<payload>.<signature>`. It is Ed25519 signed and verified **offline** against a public key built into the binary: no account, no network call, nothing phones home.

Put it in `pgcellar.yaml`:

```yaml
license:
  key: "PGC1.eyJpZCI6...."
```

or keep it in a file and point to it:

```yaml
license:
  file: /etc/pgcellar/license.key
```

Check what the application sees:

```
docker compose exec pgcellar pgcellar license show --config /etc/pgcellar/pgcellar.yaml
```

```
state:    valid
edition:  Pro
licensee: Example Ltd
id:       ...
servers:  10 allowed
expires:  2027-10-20 (grace until 2027-11-19)
footer:   Pro edition · Example Ltd · 3 of 10 servers
```

To read what a key says, without a configuration file:

```
pgcellar license verify PGC1.eyJ...
```

It prints the payload (id, edition, licensee, servers, issue and expiry dates), the state, and a verdict: `signature OK`, or `INVALID` with the reason. It exits with a non-zero status for an expired key.

`GET /api/v1/license` (viewer) returns the edition, the licensee, the servers allowed and used, the expiry and the state. The metrics `pgcellar_license_servers_max` and `pgcellar_license_servers_used` expose the quota.

`pgcellar license keygen` and `pgcellar license issue` are for the vendor: they create the signing key and sign licence keys. You never need them. The private key is never in the repository.

Get a key at https://dbfactory.io/pgcellar.

## Which servers are counted

Servers are counted as the Servers page lists them: the sections of `barman.d`, the entries of `pgcellar.yaml` and the servers in the status cache, each name once. `pgcellar license show` counts only the entries of `pgcellar.yaml`, so its footer line can differ from the one in the console.
