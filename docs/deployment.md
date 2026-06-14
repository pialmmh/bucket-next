# Deployment guide

How to deploy bucket-next to one or many servers. The deploy system is routesphere-style:
**one tenant config file per tenant, one section per profile (environment), one command pushes
every shard in that profile over SSH.**

Because shards never coordinate at runtime (interleaved counters + shard-in-bits Snowflake),
the deployer only needs SSH access to each shard's node — nothing else.

---

## Mental model

| Term | Meaning |
|---|---|
| **Tenant** | One logical cluster, e.g. `link3`. Has its own `deploy/tenants/<tenant>.conf`. |
| **Profile** | An environment within the tenant: `dev`, `staging`, `prod`. One `[section]` per profile, listing every shard. |
| **Shard** | One bucket-next process. Numbered `1..total_shards`, fixed for life. |
| **Inventory** | SSH connection details under `deploy/ssh-inventory/<inventory_tenant>/`. One file per host. |

```
deploy/
├── deploy.sh                       # the one command you run
├── tenants/
│   └── <tenant>.conf               # profiles + per-shard server mapping
├── ssh-inventory/
│   └── <inventory_tenant>/
│       ├── hosts/<server-name>     # host=, port=, user=, key=, ...
│       └── keys/<key>.pem          # private keys (gitignored, mode 0600)
└── dependencies/
    ├── bucket-next.service.tmpl    # systemd unit template
    └── remote-setup.sh             # runs on the target server
```

---

## Prerequisites

**On the machine you run `deploy.sh` from:**
- Go 1.21+ (the script cross-compiles a static Linux binary).
- `ssh` and `scp`.

**On each target server:**
- SSH reachable with a key.
- The SSH user can run `sudo` **without a password prompt** (the script creates a service
  user, installs a systemd unit, and writes to `/opt` and `/var/lib`). Configure passwordless
  sudo for the deploy user beforehand — the script never embeds a password.
- `systemd` (the service runs as a unit).
- `curl` (used for the post-deploy health probe).
- Outbound network not required — the static binary is copied in; nothing is downloaded.

---

## One-time setup for a new tenant

### 1. Create the tenant config

`deploy/tenants/<tenant>.conf`, one section per profile:

```ini
[dev]
description           = link3 dev cluster, 3 shards
inventory_tenant      = link3

# Service tuning — applied identically to every shard in this profile.
listen_port           = 7001
state_path            = /var/lib/bucket-next/state.json
segment_size          = 1000
segment_refill_watermark = 0.9
clock_drift_tolerance_ms = 0
snowflake_epoch_ms    = 1704067200000

# Cluster sizing — total_shards MUST equal the count of shardN.server keys below.
total_shards          = 3

# Per-shard server assignment. The value is a host-file name under
# ssh-inventory/<inventory_tenant>/hosts/
shard1.server         = link3-dev-1
shard2.server         = link3-dev-2
shard3.server         = link3-dev-3
```

Add `[staging]`, `[prod]`, etc. as needed. Each profile can have a different shard count.

### 2. Create the SSH inventory

One host file per server at `deploy/ssh-inventory/<inventory_tenant>/hosts/<server-name>`:

```ini
host=10.10.199.11
port=22
user=ubuntu
key=link3-deploy.pem
private_ip=10.10.199.11
# Optional — bind a non-default port on this host (use when two shards share one machine).
#listen_port_override=7011
```

### 3. Drop the private key

Put the key referenced by `key=` into
`deploy/ssh-inventory/<inventory_tenant>/keys/<key>` with mode `0600`. This directory is
gitignored, so keys never get committed.

```bash
cp ~/Downloads/link3-deploy.pem deploy/ssh-inventory/link3/keys/
chmod 600 deploy/ssh-inventory/link3/keys/link3-deploy.pem
```

---

## Deploying

### Dry run first (always)

Prints the plan and the exact `config.yaml` each shard would receive, **without contacting any
host**:

```bash
./deploy/deploy.sh link3 dev --dry-run
```

Verify each shard maps to the right server and gets the right `shard_id`.

### Deploy the whole profile

```bash
./deploy/deploy.sh link3 dev
```

This builds once, then for each shard in order: renders config, renders the systemd unit,
copies binary + config + unit + setup script, runs the setup, and health-checks the node.

### Deploy a single shard

Useful for fixing one node without touching the rest:

