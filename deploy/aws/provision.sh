#!/usr/bin/env bash
# Idempotent provisioning; requires the owner's explicit launch approval.
set -euo pipefail
# shellcheck source=deploy/aws/lib.sh
source "$(dirname "$0")/lib.sh"
load_box_env
assert_account
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
trap 'exit 1' HUP INT TERM
# Discover every cardinality before creating anything. Tags recover lost metadata.
instances=$(aws ec2 describe-instances --filters "Name=tag:project,Values=$NAME" Name=instance-state-name,Values=pending,running,stopping,stopped,shutting-down --output json | jq '[.Reservations[].Instances[]]')
instance=$(one_or_empty "$instances" InstanceId)
stored=$(box_id_get INSTANCE_ID)
if [ -n "$stored" ] && [ "$stored" != "$instance" ]; then
  detail=$(aws ec2 describe-instances --filters "Name=instance-id,Values=$stored" --output json)
  state=$(printf '%s' "$detail" | jq -r '.Reservations[].Instances[].State.Name')
  case "$state" in ''|terminated) ;; *) fail 'stored instance conflicts with project discovery' ;; esac
fi
addresses=$(aws ec2 describe-addresses --filters "Name=tag:project,Values=$NAME" --output json | jq '.Addresses')
alloc=$(one_or_empty "$addresses" AllocationId)
if [ -n "$alloc" ]; then
  owner=$(printf '%s' "$addresses" | jq -r '.[0].InstanceId // empty')
  eni=$(printf '%s' "$addresses" | jq -r '.[0].NetworkInterfaceId // empty')
  cfn=$(printf '%s' "$addresses" | jq -r '.[0].Tags[]? | select(.Key=="aws:cloudformation:stack-id") | .Value')
  if [ -n "$cfn" ] || { [ -n "$eni" ] && { [ -z "$instance" ] || [ "$owner" != "$instance" ]; }; }; then fail 'EIP is infrastructure-owned or associated elsewhere'; fi
  association=$(printf '%s' "$addresses" | jq -r '.[0].AssociationId // empty')
  if [ -n "$association" ] && [ -z "$owner" ]; then fail 'EIP association is not an instance owned by this project'; fi
  [ -z "$owner" ] || [ "$owner" = "$instance" ] || fail 'EIP belongs to another instance'
fi
sgs=$(aws ec2 describe-security-groups --filters "Name=vpc-id,Values=$VPC_ID" "Name=group-name,Values=$NAME-sg" --output json | jq '.SecurityGroups')
sg=$(one_or_empty "$sgs" GroupId)
exact_ingress() {
  aws ec2 describe-security-groups --group-ids "$sg" --output json >"$work/sg"
  jq -e '.SecurityGroups[0].IpPermissions | length==2 and (map(select(.IpProtocol=="tcp" and .FromPort==.ToPort and (.FromPort==80 or .FromPort==443) and (.IpRanges|length)==1 and .IpRanges[0].CidrIp=="0.0.0.0/0" and (.Ipv6Ranges|length)==0 and (.PrefixListIds|length)==0 and (.UserIdGroupPairs|length)==0)) | map(.FromPort) | sort)==[80,443]' "$work/sg" >/dev/null || fail 'security group ingress must be exactly IPv4 tcp/80 and tcp/443'
}
if [ -n "$sg" ]; then
  tagged_or_die ec2 "$sg" || fail 'security group is not project-owned'
  aws ec2 describe-security-groups --group-ids "$sg" --output json >"$work/sg"
  # A partially created group may lack a required port. Refuse extras first,
  # then add only missing ports so a failed prior run can converge.
  jq -e '.SecurityGroups[0].IpPermissions | all(.[]; .IpProtocol=="tcp" and .FromPort==.ToPort and (.FromPort==80 or .FromPort==443) and (.IpRanges|length)==1 and .IpRanges[0].CidrIp=="0.0.0.0/0" and (.Ipv6Ranges|length)==0 and (.PrefixListIds|length)==0 and (.UserIdGroupPairs|length)==0)' "$work/sg" >/dev/null || fail 'security group has unexpected ingress'
  for port in 80 443; do
    if ! jq -e --argjson port "$port" '.SecurityGroups[0].IpPermissions | any(.FromPort==$port)' "$work/sg" >/dev/null; then
      aws_ok_if InvalidPermission.Duplicate aws ec2 authorize-security-group-ingress --group-id "$sg" --protocol tcp --port "$port" --cidr 0.0.0.0/0
    fi
  done
  exact_ingress
