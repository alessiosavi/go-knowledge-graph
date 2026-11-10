#!/usr/bin/env bash
# Passwords travel over stdin to bcrypt, never in the helper's argv.
set -euo pipefail
LC_ALL=C
export LC_ALL
umask 077
docker_cmd="$1"
file="$2"
login="$3"
generate="$4"
fail() { printf 'edge-passwd: %s\n' "$1" >&2; exit 1; }
case "$login" in ''|[!A-Za-z0-9_]*|*[!A-Za-z0-9_.-]*) fail 'use make passwd LOGIN=<name> (letters, digits, underscore, dot, hyphen)' ;; esac
[[ ${#login} -le 64 ]] || fail 'LOGIN is too long'
case "$generate" in ''|0|1) ;; *) fail 'GENERATE must be 0 or 1' ;; esac
[[ ! -L "$file" ]] || fail 'refusing a symbolic link destination'
password=
if [[ "$generate" == 1 ]]; then
 password="$(openssl rand -base64 18)"
elif [[ -t 0 ]]; then
 IFS= read -r -s -p 'Password: ' password; printf '\n' >&2
 IFS= read -r -s -p 'Confirm: ' confirm; printf '\n' >&2
 [[ "$password" == "$confirm" ]] || fail 'passwords do not match'
 unset confirm
else
 IFS= read -r password || [[ -n "$password" ]]
fi
[[ -n "$password" && ${#password} -le 72 ]] || fail 'password must contain 1–72 bytes (bcrypt limit)'
mkdir -p "$(dirname "$file")"
hash_tmp="$(mktemp "${file}.hash.XXXXXX")"
new_tmp="$(mktemp "${file}.new.XXXXXX")"
trap 'rm -f "$hash_tmp" "$new_tmp"' EXIT
trap 'exit 1' HUP INT TERM
printf '%s\n' "$password" | "$docker_cmd" run --rm -i \
 httpd:2.4.69-alpine@sha256:698a0f59374eeb59c2c3ae6385c5f346edd2c70c2b63f7b552fb3d64dbc3cf66 \
 htpasswd -niB "$login" > "$hash_tmp"
[[ -s "$hash_tmp" ]] || fail 'bcrypt helper returned no hash'
if [[ -f "$file" ]]; then
 awk -F: -v login="$login" '$1 != login' "$file" > "$new_tmp"
fi
cat "$hash_tmp" >> "$new_tmp"
chmod 0600 "$new_tmp"
mv "$new_tmp" "$file"
if [[ "$generate" == 1 ]]; then printf '%s\n' "$password"; fi
unset password
printf '%s\n' 'edge-passwd: saved; make up copies the password file into the edge volume' >&2
