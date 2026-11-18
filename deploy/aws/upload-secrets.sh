#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=deploy/aws/lib.sh
source "$(dirname "$0")/lib.sh"
# Check whether the caller set BOX_ENV_FILE before load_box_env fills it in.
alternate_box=${BOX_ENV_FILE+x}
primary_domain_seed=$AWS_DIR/box.domain-seed.json
primary_stoplist=$AWS_DIR/box.stoplist.txt
domain_seed='' stoplist=''
if [ -z "$alternate_box" ]; then
  domain_seed=$primary_domain_seed
  stoplist=$primary_stoplist
fi
domain_seed=${BOX_DOMAIN_SEED_FILE-$domain_seed}
stoplist=${BOX_STOPLIST_FILE-$stoplist}
load_box_env
assert_account
src=${BOX_SECRETS_FILE:-$AWS_DIR/box.secrets.env}
auth=${BOX_HTPASSWD:-$AWS_DIR/box.htpasswd}
[ ! -L "$src" ] || fail 'refusing a symlink for BOX_SECRETS_FILE'
[ ! -L "$auth" ] || fail 'refusing a symlink for BOX_HTPASSWD'
[ -s "$src" ] && [ -s "$auth" ] || fail 'secret environment or password file missing'
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
trap 'exit 1' HUP INT TERM
awk '!/^[[:space:]]*(#|$)/' "$src" >"$work/env"
for key in MEMGRAPH_PASSWORD GKG_API_KEY; do
  awk -F= -v key="$key" '$1==key {n++; if(length(substr($0,length(key)+2))==0 || substr($0,length(key)+2)=="\047\047" || substr($0,length(key)+2)=="\042\042") bad=1} END {exit (n!=1 || bad)}' "$work/env" || fail 'missing or duplicate required secret key'
done
kinds=(env htpasswd)
add_optional_secret() {
  local kind=$1 file=$2 override=$3 explicit=$4
  [ ! -L "$file" ] || fail "refusing a symlink for $kind ($override)"
  if [[ -n "$alternate_box" && -n "$explicit" &&
    ( "$file" -ef "$primary_domain_seed" || "$file" -ef "$primary_stoplist" ) ]]; then
    fail "$override points to a primary box file in alternate-box mode"
  fi
  if [ -f "$file" ]; then
    kinds+=("$kind")
  elif [ -n "$explicit" ]; then
    fail "$override must point to an existing regular file"
  elif [ -z "$file" ]; then
    printf 'gkg-box: skipping optional %s (set %s for this box)\n' "$kind" "$override"
  else
    printf 'gkg-box: skipping missing optional %s file\n' "$kind"
  fi
}
add_optional_secret domain-seed "$domain_seed" BOX_DOMAIN_SEED_FILE "${BOX_DOMAIN_SEED_FILE+x}"
add_optional_secret stoplist "$stoplist" BOX_STOPLIST_FILE "${BOX_STOPLIST_FILE+x}"
# Stage and validate the decoded Value of every request before the first put.
for kind in "${kinds[@]}"; do
  case "$kind" in
    env) file=$work/env ;;
    htpasswd) file=$auth ;;
    domain-seed) file=$domain_seed ;;
    stoplist) file=$stoplist ;;
  esac
  # Bound the read before jq slurps the file: decoding never shrinks it.
  bytes=$(wc -c <"$file" | tr -d '[:space:]')
  [ "$bytes" -le 4096 ] || fail "$kind SecureString is $bytes bytes (limit 4096)"
  request=$work/$kind.json
  jq -Rs --arg name "/$NAME/$kind" '{Name:$name,Type:"SecureString",Value:.,Overwrite:true,Tier:"Standard"}' <"$file" >"$request"
  chmod 600 "$request"
  # Count the decoded Value that will be sent (invalid UTF-8 can expand);
  # raw output plus wc works on every jq release.
  bytes=$(jq -j '.Value' "$request" | wc -c | tr -d '[:space:]') || fail "$kind request could not be validated"
  [ "$bytes" -le 4096 ] || fail "$kind SecureString is $bytes bytes (limit 4096)"
done
for kind in "${kinds[@]}"; do
  aws ssm put-parameter --cli-input-json "file://$work/$kind.json" >/dev/null 2>/dev/null || fail 'secret upload failed (details withheld)'
done
# put-parameter disallows Tags with Overwrite; tag in a separate pass,
# including after rotations so teardown can verify ownership.
for kind in "${kinds[@]}"; do
  aws ssm add-tags-to-resource --resource-type Parameter --resource-id "/$NAME/$kind" --tags "Key=project,Value=$NAME" >/dev/null 2>/dev/null || fail 'parameter tagging failed'
done
