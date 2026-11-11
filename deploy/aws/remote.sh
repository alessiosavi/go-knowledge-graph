#!/usr/bin/env bash
# Only fixed scripts and validated non-secret arguments enter an SSM command.
set -euo pipefail
# shellcheck source=deploy/aws/lib.sh
source "$(dirname "$0")/lib.sh"
load_box_env
assert_account
script=${1:-}; [ "$#" -gt 0 ] || fail 'remote script required'; shift
case "$script" in bootstrap.sh|deploy.sh|acme-probe.sh|--update|--cert|--docker) ;; *) fail 'unsupported remote operation' ;; esac
case "$script" in bootstrap.sh|deploy.sh) [ "$#" = 1 ] || fail 'REF required' ;; acme-probe.sh) [ "$#" -ge 1 ] && [ "$#" -le 2 ] || fail 'nonce required' ;; *) [ "$#" = 0 ] || fail 'unexpected arguments' ;; esac
for arg in "$@"; do
  [[ "$arg" =~ ^[A-Za-z0-9_./-]+$ ]] || fail 'unsafe remote argument'
  case "$arg" in -*) [ "$script" = acme-probe.sh ] && [ "$arg" = --remove ] || fail 'unsafe remote option' ;; esac
done
instance=$(box_id_get INSTANCE_ID)
[ -n "$instance" ] || fail 'provision first (no instance metadata)'
tagged_or_die ec2 "$instance" || fail 'instance is not project-owned'
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
trap 'exit 1' HUP INT TERM
seconds=900
command="flock /run/gkg-ops.lock bash /opt/gkg/deploy/aws/box/$script"
for arg in "$@"; do command="$command '$arg'"; done
case "$script" in
  bootstrap.sh)
    seconds=5400
    command="set -eu; dnf install -y git; if [ -f /opt/gkg/.deployed-sha ]; then echo 'already bootstrapped; use deploy' >&2; exit 1; fi; if [ ! -d /opt/gkg/.git ]; then git clone '$REPO_URL' /opt/gkg; fi; git -C /opt/gkg fetch --tags origin; git -C /opt/gkg checkout --detach '$1'; export REPO_URL='$REPO_URL'; $command"
    ;;
  deploy.sh) seconds=3600 ;;
  --update) command="flock /run/gkg-ops.lock bash -c 'dnf upgrade --releasever=latest -y && systemd-run --on-active=15 /usr/bin/systemctl reboot'" ;;
  --cert) command='flock /run/gkg-ops.lock make -C /opt/gkg cert' ;;
  --docker) command='flock /run/gkg-ops.lock bash /opt/gkg/deploy/aws/box/install-docker.sh' ;;
esac
command="export AWS_REGION='$AWS_REGION' AWS_DEFAULT_REGION='$AWS_REGION' GKG_SSM_PREFIX='/$NAME'; $command"
jq -n --arg command "$command" --arg timeout "$seconds" '{commands:[$command],executionTimeout:[$timeout]}' >"$work/parameters"
command_start=$(date -u +%s)
# SendCommand delivery budget is 600 s; execution is bounded by executionTimeout=$seconds.
# Total timeout is their sum: https://docs.aws.amazon.com/systems-manager/latest/userguide/monitor-commands.html#monitor-about-status-timeouts
id=$(aws ssm send-command --document-name AWS-RunShellScript --instance-ids "$instance" --timeout-seconds 600 --parameters "file://$work/parameters" --output json | jq -r '.Command.CommandId')
[ -n "$id" ] && [ "$id" != null ] || fail 'SSM command id missing'
start=$SECONDS
while :; do
  if aws ssm get-command-invocation --command-id "$id" --instance-id "$instance" --output json >"$work/result" 2>"$work/error"; then
    status=$(jq -r '.Status' "$work/result")
    case "$status" in
      Pending|InProgress|Delayed|Cancelling) ;;
      Success|Failed|Cancelled|TimedOut)
        code=$(jq -r '.ResponseCode' "$work/result")
        case "$code" in ''|*[!0-9]*) code=1 ;; esac
        [ "$code" -le 255 ] || code=1
        [ "$status" = Success ] || { [ "$code" -ne 0 ] || code=1; }
        if [ "$code" != 0 ]; then jq -rj '.StandardOutputContent // ""' "$work/result"; jq -rj '.StandardErrorContent // ""' "$work/result" >&2; exit "$code"; fi
        break
        ;;
      *) fail 'unknown SSM invocation status' ;;
    esac
  else
    grep -q InvocationDoesNotExist "$work/error" && [ "$((SECONDS-start))" -lt 60 ] || fail 'SSM invocation lookup failed'
  fi
  [ "$((SECONDS-start))" -lt "$((seconds+660))" ] || fail 'SSM polling deadline exceeded'
  sleep 2
