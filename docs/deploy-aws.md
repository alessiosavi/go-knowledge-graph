# AWS box operations

The Mac drives AWS CLI v2; the AL2023 x86_64 box runs Docker and accepts SSM
Session Manager and Run Command access. No SSH key or SSH ingress is created.
Provisioning and DNS writes require the owner's explicit go. Merge and review
script changes before using them against AWS.

On the Mac install Bash, make, AWS CLI v2, jq, curl, dig and OpenSSL. The scripts
support the system Bash 3.2 and make 3.81. Sign into the selected SSO profile.
Copy `deploy/aws/box.env.example` to untracked `deploy/aws/box.env`, set mode
0600, and fill in the account, region, VPC, subnet, hostname and public
repository URL. Leave `HOSTED_ZONE_ID` empty for DNS managed at your domain
provider, or set it to a delegated Route 53 public zone. Only this key accepts
an empty value. Use unquoted `KEY=value` data; unknown or duplicate keys are
refused. `NAME` defaults to `gkg` and must match `^[a-z][a-z0-9-]{1,30}$`.
It selects all resource names, ownership tags and SSM paths; keep it stable
for the lifetime of the box. The subnet must have public internet routing.
Provisioning creates the DLM default snapshot role if it is missing, with a
bounded retry for IAM propagation, and preserves an existing role's IAM ARN
(including the console-created `service-role/` path). The scripts do not create a VPC, subnet or
hosted zone.

The first deployment runs in this order, using a reviewed main commit for REF:

```sh
make box-env                         # local .env -> box.secrets.env
make box-login                       # create the deploy basic-auth login
make box-provision                   # creates billable resources: owner go first
make box-secrets                     # upload the two SecureStrings
make box-bootstrap REF=<main-commit>
make box-dns                         # manual instructions, or Route 53 write with owner go
make box-dns-check                   # public A/AAAA and external ACME webroot probe
bash deploy/aws/remote.sh --cert      # issue, install and check the live certificate
```

After issuance, edit `EDGE_HSTS_MAX_AGE` in `deploy/aws/box.secrets.env` to
31536000, then run `make box-secrets` and `make deploy REF=<main-commit>`.
`make box-env` preserves this setting on reruns. Until issuance it defaults to
zero. Basic-auth users have full application access. Add human users from their
terminal with `make passwd LOGIN=<name> EDGE_HTPASSWD=deploy/aws/box.htpasswd`,
then upload and deploy again.

`box-provision` discovers tagged resources even if `.box-ids` is lost. It
refuses ambiguous EIPs/instances, foreign roles/profiles, unexpected security
group ingress, and EIPs attached elsewhere. Reused instances must also have the
required IMDS settings, the selected subnet/VPC, exactly the guarded SG and the
expected instance profile. A failed launch retains its client
token for retry. The instance is t3a.xlarge with a 100 GB encrypted gp3 root
volume, IMDSv2 required and hop limit one. Only IPv4 tcp/80 and tcp/443 are
public. Keep source/destination checking enabled. The security group is
`$NAME-sg`; the role/profile are `$NAME-ssm-role` and
`$NAME-ssm-profile`. Tags `project` and `Name` both equal `$NAME`. Resource ids
and the client token are saved atomically
with mode 0600 in `deploy/aws/.box-ids`.

With an empty `HOSTED_ZONE_ID`, `box-dns` checks A and AAAA through two public
resolvers. Any AAAA, conflicting A, or lookup error causes refusal. If A is
absent it prints one instruction block with the host, type A, EIP and TTL 600,
and exits 3 without an AWS write. Add the record at the domain's DNS provider,
then run `make box-dns-check`. A matching record reports "already published"
and exits 0. `REPLACE_A` does not override manual DNS conflicts.

With a zone configured, `box-dns` verifies the selected zone's NS against
public DNS, then writes exactly one A record. A conflicting A needs explicit
`make box-dns REPLACE_A=1`; an AAAA
or CNAME is always refused. DNS errors are not treated as absence. `--check`
queries two public resolvers and writes a nonce through the certbot service's
writable ACME volume, fetches it from the Mac and removes it even after failure.
It must pass before certificate issuance. It does not change the delegation or
other names in the shared zone.

Secrets stay in ignored mode-0600 files: `box.secrets.env`, `box.htpasswd` and
`box.netrc`. The last contains only the generated deploy login for curl. The
source provider settings are copied as single-line dotenv assignments, including
quotes and dollar signs; they are never sourced as shell code. The allowlist is
`GKG_AGENT_*`, `GKG_EMBEDDER_*`, `GKG_EXTRACTOR_*`, `GKG_JUDGE_*`, `GKG_FURLS_*`
and the repo's `EMBED_QUERY_FURL`, `EMBED_PRODUCT_FURL`, `EMBED_API_KEY` and
`EMBED_BATCH_SIZE` settings. Agent and embedder are enabled; use the same
providers as the local installation. Multiline dotenv values are unsupported.
`MEMGRAPH_PASSWORD` and `GKG_API_KEY` are generated only when missing, and remain
stable on reruns. `make box-env ENV_FILE=<source-file>` selects an alternate
source file. It prints key names only.

