#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=deploy/aws/lib.sh
source "$(dirname "$0")/lib.sh"
load_box_env
assert_account
src=${BOX_SECRETS_FILE:-$AWS_DIR/box.secrets.env}
auth=${BOX_HTPASSWD:-$AWS_DIR/box.htpasswd}
[ -s "$src" ] && [ -s "$auth" ] || fail 'secret environment or password file missing'
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
trap 'exit 1' HUP INT TERM
awk '!/^[[:space:]]*(#|$)/' "$src" >"$work/env"
for key in MEMGRAPH_PASSWORD GKG_API_KEY; do
  awk -F= -v key="$key" '$1==key {n++; if(length(substr($0,length(key)+2))==0 || substr($0,length(key)+2)=="\047\047" || substr($0,length(key)+2)=="\042\042") bad=1} END {exit (n!=1 || bad)}' "$work/env" || fail 'missing or duplicate required secret key'
done
[ "$(wc -c <"$work/env")" -le 4096 ] && [ "$(wc -c <"$auth")" -le 4096 ] || fail 'SecureString exceeds 4096 bytes'
for kind in env htpasswd; do
  file=$work/env; [ "$kind" != htpasswd ] || file=$auth
  jq -Rs --arg name "/$NAME/$kind" '{Name:$name,Type:"SecureString",Value:.,Overwrite:true,Tier:"Standard"}' "$file" >"$work/request"
  chmod 600 "$work/request"
  aws ssm put-parameter --cli-input-json "file://$work/request" >/dev/null 2>/dev/null || fail 'secret upload failed (details withheld)'
done
# put-parameter disallows Tags with Overwrite; tag in a separate pass,
# including after rotations so teardown can verify ownership.
for kind in env htpasswd; do
  aws ssm add-tags-to-resource --resource-type Parameter --resource-id "/$NAME/$kind" --tags "Key=project,Value=$NAME" >/dev/null 2>/dev/null || fail 'parameter tagging failed'
done
