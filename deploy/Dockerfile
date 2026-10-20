# pgCellar image: the pgcellar binary plus an unmodified Barman from PGDG packages.
#
# Licensing note: Barman (EnterpriseDB, GPL-3.0) is neither modified nor built
# into the pgcellar binary. It is installed from the PGDG apt repository as a
# separate program and driven through its command line only. This image is a
# mere aggregation; the two programs keep their own, separate licences.

FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.Version=${VERSION}" \
    -o /out/pgcellar ./cmd/pgcellar

FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive

# Major versions of the PostgreSQL server binaries kept for restore drills. A drill
# starts the restored copy with the binaries of the same major version, so list here
# every major version of the servers you back up. Fewer versions = a smaller image:
#   docker build --build-arg PG_MAJORS="17" ...
ARG PG_MAJORS="16 17 18"

# PGDG apt repository, as documented on postgresql.org/download/linux/debian
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl gnupg \
 && install -d /usr/share/postgresql-common/pgdg \
 && curl -fsSL -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc \
      https://www.postgresql.org/media/keys/ACCC4CF8.asc \
 && echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt bookworm-pgdg main" \
      > /etc/apt/sources.list.d/pgdg.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
      barman barman-cli postgresql-client-18 rsync openssh-client openssh-server \
      ca-certificates git tzdata postgresql-common \
# Server binaries for the restore drills, nothing else: no cluster is created
# (create_main_cluster = false), no service is started, and whatever a postinst
# left behind is removed. Binaries end up in /usr/lib/postgresql/<major>/bin.
 && printf 'create_main_cluster = false\n' >> /etc/postgresql-common/createcluster.conf \
 && apt-get install -y --no-install-recommends $(for v in ${PG_MAJORS}; do printf 'postgresql-%s ' "$v"; done) \
 && rm -rf /etc/postgresql /var/lib/postgresql/* /var/log/postgresql \
 && for v in ${PG_MAJORS}; do test -x /usr/lib/postgresql/$v/bin/pg_ctl || { echo "missing pg_ctl for PostgreSQL $v" >&2; exit 1; }; done \
 && apt-get purge -y curl gnupg \
 && apt-get autoremove -y \
 && rm -rf /var/lib/apt/lists/*

# The user "barman" is created by the barman package.
RUN mkdir -p /etc/barman.d /var/lib/barman /etc/pgcellar /var/lib/pgcellar \
 && chown -R barman:barman /etc/barman.d /var/lib/barman /etc/pgcellar /var/lib/pgcellar

COPY --from=build /out/pgcellar /usr/local/bin/pgcellar
# Entry point of the SSH companion container (same image, other entrypoint).
COPY --chmod=0755 deploy/sshd/entrypoint.sh /usr/local/bin/pgcellar-sshd

VOLUME ["/var/lib/barman", "/var/lib/pgcellar"]
EXPOSE 8080 2222

# pgcellar runs as the barman user itself: no root in the container and no
# sudo (the image has none), Barman is executed directly. Bind-mounted
# directories must be writable by that user (uid of "barman" in this image).
USER barman
WORKDIR /var/lib/barman

ENTRYPOINT ["/usr/local/bin/pgcellar"]
CMD ["serve", "--config", "/etc/pgcellar/pgcellar.yaml"]