```bash
./deploy/deploy.sh link3 prod --shard 4
```

### Redeploy without rebuilding

```bash
./deploy/deploy.sh link3 staging --skip-build
```

### List what's available

```bash
./deploy/deploy.sh --list
```

---

## What happens on each shard

The script runs this pipeline per shard:

1. **Build once** (first shard only): `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build`
   produces `bin/bucket-next` and `bin/bucket-next-cli`. The target never needs Go.
2. **Render `config.yaml`** with this shard's `shard_id`, the profile's `total_shards`, and the
   tuning knobs. Port is `listen_port` unless the host file sets `listen_port_override`.
3. **Render the systemd unit** from `dependencies/bucket-next.service.tmpl`.
4. **Copy** binary, CLI, `config.yaml`, unit, and `remote-setup.sh` over a multiplexed SSH
   connection.
5. **Run `remote-setup.sh`** on the target, which:
   - creates the `bucket-next` system user (if missing),
   - installs binaries to `/opt/bucket-next/`,
   - installs config to `/opt/bucket-next/config.yaml`,
   - ensures `/var/lib/bucket-next/` exists and is owned by the service user,
   - installs and enables the systemd unit, then `systemctl restart bucket-next`.
6. **Health probe**: `curl http://127.0.0.1:<port>/health` on the target; warns if the
   expected payload is missing.

A summary at the end reports which shards succeeded.

---

## Verifying a live deployment

```bash
# From your machine, against a node's reachable address:
curl http://<node-host>:7001/health
curl http://<node-host>:7001/shard-info     # confirm shard_id is what you expect

# On the node itself:
sudo systemctl status bucket-next
sudo journalctl -u bucket-next -f
```

Confirm each node reports a **distinct** `shardId` and the **same** `totalShards`.

---

## Two shards on one host

Point two `shardN.server` keys at two inventory files that reference the same machine with
different `listen_port_override` values:

```ini
# tenants/tcteltd.conf
[dev]
inventory_tenant = tcteltd
total_shards     = 2
shard1.server    = tcteltd-dev-1a
shard2.server    = tcteltd-dev-1b
```

```ini
# ssh-inventory/tcteltd/hosts/tcteltd-dev-1a
host=10.10.198.5
...
listen_port_override=7001

# ssh-inventory/tcteltd/hosts/tcteltd-dev-1b
host=10.10.198.5
...
listen_port_override=7002
```

Each shard binds its own port, keeps its own state file path (set `state_path` per host if they
must differ), and runs as the same systemd unit name — so for production you would typically
template the unit name too. For dev, two ports on one box is fine.

> Note: the bundled `bucket-next.service` uses a fixed unit name. To run two instances on one
> host as systemd services, give each a distinct unit name and `state_path`. For a quick dev
> setup, running the second instance manually (`./bin/bucket-next -config shard2.yaml`) is
> simpler.

---

## Rollback

The deploy installs the new binary over the old one and restarts. To roll back, redeploy the
previous git tag:

```bash
git checkout <previous-tag>
./deploy/deploy.sh link3 prod --shard <n>
git checkout main
```

State on disk (`/var/lib/bucket-next/state.json`) is **not** touched by a binary swap, so
counters survive a rollback.

---

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `ERROR: tenant config not found` | Wrong tenant name; run `./deploy/deploy.sh --list`. |
| `ERROR: profile '<x>' not found` | Section missing in the tenant `.conf`. |
| `ERROR: total_shards=N but found M shardN.server entries` | Count mismatch — fix the `.conf`. |
| `ERROR: SSH key not found` | Key not placed in `ssh-inventory/<tenant>/keys/`, or `key=` name wrong. |
| `Permission denied (publickey)` | Wrong key, wrong `user=`, or key not authorized on the host. |
| `sudo: a password is required` | The SSH user lacks passwordless sudo on the target. |
| Health probe warns | Service didn't start — `sudo journalctl -u bucket-next -n 50` on the node. |

---

## Security notes

- Private keys live only under `ssh-inventory/**/keys/` and are gitignored. Never commit them.
- The service has **no authentication**. Bind it to a private subnet / management network, and
  restrict the port with a firewall or security group. Do not expose 7001 publicly.
- The deploy script embeds **no passwords**. It relies on key-based SSH and passwordless sudo
  configured out-of-band on the target.
