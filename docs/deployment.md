# Deployment guide

How to deploy bucket-next to one or many servers, by the routesphere convention: **the service's config per tenant and
profile, a tenant conf saying where it runs, the shared ssh inventory, and one command** that pushes every shard of the
profile over SSH. The reference with every key: [tools/deploy/README.md](../tools/deploy/README.md).

Shards never talk to each other at runtime (shard-interleaved counters, shard id in the
Snowflake bits), so the deployer only needs SSH access to each shard's host.

---

## Mental model

| Term | Meaning |
|---|---|
| **Tenant (of the deploy tree)** | The INSTALLATION — whose boxes, e.g. `btcl`. One bucket-next per installation. |
| **Served tenants** | The data tenants the service serves: the profile's `tenants: [btcl, …]`. Every call names one (`/api/v2/{tenant}/…`). |
| **Profile** | An environment within the tenant: `bed`, `staging`, `prod`. |
| **Shard** | One bucket-next process: systemd unit `bucket-next@<shard_id>`, numbered `1..total_shards`, fixed for life. |
| **The service's config** | `config/tenants/<tenant>/<profile>/profile-<profile>.yml` — bucket-next's own keys (`tenants` among them), the same for every shard. |
| **The tenant conf** | `tools/deploy/tenant-conf-v2/<tenant>.conf`, one `[profile]` section: which server, which shards are deployed. |
| **Inventory** | The SHARED ssh inventory, by reference: `servers/<inventory_tenant>/hosts/<server>` and its `keys/`. Never copied here. |

```
config/tenants/<tenant>/<profile>/profile-<profile>.yml   # WHAT the service is configured with
tools/deploy/
├── remote-deploy-v2.sh             # the one command you run
├── tenant-conf-v2/<tenant>.conf    # WHERE each profile runs
└── dependencies/
    ├── bucket-next@.service        # systemd template unit, one instance per shard
    └── remote-setup.sh             # runs on the target host
```

On each target host:

| Path | What |
|---|---|
| `/opt/bucket-next/bucket-next`, `bucket-next-cli` | binaries (shared by all shards on the host) |
| `/etc/bucket-next/shard-<id>.yaml` | that shard's config (0640, may hold the uuid key) |
| `/var/lib/bucket-next/shard-<id>/` | that shard's state: `shard.json`, `tenants/<tenant>.json`, `.lock` (held by the running unit) |
| `bucket-next@<id>.service` | that shard's systemd unit |

---

## Prerequisites

**On the machine you run the deploy from:** Go 1.21+, `ssh`, `scp`, and the shared ssh inventory.

**On each target host:**
- SSH reachable with a key.
- The SSH user can run `sudo` **without a password** (the setup creates a service user,
  installs a systemd unit, writes to `/opt`, `/etc` and `/var/lib`). Configure this beforehand;
  the scripts never contain a password.
- `systemd` and `curl`.
- No internet access is needed: the static binary is copied in.

---

## One-time setup for a new tenant or profile

1. **The server** must be in the shared ssh inventory: `servers/<inventory_tenant>/hosts/<server>` with `host=`, `port=`,
   `user=`, `key=`, `private_ip=`. If it is not there, add it THERE (it serves every deploy tool); never a copy here.
2. **The service's config** — `config/tenants/<tenant>/<profile>/profile-<profile>.yml`:

   ```yaml
   total_shards: 2                 # the cluster's size FOR LIFE; identical on every node
   listen_address: 10.10.188.2     # the host's 10.10.x.x service address, or "{private_ip}"; the API has no authentication
   listen_port: 7001
   state_dir: /var/lib/bucket-next/shard-{shard_id}
   tenants: [btcl]                 # one line; tenant-master's tenant ids; a new tenant = one more name
   # legacy_tenant: btcl           # only while a caller still uses the pre-tenant roads (docs/tenancy.md)
   segment_size: 1000
   segment_refill_watermark: 0.5
   ```

3. **The tenant conf** — `tools/deploy/tenant-conf-v2/<tenant>.conf`:

   ```ini
   [bed]
   inventory_tenant = btcl
   server           = radius2
   shards           = 1            ; shard 2 is declared (total_shards 2) and deployed nowhere yet
   ```

