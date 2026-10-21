# Moving the test bed to the GHCR image and the updater

Today the test bed on rasp001 runs `pgcellar:testbed`, an image built by hand and loaded with `docker load` (README, B2). Once the first release with an image on `ghcr.io/dbfactory-io/pgcellar` is published, the Pi can pull that image instead and be kept up to date by `pgcellar-updater` (manual chapter 16, "Automatic updates (Docker)").

Do it only after that release exists: its GitHub page must show `manifest.json`, and `ghcr.io/dbfactory-io/pgcellar:<version>` must list `linux/arm64`. Below, `X.Y.Z` is that version. All commands run on rasp001, in `~/pgcellar-testbed`, unless stated otherwise. Nothing here touches hawking.

Legend: **safe** = no effect on the running test bed. **restarts the test bed** = the containers are recreated (a backup or drill in progress would be cut: check the Runs page first).

## 1. Check the prerequisites (safe)

```bash
uname -m                                   # aarch64
curl --version | head -1                   # 7.76 or later (--fail-with-body)
command -v jq flock || sudo apt install -y jq util-linux
sudo docker compose version
```

Note the version that runs now (`sudo docker exec pgcellar pgcellar version`) and keep the old image: `pgcellar:testbed` stays on the Pi as the way back until step 6 is validated.

## 2. Pull and inspect the release image (safe)

```bash
sudo docker pull ghcr.io/dbfactory-io/pgcellar:X.Y.Z
sudo docker buildx imagetools inspect ghcr.io/dbfactory-io/pgcellar:X.Y.Z | head -3
curl -sL https://github.com/dbfactory-io/pgcellar/releases/download/vX.Y.Z/manifest.json | jq -r .image_digest
sudo docker run --rm --entrypoint id ghcr.io/dbfactory-io/pgcellar:X.Y.Z barman
```

The digest printed by `imagetools` (or `docker image inspect --format '{{.RepoDigests}}'` without buildx) must equal `image_digest` of the manifest. `id barman` must say `uid=997 gid=997`, the owner of `/mnt/usbdata/pgcellar/*` (README, B3); if it differs, stop here. The image holds PostgreSQL 16, 17 and 18 and the client 18 by default, the same as the hand-built one.

## 3. Switch the compose file to GHCR (restarts the test bed)

```bash
cp docker-compose.yml docker-compose.yml.bak-$(date +%Y%m%d)
sed -i 's#image: pgcellar:testbed#image: ghcr.io/dbfactory-io/pgcellar:${PGCELLAR_TAG:-latest}#' docker-compose.yml
grep -n 'image:' docker-compose.yml        # two lines, pgcellar and pgcellar-ssh
grep -q '^PGCELLAR_TAG=' .env || echo 'PGCELLAR_TAG=X.Y.Z' >> .env
sudo docker compose config | grep 'image:' # ghcr.io/dbfactory-io/pgcellar:X.Y.Z twice
sudo docker compose up -d
curl -s http://127.0.0.1:8082/healthz      # ok
sudo docker exec pgcellar pgcellar version # X.Y.Z
```

`.env` stays mode 0600: it also holds the test bed's secrets (it is the `env_file` of the service), and `PGCELLAR_TAG` in it is harmless inside the container. Then check Status and the SSH companion (`barman switch-wal --archive chutag` then `barman check chutag`, README "SSH companion"). Way back: `cp docker-compose.yml.bak-* docker-compose.yml && sudo docker compose up -d`.

## 4. Install the updater (safe)

From the Mac, copy the updater next to the kit, then install it on the Pi:

```bash
rsync -a deploy/updater/ rasp001:pgcellar-testbed/updater/          # on the Mac
cd ~/pgcellar-testbed/updater && sudo ./install.sh                    # on the Pi, enables nothing
```

Edit `/etc/pgcellar/updater.env` (`sudo nano`): `COMPOSE_DIR` = the absolute path of the kit (`echo ~/pgcellar-testbed`), `API_URL=http://127.0.0.1:8082` (the test bed publishes 8082, not 8080), `SERVICE="pgcellar pgcellar-ssh"`. Leave `ENV_FILE` and `TAG_VAR` at their defaults (`.env`, `PGCELLAR_TAG`).

## 5. API key, update window, dry run (safe)

```bash
cd ~/pgcellar-testbed
sudo install -m 600 /dev/null /etc/pgcellar/updater.key     # empty, 0600, before the key is in it
sudo docker compose exec -T pgcellar pgcellar apikey create \
  --config /etc/pgcellar/pgcellar.yaml --name updater --role operator \
  | sudo tee /etc/pgcellar/updater.key >/dev/null
```

The key goes from the command to the file without being shown.

Set `updates.window` in `etc/pgcellar/pgcellar.yaml` to a quiet slot of the Pi, away from the nightly backups and drills (manual chapter 10). Then, without changing anything:

```bash
sudo DRY_RUN=1 /usr/local/bin/pgcellar-updater
```

It must print "up to date (X.Y.Z)". A 401 means the key file is wrong; "not reachable" means `API_URL`.

## 6. Enable and watch the first real update (restarts the test bed at the next release)

```bash
sudo systemctl enable --now pgcellar-updater.timer
systemctl list-timers pgcellar-updater.timer
journalctl -u pgcellar-updater -f
```

At the next release the journal shows "update available", "digest verified", then "updated from X.Y.Z to the new version" (or "rolled back" with exit 2 if it did not come up healthy). Check `PGCELLAR_TAG` in `.env` and `.pgcellar-updater.prev` afterwards. When this has worked once, remove the old image (`sudo docker image rm pgcellar:testbed`) and bring `deploy/testbed/docker-compose.yml` in the repository in line with the Pi (GHCR image line, `PGCELLAR_TAG` in `.env.example`), so the kit and the Pi say the same thing. To stop automatic updates: `sudo systemctl disable --now pgcellar-updater.timer`.