`box-secrets` removes blank/comment lines and uploads `/$NAME/env` and
`/$NAME/htpasswd` as Standard SecureStrings, refusing either value above
4096 bytes. Temporary JSON is private and removed on success and failure.
The remote command exports the validated `GKG_SSM_PREFIX=/$NAME`; the box
refuses a missing or invalid prefix and reads exactly those two parameter
names into `/opt/gkg/.env` and
`/opt/gkg/deploy/edge/htpasswd` with mode 0600 using temporary files and rename,
on both bootstrap and deploy. The role allows those two reads and explicitly
denies `ssm:GetParameter*` for all other resources. This also blocks
`/aws/service/*`, so the Mac resolves the AMI. Decryption uses the `aws/ssm` key
policy. No secrets enter SSM command payloads or curl arguments. Docker access
is root-equivalent: `docker inspect` exposes container environment values.
Use SSM Session Manager for administration; do not dump env, Compose config,
nginx config or private build logs into shared output.

Rotate provider settings with `make box-env`, or rotate a human login with
`make passwd LOGIN=<name> EDGE_HTPASSWD=deploy/aws/box.htpasswd`; then run
`make box-secrets` and `make deploy REF=<main-commit>`. To rotate the application
or database password, deliberately edit the corresponding stable key in
`box.secrets.env`, upload and deploy. Database password changes may require
coordinated database administration; test that procedure before rotation.

Deploys run under `/run/gkg-ops.lock`. Fetch and commit validation precede
checkout; build precedes `make up`. A failed checkout restores the previous
checkout; a failed build also rebuilds the previous images before returning
nonzero. Existing containers keep serving during that build. After startup,
curl reads the effective bearer via a private header file and checks loopback
readiness. `.deployed-sha` records the last passing box-side revision. The Mac
prints it only after anonymous `/healthz` and authenticated `/v1/agent/status`
and `/v1/documents` all return 200 through HTTPS within 60 seconds. `/readyz`
is intentionally closed at the edge. A recreate can produce a short 502
window; measure it during live acceptance. Startup and loopback-readiness
failures restore the previous checkout,
rebuild its images and run `make up` again. Recovery attempts use private logs;
the script exits nonzero with "previous revision restored", or "recovery
required" if any recovery step fails. Failed deploys leave `.deployed-sha`
unchanged. A failed through-edge check still needs operator inspection.

The box uses Docker's static x86_64 Engine 29.9.3 binaries with checksum-pinned
Compose 5.7.0 and buildx 0.38.2. `install-docker.sh`, called by bootstrap and
`make box-docker`, refuses installed `docker`, `docker-ce`, `containerd`,
`containerd.io` or `runc` RPMs. It verifies all three downloads before installing
Docker files, installs the matched runtime into `/usr/bin` and the plugins into
`/usr/local/lib/docker/cli-plugins`. Vendored Apache-2.0 systemd units record
their pinned upstream URLs. It configures kernel modules and IP forwarding,
enables containerd and Docker, and verifies the Server and Compose versions.
Unchanged binaries and daemon configuration do not restart Docker; changed
runtime binaries also restart containerd.

To bump versions, change the URLs and SHA-256 pins in `install-docker.sh`,
refresh the vendored units from the corresponding upstream tags, and update
tests and documentation in a PR. Verify the release checksums and rehearse
installation before merging. Then run `make deploy REF=<reviewed-main-commit>`
to place the new installer on the box, followed by `make box-docker` during a
maintenance window. A changed Engine restarts Docker, which stops every
container; restart policies bring them back without Compose's dependency
order, so finish with `make deploy REF=<the same commit>` to reconcile the stack
and re-run the through-edge check. Static Docker and its plugins are updated
through this workflow, independently of RPM OS updates.

Bootstrap configures json-file rotation (50m, three files), 4 GiB swap and the
stack/renewal systemd units. Boot runs `make up-cached` with `--no-build --pull
never` and a 900-second limit. Renewal runs twice daily with up to an hour's
jitter and uses `make renew` for fingerprint convergence. Verify live renewal
with a certbot `renew --dry-run` using the live lineage/webroot, followed by
`make renew` and the served/lineage fingerprint check.

AL2023 repositories are release-locked. Monthly, run `make box-update` in a
maintenance window: it runs `dnf upgrade --releasever=latest -y`, then schedules
`systemd-run --on-active=15 /usr/bin/systemctl reboot` so the SSM invocation can
finish successfully. The Mac waits up to 15 minutes for SSM Online with a
LastPingDateTime newer than the command start, then runs the same HTTPS edge
checks as deploy with a separate 900-second deadline to match the boot service's
startup limit. Deploy keeps its 60-second edge-check deadline. `BOX_CHECK_TIMEOUT`
may shorten either deadline but cannot exceed 900 seconds for update or 60 seconds
for deploy. Failure to reconnect or pass those checks returns nonzero. Updates
are not automatic.

The volume has `$NAME-backup=daily`. DLM creates daily crash-consistent snapshots
and retains seven. The pinned Memgraph image enables WAL, recovery on startup,
a snapshot every 300 seconds and a snapshot on clean exit, retaining three
snapshots. The target recovery loss is at most about five minutes of graph
writes; documents can be re-ingested. SIGKILL recovery, reboot recovery, the
first daily snapshot and a restore drill remain live acceptance checks.

`make box-teardown YES=1` terminates and waits, removes only a matching A record
from Route 53 when a zone is configured, releases the EIP and deletes the SG.
For manual DNS it skips Route 53 and reminds you to delete the manual record
at the domain's provider. Every deletion checks the project tag;
foreign or missing resources are skipped. If the instance is skipped, dependent
cleanup is skipped too. Rerun after an interruption. `PURGE=1` also removes the
DLM policy, IAM role/profile and both parameters. Snapshots survive and continue
to cost money; `PURGE_SNAPSHOTS=1` explicitly deletes project-tagged snapshots.
Removing DLM without removing snapshots leaves their cleanup to the operator.
Local private files are retained for deliberate operator cleanup.
