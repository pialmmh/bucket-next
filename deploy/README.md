# bucket-next — deploy

Routesphere-style tenant/profile deploy. One command pushes the binary plus a per-shard config
to every shard of a profile. Full guide: [../docs/deployment.md](../docs/deployment.md).

```
deploy/
├── deploy.sh                       # single entry point
├── tenants/<tenant>.conf           # one [profile] section per environment, listing every shard
├── ssh-inventory/<tenant>/
│   ├── hosts/<server-name>         # host=, port=, user=, key=, [listen_port_override=]
│   └── keys/                       # SSH keys + uuid key files (gitignored, mode 0600)
└── dependencies/
    ├── bucket-next@.service        # systemd template: one unit per shard
    └── remote-setup.sh             # runs on the target host
```

Each shard runs as `bucket-next@<shard_id>` with its own config
(`/etc/bucket-next/shard-<id>.yaml`) and state file
(`/var/lib/bucket-next/shard-<id>/state.json`), so several shards can share a host.

## Usage

```bash
./deploy/deploy.sh --list                          # tenants + profiles
./deploy/deploy.sh link3 dev --dry-run             # plan + rendered configs, no SSH
./deploy/deploy.sh link3 dev --init-state          # FIRST deploy: create missing state files
./deploy/deploy.sh link3 dev                       # every shard of the profile
./deploy/deploy.sh link3 prod --shard 4            # one shard
./deploy/deploy.sh link3 staging --skip-build      # reuse bin/ (make build first)
```

## Safety checks built in

- The shards with a server are numbered `1..K` without gaps and each has a host file; `K` may be less than
  `total_shards` (shards `K+1..total_shards` are declared, not yet deployed): the id space is partitioned for
  `total_shards` from day one, so a later shard is added as a `shardN.server` line with its own `--init-state`,
  never by changing `total_shards` (every running shard's state file is bound to it).
- No two shards on the same host + port, or the same host + state file.
- A missing state file fails the deploy unless `--init-state` (never use that for a *lost* file).
- After start, each node's `/shard-info` must report the deployed `shardId` and `totalShards`.
- The service itself refuses a state file from another shard, `total_shards`, uuid key or epoch.

## Tenant `.conf` keys (per profile)

| Key | Required | Default |
|---|---|---|
| `inventory_tenant` | yes | — |
| `total_shards` | yes | — |
| `shard1.server` … `shardN.server` | yes | — |
| `listen_address` (an IP, or `private_ip` = each host's own) | no | all interfaces — set it, the API has no auth |
| `state_path` (`{shard_id}` replaced) | no | `/var/lib/bucket-next/shard-{shard_id}/state.json` |
| `uuid_key_file` | no | built-in key (unique but predictable) |
| `listen_port` | no | 7001 |
| `segment_size` | no | 1000 |
| `segment_refill_watermark` | no | 0.5 |
| `clock_drift_tolerance_ms` | no | 0 |
| `snowflake_epoch_ms` | no | 1704067200000 |

Target hosts need passwordless sudo for the SSH user, `systemd` and `curl`. The scripts hold no
passwords.