done
if [ "$script" = --update ]; then
  timeout=900
  if [ -n "${BOX_UPDATE_TIMEOUT:-}" ]; then
    case "$BOX_UPDATE_TIMEOUT" in *[!0-9]*|'') fail 'invalid reconnect timeout' ;; esac
    [ "$BOX_UPDATE_TIMEOUT" -le 900 ] || fail 'reconnect timeout exceeds 15 minutes'
    timeout=$BOX_UPDATE_TIMEOUT
  fi
  deadline=$((SECONDS+timeout))
  # Allow the scheduled reboot to begin before accepting a refreshed ping.
  sleep 20
  while :; do
    aws ssm describe-instance-information --filters "Key=InstanceIds,Values=$instance" --output json >"$work/ping" 2>"$work/error" || fail 'SSM reconnect lookup failed'
    # AWS CLI may render timestamps as epoch numbers or ISO-8601 strings,
    # with fractional seconds and a UTC offset. Never accept an old Online row.
    if jq -e --arg instance "$instance" --argjson start "$command_start" '
      def epoch:
        if type=="number" then . else
          sub("\\+00:00$";"Z") | sub("\\.[0-9]+Z$";"Z") | fromdateiso8601
        end;
      any(.InstanceInformationList[]; .InstanceId==$instance and .PingStatus=="Online" and ((.LastPingDateTime | epoch) > $start))
    ' "$work/ping" >/dev/null 2>"$work/error"; then break; fi
    [ "$SECONDS" -lt "$deadline" ] || fail 'SSM did not reconnect with a fresh ping within 15 minutes'
    sleep 5
  done
fi
if [ "$script" = deploy.sh ] || [ "$script" = --update ]; then
  netrc=${BOX_NETRC:-$AWS_DIR/box.netrc}
  [ -s "$netrc" ] || fail 'deploy login missing; run make box-login'
  timeout=60
  # Reboot startup can take the full gkg-stack.service TimeoutStartSec budget.
  [ "$script" != --update ] || timeout=900
  # Allow a shorter deadline, never a longer one than the operation's default.
  if [ -n "${BOX_CHECK_TIMEOUT:-}" ]; then
    case "$BOX_CHECK_TIMEOUT" in *[!0-9]*|'') fail 'invalid check timeout' ;; esac
    [ "$BOX_CHECK_TIMEOUT" -le "$timeout" ] || fail "check timeout exceeds $timeout seconds"
    timeout=$BOX_CHECK_TIMEOUT
  fi
  deadline=$((SECONDS+timeout))
  while :; do
    healthy=1
    for path in /healthz /v1/agent/status /v1/documents; do
      auth=(); [ "$path" = /healthz ] || auth=(--netrc-file "$netrc")
      remaining=$((deadline-SECONDS)); [ "$remaining" -gt 0 ] || { healthy=0; break; }
      max=$remaining; [ "$max" -le 5 ] || max=5
      http=$(curl -q --silent --noproxy '*' --connect-timeout "$max" --max-time "$max" ${auth[@]+"${auth[@]}"} --output /dev/null --write-out '%{http_code}' "https://$HOSTNAME$path" 2>/dev/null) || http=000
      [ "$http" = 200 ] || healthy=0
    done
    [ "$healthy" = 0 ] || break
    [ "$SECONDS" -lt "$deadline" ] || fail 'through-edge deploy check failed'
    sleep 1
  done
fi
jq -rj '.StandardOutputContent // ""' "$work/result"
jq -rj '.StandardErrorContent // ""' "$work/result" >&2
