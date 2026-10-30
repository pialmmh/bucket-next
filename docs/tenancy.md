# Tenancy: one id service per installation, the tenant a key on every call

The owner, 2026-10-30 (via the routesphere architect, 22:4x–22:5x +06): *"add multi-tenancy to all api, must have tenant
id"*, then *"one unique_id service per installation, serving multiple tenants, is enough; it's a small service."* The
routesphere design it adapts to: `docs/architecture/two-db-two-tenant-organisation.md` on routesphere main — the platform
is multi-tenant by design, ONE tenant `btcl` today (both applications, ad-sphere and wifi-sphere, under it), every stored
entity carries its tenant, a second tenant later = configuration, no code. The contract below was accepted whole by the
architect on the owner's word ("keep going unattended as you best recommend").

## The model

```
one installation ── one bucket-next: its shards are its own scale-out across boxes, never a shard per tenant
     every shard serves every tenant, each (tenant, entity) counter interleaved by shard as before:
        shard 1 of 2:  btcl/partner.btcl → 1, 3, 5, …     acme/partner.btcl → 1, 3, 5, …
        shard 2 of 2:  btcl/partner.btcl → 2, 4, 6, …     acme/partner.btcl → 2, 4, 6, …
```

| What | Scope |
|---|---|
| counters, their reservations, `init`, `reset`, `status`, `segment-state` | per (tenant, entity) |
| `list`, the state file | per tenant |
| the identity (`shard_id`, `total_shards`), the uuid key, the Snowflake epoch and forge | per shard, the same for every tenant |
| `/health`, `/shard-info` | global |

- **The tenant** is tenant-master's tenant id: `^[a-z][a-z0-9]{1,15}$` (prime-context `TenantRegistry.PLATFORM_TENANT_ID`)
  — the word prime-context's roots carry: `btcl` = the root schema = the topic suffix = the tenant id. Not the facade's
  wider tenant code (`^[a-z0-9][a-z0-9_-]{0,63}$`), not Odoo's company number. Never case-folded.
- **Declared, never created by a call:** `tenants: [btcl]` in the config. A second tenant = one more name + a deploy.
- **The record ids of the money and master tables** live in Odoo's database now (`odoo_btcl`, our tier schemas inside,
  `company_id` per row). Nothing changes for this service: the id is the id.
