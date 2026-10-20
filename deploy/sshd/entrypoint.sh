#!/bin/sh
# Entry point of the pgCellar SSH companion: runs as the barman user.
# Creates the host key once (it lives in the Barman volume, so it survives
# container recreation and PostgreSQL servers keep trusting it), then runs sshd
# in the foreground, logging to stderr.
set -eu

home=/var/lib/barman
dir="$home/.ssh"
hostdir="$dir/host"
key="$hostdir/ssh_host_ed25519_key"

# sshd (StrictModes) refuses a home or .ssh writable by others.
umask 077
mkdir -p "$hostdir"
chmod 700 "$dir" "$hostdir"

if [ ! -f "$key" ]; then
    ssh-keygen -q -t ed25519 -N '' -C "pgcellar-ssh" -f "$key"
fi
chmod 600 "$key"

exec /usr/sbin/sshd -D -e -f /etc/pgcellar/sshd_config