fi
if [ -z "$sg" ]; then
  sg=$(aws ec2 create-security-group --group-name "$NAME-sg" --vpc-id "$VPC_ID" --description 'gkg edge only; SSM access' --tag-specifications "ResourceType=security-group,Tags=[{Key=project,Value=$NAME},{Key=Name,Value=$NAME}]" --output json | jq -r '.GroupId')
  box_id_set SG_ID "$sg"
  for port in 80 443; do aws_ok_if InvalidPermission.Duplicate aws ec2 authorize-security-group-ingress --group-id "$sg" --protocol tcp --port "$port" --cidr 0.0.0.0/0; done
  exact_ingress
fi
box_id_set SG_ID "$sg"
role=$NAME-ssm-role; profile=$NAME-ssm-profile
if result=$(aws iam get-role --role-name "$role" --output json 2>&1); then
  tagged_or_die role "$role" || fail 'IAM role is not project-owned'
else
  case "$result" in *NoSuchEntity*) ;; *) fail 'cannot inspect IAM role' ;; esac
  aws_ok_if EntityAlreadyExists aws iam create-role --role-name "$role" --tags "Key=project,Value=$NAME" "Key=Name,Value=$NAME" --assume-role-policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}'
fi
box_id_set ROLE "$role"
aws iam attach-role-policy --role-name "$role" --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore >/dev/null
jq -n --arg prefix "arn:aws:ssm:$AWS_REGION:$EXPECTED_ACCOUNT:parameter/$NAME/" '{Version:"2012-10-17",Statement:[{Effect:"Allow",Action:["ssm:GetParameter"],Resource:[($prefix+"env"),($prefix+"htpasswd")]},{Effect:"Deny",Action:["ssm:GetParameter*"],NotResource:[($prefix+"env"),($prefix+"htpasswd")]}]}' >"$work/policy"
aws iam put-role-policy --role-name "$role" --policy-name "$NAME-secrets" --policy-document "file://$work/policy" >/dev/null
if result=$(aws iam get-instance-profile --instance-profile-name "$profile" --output json 2>&1); then
  tagged_or_die profile "$profile" || fail 'IAM profile is not project-owned'
  roles=$(printf '%s' "$result" | jq -r '.InstanceProfile.Roles[].RoleName')
  [ -z "$roles" ] || [ "$roles" = "$role" ] || fail 'profile has a different role'
else
  case "$result" in *NoSuchEntity*) ;; *) fail 'cannot inspect IAM profile' ;; esac
  aws_ok_if EntityAlreadyExists aws iam create-instance-profile --instance-profile-name "$profile" --tags "Key=project,Value=$NAME" "Key=Name,Value=$NAME"
  roles=
fi
if [ -z "$roles" ]; then aws iam add-role-to-instance-profile --instance-profile-name "$profile" --role-name "$role" >/dev/null; sleep 15; fi
box_id_set PROFILE "$profile"
if [ -z "$alloc" ]; then
  addresses=$(aws ec2 allocate-address --domain vpc --tag-specifications "ResourceType=elastic-ip,Tags=[{Key=project,Value=$NAME},{Key=Name,Value=$NAME}]" --output json | jq '[.]')
  alloc=$(printf '%s' "$addresses" | jq -r '.[0].AllocationId')
fi
ip=$(printf '%s' "$addresses" | jq -r '.[0].PublicIp')
box_id_set EIP_ALLOC "$alloc"; box_id_set EIP_IP "$ip"
if [ -z "$instance" ]; then
  token=$(box_id_get CLIENT_TOKEN)
  if [ -z "$token" ]; then token=$(openssl rand -hex 16); box_id_set CLIENT_TOKEN "$token"; fi
  ami=$(aws ssm get-parameter --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64 --output json | jq -r '.Parameter.Value')
  mappings=$(jq -nc --argjson size "$DISK_GB" '[{DeviceName:"/dev/xvda",Ebs:{VolumeSize:$size,VolumeType:"gp3",Encrypted:true,DeleteOnTermination:true}}]')
  instance=$(aws ec2 run-instances --client-token "$token" --image-id "$ami" --instance-type "$INSTANCE_TYPE" --subnet-id "$SUBNET_ID" --security-group-ids "$sg" --iam-instance-profile "Name=$profile" --block-device-mappings "$mappings" --metadata-options HttpTokens=required,HttpEndpoint=enabled,HttpPutResponseHopLimit=1 --tag-specifications "ResourceType=instance,Tags=[{Key=project,Value=$NAME},{Key=Name,Value=$NAME}]" "ResourceType=volume,Tags=[{Key=project,Value=$NAME},{Key=Name,Value=$NAME},{Key=$NAME-backup,Value=daily}]" --output json | jq -r '.Instances[0].InstanceId')
