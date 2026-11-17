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
make box-secrets                     # upload required and optional SecureStrings
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
`GKG_AGENT_*`, `GKG_EMBEDDER_*`, `GKG_EXTRACTOR_*`, `GKG_JUDGE_*`, `GKG_FURLS_*`,
`GKG_DOMAIN_*`, `GKG_FACTS_*`, `GKG_INGEST_STOPLIST_FILE`,
the converter limits `GKG_CONVERTER_TIMEOUT_S`, `GKG_CONVERTER_OCR_ENGINE`,
`GKG_CONVERTER_OCR_LANG`, `GKG_CONVERTER_IMAGES`, `GKG_CONVERTER_MAX_IMAGE_MP`,
`GKG_CONVERTER_MAX_RESPONSE_MB` and `DOCLING_MAX_SYNC_WAIT` (never the converter
URL, mode or key, which Compose wires for the box), and the repo's
`EMBED_QUERY_FURL`, `EMBED_PRODUCT_FURL`, `EMBED_API_KEY` and
`EMBED_BATCH_SIZE` settings. Agent and embedder are enabled; use the same
providers as the local installation. Multiline dotenv values are unsupported.
`MEMGRAPH_PASSWORD` and `GKG_API_KEY` are generated only when missing, and remain
stable on reruns. `make box-env ENV_FILE=<source-file>` selects an alternate
source file. It prints key names only.

`box-secrets` removes blank/comment lines and uploads `/$NAME/env` and
`/$NAME/htpasswd` as Standard SecureStrings, refusing either value above
4096 bytes. Optional local `deploy/aws/box.domain-seed.json` and
`deploy/aws/box.stoplist.txt` upload to `/$NAME/domain-seed` and
`/$NAME/stoplist` with the same Standard SecureString limit. An oversized
file fails with its byte count before any upload; a missing optional file is
skipped with a note and does not delete an existing remote parameter.
To stop delivering an optional file, delete its SSM parameter; deleting the local
upload file alone does not delete it (see the teardown/purge command below).
Temporary JSON is private and removed on success and failure.
The remote command exports the validated `GKG_SSM_PREFIX=/$NAME`; the box
refuses a missing or invalid prefix and reads those two required parameter
names into `/opt/gkg/.env` and
`/opt/gkg/deploy/edge/htpasswd` with mode 0600 using temporary files and rename,
on both bootstrap and deploy. It also fetches the optional parameters into
`/opt/gkg/deploy/box-data/domain-seed.json` and `stoplist.txt`, owned by
10001:10001 with mode 0400 in a root-owned 0755 directory. A missing parameter
removes its stale local file. An access-denied optional parameter is also skipped
and its stale local file removed, with a note to run `make box-policy`;
other fetch errors and all required-parameter errors fail the operation. Compose
mounts this directory at `/etc/gkg/data` read-only. Both the data directory
and the private upload files are excluded from Docker builds and Git.
The role allows these four reads and explicitly
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
checkout; build and preflight precede `make up`. A failed checkout restores the previous
checkout; a failed build also rebuilds the previous images before returning
nonzero. Existing containers keep serving during that build. After startup,
curl reads the effective bearer via a private header file and checks loopback
readiness. `.deployed-sha` records the last passing box-side revision. The Mac
prints it only after anonymous `/healthz` and authenticated `/v1/agent/status`
and `/v1/documents` all return 200 through HTTPS within 60 seconds. `/readyz`
is intentionally closed at the edge. A recreate can produce a short 502
window; measure it during live acceptance. The edge renders its nginx and
ModSecurity files from `deploy/edge/` only at container start, so `make up`
labels it with a digest of those files and recreates it whenever one changes;
open streams (ask, MCP) drop once and clients reconnect. When automatic rollback
is allowed (no schema migration and no `NO_ROLLBACK=1`), startup and loopback-readiness
failures restore the previous checkout,
rebuild its images and run `make up` again. Recovery attempts use private logs;
the script exits nonzero with "previous revision restored", or "recovery
required" if any recovery step fails. Failed deploys leave `.deployed-sha`
unchanged. A failed through-edge check still needs operator inspection.

