# bucket-next — deploy

Routesphere-style tenant/profile deploy. One command pushes the binary plus a per-shard config to every node in the chosen profile.

```
deploy/
├── deploy.sh                     # single entry point
├── tenants/                      # one .conf per tenant (INI format)
│   ├── link3.conf
│   └── tcteltd.conf
├── ssh-inventory/                # SSH connection details — keep private
│   └── <inventory_tenant>/
│       ├── hosts/                # one INI file per server
│       │   ├── link3-dev-1
│       │   └── ...
│       └── keys/                 # PEM keys (gitignored)
└── dependencies/
    ├── bucket-next.service.tmpl  # systemd template with __PLACEHOLDERS__
    └── remote-setup.sh           # runs on the target server
```

## Concepts

| Term | Meaning |
|---|---|
| **Tenant** | One logical cluster of shards (e.g. `link3`). Each tenant has its own `.conf`. |
| **Profile** | An environment within the tenant (e.g. `dev`, `staging`, `prod`). Each profile is a `[section]` in the tenant `.conf` and lists every shard in that environment. |
| **Shard** | One bucket-next process. Numbered `1..total_shards`, fixed for life. |
| **Inventory** | SSH connection metadata, kept in `ssh-inventory/<inventory_tenant>/`. One file per host. |

The interleaved-counter and shard-in-bits designs mean **shards never talk to each other** — the deploy script only needs SSH access to each shard's node.

## Tenant config (INI)

`deploy/tenants/<tenant>.conf` — one section per profile.

Required keys per section:

| Key | Purpose |
|---|---|
| `description` | Free-text label |
| `inventory_tenant` | Sub-directory under `ssh-inventory/` where host files live |
| `total_shards` | Must equal the count of `shardN.server` keys below |
| `shard1.server`, `shard2.server`, … | Maps each shard to a host file in the inventory |

Service-tuning keys per section (all passed through into the rendered YAML):

| Key | Default |
|---|---|
| `listen_port` | 7001 |
| `state_path` | `/var/lib/bucket-next/state.json` |
| `segment_size` | 1000 |
| `segment_refill_watermark` | 0.9 |
| `clock_drift_tolerance_ms` | 0 |
| `snowflake_epoch_ms` | 1704067200000 (2024-01-01 UTC) |

## SSH inventory

`deploy/ssh-inventory/<inventory_tenant>/hosts/<server-name>` — flat key=value file:

```
host=10.10.199.11
port=22
user=ubuntu
key=link3-deploy.pem
private_ip=10.10.199.11
# Optional — bind a non-default port on this host (use when two shards share a host)
#listen_port_override=7011
```

`deploy/ssh-inventory/<inventory_tenant>/keys/` — private SSH keys. Mode 0600. **Gitignored.**

## Usage

```
# List what's available
./deploy/deploy.sh --list

# Deploy the whole link3 dev cluster (3 shards in sequence)
./deploy/deploy.sh link3 dev

# Push one shard only — useful for fixing a single node
./deploy/deploy.sh link3 prod --shard 4

# Re-deploy without rebuilding (binaries must already exist)
./deploy/deploy.sh link3 staging --skip-build

# Print the plan + rendered configs without contacting any host
./deploy/deploy.sh link3 dev --dry-run
```

## What gets pushed

For each shard the script:
1. Generates `config.yaml` with this shard's `shard_id` and the profile's tuning knobs.
2. Renders `bucket-next.service` from the systemd template.
3. Copies `bucket-next`, `bucket-next-cli`, `config.yaml`, `bucket-next.service`, `remote-setup.sh` over a multiplexed SSH session.
4. Runs `remote-setup.sh` on the target — creates the `bucket-next` user, installs files into `/opt/bucket-next/`, ensures `/var/lib/bucket-next/` exists, reloads systemd, restarts the service.
5. Curls `/health` from the target host to confirm.

## Cross-compile note

The build step runs `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build`. The remote host never needs Go installed — only the static binary lands there.

## Adding a new tenant

1. Drop `<tenant>.conf` in `deploy/tenants/`.
2. Create `deploy/ssh-inventory/<tenant>/hosts/<name>` for each server.
3. Put the private key in `deploy/ssh-inventory/<tenant>/keys/` (mode 0600).
4. Run `./deploy/deploy.sh <tenant> <profile> --dry-run` to verify parsing.
5. Run the same without `--dry-run` to deploy.

## Adding a profile to an existing tenant

Edit the tenant `.conf`, add `[<profile>]`, fill the required keys, add `shardN.server` lines, and ensure each referenced host file exists in the inventory.