fi
box_id_set INSTANCE_ID "$instance"
state=$(aws ec2 describe-instances --instance-ids "$instance" --output json | jq -r '.Reservations[].Instances[].State.Name')
case "$state" in stopping|shutting-down) fail 'instance is transitioning; retry when stable' ;; stopped) fail 'instance is stopped; start it explicitly and retry' ;; esac
aws ec2 wait instance-running --instance-ids "$instance"
aws ec2 describe-instances --instance-ids "$instance" --output json >"$work/instance"
jq -e '.Reservations[0].Instances[0].SourceDestCheck==true' "$work/instance" >/dev/null || fail 'SourceDestCheck must be true'
jq -e --arg sg "$sg" --arg subnet "$SUBNET_ID" --arg vpc "$VPC_ID" --arg profile "arn:aws:iam::$EXPECTED_ACCOUNT:instance-profile/$profile" '
  .Reservations[0].Instances[0] | .MetadataOptions.HttpTokens=="required" and .MetadataOptions.HttpPutResponseHopLimit==1 and (.SecurityGroups|map(.GroupId))==[$sg] and .SubnetId==$subnet and .VpcId==$vpc and .IamInstanceProfile.Arn==$profile
' "$work/instance" >/dev/null || fail 'instance metadata, network or IAM profile does not match the guarded configuration'
if [ "${owner:-}" != "$instance" ]; then aws ec2 associate-address --instance-id "$instance" --allocation-id "$alloc" --no-allow-reassociation >/dev/null; fi
policies=$(aws dlm get-lifecycle-policies --output json | jq --arg name "$NAME" '[.Policies[] | select(.Tags.project==$name)]')
policy=$(one_or_empty "$policies" PolicyId)
if [ -z "$policy" ]; then
  jq -n --arg backup "$NAME-backup" '{PolicyType:"EBS_SNAPSHOT_MANAGEMENT",ResourceTypes:["VOLUME"],TargetTags:[{Key:$backup,Value:"daily"}],Schedules:[{Name:"daily",CreateRule:{Interval:24,IntervalUnit:"HOURS",Times:["03:00"]},RetainRule:{Count:7},CopyTags:true}]}' >"$work/dlm"
  if result=$(aws iam get-role --role-name AWSDataLifecycleManagerDefaultRole --output json 2>&1); then
    # Console-created roles have a service-role/ path; CLI-created roles do not.
    execution_role_arn=$(printf '%s' "$result" | jq -er '.Role.Arn | select(type=="string" and length>0)') || fail 'DLM default role ARN missing'
  else
    case "$result" in
      *NoSuchEntity*)
        aws dlm create-default-role --resource-type snapshot >"$work/default-role" 2>&1 || fail 'cannot create DLM default role'
        # create-default-role uses the documented CLI role path and has no output.
        execution_role_arn="arn:aws:iam::$EXPECTED_ACCOUNT:role/AWSDataLifecycleManagerDefaultRole"
        ;;
      *) fail 'cannot inspect DLM default role' ;;
    esac
  fi
  # IAM propagation can temporarily prevent DLM from assuming the new role.
  for ((attempt=1;attempt<=12;attempt++)); do
    if aws dlm create-lifecycle-policy --execution-role-arn "$execution_role_arn" --description 'gkg daily EBS snapshots retain seven' --state ENABLED --tags "project=$NAME,Name=$NAME" --policy-details "file://$work/dlm" --output json >"$work/dlm-result" 2>"$work/dlm-error"; then
      policy=$(jq -er '.PolicyId' "$work/dlm-result")
      break
    fi
    # Retry only a role-validation failure, never permissions or
    # unrelated invalid requests. Keep AWS diagnostics private.
    grep -Eiq '(execution role|executionRoleArn).*(not.*(assum|usable|valid)|cannot.*assum)|unable to assume.*role' "$work/dlm-error" || fail 'DLM policy creation failed'
    [ "$attempt" -lt 12 ] || fail 'DLM execution role not usable after bounded retries'
    sleep 5
  done
fi
box_id_set DLM_POLICY_ID "$policy"
printf '%s\n' 'gkg-box: provisioning complete (ids saved privately)'
