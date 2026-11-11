#!/usr/bin/env bash
# First installation only. remote.sh prepares the trusted checkout with git.
set -euo pipefail
umask 077
root=${GKG_ROOT:-/opt/gkg}
units=${GKG_SYSTEMD_DIR:-/etc/systemd/system}
swap=${GKG_SWAP_FILE:-/swapfile}
[ "$#" = 1 ] && [[ "$1" =~ ^[A-Za-z0-9_./-]+$ ]] && [[ "$1" != -* ]] || { echo 'gkg-box: invalid REF' >&2; exit 1; }
[ ! -f "$root/.deployed-sha" ] || { echo 'gkg-box: already bootstrapped; use deploy' >&2; exit 1; }
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
trap 'exit 1' HUP INT TERM
failed() { echo 'gkg-box: bootstrap failed (private diagnostics withheld)' >&2; exit 1; }
repo_git() ( umask 022; git "$@"; )
bash "$root/deploy/aws/box/install-docker.sh" >"$work/docker" 2>&1 || failed
mkdir -p "$units"
if [ ! -f "$swap" ]; then
  fallocate -l 4G "$swap"; chmod 600 "$swap"
fi
if ! swapon --show=NAME --noheadings | grep -Fxq "$swap"; then
  mkswap "$swap" >"$work/swap" 2>&1 || failed
  swapon "$swap"
fi
# Avoid changing the host fstab in test installations with an alternate root.
if [ "$swap" = /swapfile ] && ! grep -q '^/swapfile ' /etc/fstab; then printf '/swapfile none swap defaults 0 0\n' >>/etc/fstab; fi
if [ ! -d "$root/.git" ]; then repo_git clone "${REPO_URL:?REPO_URL required}" "$root" >"$work/git" 2>&1 || failed; fi
cd "$root"
repo_git checkout --detach "$1" >"$work/git" 2>&1 || failed
bash "$root/deploy/aws/box/fetch-secrets.sh" || failed
make up >"$work/up" 2>&1 || failed
sha=$(git rev-parse --verify HEAD)
install -m 644 "$root/deploy/aws/box/gkg-stack.service" "$units/gkg-stack.service"
install -m 644 "$root/deploy/aws/box/gkg-renew.service" "$units/gkg-renew.service"
install -m 644 "$root/deploy/aws/box/gkg-renew.timer" "$units/gkg-renew.timer"
systemctl daemon-reload
systemctl enable gkg-stack.service
systemctl enable --now gkg-renew.timer
# Finalize only after boot and renewal are installed, so partial setup can retry.
printf '%s\n' "$sha" >"$root/.deployed-sha"
printf '%s\n' 'gkg-box: bootstrap complete; check DNS and issue the certificate next'
