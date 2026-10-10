# Deployment guide

How to deploy bucket-next to one or many servers. Routesphere-style: **one tenant config file
per tenant, one section per profile (environment), one command pushes every shard of that
profile over SSH.**

Shards never talk to each other at runtime (shard-interleaved counters, shard id in the
Snowflake bits), so the deployer only needs SSH access to each shard's host.

---

## Mental model

| Term | Meaning |
|---|---|
| **Tenant** | One logical cluster, e.g. `link3`. Has its own `deploy/tenants/<tenant>.conf`. |
| **Profile** | An environment within the tenant: `dev`, `staging`, `prod`. One `[section]` per profile, listing every shard. |
| **Shard** | One bucket-next process: systemd unit `bucket-next@<shard_id>`, numbered `1..total_shards`, fixed for life. |
| **Inventory** | SSH details under `deploy/ssh-inventory/<inventory_tenant>/`. One file per host. |

```
deploy/
├── deploy.sh                       # the one command you run
├── tenants/
│   └── <tenant>.conf               # profiles + per-shard server mapping
├── ssh-inventory/
│   └── <inventory_tenant>/
│       ├── hosts/<server-name>     # host=, port=, user=, key=, ...
│       └── keys/                   # SSH keys + uuid key files (gitignored, mode 0600)
└── dependencies/
    ├── bucket-next@.service        # systemd template unit, one instance per shard
    └── remote-setup.sh             # runs on the target host
```

On each target host:

| Path | What |
|---|---|
| `/opt/bucket-next/bucket-next`, `bucket-next-cli` | binaries (shared by all shards on the host) |
| `/etc/bucket-next/shard-<id>.yaml` | that shard's config (0640, may hold the uuid key) |
| `/var/lib/bucket-next/shard-<id>/state.json` | that shard's state file |
| `bucket-next@<id>.service` | that shard's systemd unit |

---

## Prerequisites

**On the machine you run `deploy.sh` from:** Go 1.21+, `ssh`, `scp`.

**On each target host:**
- SSH reachable with a key.
- The SSH user can run `sudo` **without a password** (the setup creates a service user,
  installs a systemd unit, writes to `/opt`, `/etc` and `/var/lib`). Configure this beforehand;
  the scripts never contain a password.
- `systemd` and `curl`.
- No internet access is needed: the static binary is copied in.

---

## One-time setup for a new tenant

### 1. Tenant config — `deploy/tenants/<tenant>.conf`

```ini
[dev]
description           = link3 dev cluster, 3 shards
inventory_tenant      = link3

# Applied to every shard of this profile.
listen_address        = private_ip
listen_port           = 7001
state_path            = /var/lib/bucket-next/shard-{shard_id}/state.json
segment_size          = 1000
segment_refill_watermark = 0.5
snowflake_epoch_ms    = 1704067200000
uuid_key_file         = ssh-inventory/link3/keys/uuid-dev.key

total_shards          = 3
shard1.server         = link3-dev-1
shard2.server         = link3-dev-2
shard3.server         = link3-dev-3
```

| Key | Required | Notes |
|---|---|---|
| `inventory_tenant` | yes | sub-dir of `ssh-inventory/` holding the host files |
| `total_shards` | yes | the cluster size the id space is partitioned for, fixed once; the `shardN.server` keys are numbered `1..K` without gaps with `K <= total_shards` (a declared, not yet deployed shard is added later as a line of its own) |
| `shardN.server` | yes | host-file name for shard N |
| `listen_address` | no | empty = all interfaces; an IP = that IP on every shard; `private_ip` = each shard binds the `private_ip` from its own host file (recommended for servers) |
| `state_path` | no | `{shard_id}` is replaced per shard. Default `/var/lib/bucket-next/shard-{shard_id}/state.json` |
| `uuid_key_file` | no | file with 32/48/64 hex chars, relative to `deploy/` or absolute. Same file for every shard. Unset = built-in key (unique but predictable) |
| `listen_port`, `segment_size`, `segment_refill_watermark`, `clock_drift_tolerance_ms`, `snowflake_epoch_ms` | no | copied into each shard's config |

Every `shardN.server` of a profile must have a host file, even when you deploy with
`--shard`, because the script checks all shards for clashes first.

### 2. SSH inventory — one host file per server

`deploy/ssh-inventory/<inventory_tenant>/hosts/<server-name>`:

```ini
host=10.10.199.11
port=22
user=ubuntu
key=link3-deploy.pem
private_ip=10.10.199.11
# Optional — this shard's port when two shards share the host
#listen_port_override=7002
```

### 3. Keys (never committed)

```bash
cp ~/Downloads/link3-deploy.pem deploy/ssh-inventory/link3/keys/
chmod 600 deploy/ssh-inventory/link3/keys/link3-deploy.pem

# uuid key: once per profile, before the first uuid ID; never change it afterwards
openssl rand -hex 16 > deploy/ssh-inventory/link3/keys/uuid-dev.key
chmod 600 deploy/ssh-inventory/link3/keys/uuid-dev.key
```

`*.pem`, `*.key` and `id_*` under `ssh-inventory/**/keys/` are gitignored. **Back up the uuid
key file**: losing it means uuid entities cannot continue without risking repeats.

---

## Deploying

### Always dry-run first

Prints the plan and the exact config each shard would get (uuid key hidden), without
contacting any host:

```bash
./deploy/deploy.sh link3 dev --dry-run
```

Check each shard's server, port and state path.

### First deploy of a profile — `--init-state`

New shards have no state file, and the service refuses to start without one. Create them on
the first deploy:

```bash
./deploy/deploy.sh link3 dev --init-state
```

