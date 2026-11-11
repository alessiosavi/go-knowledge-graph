#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=deploy/aws/lib.sh
source "$(dirname "$0")/lib.sh"
load_box_env
assert_account
ip=$(box_id_get EIP_IP)
[ -n "$ip" ] || fail 'provision first (no EIP metadata)'
work=$(mktemp -d); nonce=; probe=0
cleanup() {
  result=$?
  trap - EXIT
  if [ "$probe" = 1 ]; then bash "$AWS_DIR/remote.sh" acme-probe.sh "$nonce" --remove >/dev/null || result=1; fi
  rm -rf "$work"
  exit "$result"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
lookup() {
  local typ=$1 name=$2 resolver=$3
  dig "@$resolver" "$name" "$typ" +time=5 +tries=1 +noall +comments +answer >"$work/dig" 2>/dev/null || fail 'public DNS lookup failed'
  # A transport success with SERVFAIL is not an authoritative absence.
  grep -Eq 'status: (NOERROR|NXDOMAIN),' "$work/dig" || fail 'public DNS lookup did not return NOERROR or NXDOMAIN'
  awk -v typ="$typ" '$4==typ {print tolower($5)}' "$work/dig" | sed 's/\.$//' | sort -u
}
if [ "${1:-}" = --check ]; then
  for resolver in one.one.one.one dns.google; do
    answer=$(lookup A "$HOSTNAME" "$resolver") || fail 'public A lookup failed'
    [ "$answer" = "$ip" ] || fail 'public A record does not match EIP'
    answer=$(lookup AAAA "$HOSTNAME" "$resolver") || fail 'public AAAA lookup failed'
    [ -z "$answer" ] || fail 'public AAAA record exists'
  done
  nonce=$(openssl rand -hex 16)
  probe=1
  bash "$AWS_DIR/remote.sh" acme-probe.sh "$nonce" >/dev/null
  code=$(curl -q --silent --noproxy '*' --connect-timeout 5 --max-time 15 --output "$work/body" --write-out '%{http_code}' "http://$HOSTNAME/.well-known/acme-challenge/$nonce") || fail 'ACME probe fetch failed'
  printf '%s' "$nonce" >"$work/expected"
  if [ "$code" != 200 ] || ! cmp -s "$work/body" "$work/expected"; then fail 'ACME probe body mismatch'; fi
  exit 0
fi
[ "$#" = 0 ] || fail 'usage: dns.sh [--check]'
if [ -z "$HOSTED_ZONE_ID" ]; then
  absent=0
  for resolver in one.one.one.one dns.google; do
    answer=$(lookup AAAA "$HOSTNAME" "$resolver") || fail 'public AAAA lookup failed'
    [ -z "$answer" ] || fail 'public AAAA record exists'
    answer=$(lookup A "$HOSTNAME" "$resolver") || fail 'public A lookup failed'
    if [ -z "$answer" ]; then
      absent=1
    else
      [ "$answer" = "$ip" ] || fail 'conflicting public A record; refusing manual DNS instructions'
    fi
  done
  if [ "$absent" = 0 ]; then
    printf '%s\n' 'gkg-box: A record already published'
    exit 0
  fi
  printf "Host: %s\nType: A\nValue: %s\nTTL: 600\nadd it at the domain's DNS provider, then run make box-dns-check\n" "$HOSTNAME" "$ip"
  exit 3
fi
aws route53 get-hosted-zone --id "$HOSTED_ZONE_ID" --output json >"$work/zone"
zone=$(jq -r '.HostedZone.Name' "$work/zone")
case "$HOSTNAME." in *".$zone") ;; *) fail 'hostname is outside the selected zone' ;; esac
jq -r '.DelegationSet.NameServers[] | ascii_downcase | rtrimstr(".")' "$work/zone" | sort -u >"$work/expected-ns"
[ -s "$work/expected-ns" ] || fail 'hosted zone has no public delegation'
for resolver in one.one.one.one dns.google; do
  lookup NS "$zone" "$resolver" >"$work/public-ns"
  cmp -s "$work/expected-ns" "$work/public-ns" || fail 'public delegation does not match the selected hosted zone'
  answer=$(lookup AAAA "$HOSTNAME" "$resolver") || fail 'public AAAA lookup failed'
  [ -z "$answer" ] || fail 'public AAAA record exists'
done
aws route53 list-resource-record-sets --hosted-zone-id "$HOSTED_ZONE_ID" --output json >"$work/records"
jq --arg name "$HOSTNAME." '[.ResourceRecordSets[] | select((.Name|ascii_downcase)==($name|ascii_downcase))]' "$work/records" >"$work/host"
jq -e 'all(.[]; .Type!="AAAA" and .Type!="CNAME")' "$work/host" >/dev/null || fail 'AAAA or CNAME exists for hostname'
if [ "${REPLACE_A:-0}" != 1 ]; then
  jq -e --arg ip "$ip" 'all(.[]; .Type!="A" or ((has("AliasTarget")|not) and (.ResourceRecords|length)==1 and .ResourceRecords[0].Value==$ip))' "$work/host" >/dev/null || fail 'conflicting A record; review before setting REPLACE_A=1'
fi
jq -n --arg host "$HOSTNAME." --arg ip "$ip" '{Changes:[{Action:"UPSERT",ResourceRecordSet:{Name:$host,Type:"A",TTL:600,ResourceRecords:[{Value:$ip}]}}]}' >"$work/batch"
aws route53 change-resource-record-sets --hosted-zone-id "$HOSTED_ZONE_ID" --change-batch "file://$work/batch" >/dev/null
printf '%s\n' 'gkg-box: A record submitted; run dns.sh --check before issuance'
