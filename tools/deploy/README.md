# bucket-next — deploy (the routesphere convention)

One command deploys every shard of a tenant's profile. Three trees, each with one concern:

```
config/tenants/<tenant>/<profile>/profile-<profile>.yml   WHAT   the service's own config, stated once for every shard
tools/deploy/tenant-conf-v2/<tenant>.conf   [<profile>]   WHERE  inventory_tenant, server, which shards are deployed
the SHARED ssh inventory (by reference, never a copy)     REACH  servers/<inventory_tenant>/hosts/<server>
tools/deploy/remote-deploy-v2.sh <tenant> <profile>       the one command
tools/deploy/dependencies/                                the systemd template unit + what runs on the host
```

The inventory is `/home/mustafa/telcobright-projects/routesphere/routesphere-core/tools/ssh-automation/servers`
(`BUCKET_NEXT_SSH_INVENTORY=<path>` overrides it). A host file there gives `host=`, `port=`, `user=`, `key=`
(a file under `<inventory_tenant>/keys/`) and `private_ip=`. No key and no password is in this repository.

## Usage

```bash
tools/deploy/remote-deploy-v2.sh --list                       # tenants and their profiles
tools/deploy/remote-deploy-v2.sh btcl bed --dry-run           # ALWAYS first: the plan + each shard's rendered config; no ssh
tools/deploy/remote-deploy-v2.sh btcl bed --init-state --yes  # FIRST deploy of a shard: create its missing state file
tools/deploy/remote-deploy-v2.sh btcl bed --yes               # later deploys
tools/deploy/remote-deploy-v2.sh btcl bed --shard 1           # one shard
tools/deploy/remote-deploy-v2.sh btcl bed --skip-build        # ship the binaries already in bin/
```

Phases: args · validate (the three files exist) · tenant conf · the service's config · ssh from the inventory · the plan ·
build (`GOOS=linux GOARCH=amd64 CGO_ENABLED=0`, static) · ship + install + (first time) init-state + start · health
(`/health` 200 and `/shard-info` reporting exactly the `shardId` and `totalShards` that were deployed).

## The service's config — `config/tenants/<tenant>/<profile>/profile-<profile>.yml`

bucket-next's own YAML ([docs/configuration.md](../../docs/configuration.md)), the same for every shard of the profile:
`total_shards`, `listen_address`, `listen_port`, `state_path`, `segment_size`, `segment_refill_watermark`,
`clock_drift_tolerance_ms`, `snowflake_epoch_ms`. Any other key is refused (a mistyped key never becomes a silent
default). The deploy writes `shard_id` (and `uuid_key`) itself, and replaces two words inside a value:

| word | becomes |
|---|---|
| `{shard_id}` | the shard's number — keep it in `state_path` so two shards on one host never share a file |
| `{private_ip}` | the `private_ip` of the shard's host in the inventory — `listen_address: "{private_ip}"` |

The API has no authentication: bind `127.0.0.1` when the only client is on the same host, a private address otherwise.

## Where it runs — `tools/deploy/tenant-conf-v2/<tenant>.conf`

| key (per `[profile]`) | required | meaning |
|---|---|---|
| `inventory_tenant` | yes | the tenant folder of the ssh inventory |
| `server` | yes | the host of every shard of the profile, unless a shard says otherwise |
| `shards` | no | which shard ids are deployed (comma list); default = `1..total_shards` |
| `shard<N>.server` | no | another host for shard N |
| `shard<N>.listen_port` | no | another port for shard N (two shards on one host) |
| `uuid_key_file` | no | a file OUTSIDE git with the cluster's uuid key (32/48/64 hex); unset = the built-in key |
| `deploy` | no | `false` = declared only: `--dry-run` plans, a real deploy is refused |
| `description` | no | one line for the plan |

**Declared, not deployed.** `total_shards` fixes the cluster's size for life — the state file is bound to it and the id
space is split by it — so a cluster that will grow is declared at its final size, and `shards` names the ones that run
today. The plan says `DECLARED, NOT DEPLOYED: 2`. A shard added later is one more id in `shards` (or a `shard<N>.server`)
and its own first `--init-state`; the count never changes.

Two shards on one host: `shards = 1,2` and `shard2.listen_port = 7002`. The script stops before touching anything when
two shards would share a host + port or a host + state file.

## The state file, and `--init-state`

A shard refuses to start without its state file. `--init-state` creates it — only where it is MISSING, and it is meant
for the first deploy of a brand-new shard. After a shard has minted, its file is never initialised again: a fresh file
would re-issue ids. A lost file is restored from backup (then every counter is moved past anything issued since).
Back up `/var/lib/bucket-next/shard-*/state.json`.

## On the host

| path | what |
|---|---|
| `/opt/bucket-next/bucket-next`, `bucket-next-cli` | the binaries (shared by every shard on the host) |
| `/opt/bucket-next/version-shard-<id>.txt` | commit, tenant, profile, binary digest, when and by whom |
| `/etc/bucket-next/shard-<id>.yaml` | that shard's rendered config (root:bucket-next 0640) |
| `/var/lib/bucket-next/shard-<id>/state.json` | that shard's state |
| `bucket-next@<id>.service` | the unit: `User=bucket-next`, `NoNewPrivileges`, `ProtectSystem=full`, `ProtectHome`, `StateDirectory` |

The ssh user needs passwordless `sudo`; the host needs `systemd` and `curl`. Nothing is downloaded on the host.

## Tenants

| tenant | profile | where | state |
|---|---|---|---|
| btcl | bed | radius2, `127.0.0.1:7001`, shard 1 of a declared 2 | the wifi bed; its client is `prime-context-core-s2` (`primecontext.ids.shards=http://127.0.0.1:7001`) |
| btcl | prod | radius1 | DECLARED, NOT DEPLOYED (`deploy = false`) |
