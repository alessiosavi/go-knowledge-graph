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
kinds=(env htpasswd)
for kind in domain-seed stoplist; do
  file=$AWS_DIR/box.domain-seed.json
  [ "$kind" != stoplist ] || file=$AWS_DIR/box.stoplist.txt
  if [ -f "$file" ]; then
    kinds+=("$kind")
  else
    printf 'gkg-box: skipping missing optional %s file\n' "$kind"
  fi
done
secret_file() {
  case "$1" in
    env) file=$work/env ;;
    htpasswd) file=$auth ;;
    domain-seed) file=$AWS_DIR/box.domain-seed.json ;;
    stoplist) file=$AWS_DIR/box.stoplist.txt ;;
  esac
}
# Validate every value before uploading any of them, counting bytes rather
# than characters for Standard SecureString's limit.
for kind in "${kinds[@]}"; do
  secret_file "$kind"
  bytes=$(wc -c <"$file" | tr -d '[:space:]')
  [ "$bytes" -le 4096 ] || fail "$kind SecureString is $bytes bytes (limit 4096)"
done
for kind in "${kinds[@]}"; do
  secret_file "$kind"
  jq -Rs --arg name "/$NAME/$kind" '{Name:$name,Type:"SecureString",Value:.,Overwrite:true,Tier:"Standard"}' "$file" >"$work/request"
  chmod 600 "$work/request"
  aws ssm put-parameter --cli-input-json "file://$work/request" >/dev/null 2>/dev/null || fail 'secret upload failed (details withheld)'
done
# put-parameter disallows Tags with Overwrite; tag in a separate pass,
# including after rotations so teardown can verify ownership.
for kind in "${kinds[@]}"; do
  aws ssm add-tags-to-resource --resource-type Parameter --resource-id "/$NAME/$kind" --tags "Key=project,Value=$NAME" >/dev/null 2>/dev/null || fail 'parameter tagging failed'
done
