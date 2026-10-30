# Report — the tenant-first id service (2026-10-30 23:45 +06)

Branch `tenant-first` (pushed by its own name; not merged; nothing deployed; the bed untouched). Base: `fc779e7` = what
the bed runs. Asked by the routesphere architect on the owner's word (22:4x–22:5x +06: one id service per installation,
the tenant a key on every call; "keep going unattended as you best recommend, I will verify later"). The contract and
the plan: [../tenancy.md](../tenancy.md).

## The steps

| # | Step | Commit |
|---|---|---|
| 1 + 2 | state format 3 (`shard.json` + `tenants/<t>.json`, the roster, the per-file identity, the stored uuid scramble), the carry-over of a pre-tenant `state.json` (marks only forward, crash-safe), `ExportV2`, the directory lock; config `tenants` / `legacy_tenant` / `state_dir` (`state_path` refused naming it); one allocator per tenant, one forge; the CLI per tenant (`check`, `info`, `tenants`, `adopt-tenant`, `rename`, `export-v2`) | `d3a76cb` |
| 3 | `/api/v2/{tenant}/…` — the tenant first and mandatory; 400 `Tenant is required` / `Invalid tenant` / `Unknown tenant` (never 404); `Invalid entity name`; every answer names its tenant; the pre-tenant roads on `legacy_tenant`, counted per caller, switched off by one line | `fa237f2` |
| 4 | deploy: the profile's tenants, the NEW binary's read-only `check` before anything is installed, the tenants verified on `/shard-info`; the btcl profiles (bed: `tenants: [btcl]`, `legacy_tenant: btcl`; prod: `tenants: [btcl]`); every doc | `0ff53fd` |
| 5 | the rehearsals (`tools/rehearsal/`) and this report | this commit |

The two step-1/2 commits were one: the state API and its callers change together, and every commit is green.

## The numbers

| Check | Result |
|---|---|
| `go test -race ./...` | **153 pass, 0 fail, 0 skip** (135 tests + 18 subtests); `fc779e7` had 105. `go vet` clean, `gofmt` clean |
| breaks seen red (one rule broken at a time, its tests run, the file restored) | **15/15 red**: marks only forward · a lost tenant file refused · a file copied to another tenant refused · the owner of `state.json` never guessed · the lock exclusive · a new uuid entity gets the tenant's scramble · export-v2 refuses to strand a tenant · an unknown tenant 400 not 404 · every v2 answer names its tenant · the uuid scramble includes the tenant · v1 acts on `legacy_tenant` · v1 off never falls back to a default tenant · entity names checked on v2 · a tenant never case-folds or grows past 16 · the carry-over keeps every mark |
| `tools/rehearsal/tenant-first.sh --kills 60` (local, ext4, no root) | **8/8**. The bed's binary minted 2007…2015; the new binary's first start SIGKILLed 60 times at random moments (killed with: nothing written 3, tenant file without `shard.json` 2, done 55 — over the three runs, 140 kills: nothing written 10, tenant file without `shard.json` 6, `shard.json` before the rename 2, done 122 — every step of the carry-over hit); 60/60 restarts served partner ids above 2015 and no old uuid string; then 4 workers on `/api/next-id/` + 4 on `/api/v2/btcl/next-id/`: **2000 ids, 2000 distinct, lowest 4007**; `v1Calls` 1000 |
| `tools/rehearsal/deploy-in-lxc.sh` (the real `remote-setup.sh` under systemd, the unit's sandbox included, throwaway Debian 12 container, deleted after) | **26/26 in 197 s.** A the bed today (`fc779e7`, its own remote-setup): 2007 2009 2011 · B the move: the check said "carry the pre-tenant state.json (format 2, 1 entities) over into tenant btcl, every mark kept", then v1 + v2 on one counter: 4007…4017, the restart gap only · C a refused deploy (another cluster size): "total_shards: file=2 config=3", nothing installed, the shard kept serving (4019) · D `legacy_tenant` removed: v1 = 400 `Tenant is required`, v2 serves · E `export-v2` → the old binary continued above (6007) → forward again (8007, 8009 … 10009); **15 ids, all distinct, each above every earlier one** |
| deploy dry-runs | bed and prod render as committed; 6/6 bad profiles refused in words (`state_path`, a block list, `BTCL`, a tenant twice, `legacy_tenant` not served, no `tenants`) |

## What the callers must know

- **routesphere-seed `seed-idgen`** (the architect's round): every road becomes `/api/v2/{tenant}/…`, the tenant passed
  in by the caller. Its 404-on-status rule stays right: an unknown tenant is 400, never 404. Its `BucketNextLiveTest`
  starts `bucket-next/bin/bucket-next` with a pre-tenant config (`state_path`): the tenant-first binary refuses that
  config, so the test needs `state_dir` + `tenants` (+ `legacy_tenant` to keep its v1 roads) once this branch is built
  into `bin/`. This round built nothing into `bin/` (the rehearsals build in temp directories).
- **prime-context**: unchanged until it takes the moved seed client; on the bed it keeps calling the pre-tenant roads
  through `legacy_tenant: btcl`.
- The WiFi side and the BSS have no caller. `uniqueid-helper` (Java) has no caller: its tenant parameter waits, as the
  contract says.

## Follow-ups

- The uuid key from the environment, before the first uuid entity (secrets never in files). The bed runs the built-in
  key (fingerprint `987a907e1b019511`, confirmed by the CLI's `info`).
- Init-only money entities: one config line per tenant, if asked.
- The clean name `partner` under `btcl`: `bucket-next-cli -tenant btcl rename partner.btcl partner`, offline, after the
  pre-tenant roads are off, if asked.
- The bed's switch-over itself: the owner's window ([../tenancy.md](../tenancy.md#the-beds-switch-over-the-owner-opens-the-window-devops-runs-it)).

## Re-run

```bash
go test -race ./...
tools/rehearsal/tenant-first.sh --kills 60     # local, binds 127.0.0.1:7094
tools/rehearsal/deploy-in-lxc.sh               # needs LXD with a debian-12 image; --keep leaves the container
```