`--init-state` only creates **missing** files; it never touches an existing one. Do **not** use
it to "fix" a shard whose state file was lost — restore that file from backup instead.

### Normal deploys

```bash
./deploy/deploy.sh link3 dev                    # every shard, in order
./deploy/deploy.sh link3 prod --shard 4         # one shard
./deploy/deploy.sh link3 staging --skip-build   # reuse bin/ (run `make build` first)
./deploy/deploy.sh --list                       # tenants and profiles
```

---

## What happens

Before contacting any host, the script checks:
- every shard has a host file, and shards are numbered `1..total_shards`;
- no two shards use the **same host + port**, or the **same host + state file**;
- the uuid key file exists and holds valid hex.

Then, per shard:
1. **Build once**: `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build` → `bin/`.
2. **Render** `config.yaml` with this shard's `shard_id`, port and state path, plus the
   profile's settings and uuid key.
3. **Copy** binaries, config, `bucket-next@.service` and `remote-setup.sh` over one multiplexed
   SSH connection.
4. **`remote-setup.sh`** on the host:
   - creates the `bucket-next` user if missing;
   - installs binaries to `/opt/bucket-next/` and the config to `/etc/bucket-next/shard-<id>.yaml`;
   - creates the state directory;
   - moves a legacy `/var/lib/bucket-next/state.json` (from the old single-unit layout) to this
     shard's path if this shard has no file yet — the service then upgrades it and refuses it
     if it belongs to another shard;
   - with `--init-state`, creates a missing state file; without it, a missing file **fails**
     the deploy;
   - retires the legacy `bucket-next.service` unit if present;
   - installs the template unit and restarts `bucket-next@<id>`.
5. **Verify**: reads `/shard-info` on the host — at the address the shard binds — and checks it
   reports this `shardId` and `totalShards`.

The summary lists failed shards. Afterwards, compare `uuidKeyFingerprint` and
`snowflakeEpochMs` in the printed `/shard-info` lines — they must match on every shard.

---

## Verifying a live cluster

```bash
for h in 10.10.199.11 10.10.199.12 10.10.199.13; do curl -s http://$h:7001/shard-info; echo; done
```

Expect distinct `shardId`s and identical `totalShards`, `uuidKeyFingerprint`,
`snowflakeEpochMs`.

On a host:

```bash
systemctl list-units 'bucket-next@*'
sudo systemctl status bucket-next@2
sudo journalctl -u bucket-next@2 -f
```

---

## Two shards on one host

Point two `shardN.server` keys at two host files for the same machine, with different
`listen_port_override` values. Each shard gets its own unit (`bucket-next@1`, `bucket-next@2`),
config and state file (`{shard_id}` in `state_path`):

```ini
# tenants/tcteltd.conf
[dev]
inventory_tenant = tcteltd
total_shards     = 2
state_path       = /var/lib/bucket-next/shard-{shard_id}/state.json
shard1.server    = tcteltd-dev-1a
shard2.server    = tcteltd-dev-1b
```

```ini
# ssh-inventory/tcteltd/hosts/tcteltd-dev-1a      # ssh-inventory/tcteltd/hosts/tcteltd-dev-1b
host=10.10.198.5                                  host=10.10.198.5
listen_port_override=7001                         listen_port_override=7002
```

If two shards would share a port or a state file, `deploy.sh` stops before touching anything.

---

## Backups and rollback

- **Back up** `/var/lib/bucket-next/shard-*/state.json` on every host, and the uuid key files.
  A shard whose state file is lost cannot safely restart (the service refuses); restore the
  file from backup. An older backup is fine **only** if you then move each counter forward
  past anything issued since (the HTTP `reset` endpoint or CLI `reset`).
- **Rollback** a binary by redeploying an older git tag:

  ```bash
  git checkout <previous-tag>
  ./deploy/deploy.sh link3 prod --shard <n>
  git checkout main
  ```

  State files are not touched by a binary swap. Note: a pre-v2 binary cannot read a v2 state
  file, so roll back no further than this release once it has run.

---

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `ERROR: tenant config not found` | Wrong tenant name; run `./deploy/deploy.sh --list`. |
| `ERROR: profile '<x>' not found` | Section missing in the tenant `.conf`. |
| `ERROR: total_shards=N but found M shardN.server entries` | Count mismatch in the `.conf`. |
| `ERROR: shard N: host file not found` | Every shard of the profile needs a host file. |
| `ERROR: shards 1 and 2 both listen on …` | Same host + port: set `listen_port_override`. |
| `ERROR: shards 1 and 2 share state file …` | Put `{shard_id}` in `state_path`. |
| `ERROR: SSH key not found` | Key missing in `ssh-inventory/<tenant>/keys/`, or `key=` wrong. |
| `Permission denied (publickey)` | Wrong key or `user=`, or key not authorized on the host. |
| `sudo: a password is required` | The SSH user lacks passwordless sudo. |
| `[remote] ERROR: state file … does not exist` | First deploy → add `--init-state`. Lost file → restore from backup. |
| unit fails; journal says `does not belong to this config` | Wrong state file for this shard, or `total_shards` / uuid key / epoch changed. Fix the config. |
| unit fails; journal says `clock is … behind the last persisted snowflake mark` | The host clock jumped back. Fix NTP. |

---

## Security notes

- Keys live only under `ssh-inventory/**/keys/` (gitignored). Never commit them.
- Rendered configs on hosts are `0640 root:bucket-next` because they may hold the uuid key.
- The service has **no authentication**. Set `listen_address = private_ip` (or an explicit IP) so
  it never binds a public interface, and firewall the port. The plan line `Bind:` warns when a
  profile binds every interface.
- The scripts contain **no passwords**; they need key-based SSH and passwordless sudo.