Deploy this script release before any revision that activates schema v4:
SSM initially executes the deploy script already installed on the box.
`gkg preflight` runs read-only on the newly built image with the deployment
environment. Exit 0 continues normally; exit 2 means an older binary with no
preflight command and skips migration. Exit 3 aborts with `seed required`;
all other exits except 10 also abort and restore the previous revision and image
tags before startup. A failure to stop gkg before migration restores them too
and attempts `make up` to restart a stack that may have been partially stopped.
Exit 10 stops gkg, runs `gkg migrate --schema-only` on the new image,
then starts the stack. Migration failure always leaves the new revision in
place and prints recovery instructions; it never rolls back automatically.
After a successful migration, startup, bearer-read and readiness failures also
require manual recovery, even without `NO_ROLLBACK=1`.
If `migrate --schema-only` fails with "unmatched sources", nothing was changed:
extend the seed and re-run `make deploy`; no snapshot restore is needed.

For a schema-changing deployment, first update the existing instance policy
with `make box-policy` (only the inline secrets policy is replaced). Run it
before the first deploy that needs a seed; until then, optional
files that the old policy cannot read are skipped with a note. Set
`GKG_DOMAIN_SEED_FILE=/etc/gkg/data/domain-seed.json` and, when used,
`GKG_INGEST_STOPLIST_FILE=/etc/gkg/data/stoplist.txt` in the source environment;
place the private upload files, run `make box-env`, then `make box-secrets`.
If enabling `GKG_FACTS_REQUIRE_CONFIRMATION=true`, also set `GKG_JUDGE_MODE=all`.
A seedless `gkg serve` or `gkg mcp` start on a document-bearing v3 graph
creates indexes and constraints idempotently before refusing migration.

After the owner's approval for the rebuild and any provider spend, run
`make box-snapshot`, followed by `make deploy REF=<main-commit> NO_ROLLBACK=1`.
That explicit switch disables automatic rollback after startup or readiness
failure. It leaves `.deployed-sha` unchanged and prints recovery steps.

The v4 migration clears every chunk's `duplicate_of` link. The rebuild extracts
every chunk once as its own original, with no dedup borrowing; the extractor
and judge budget estimate must therefore count all chunks, including former
duplicates.

After the v4 deploy, verify `/readyz` and wait for re-extraction to finish.
Then run `gkg judge backfill --budget-calls N` with the owner's approved call
budget. A crash during verification can leave candidates unjudged even when
their document is `done`; backfill judges those candidates before the quality,
retrieval and audit measurements. Also check `/readyz` after any failed v4
deploy, since stopping gkg may have partially succeeded.

`box-snapshot` uses the same host lock, asks Memgraph to `CREATE SNAPSHOT;`
with credentials expanded only from its container environment, and archives
the newest snapshot created by that run to
`/var/backups/gkg/<UTC timestamp>.tar` with mode 0600. Backups are outside
the repository and Docker build context. Snapshot/console failures propagate;
less than twice the snapshot's size in free backup disk causes refusal.
Copy successful backups to the approved private backup destination before
host maintenance; these archives are separate from DLM snapshots.

Recovery after a started migration requires restoring data as well as the
previous revision. Keep gkg stopped and stop Memgraph before changing its
volume. Select the pre-deploy archive from `/var/backups/gkg` and the previous
revision printed by the failed deploy. Preserve the failed volume for diagnosis,
then restore the archived snapshot into a clean Memgraph data volume's
`snapshots` directory with the container's required ownership; do not retain
newer snapshots or WAL files from the failed migration. Restore the previous
checkout, rebuild its images, start the stack, and verify readiness and the
application before resuming writes. Do not start the previous binary against
the migrated data. Rehearse this restore procedure privately before the rebuild.

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
DLM policy, IAM role/profile and all four parameters. Snapshots survive and continue
to cost money; `PURGE_SNAPSHOTS=1` explicitly deletes project-tagged snapshots.
Removing DLM without removing snapshots leaves their cleanup to the operator.
Local private files are retained for deliberate operator cleanup.
