#!/usr/bin/env bash
# Matched static runtime + pinned CLI plugins. Never mix with RPM runtimes.
set -euo pipefail
umask 077
root=${GKG_ROOT:-/opt/gkg}
bin=${GKG_DOCKER_BIN_DIR:-/usr/bin}
plugins=${GKG_PLUGIN_DIR:-/usr/local/lib/docker/cli-plugins}
units=${GKG_SYSTEMD_DIR:-/etc/systemd/system}
daemon=${GKG_DOCKER_DIR:-/etc/docker}
modules=${GKG_MODULES_DIR:-/etc/modules-load.d}
sysctls=${GKG_SYSCTL_DIR:-/etc/sysctl.d}
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
trap 'exit 1' HUP INT TERM
failed() { echo "gkg-box: $1 (private diagnostics withheld)" >&2; exit 1; }
for package in docker docker-ce containerd containerd.io runc; do
  if rpm -q "$package" >"$work/rpm" 2>&1; then
    failed 'RPM container runtime installed; remove it deliberately before using static Docker'
  else
    status=$?
    [ "$status" = 1 ] || failed 'cannot inspect installed RPMs'
  fi
done
dnf install -y git make tar gzip kmod shadow-utils iptables-nft nftables >"$work/dnf" 2>&1 || failed 'Docker prerequisites failed'
# Verify every artifact before installing any Docker binary, plugin or unit.
curl -q --fail --silent --show-error --location --proto '=https' --tlsv1.2 --output "$work/engine.tgz" https://download.docker.com/linux/static/stable/x86_64/docker-29.9.3.tgz >"$work/curl" 2>&1 || failed 'Engine download failed'
printf '3fb5d1f13014c18cc54945bc177c58a9056a482dada031e17d12cf2e6922c973  %s\n' "$work/engine.tgz" | sha256sum -c - >"$work/checksum" 2>&1 || failed 'Engine checksum failed'
curl -q --fail --silent --show-error --location --proto '=https' --tlsv1.2 --output "$work/compose" https://github.com/docker/compose/releases/download/v5.7.0/docker-compose-linux-x86_64 >"$work/curl" 2>&1 || failed 'Compose download failed'
printf '7758048ad38e580f22559603312874fec8bb638d021a9e64182385fc17cc12a2  %s\n' "$work/compose" | sha256sum -c - >"$work/checksum" 2>&1 || failed 'Compose checksum failed'
curl -q --fail --silent --show-error --location --proto '=https' --tlsv1.2 --output "$work/buildx" https://github.com/docker/buildx/releases/download/v0.38.2/buildx-v0.38.2.linux-amd64 >"$work/curl" 2>&1 || failed 'buildx download failed'
printf '7992ec09413b80ff57ba4e081f56fc833d7bac1d5e71143c6c809e7e6065f6d6  %s\n' "$work/buildx" | sha256sum -c - >"$work/checksum" 2>&1 || failed 'buildx checksum failed'
tar -xzf "$work/engine.tgz" -C "$work" >"$work/tar" 2>&1 || failed 'Engine extraction failed'
mkdir -p "$bin" "$plugins" "$units" "$daemon" "$modules" "$sysctls"
changed=0
install_changed() {
  if ! cmp -s "$1" "$2"; then
    install -m "$3" "$1" "$2" >"$work/install" 2>&1 || failed 'Docker file installation failed'
    changed=1
  fi
}
# Install the complete tarball, including containerd, ctr, runc and shims.
[ -f "$work/docker/docker" ] && [ -f "$work/docker/dockerd" ] && [ -f "$work/docker/containerd" ] || failed 'incomplete Engine archive'
for binary in "$work/docker/"*; do
  [ -f "$binary" ] || failed 'unexpected Engine archive entry'
  install_changed "$binary" "$bin/${binary##*/}" 755
done
runtime_changed=$changed
install_changed "$work/compose" "$plugins/docker-compose" 755
install_changed "$work/buildx" "$plugins/docker-buildx" 755
binaries_changed=$changed
for unit in docker.service docker.socket containerd.service; do
  install_changed "$root/deploy/aws/box/systemd/$unit" "$units/$unit" 644
done
groupadd -f -r docker >"$work/group" 2>&1 || failed 'Docker group setup failed'
printf 'overlay\nbr_netfilter\n' >"$work/modules"
install_changed "$work/modules" "$modules/docker.conf" 644
modprobe overlay >"$work/modprobe" 2>&1 || failed 'overlay module unavailable'
modprobe br_netfilter >"$work/modprobe" 2>&1 || failed 'bridge filtering module unavailable'
printf 'net.ipv4.ip_forward=1\n' >"$work/sysctl"
install_changed "$work/sysctl" "$sysctls/99-docker.conf" 644
sysctl --system >"$work/sysctl-output" 2>&1 || failed 'Docker sysctl setup failed'
printf '{"log-driver":"json-file","log-opts":{"max-size":"50m","max-file":"3"}}\n' >"$work/daemon.json"
changed=0
install_changed "$work/daemon.json" "$daemon/daemon.json" 644
daemon_changed=$changed
systemctl daemon-reload >"$work/systemctl" 2>&1 || failed 'systemd reload failed'
systemctl enable --now containerd docker >"$work/systemctl" 2>&1 || failed 'Docker service startup failed'
if [ "$runtime_changed" = 1 ]; then
  systemctl restart containerd >"$work/systemctl" 2>&1 || failed 'containerd restart failed'
fi
if [ "$binaries_changed" = 1 ] || [ "$daemon_changed" = 1 ]; then
  systemctl restart docker >"$work/systemctl" 2>&1 || failed 'Docker restart failed'
fi
version=$(docker version --format '{{.Server.Version}}' 2>"$work/version") || failed 'Docker version check failed'
[ "$version" = 29.9.3 ] || failed 'unexpected Docker Server version'
version=$(docker compose version --short 2>"$work/version") || failed 'Compose version check failed'
[ "$version" = 5.7.0 ] || failed 'unexpected Compose version'
printf '%s\n' 'gkg-box: Docker 29.9.3 and Compose 5.7.0 installed'
