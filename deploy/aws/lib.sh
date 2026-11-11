#!/usr/bin/env bash
# Shared Mac-side helpers. Configuration is data, never shell code.
set -euo pipefail
umask 077
export LC_ALL=C AWS_PAGER="" AWS_CLI_AUTO_PROMPT=off
AWS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
fail() { printf 'gkg-box: %s\n' "$1" >&2; exit 1; }
load_box_env() {
  local line key value seen='|'
  BOX_ENV_FILE=${BOX_ENV_FILE:-$AWS_DIR/box.env}
  BOX_IDS_FILE=${BOX_IDS_FILE:-$AWS_DIR/.box-ids}
  [ -r "$BOX_ENV_FILE" ] || fail 'box.env missing; copy box.env.example and fill it locally'
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in ''|\#*) continue ;; esac
    case "$line" in *=*) ;; *) fail 'invalid configuration line' ;; esac
    key=${line%%=*}; value=${line#*=}
    case "$key" in AWS_PROFILE|AWS_REGION|EXPECTED_ACCOUNT|VPC_ID|SUBNET_ID|NAME|HOSTNAME|HOSTED_ZONE_ID|INSTANCE_TYPE|DISK_GB|REPO_URL) ;; *) fail 'unknown configuration key' ;; esac
    case "$seen" in *"|$key|"*) fail 'duplicate configuration key' ;; esac
    seen="$seen$key|"
    if [ "$key" != HOSTED_ZONE_ID ] || [ -n "$value" ]; then
      [[ "$value" =~ ^[A-Za-z0-9_./:@+-]+$ ]] || fail 'invalid configuration value (use unquoted data)'
    fi
    printf -v "$key" '%s' "$value"
  done <"$BOX_ENV_FILE"
  for key in AWS_PROFILE AWS_REGION EXPECTED_ACCOUNT VPC_ID SUBNET_ID NAME HOSTNAME HOSTED_ZONE_ID INSTANCE_TYPE DISK_GB REPO_URL; do
    case "$seen" in *"|$key|"*) ;; *) fail 'missing required configuration key' ;; esac
  done
  [[ "$EXPECTED_ACCOUNT" =~ ^[0-9]{12}$ ]] || fail 'invalid expected account'
  [[ "$DISK_GB" =~ ^[1-9][0-9]*$ ]] && [ "$DISK_GB" -ge 100 ] || fail 'disk must be at least 100 GB'
  [[ "$NAME" =~ ^[a-z][a-z0-9-]{1,30}$ ]] || fail 'invalid NAME (use 2-31 lowercase letters, digits or hyphens; start with a letter)'
  [[ "$HOSTNAME" =~ ^[a-zA-Z0-9][a-zA-Z0-9.-]+$ ]] || fail 'invalid hostname'
  case "$REPO_URL" in https://*) ;; *) fail 'REPO_URL must use HTTPS without credentials' ;; esac
  case "$REPO_URL" in *@*) fail 'REPO_URL must not contain credentials' ;; esac
  export AWS_PROFILE AWS_REGION AWS_DEFAULT_REGION="$AWS_REGION"
  validate_ids
}
validate_ids() {
  local line key seen='|'
  [ ! -e "$BOX_IDS_FILE" ] || [ -f "$BOX_IDS_FILE" ] || fail 'invalid metadata file'
  [ ! -L "$BOX_IDS_FILE" ] || fail 'metadata must not be a symlink'
  [ -f "$BOX_IDS_FILE" ] || return 0
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in *=*) ;; *) fail 'invalid metadata line' ;; esac
    key=${line%%=*}
    case "$key" in SG_ID|EIP_ALLOC|EIP_IP|ROLE|PROFILE|CLIENT_TOKEN|INSTANCE_ID|DLM_POLICY_ID) ;; *) fail 'unknown metadata key' ;; esac
    case "$seen" in *"|$key|"*) fail 'duplicate metadata key' ;; esac
    seen="$seen$key|"
    [[ "${line#*=}" =~ ^[A-Za-z0-9._-]+$ ]] || fail 'invalid metadata value'
  done <"$BOX_IDS_FILE"
}
assert_account() {
  local actual
  actual=$(aws sts get-caller-identity --query Account --output text 2>/dev/null) || fail 'account assertion failed; refresh AWS SSO login'
  [ "$actual" = "$EXPECTED_ACCOUNT" ] || fail 'account mismatch; refusing AWS operations'
}
box_id_get() { [ ! -f "$BOX_IDS_FILE" ] || awk -F= -v key="$1" '$1==key {print substr($0,length(key)+2)}' "$BOX_IDS_FILE"; }
box_id_set() (
  local tmp
  tmp=$(mktemp "${BOX_IDS_FILE}.tmp.XXXXXX")
  trap 'rm -f "$tmp"' EXIT
  if [ -f "$BOX_IDS_FILE" ]; then awk -F= -v key="$1" '$1!=key' "$BOX_IDS_FILE" >"$tmp"; fi
  [ -z "$2" ] || printf '%s=%s\n' "$1" "$2" >>"$tmp"
  chmod 600 "$tmp"; mv -f "$tmp" "$BOX_IDS_FILE"
)
aws_ok_if() {
  local allowed=$1 result
  shift
  if result=$("$@" 2>&1); then return 0; fi
  case "$result" in *"$allowed"*) return 0 ;; esac
  fail 'AWS operation failed (details withheld)'
}
tagged_or_die() {
  local data
  case "$1" in
    role) data=$(aws iam get-role --role-name "$2" --output json) || return 1; printf '%s' "$data" | jq -e --arg name "$NAME" '.Role.Tags | any(.Key=="project" and .Value==$name)' >/dev/null ;;
    profile) data=$(aws iam get-instance-profile --instance-profile-name "$2" --output json) || return 1; printf '%s' "$data" | jq -e --arg name "$NAME" '.InstanceProfile.Tags | any(.Key=="project" and .Value==$name)' >/dev/null ;;
    dlm) data=$(aws dlm get-lifecycle-policy --policy-id "$2" --output json) || return 1; printf '%s' "$data" | jq -e --arg name "$NAME" '.Policy.Tags.project==$name' >/dev/null ;;
    parameter) data=$(aws ssm list-tags-for-resource --resource-type Parameter --resource-id "$2" --output json) || return 1; printf '%s' "$data" | jq -e --arg name "$NAME" '.TagList | any(.Key=="project" and .Value==$name)' >/dev/null ;;
    *) data=$(aws ec2 describe-tags --filters "Name=resource-id,Values=$2" --output json) || return 1; printf '%s' "$data" | jq -e --arg name "$NAME" '.Tags | any(.Key=="project" and .Value==$name)' >/dev/null ;;
  esac
}
one_or_empty() {
  local count
  count=$(printf '%s' "$1" | jq 'length')
  [ "$count" -le 1 ] || fail 'ambiguous resources; refusing to guess'
  if [ "$count" = 1 ]; then printf '%s' "$1" | jq -r ".[0].$2"; fi
}