- **Entities still register at their first `next-id`**, as before (seed-idgen's plain `mint` relies on it). Making money
  entities init-only would be one more config line per tenant later — not built.

## The contract

`/api/v2/{tenant}/{next-id|next-batch|init|reset|status|segment-state|list|types|parse-snowflake}[/{entity or id}]` —
everything under `/api/` names its tenant first; `/health` and `/shard-info` stay global. Every v2 answer carries
`"tenant"`, the refusals too. The words (the JSON `error` field) are the contract:

| | Status | `error` |
|---|---|---|
| NEW | 400 | `Tenant is required` — no tenant segment; every pre-tenant road once they are switched off |
| NEW | 400 | `Invalid tenant` — breaks the spelling (`BTCL`, `bt_cl`) |
| NEW | 400 | `Unknown tenant` — well-formed, not served here |
| NEW | 400 | `Invalid entity name` — breaks `^[A-Za-z0-9._-]{1,64}$` |
| NEW | 404 | `Not found` — no such road under `/api/v2/{tenant}/` |
| kept | | every word of the pre-tenant API, same status: `Entity not found` 404, `Entity already exists` / `Backward reset refused` / `Range exhausted` 409, … |

**400, never 404, for every tenant refusal:** 404 is "Entity not found", and seed-idgen's `ShardCounter.raiseAbove`
registers an entity when `status` answers 404 — an unknown tenant must never look like an unregistered entity.

**The fingerprint rule is unchanged by tenants:** one uuid key and one Snowflake epoch per installation, the same on
every shard. `/shard-info` adds the served `tenants`, and the deploy checks them on every shard. Inside the one key, the
uuid scramble's tweak is per (tenant, entity), so two tenants' strings for the same entity name are unrelated — one
tenant cannot predict another's tokens from its own. The scramble is stored on each record (`uuidTweak`), never
re-derived, so an entity carried over keeps its old one for life.

## The state on disk

```
<state_dir>/                       e.g. /var/lib/bucket-next/shard-1
├── shard.json                     format 3: identity · snowflakeMarkMs · the tenant roster · migratedFrom
├── tenants/
│   ├── btcl.json                  format 3: tenant "btcl" · the same identity · records [{entityName, dataType, currentIteration, shardId, uuidTweak?}]
│   └── acme.json
├── state.json.migrated-v2         the pre-tenant file, kept after its carry-over, never read again
└── .lock                          held by the running service: the CLI's writes are refused meanwhile
```

| At start | What happens |
|---|---|
| a declared tenant on the roster, its file present | served, its identity and its tenant name checked |
| a declared tenant on the roster, its file **missing** | **refused**: the file was lost — restore it (a fresh one would re-issue its ids) |
| a declared tenant not on the roster, no file | **new**: its file is created, then it joins the roster |
| a declared tenant not on the roster, a file with entities | refused, naming `bucket-next-cli adopt-tenant <t>` (shard.json restored from an older backup) |
| a file copied or renamed to another tenant | refused: it holds another tenant |
| a tenant on the roster no longer declared | kept, logged, not served; declaring it again resumes its counters |
| `shard.json` missing, tenant files present | refused: the shard's state is incomplete — restore `shard.json` |

Every write is durable (temp file, fsync, rename, directory fsync) and touches only its own tenant's file.

## The carry-over of a pre-tenant state.json

The pre-tenant service kept one `state.json` (format 1 or 2) per shard, with no tenant anywhere: records keyed by the
entity name alone, the uuid tweak derived from the entity name alone. At the new binary's first start:

1. The owner is never guessed: `state.json` is carried over only into the tenant `legacy_tenant` names (or, later, the
   tenant `shard.json`'s `migratedFrom` names). No owner = refused to start, in words, nothing written.
2. `tenants/<owner>.json` is written with every record (same name, same mark; a uuid record gets `uuidTweak: "entity"`,
   its old scramble), then `shard.json` (identity, the Snowflake mark, the roster, `migratedFrom`) — its presence means
   "done" — then `state.json` is renamed `state.json.migrated-v2`.
3. A crash at any step re-runs to the same result: marks only move forward (an entity in both files keeps the higher
   mark). Rehearsed with SIGKILL at random moments of the first start (below).
4. A `state.json` found beside `shard.json` later (an older binary ran after the carry-over) is merged forward the same
   way, with a WARNING in the log.

**The bed's shard 1** (read 2026-10-27 ~21:07 +06) holds ONE record: `partner.btcl` (int), mark 1003 = next value 2007
(prime-context may have minted since). It moves unchanged — same name, same mark — into `tenants/btcl.json`. The next
partner id after the restart is what any restart gives today (the mark's value; a gap of at most `segment_size`), never
a repeat. **No rename:** prime-context on the pre-tenant roads and anything on `/api/v2/btcl/…/partner.btcl` draw from
the SAME counter. Renaming to `partner` while a pre-tenant caller remains would make two counters for the same partner
ids. If the clean name is ever wanted: `bucket-next-cli -tenant btcl rename partner.btcl partner`, offline, after the
pre-tenant roads are off (the CLI refuses it before) — it carries the mark; never a fresh init above the database's
highest (an id minted for a row later deleted would come back).

## The bed's switch-over (the owner opens the window; devops runs it)

```
(a) deploy                         (b) callers move                     (c) switch the old roads off
tenants: [btcl]                    seed-idgen → /api/v2/{tenant}/…      remove legacy_tenant, deploy
legacy_tenant: btcl                prime-context redeploys with it      /api/next-id/… → 400 "Tenant is required"
→ check, carry-over at start       no bucket-next step                  watch v1Calls first: it must stop moving
→ prime-context unchanged, ~1 s
```

(a) `tools/deploy/remote-deploy-v2.sh btcl bed` with the profile as committed (`tenants: [btcl]`, `legacy_tenant: btcl`).
On radius-2 the NEW binary first reads the shard's state read-only and prints what its start will do ("carry the
pre-tenant state.json (format 2, 1 entities) over into tenant btcl, every mark kept"); a refusal stops the deploy before
anything is installed. The restart is the only gap (~1 s, partner ids only); prime-context keeps calling the pre-tenant
roads, unchanged. (b) and (c) need no joint window: bucket-next and prime-context move independently.

## The route back

Never put `state.json.migrated-v2` back: its marks are behind. Stop the shard, then
`bucket-next-cli -config /etc/bucket-next/shard-1.yaml -tenant btcl export-v2` writes a format-2 `state.json` from the
CURRENT marks and retires `shard.json`; deploy the pre-tenant tag (it reads `state_path: …/shard-1/state.json`). A later
deploy of the tenant-first binary carries that file over again, marks only forward. `export-v2` refuses while another
tenant has entities (a pre-tenant binary serves one tenant) and for a uuid entity with the tenant's scramble.

## The callers

| Caller | Today | Moves how (not this repo) |
|---|---|---|
| routesphere-seed `seed-idgen` — `dependencies.BucketNext` → `internal.BucketNextSource` + `ShardCounter` (`mint`, `mintAbove`) | 5 pre-tenant roads: GET status, POST init, PUT reset, GET next-id, GET next-batch; matches `404` from status (→ register), `409 Entity already exists`, `409 Backward reset refused`, `Range exhausted` and 5xx (→ the next shard), any other 4xx (→ `IdRefused`) | the routesphere architect's round: every road `/api/v2/{tenant}/…`, the tenant passed in by the caller (`IdSource.mint`/`mintAbove`, `PartnerIds.next`) — no default tenant in the client either. Its `ENTITY_NAME` rule fits. Its `BucketNextLiveTest` writes a pre-tenant config (`state_path`): it needs `state_dir` + `tenants` |
| prime-context — `PartnerIdGenerator` → `TreePartnerIds` → seed's `PartnerIds`, entity `partner.<root>` (int); `primecontext.ids.shards=http://10.10.188.2:7001` on the bed | through seed-idgen | redeploys with the moved seed client |
| ad-sphere-s2 (round L3: `packageaccount`, `packagepurchase`, long) | not a caller yet | born on `/api/v2/btcl/` |
| the WiFi side, the BSS | no caller (the WiFi architect's read, wifi master `a043ba7c2`) | — |
| `uniqueid-helper` (Java) | no caller | — |

## Rehearsed

| Script | What it proves |
|---|---|
| `tools/rehearsal/tenant-first.sh` | local, no root: the bed's binary writes a bed-shaped state; the new binary's first start is SIGKILLed at random moments N times (every step of the carry-over hit); every restart serves above every old id and no old uuid string; then 4 workers on the pre-tenant road and 4 on `/api/v2/btcl/` draw 2000 distinct ids from one counter |
| `tools/rehearsal/deploy-in-lxc.sh` | the REAL `remote-setup.sh` under systemd (the unit's sandbox included) in a throwaway Debian 12 container: the bed today (fc779e7) → the move → a refused deploy leaves the shard serving → the old roads off → `export-v2`, the old binary, forward again; every id above every earlier one |

## Follow-ups (not in this round)

- The uuid key from the environment, before the first uuid entity (the owner's rule: secrets never in files). Today a
  real key travels as a file (`uuid_key_file` → the host's 0640 config); the bed runs the built-in key (fingerprint
  `987a907e1b019511`) — fine while no uuid entity exists.
- Init-only money entities: one config line per tenant, if asked.
- The clean entity name `partner` under `btcl`: the offline rename, after the pre-tenant roads are off, if asked.
