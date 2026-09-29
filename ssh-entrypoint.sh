#!/bin/sh
set -eu

socket=/var/run/docker.sock
test -S "$socket" || { echo "Docker socket is not mounted" >&2; exit 1; }
test -s /home/jobrunner/.ssh/authorized_keys || {
  echo "authorized_keys is missing or empty" >&2; exit 1;
}

# SSHD establishes login groups from /etc/group, so add the socket's numeric GID
# to the login user, rather than relying only on Compose group_add.
socket_gid="$(stat -c '%g' "$socket")"
socket_group="$(awk -F: -v gid="$socket_gid" '$3 == gid {print $1; exit}' /etc/group)"
if [ -z "$socket_group" ]; then
  socket_group=dockersock
  addgroup -g "$socket_gid" "$socket_group"
fi
addgroup jobrunner "$socket_group"

key=/etc/jobrunner/hostkeys/ssh_host_ed25519_key
if [ ! -s "$key" ]; then
  ssh-keygen -q -t ed25519 -N '' -f "$key"
fi
chmod 600 "$key"
chmod 644 "$key.pub"
chmod 755 /run/sshd

exec /usr/sbin/sshd -D -e -f /etc/ssh/sshd_config