4. **A uuid key** (only when uuid ids will be issued): `openssl rand -hex 16` into a file OUTSIDE git, mode 0600, named by
   `uuid_key_file` in the tenant conf. Set it before the first uuid id; it can never change. **Back it up**: losing it
   means uuid entities cannot continue without risking repeats.

---

## Deploying

### Always dry-run first

Prints the plan and the exact config each shard would get (uuid key hidden), without
contacting any host:

```bash
tools/deploy/remote-deploy-v2.sh btcl bed --dry-run
```

Check each shard's server, port and state path.

### First deploy of a profile — `--init-state`

New shards have no state, and the service refuses to start without it. Create it on
the first deploy:

```bash
tools/deploy/remote-deploy-v2.sh btcl bed --init-state
```

`--init-state` only creates **missing** state; it never touches existing state. Do **not** use
it to "fix" a shard whose state was lost — restore it from backup instead. A new tenant needs no `--init-state`: the
shard creates the tenant's file at the next start.

### Normal deploys

```bash
tools/deploy/remote-deploy-v2.sh btcl bed                 # every deployed shard, in order
tools/deploy/remote-deploy-v2.sh btcl bed --shard 1       # one shard
tools/deploy/remote-deploy-v2.sh btcl bed --skip-build    # reuse bin/ (run `make build` first)
tools/deploy/remote-deploy-v2.sh --list                   # tenants and profiles
# add --yes to skip the question before the server is touched
```

---

## What happens

Before contacting any host, the script checks:
- the three files of the tenant + profile exist, and the profile YAML states only keys the service reads (`state_path`
  is refused, naming `state_dir`);
- `tenants` is one line of tenant ids (`^[a-z][a-z0-9]{1,15}$`), none twice, and `legacy_tenant` is one of them;
- every deployed shard is one of `1..total_shards` and its server has a host file in the shared inventory (a shard of
  the cluster that is not deployed is named in the plan as DECLARED, NOT DEPLOYED);
- a profile with `deploy = false` is planned, never deployed;
- no two shards use the **same host + port**, or the **same host + state directory**;
- the uuid key file exists and holds valid hex.

Then, per shard:
1. **Build once**: `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build` → `bin/`.
2. **Render** `config.yaml` from the profile YAML with this shard's `shard_id`, port and state path, and the uuid key.
3. **Copy** binaries, config, `bucket-next@.service` and `remote-setup.sh` over one multiplexed
   SSH connection.
4. **`remote-setup.sh`** on the host:
   - creates the `bucket-next` user if missing;
   - installs binaries to `/opt/bucket-next/` and the config to `/etc/bucket-next/shard-<id>.yaml`;
   - creates the state directory;
   - moves a legacy `/var/lib/bucket-next/state.json` (from the old single-unit layout) into this
     shard's directory if the shard has no state yet — the service then carries it over into `legacy_tenant`, and
     refuses it if it belongs to another shard;
   - **checks the shard's state with the NEW binary, read-only, before installing anything**
     (`bucket-next-cli check` with the staged binary and config) and prints what the start will do — e.g. "carry the
     pre-tenant state.json (format 2, 1 entities) over into tenant btcl, every mark kept". A refusal stops here: nothing
     is installed and the running shard keeps serving. No state: `--init-state` creates it (after the install), without
     it the deploy **fails**;
   - retires the legacy `bucket-next.service` unit if present;
   - installs binaries and config, the template unit, and restarts `bucket-next@<id>`; prints the start's `state` lines
     from the journal.
5. **Verify**: reads `/shard-info` on the host — at the address the shard binds — and checks it
   reports this `shardId`, `totalShards` and the profile's `tenants`.

The summary lists failed shards. Afterwards, compare `uuidKeyFingerprint`, `snowflakeEpochMs` and `tenants` in the
printed `/shard-info` lines — they must match on every shard.

---

## Verifying a live cluster

```bash
for h in 10.10.199.11 10.10.199.12 10.10.199.13; do curl -s http://$h:7001/shard-info; echo; done
```

Expect distinct `shardId`s and identical `totalShards`, `uuidKeyFingerprint`,
`snowflakeEpochMs` and `tenants`. While `legacy_tenant` is set, `v1Calls` / `v1Callers` show who still calls the
pre-tenant roads.

On a host:

