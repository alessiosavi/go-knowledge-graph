#!/usr/bin/env bash
# Destructive operations are restricted to tagged resources and explicit opt-ins.
set -euo pipefail
# shellcheck source=deploy/aws/lib.sh
source "$(dirname "$0")/lib.sh"
load_box_env
assert_account
[ "${YES:-0}" = 1 ] || fail 'teardown requires YES=1'
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
trap 'exit 1' HUP INT TERM
owned() { if tagged_or_die "$1" "$2" 2>/dev/null; then return 0; fi; printf 'gkg-box: skipping missing or foreign %s resource\n' "$1" >&2; return 1; }
instance=$(box_id_get INSTANCE_ID)
if [ -n "$instance" ] && owned ec2 "$instance"; then
  aws_ok_if InvalidInstanceID.NotFound aws ec2 terminate-instances --instance-ids "$instance"
  aws ec2 wait instance-terminated --instance-ids "$instance"
  box_id_set INSTANCE_ID ''
elif [ -n "$instance" ]; then
  # A terminated instance eventually disappears from EC2. Positive absence
  # permits continuing an interrupted teardown; lookup errors still refuse.
  aws ec2 describe-instances --filters "Name=instance-id,Values=$instance" --output json >"$work/instance"
  if jq -e '[.Reservations[].Instances[]] | length==0' "$work/instance" >/dev/null; then box_id_set INSTANCE_ID ''; fi
fi
# Never detach an address or remove its SG while a skipped instance may be live.
[ -z "$(box_id_get INSTANCE_ID)" ] || { printf '%s\n' 'gkg-box: instance remains; dependent cleanup skipped' >&2; exit 0; }
alloc=$(box_id_get EIP_ALLOC); ip=$(box_id_get EIP_IP)
if [ -z "$HOSTED_ZONE_ID" ]; then
  printf 'gkg-box: delete the manual A record for %s at the domain DNS provider\n' "$HOSTNAME"
fi
if [ -n "$alloc" ] && owned ec2 "$alloc"; then
  if [ -n "$HOSTED_ZONE_ID" ]; then
    # Only this name and an exact single-value A record are candidates for DELETE.
    aws route53 list-resource-record-sets --hosted-zone-id "$HOSTED_ZONE_ID" --output json >"$work/records"
    jq --arg name "$HOSTNAME." --arg ip "$ip" '[.ResourceRecordSets[] | select(.Name==$name and .Type=="A" and (.ResourceRecords|length)==1 and .ResourceRecords[0].Value==$ip)]' "$work/records" >"$work/a"
    if [ "$(jq length "$work/a")" = 1 ]; then
      jq '{Changes:[{Action:"DELETE",ResourceRecordSet:.[0]}]}' "$work/a" >"$work/batch"
      aws route53 change-resource-record-sets --hosted-zone-id "$HOSTED_ZONE_ID" --change-batch "file://$work/batch" >/dev/null
    fi
  fi
  aws_ok_if InvalidAllocationID.NotFound aws ec2 release-address --allocation-id "$alloc"
  box_id_set EIP_ALLOC ''; box_id_set EIP_IP ''
fi
sg=$(box_id_get SG_ID)
if [ -n "$sg" ] && owned ec2 "$sg"; then
  aws_ok_if InvalidGroup.NotFound aws ec2 delete-security-group --group-id "$sg"
  box_id_set SG_ID ''
fi
if [ "${PURGE:-0}" = 1 ]; then
  policy=$(box_id_get DLM_POLICY_ID)
  if [ -n "$policy" ] && owned dlm "$policy"; then
    aws_ok_if ResourceNotFoundException aws dlm delete-lifecycle-policy --policy-id "$policy"
    box_id_set DLM_POLICY_ID ''
  fi
  role=$(box_id_get ROLE); profile=$(box_id_get PROFILE)
  if [ -n "$profile" ] && owned profile "$profile"; then
    if [ -n "$role" ] && owned role "$role"; then
      aws_ok_if NoSuchEntity aws iam remove-role-from-instance-profile --instance-profile-name "$profile" --role-name "$role"
    fi
    aws_ok_if NoSuchEntity aws iam delete-instance-profile --instance-profile-name "$profile"
    box_id_set PROFILE ''
  fi
  if [ -n "$role" ] && owned role "$role"; then
    aws_ok_if NoSuchEntity aws iam detach-role-policy --role-name "$role" --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore
    aws_ok_if NoSuchEntity aws iam delete-role-policy --role-name "$role" --policy-name "$NAME-secrets"
    aws_ok_if NoSuchEntity aws iam delete-role --role-name "$role"
    box_id_set ROLE ''
  fi
  for name in "/$NAME/env" "/$NAME/htpasswd"; do
    if owned parameter "$name"; then aws_ok_if ParameterNotFound aws ssm delete-parameter --name "$name"; fi
  done
fi
if [ "${PURGE_SNAPSHOTS:-0}" = 1 ]; then
  aws ec2 describe-snapshots --owner-ids self --filters "Name=tag:project,Values=$NAME" --output json >"$work/snapshots"
  jq -r '.Snapshots[].SnapshotId' "$work/snapshots" >"$work/ids"
  while IFS= read -r id; do
    if owned ec2 "$id"; then aws_ok_if InvalidSnapshot.NotFound aws ec2 delete-snapshot --snapshot-id "$id"; fi
  done <"$work/ids"
fi
printf '%s\n' 'gkg-box: teardown complete; retained snapshots continue to incur storage cost'