```bash
systemctl list-units 'bucket-next@*'
sudo systemctl status bucket-next@2
sudo journalctl -u bucket-next@2 -f
```

---

## Two shards on one host

Deploy both ids and give one its own port; each shard gets its own unit (`bucket-next@1`, `bucket-next@2`), config and
state directory (`{shard_id}` in `state_dir`):

```ini
# tools/deploy/tenant-conf-v2/<tenant>.conf
[dev]
inventory_tenant   = <tenant>
server             = <server>
shards             = 1,2
shard2.listen_port = 7002
```

Two shards on two hosts: `shard2.server = <other-server>`. If two shards would share a port or a state directory, the
script stops before touching anything.

---

## Backups and rollback

- **Back up** `/var/lib/bucket-next/shard-*/` (`shard.json` and `tenants/`) on every host, and the uuid key files.
  A shard whose state is lost cannot safely restart (the service refuses — a lost tenant file too); restore it from
  backup. An older backup is fine **only** if you then move each counter forward past anything issued since (the HTTP
  `reset` endpoint or CLI `reset`). A `shard.json` older than a tenant's file: `bucket-next-cli adopt-tenant <t>`.
- **Rollback** a binary by redeploying an older git tag:

  ```bash
  git checkout <previous-tag>
  tools/deploy/remote-deploy-v2.sh <tenant> <profile> --shard <n>
  git checkout main
  ```

  State is not touched by a binary swap. A pre-tenant binary cannot read the tenant-first state: to roll back past the
  tenant-first release, stop the shard and run `bucket-next-cli -config /etc/bucket-next/shard-<n>.yaml -tenant <t>
  export-v2` first — it writes `state.json` from the CURRENT marks for the old binary. Never put
  `state.json.migrated-v2` back: its marks are behind. See [tenancy.md](tenancy.md#the-route-back).

---

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `ERROR: tenant conf not found` | Wrong tenant name; run `tools/deploy/remote-deploy-v2.sh --list`. |
| `ERROR: profile '<x>' not found` | Section missing in the tenant conf. |
| `ERROR: the service's config for <t>/<p> is missing` | No `config/tenants/<t>/<p>/profile-<p>.yml`. |
| `ERROR: ... is not a key bucket-next reads` | A mistyped key in the profile YAML. |
| `ERROR: ... states shard_id` | `shard_id` belongs to the deploy (the tenant conf's `shards`), not the profile YAML. |
| `ERROR: [p].shards: 'N' is not a shard of this cluster` | An id beyond `total_shards`. |
| `ERROR: shard N: no host file for server` | The server is not in the shared inventory; add it there. |
| `ERROR: shards 1 and 2 both listen on …` | Same host + port: set `shard<N>.listen_port`. |
| `ERROR: shards 1 and 2 share the state directory …` | Put `{shard_id}` in `state_dir`. |
| `ERROR: [p].deploy = false` | The profile is declared, not deployed. |
| `ERROR: ssh key not found` | The key named by the host file is not under the inventory's `keys/`. |
| `Permission denied (publickey)` | Wrong key or `user=`, or key not authorized on the host. |
| `sudo: a password is required` | The SSH user lacks passwordless sudo. |
| `[remote] ERROR: shard N has no state in …` | First deploy → add `--init-state`. Lost state → restore from backup. |
| `[remote] ERROR: the new binary would refuse this state` | The `check` line above says why; nothing was installed, the shard runs as it ran. |
| `does not belong to this config` | Wrong state for this shard, or `total_shards` / uuid key / epoch changed. Fix the config. |
| `is a pre-tenant state file … no tenant owns its counters` | Set `legacy_tenant` to the tenant that owns them (one of `tenants`). |
| `is on this shard's roster but its state file … is missing` | A tenant's file was lost: restore it from backup. |
| `… is not on this shard's roster: it was not created here` | `shard.json` is older than the tenant's file: `bucket-next-cli adopt-tenant <t>` if the file is this shard's own. |
| `ERROR: … states state_path` | Replace it with `state_dir: /var/lib/bucket-next/shard-{shard_id}`. |
| `/shard-info … (want …, "tenants":[…])` | A shard serves other tenants than the profile: deploy it again. |
| unit fails; journal says `clock is … behind the last persisted snowflake mark` | The host clock jumped back. Fix NTP. |
