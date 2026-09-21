# Chaos scenarios

Real situations the system must survive, reduced to repeatable steps. All run
against the docker stack (`make stack-up`) with the local agentsim fleet. Chaos
always goes through agents (chaos-ui Chaos tab, or the agentsim control API on
:7780), never straight into the database — the agent lies or goes silent over
the real gRPC transport.

chaos-ui has no database access at all: it reads topology and writes desired
state through the apiserver control plane (`CONTROL_PLANE_URL`, default :7080),
so UI and CLI share the same project layer. Operator chaos (cordon/drain/delete
replica) goes to that control plane; agent chaos goes to agentsim.

## Host state: two columns, two owners

`hosts.host_healthy` (bool) is written only by heartbeat/watchdog: heartbeat →
`true`, 30s of silence → `false`. `hosts.status` is written only by the
operator: `open | cordoned | draining`. Free placement requires
`host_healthy AND status='open'`; a volume-pinned replica may return to its own
host even when `cordoned`/`draining` (the disk is there and nowhere else).
Heartbeat never touches `status`; host death never clears a drain.

## Thresholds (internal/engine)

| Constant | Value | Meaning |
|---|---|---|
| `watchdogInterval` | 5s | how often the watchdog checks staleness and settles drains |
| `hostUnhealthyAfter` | 30s | silence → `host_healthy=false`, out of scheduling (reversible) |
| `hostDeadAfter` | 2min | silence → replicas are released (one-way) |
| `drainStalledAfter` | 10min | drain in flight longer than this → WARN every sweep; no action |
| `volumeLeaseTTL` | 90s | no healthy observation → lease expires, failover allowed |
| startup grace | = `hostDeadAfter` | after engine boot, no death verdicts until a full window passes |

## Setup

```bash
make stack-up      # postgres + engine + apiserver + agentsim + chaos-ui (localhost:3000)
make build
# in an empty folder:
./build/conductor init -n chaos-demo
./build/conductor add --service --name web --image nginx:alpine
./build/conductor up -s web                    # config.toml: 2 replicas, us-east-1
```

Agentsim is part of the stack (one sim-agent per host, control API on :7780).
Chaos goes through the UI (Chaos tab) or straight to the control API; IDs come
from `curl localhost:7780/agents` (host + replica + phase + chaos mode).
Action: `curl -XPOST localhost:7780/chaos -d '{"action":"host_kill","host":"<id>"}'`
(actions: `host_kill|host_recover`, `replica_crash|replica_crashloop|replica_stall_health|replica_heal`,
`volume_stall_resize|volume_heal` with `"volume":"<id>"`).

## 1. Network blip (< 2min) — nothing moves

Requirement: a short disturbance must not scramble the workload.

- chaos `host_kill <host>` → the host goes silent
- ~30s: host `unhealthy`, out of scheduling; **replicas stay bound and active**
- chaos `host_recover <host>` before the 2min mark
- first heartbeat restores `healthy`; no replica moved

Measured: kill 11:54:42 → unhealthy 11:55:14 (32s) → recover 11:55:27 →
healthy 11:55:32 (5s). Replicas untouched throughout.

## 2. Host death (> 2min) — re-place onto survivors

Requirement: a dead host loses its replicas; they are re-placed automatically on
other hosts in the same region; a host that comes back later rejoins the pool
empty.

- chaos `host_kill <host>`, do not recover
- ~30s: `unhealthy` (as above)
- ~2min: watchdog `MarkHostDown` — replicas become hostless `replacing`; next
  tick the placer assigns another host, the agent brings them up through
  start → health → active
- `host_recover` any time after: host returns `healthy` and empty; the agent
  kills orphan containers itself on the first full snapshot (they are no longer
  in its list)

Measured: kill 11:55:47 → unhealthy 11:56:19 (32s) → replica released 11:57:49
(2min02s) → active+healthy on a new host 11:57:53 (4s after the verdict).

## 3. Control plane restart — startup grace, no massacre

Requirement: an engine crash/redeploy longer than the staleness window must not
declare the whole fleet dead on boot (heartbeats stopped because the gateway was
down, not because the hosts died).

- `docker compose stop engine`, wait 2-3min (all heartbeats stop)
- `docker compose start engine`
- agents reconnect (gRPC backoff can add ~30s after a longer outage); the sweep
  may demote hosts to `unhealthy` (reversible), but no death verdict falls until
  engine uptime exceeds `hostDeadAfter` — by then every live host has checked in
- expected: zero released replicas, fleet back to `healthy` without a single
  restart

Measured: outage 11:58:29→12:00:59 (2.5min) → at +30s the sweep demoted all 6
hosts (agents still in backoff) → agents back at +39s → heartbeat restored
`healthy`. `hostless=0`, `active=2` the whole cycle — no replica restarted or
moved.

## 4. Crash-looping replica during rollout — restart budget fails the deployment

Requirement: a container that keeps dying must not spin forever.

Applies **only to a rollout in progress** (deployment `pending`/`draining`):
there the offender is the canary or its batch, so the revision itself is
suspect. For an already-`active` deployment see 4b — there only the replica is
frozen.

- chaos `replica_crashloop <replica>` → restart_count grows every tick
- past `restart_max`, the reconciler's `crashLooping` rule fails the **whole
  deployment** and `deploymentFrozen` freezes it: no re-place, no replacement,
  the outgoing version keeps serving
- deliberate: an exhausted restart budget mid-rollout is a signal for a human,
  not for automation
- chaos `replica_heal <replica>` afterwards restores the container, but the
  deployment stays `failed` — the only way out is `conductor up`/`rollback`

Measured: restart_max=5 → deployment `failed` in ~6s; the replica stayed
`starting` with restart_count in the hundreds until the next deploy arrived.

## 4b. Crash-loop in an active deployment — freeze the replica

Requirement: one bad replica in an already-`active` deployment must not fail the
whole deployment nor spin forever; the rest of the group keeps serving,
degradation is visible, the operator decides what next.

Model: `crashLooping` looks at deployment status. For `active` it emits
`IntentFreezeReplica` **for offenders only** (not `IntentFail`). Actuator →
`FreezeReplica`: `phase='failed', healthy=false` with a phase guard (like
`MarkHostDown`), **without a revision CAS** — the sensor bumps `revision` every
second so a CAS would always lose, and the decision depends only on the
monotonic `restart_count`. The consequences close the loop:

- `RecordReplicaObservation` rejects reports for `failed` → `restart_count`
  stays frozen at the value that crossed max
- `ListReplicasByHost` (+ the `hostState` guard) does not send `failed` to the
  agent → the replica disappears from `HostState`, the agent deletes the
  container and stops reporting; the agent never learns the word `failed`
- the snapshot still sees it (`phase<>'reaped'`), but `buildReplicaGroups` puts
  it in a third bucket, `FrozenReplicas` — not in `TargetReplicas`, so
  `crashLooping`/`notAllHealthy`/`newHealthOpenPastDeadline` don't see it
- **no replacement**: `rollingRampUp`/`recreateRampUp` count `healthy + frozen`
  against desired (`heldSlots`) — frozen holds the slot; a crash-loop is almost
  always image/config, a clone would crash the same. A stateful frozen replica
  also holds its lease.
- `rollingScaleDown` spends surplus on frozen first (`destroy`, no drain
  window), only then drains live ones newest-first
- `rolloutComplete` counts `heldSlots` too: a rollback to a revision with a
  frozen replica ends `active` (degraded) instead of hanging in `draining`
- deployment status stays `active`; degradation is derived in the read path:
  `conductor status` prints `active (degraded)` when `healthy < desired`, and
  chaos-ui shows a yellow `v1 · active · degraded` badge

Thawing (three ways, all exist):

1. `POST /v1/replicas/{id}/restart` (UI "Restart (thaw)", only on `failed`) →
   the row goes hostless `replacing`, `restart_count=0` → `anyHostlessReplicas`
   → placer → `scheduling` → agent starts a fresh container with no chaos mode
2. `DELETE /v1/replicas/{id}` → the row disappears, `rollingRampUp` sees the
   deficit → a new replica (manual "one replacement")
3. `conductor up` / `rollback` → frozen becomes outgoing and
   `reapFailedOutgoing` deletes it with no drain window while the old live
   replica is still draining

Setup (parallel stack `-p freeze`, apiserver :27080, agentsim :27780):

```bash
mkdir demo && cd demo
export CONDUCTOR_DATABASE_URL=postgres://conductor:conductor@localhost:25432/conductor?sslmode=disable
conductor init -n freeze-demo
conductor add --service --name web --image nginx:alpine
printf '[deploy]\nnum_replicas = 2\nregion = "us-east-1"\nrestart_max_retries = 5\ndrain_seconds = 10\ncpu = "200m"\nmemory = "128Mi"\n' > config.toml
conductor up -s web
A=<replica id>   # from GET :27080/v1/topology
curl -XPOST localhost:27780/chaos -d "{\"action\":\"replica_crashloop\",\"replica\":\"$A\"}"
conductor status                                   # active (degraded) 1/2
curl -XPOST localhost:27080/v1/replicas/$A/restart # 200; 409 if not failed, 404 unknown
curl -XDELETE localhost:27080/v1/replicas/$A       # manual replacement
conductor up -s web                                # v2 picks up frozen as failed outgoing
```

Measured (2026-09-18, reconcile 2s, agentsim tick 1s, restart_max 5):

- **freeze**: chaos 17:42:55 → `restarts` 1..5 per second → +7s replica A
  `failed`, `healthy=false`, `restart_count` frozen at 6; deployment `active`
  the whole time, `healthy=1/2 observed=2`; agentsim `/agents` shows
  `containers: []` for A's host; **no new replica** even after 10+ ticks;
  `conductor status` → `active (degraded)  2  1/2`
- **restart**: `POST …/restart` 17:43:27 (200) → +1s `health_check`
  `restart_count=0` → +2s `active healthy=true`, deployment `2/2`, no
  `(degraded)`. Second `restart` → 409, restart of a live replica → 409,
  unknown id → 404
- **delete**: crashloop 17:43:46 → frozen +8s → `DELETE` 17:43:58 → row `GONE`
  immediately, `observed=1` → +2s new replica `pending` → +5s `active`, `2/2`
- **redeploy**: crashloop 17:44:48 → frozen +8s → `conductor up` (v2) 17:45:00 →
  +2s v2 canary → +9s second v2 healthy, `2/2` → +10s live v1 `draining`
  (traffic switch) → **+12s frozen v1 `GONE`** (`reapFailedOutgoing`, no drain
  window, while the live one is still draining) → +21s live v1 `GONE` →
  +23s v2 `active 2/2`
- **stateful** (`pg` with a volume, `num_replicas=1`): crashloop 17:50:01 →
  frozen +5s; 10s later `active healthy=0/1 observed=1`, **no create**, the
  lease still held by the frozen replica. `POST …/restart` 17:50:17 → +3s
  `active healthy=true` on the **same host as the volume** (pinned placer
  branch), lease reacquired by the same replica

Note (recorded, not fixed): the SQL belt `ReserveReplicaOnHost` does not count
`failed` replicas toward host capacity, while the placer's in-memory ledger
(`placer.go`) does — the two ledgers disagree while a replica sits frozen. That
is why restart goes through hostless `replacing` rather than "same host".

## 5. Stuck health check — progress deadline

Requirement: a deploy that never becomes healthy must not hang forever.

- chaos `replica_stall_health <replica>` → the container starts, probes never pass
- the replica sits in `health_check`; the progress-deadline path fails it and
  the rollout ends as failed instead of hanging

Measured (progress_deadline=60): canary stall → deployment `failed` in 56s, the
served revision stayed on the old version. Note: the container exists on the
agent only ~3s after the deploy; a stall before that returns 404
(`no agent runs replica`).

## 6. Zombie agent + stateful lease — single writer

Requirement: a partitioned-but-alive agent must not hold a volume forever nor
resurrect a written-off replica.

- stateful service (`conductor add --database ...` + volume), the replica holds
  the lease
- `host_kill` its host; after 2min the replica goes `replacing`
- the agent keeps reporting it healthy (zombie): observations hit the SQL guard
  (`replacing` is orchestrator-owned) and — crucially — **do not renew the lease**
- the lease expires 90s after the last observation that actually landed; only
  then may the replacement replica `AcquireVolumeLease` → single writer
  preserved, failover not blocked

Measured: `replacing` at +125s, replica stays hostless (pinned to the volume's
host), lease expired ~+90s; recover → the same replica returns to the same host,
`active` in 4s, lease reacquired.

## Regular scenarios (no chaos)

### 7. Scale up / down

- `conductor scale us-east-1=4 -s web` → the placer adds replicas with
  anti-affinity (spreads across hosts before doubling up)
- `conductor scale us-east-1=2 -s web` → surplus goes `draining`, reaped after
  `drain_seconds`; the traffic pointer is NOT touched (scale-down is not a rollout)

Measured: 2→4 active+healthy in **8s** (spread over 3 hosts); 4→2: draining at
+9s, reaped at +12s (drain window 10s).

### 8. New deploy — blue/green

- change the spec/image, then `conductor up -s web` → v2 replicas come up
  alongside v1; once all are healthy the traffic switch is one atomic batch
  (SetServedRevision + drain of the old ones in the same transaction); v1
  replicas drain, then are reaped
- if v2 fails (crash/stall before healthy) → the progress deadline fails the
  rollout, v1 keeps serving

Measured: `up` v2 → v2 current (2 active) and v1 reaped in **12s**; through
chaos-ui 21-22s (drain 10s).

### 9. Stateful service — volume, lease, recreate

- `conductor add --database --engine postgres --name pg` +
  `conductor volume add --mount /var/lib/postgresql/data --size 2 -s pg` +
  `conductor up -s pg -f pg-config.toml` (num_replicas=1 — stateful is single
  instance)
- a volume belongs to an environment-service, not a service (migration 00007):
  `volume *` resolves project, environment and service like `up` and `scale` —
  `-e` defaults to the linked environment, and `conductor init` links
  `production`, so the commands above work without `-e`. The same service in
  another environment gets its own disk (9a).
- the placer places the volume first (3D bin-pack: cpu/mem/disk), the replica
  follows the volume's host; the lease is taken in the same transaction as the
  host assignment
- a redeploy (`up` again) is recreate, not blue/green: the old replica goes, the
  new one takes over the lease — never two writers

Measured: deploy→active+lease in **13s** (replica and volume on the same host);
recreate v1→v2 with lease handover in **19s**, exactly one live replica the
whole time.

#### 9a. One service in two environments — two volumes, two leases

A volume belongs to an `environment_services` row — the same
`replicaSlot{EnvironmentServiceID, Region}` key the engine uses for everything
else. `UNIQUE (environment_service_id, mount_path)`: the same mount on the same
service in another environment is a different disk, not `ErrExists`.
`ON DELETE RESTRICT` stays — unbinding a service that has a volume fails loudly.

Steps (setup as in 9; `environment create` clones the bindings from the linked
environment, so `pg` ends up bound in `staging` too):

```bash
conductor init -n env-demo                                   # links production
conductor add --database --engine postgres --name pg
conductor volume add --mount /var/lib/postgresql/data --size 2 -s pg
conductor up -s pg -f pg-config.toml
conductor environment create -n staging                      # clone of production → pg bound here too
conductor volume add --mount /var/lib/postgresql/data --size 2 -s pg -e staging
conductor up -s pg -e staging -f pg-config.toml
conductor volume list -s pg              # production's only
conductor volume list -s pg -e staging   # staging's only
conductor volume list -s pg -e nosuch    # "service "pg" in env-demo/nosuch: not found"
```

Expected: two `volumes` rows with different `environment_service_id`, two
leases, two `active` replicas each pinned to its own volume; no holds and no
lease conflicts in the engine log.

Measured (2026-09-19 UTC, agentsim tick 1s, reconcile 2s, both `up` in the same
second):

- 10:33:10 both `up` → one pass 10:33:11 `place_volume:2 create:2` (both volumes
  on `ue1-small-1`) → 10:33:13 `assign_host:2` → 10:33:15 `recreateComplete` for
  both slots (**5s**, `holds=0` in every pass)
- state at 10:33:50: two `attached` 2GiB volumes, two live leases, two
  `active|healthy` replicas each on its own volume
- the full §11 grow/park/revert cycle run on the production volume while the
  staging volume stayed `attached 2GiB/2GiB` throughout — grow keyed by
  `(environment_service_id, mount_path)` does not touch the neighbour

### 10. Operator actions

- `cordon` (UI → apiserver): the host keeps serving what it has, receives
  nothing new; only allowed on `open` (409 for `draining` — cordon would erase
  the drain)
- `drain` (UI → apiserver): graceful evacuation of stateless replicas, the host
  ends up `cordoned` on its own — details and measurements in 12. Always
  succeeds (no `force`, 409 only if already draining)
- `uncordon` (API only, `POST /v1/hosts/{id}/uncordon`): returns both `cordoned`
  and `draining` hosts to `open` and clears `drain_started_at` — this is how a
  drain is cancelled
- replica `restart` (UI "Restart (thaw)" on a `failed` replica →
  `POST /v1/replicas/{id}/restart`): thaws a replica frozen in 4b — the row goes
  hostless `replacing` with `restart_count=0`, the placer places it next tick
  (same path as host death; "same host" is not guaranteed, since the DB belt
  doesn't count a failed replica toward capacity). 404 unknown id, 409 if the
  replica isn't `failed` — a live replica is restarted by the agent, not the
  control plane
- replica `delete` (UI "Orphan", `DELETE /v1/replicas/{id}`): on a frozen
  replica this is the manual "one replacement" — the row disappears,
  `rollingRampUp` sees the deficit and creates a new one
- `conductor rollback`: restore the previous deployment version

### 11. Volume resize — grow-only, engine gates space, `resize_pending` + `revert`

Requirement: `conductor volume update --size N` must grow the disk live without
restarting the replica (like Railway live resize); shrink does not exist; a
request the host cannot take must not end in `failed` but wait until space
appears — and while waiting it must be **visible** as a state, must not block
other volumes on the host, and the operator must be able to **take it back**
(`volume revert`).

Model:

- **The engine is the only writer of `volumes.status`.** CLI/project write only
  `desired_size_bytes` (+ `previous_desired_size_bytes`, the revert target).
- **Desired becomes a reservation only once approved.** The ledger charges
  `VolumeSizing.Committed()`: `resizing → desired`; otherwise `observed` if the
  agent has reported; otherwise `desired` (fresh placement). The downlink
  (`volumeTargetSize`) and the CLI advisory use the same formula — an unapproved
  100GiB grow does not stop a new volume from landing on the host.
- **Grow is a delta item through `fits`.** The `resize` item carries
  `GrowDelta()` (Committed already charged what is on disk), skips `DiskReserve`
  (the reserve exists precisely for grows) and cpu/mem headroom. An approved
  grow is written to the ledger immediately — a second grow on the same host in
  the same tick sees the first (ordered by `id`).
- **New status `resize_pending`** = exactly "grow requested, host has no room".
  Written by the engine (`MarkVolumeResizePending`), never by the CLI. It also
  applies to an unhealthy host (outside the ledger): the grow is parked, so
  `revert` stays available while the host is down, and the settle branch needs
  no ledger — a reverted volume is `attached` on the very next tick, not when
  the host returns.
- **One grow in flight.** `update` is accepted only for `pending` (unplaced) or
  `attached` **and converged** (`observed == desired`, or never reported);
  grow-only (`size > desired`).
- **`volume revert`** resets `desired` to `previous_desired_size_bytes` (a full
  `update`, clearing `revert` → works once). Allowed **only** from
  `resize_pending`. Not from `resizing` (approved = promised to the agent; a
  stuck resize is a job for a future watchdog/alarm, not for revert). It does
  not touch status — the engine settles `resize_pending → attached` itself since
  `observed >= desired` now holds.
- SQL predicates in `Mark*` stay explicit (the commit-time belt must live in the
  database) and mirror `VolumeSizing.Drifting`/`CaughtUp`; a lost race is a
  drop, the next tick decides again.

```
pending ──place──▶ attached ──drift, fits───────────────▶ resizing ──observed≥desired──▶ attached
                      │                                      ▲
                      └──drift, !fits──▶ resize_pending ─────┘ (fits on some later tick)
                                             │
                                             └──revert──▶ desired=previous ──engine: observed≥desired──▶ attached
```

| status | `update` (grow-only) | `revert` |
|---|---|---|
| `pending` (unplaced), `attached` converged | ✅ | ❌ (`previous` is NULL anyway) |
| `attached` with drift (≤2s window before the engine classifies) | ❌ "grow already requested; revert first" | ❌ "not classified yet, retry" |
| `resize_pending` | ❌ "is resize_pending; revert or wait" | ✅ |
| `resizing` | ❌ | ❌ "nothing to revert" |

Steps (setup as in 9: stateful `pg`, `volume add --size 2`, `up`; the volume
landed on `ue1-small-1` — 80GB, budget 64GiB; all `volume` commands below target
the linked `production` — add `-e` for another environment):

- grow with room: `volume update --mount /var/lib/postgresql/data --size 4 -s pg`
  → CLI "host has room"; `resizing` next tick; the agent grows and reports;
  `attached`
- grow without room: `--size 100` → CLI "host is short 36GiB … waits as
  resize_pending (volume revert takes it back)"; an immediate second `update` →
  "already has a grow requested (4G → 100G); revert first"; after the tick
  `volume list` shows `SIZE 100GiB / ON DISK 4GiB / resize_pending`; `update`
  now → "is resize_pending; revert or wait"
- another volume alongside a pending grow: `cordon` medium and large (to force
  the placer onto small), `add --database --name pg2`,
  `volume add --mount /data --size 30 -s pg2`, `up -s pg2` → lands on the same
  host (30 ≤ 64 − 4 − 12.8); under the old logic (desired as reservation) the
  host would have looked 40GiB overcommitted
- `volume revert --mount /var/lib/postgresql/data -s pg` → "reverted … → 4GiB
  (engine settles on next tick)"; `attached` next tick; a second `revert` →
  "is attached; nothing to revert"
- space appears: `--size 60` → `resize_pending` (delta 56 > 64−4−30);
  `update hosts set disk_bytes=200G` → the engine approves by itself →
  `resizing` → `attached` at 60GiB, no operator action
- chaos `volume_stall_resize <volume>` then `--size 70` (room exists): the engine
  approves (`resizing`), the agent never reports → the volume **sits in
  `resizing`**, `ON DISK` lags; `update` → "is resizing; revert or wait";
  `revert` → "is resizing; nothing to revert" — there is deliberately no CLI
  exit from `resizing`; `volume_heal` → the agent grows and the engine settles.
  No timeout, no `failed` (same stance as 4).

Measured (2026-09-18 UTC, agentsim tick 1s, reconcile 2s; replica
`active|healthy|restart_count=0` throughout, lease untouched):

- deploy 15:25:59 → volume `attached` 2GiB + replica `active` 15:26:07
- `--size 4` 15:26:47 → `attached` 15:26:50 (**3s**)
- `--size 100` 15:27:22 → `resize_pending` 15:27:24 (**2s**); afterwards only a
  DEBUG "still waiting" per tick, status unchanged
- `up -s pg2` (30GiB) 15:28:04 → volume `attached` on `ue1-small-1` 15:28:07,
  replica `active` 15:28:11 — alongside the parked 100GiB request
- `revert` 15:29:08 → `attached` 4GiB 15:29:09 (**1s**), `previous` NULL
- `--size 60` 15:29:12 (CLI short 26GiB) → `resize_pending` 15:29:15; host
  80→200GB 15:29:17 → `resizing` 15:29:18 → `attached` 15:29:20 (**3s** from disk)
- stall 15:29:55 + `--size 70` → `resizing` 15:29:57, `ON DISK` stuck at 60GiB
  for 4+ ticks; `volume_heal` 15:30:01 → `attached` 70GiB 15:30:03 (**2s**)

Engine log: one INFO `volume grow waiting for host space` when parking, then
DEBUG `still waiting` per tick; `volume grow approved` /
`volume resized from=<status>` on flips.

#### Through chaos-ui

Volumes travel with the topology (`GET /v1/topology` → `services[].volumes`, one
`TopologyVolumes` query joining hosts, keyed by `environment_service_id`; a
stateless service carries `[]`), so a stateful service gets one row per volume
below its replica table: MOUNT | HOST | SIZE (desired, with `← previous` while a
revert target exists) | ON DISK (observed, orange while lagging; `—` until the
agent reports) | STATUS badge (`attached` green, `resizing` blue,
`resize_pending` orange, `pending` grey) | ⋯ menu. Two lanes, as with replicas:
**Grow** and **Revert** are operator intent and go to the control plane
(`POST /v1/volumes/resize` and `/v1/volumes/revert`; guards stay in `project`,
`ErrInvalid` → 409 with the message the CLI prints, `ErrNotFound` → 404);
**Stall resize** and **Heal** go to agentsim (`volume_stall_resize` /
`volume_heal` with the volume UUID from the row). GiB in the form, bytes on the
wire; API identity is `(target, mount_path)` as in the CLI, not the UUID.

- **Grow…** is offered only for `pending` or `attached` without drift (the same
  condition `update` accepts): inline GiB field → Activity
  `resize /var/lib/postgresql/data → 6GiB`; when the host has no room the
  response carries `waiting_for_space` + `shortfall_bytes` and the row shows the
  hint "host short 38GiB — parked as resize_pending" before the engine changes
  the status
- **Revert** only from `resize_pending`: Activity `revert … → 6GiB`; a second
  click → 409 "is attached; nothing to revert"
- **Stall resize** only for `attached` converged (so it catches the *next* grow),
  then Grow → the volume sits in `resizing`, ON DISK lags; **Heal** → the agent
  grows, the engine settles

Measured (2026-09-19 UTC, through the UI routes `/api/volumes/resize` and
`/api/chaos`): same latencies as the CLI path — grow 3s, park 2s, revert <1s,
heal 1s. The 409/404 messages come back verbatim from `project`: a second grow
while one is in flight → "already has a grow requested (6G → 100G); revert
first"; after the tick → "is resize_pending; revert or wait for it to attach";
unknown mount → 404.

Not verified by clicking in a browser: the Activity log and the hint are page
state after a click, and the scenario was driven through the same Next routes
the buttons call. Table and badge rendering was checked with a headless
screenshot in the `resize_pending` and `resizing` states.

### 12. Host drain — graceful evacuation

Requirement: the operator says "take everything off this host" and service
capacity must never drop below desired. Stateful replicas (with a volume) are
**not moved** — the disk is local; they stay and the operator migrates them
manually later.

Model: no new rule, no new Intent. `POST /v1/hosts/{id}/drain` writes
`status='draining'`, `drain_started_at=now()`. The snapshot carries
`host_draining` per replica (LEFT JOIN hosts), and the engine narrows that to
stateless replicas. Two changes in the rolling cascade:

- `healthyTargets` does not count a replica on a draining host → `rollingRampUp`
  creates the surge itself (replacements are created while the old ones still
  serve)
- `rollingScaleDown` sorts draining-host replicas first, then newest-first → once
  the replacement is healthy, the surplus that leaves is exactly the old ones

`notAllHealthy` between them looks at raw `Healthy`, not capacity — otherwise a
healthy replica on a draining host would hold the group forever before the
replacement even existed. The end of a drain is decided by the watchdog sweep:
`CompleteDrainedHosts` moves `draining → cordoned` when the host has no live
stateless replicas (`phase NOT IN (reaped, failed)` — it waits for the **reap**,
not the drain, since a drained replica still serves until the window expires).
The placer ledger holds all `host_healthy` hosts; `status='open'` filters only
`pick` (free placement); the DB belt `ReserveReplicaOnHost` does the same:
`host_healthy AND (status='open' OR volume_id IS NOT NULL)`.

Setup (all sub-scenarios; 3 web replicas in us-east-1 so anti-affinity puts one
on each of the 3 hosts):

```bash
make stack-up && make build
mkdir demo && cd demo
../build/conductor init -n chaos-demo
../build/conductor add --service --name web --image nginx:alpine
printf '[deploy]\nnum_replicas = 3\nregion = "us-east-1"\ndrain_seconds = 10\ncpu = "200m"\nmemory = "128Mi"\n' > config.toml
../build/conductor up -s web
curl -s localhost:7080/v1/hosts | jq -r '.[] | "\(.hostname) \(.id) healthy=\(.host_healthy) \(.status) repl=\(.replicas_on_host)"'
```

`GET /v1/hosts` (and topology `hosts`) returns `host_healthy`, `status`,
`drain_started_at`, `replicas_on_host` — track a drain from there or from the
chaos-ui Hosts panel (two badges: health + status, "draining since …", replica
count).

#### 12a. Drain a host with a stateless replica — surge, then scale-down

- `curl -XPOST localhost:7080/v1/hosts/<ue1-medium-1>/drain`
- next tick: `rollingRampUp → create` (one replacement; a canary if **all**
  replicas of the group sit on the draining host, otherwise the whole deficit at
  once)
- the replacement gets an open host, becomes healthy → `rollingScaleDown → drain`
  the old one
- the old one keeps serving for `drain_seconds` while `draining`, then
  `reapDrained → destroy`
- sweep ≤5s later: `watchdog -> drain complete, host cordoned`; the host stays
  `healthy` and `drain_started_at` is cleared

Measured (2026-09-18, reconcile 2s, drain_seconds 10): drain 10:15:08 → create
+1s → replacement `active+healthy` +4s → old one `draining` +5s → `reaped` +15s
→ host `cordoned` +17s. Healthy web replicas ≥ 3 the whole time.

#### 12b. Drain + host death with stateful and stateless replicas (reboot)

Extra setup: `add --database --engine postgres --name pg`,
`volume add --mount /var/lib/postgresql/data --size 2 -s pg`,
`up -s pg -f pg-config.toml` (num_replicas=1, us-east-1). pg landed on
`ue1-small-1` next to 2 web replicas.

- `drain <ue1-small-1>` and immediately chaos `host_kill <ue1-small-1>` (the
  agent goes silent)
- web: 2 replacements at once (a healthy web exists on another host, no canary
  needed) → healthy → the old 2 `draining` → reaped → **the sweep cordons the
  host while it is dead**: the end of a drain is read from replica rows, not
  from heartbeats
- pg stays bound (stateful does not move); +30s host `unhealthy`; +2min
  `MarkHostDown` releases pg → `replacing`, hostless, pinned to a volume whose
  host is out of the ledger → waits (assign_host is rejected every tick)
- `status` after death stays `cordoned` (MarkHostDown only touches `host_healthy`)
- chaos `host_recover` → heartbeat restores `healthy`, status still `cordoned`;
  the pinned placer branch ignores status → pg returns to the **same** host,
  lease reacquired, active

Measured (2026-09-18): drain+kill 10:17:22 → 2× create +1s → replacements active
+6s → old ones draining +6s → reaped +15s → `cordoned` +18s → `unhealthy` +33s →
MarkHostDown +2min03s (pg `replacing`, hostless) → recover 10:20:14 → healthy +1s
→ pg `health_check` on the same host +2s → `active` +3s. web 3 healthy throughout.

#### 12c. Drain a host that holds only stateful

- host with only pg on it (after 12b): `uncordon`, then `drain`
- nothing to move → first sweep: `cordoned`, pg untouched, `active`

Measured: drain 10:20:32 → `cordoned` 10:20:36 (one sweep). This is also the
signal to the operator: the host is "as empty as automation can make it", the
rest is manual.

#### 12d. Cancelling a drain / uncordon

- `POST /v1/hosts/{id}/uncordon` on `draining` or `cordoned` → 200,
  `status=open`, `drain_started_at=null`; the host accepts placement again
  immediately
- `cordon` on `draining` → 409 (don't erase a drain by accident); `drain` on
  `cordoned` → 200 (a cordon is "upgraded" to an evacuation)

Measured: uncordon 10:16:36 → `open`, `drain_started_at` null the same second.

#### 12e. Stalled drain — WARN, no action

When the replacement has nowhere to go (no open host with capacity in the
region, or it never becomes healthy), the drain sits: the old replica serves,
the host stays `draining`.

- `cordon` every other host in the region that has room; `drain` the host with
  the replica
- `rollingRampUp → create`, replacement `pending` hostless; `anyHostlessReplicas`
  emits `assign_host` every tick, the placer rejects it (no open host)
- after `drainStalledAfter` (10min): `watchdog -> drain stalled, still holding
  stateless replicas draining_for=…` WARN every sweep; `GET /v1/hosts` shows
  `drain_started_at` 10+ min old and `replicas_on_host > 0`
- nothing resolves by itself — a human frees capacity (`uncordon`, a new host)
  and the drain continues on its own

Measured (2026-09-18): cordon `ue1-medium-1` + drain `ue1-large-1` 10:21:10 →
create +1s, replacement `pending` with no host → first WARN 10:31:15
(`draining_for=10m5s`), then every 5s → uncordon `ue1-medium-1` 10:31:29 →
replacement `active` +3s → old one `draining` +4s → `reaped` +14s → `cordoned`
+17s. The old replica served through all 10min of waiting; healthy web ≥ 3.

Note: while the replacement waits for a host, `anyHostlessReplicas` logs
`assign_host` at INFO every tick (2s) — same as for a pinned replica on a dead
host (12b); pre-existing noise, not part of drain.

### 13. Log stream — replay, resume, engine restart

The engine keeps the last 5000 lines in a ring buffer (`internal/logbuf`) and
serves them over SSE on `CONDUCTOR_LOGS_ADDR` (`:7090`); chaos-ui only relays
them to the browser at `/api/logs/stream`, so the engine stays private. No
shared log volume, no file tailing — every line has a monotonic `id`, and that
is what a reconnect resumes from.

Requirement: a freshly opened Logs page must show history, a broken connection
must resume where it stopped, and a slow reader must not slow down the engine.

```bash
curl -N 'http://localhost:7090/logs/stream?tail=3'                 # straight from the engine
curl -N 'http://localhost:3000/api/logs/stream?tail=2'             # through the UI relay (what the browser calls)
curl -N -H 'Last-Event-ID: 23' 'http://localhost:3000/api/logs/stream?tail=800'
docker restart conductor-engine                                     # reconnect with the page open
```

- `?tail=N` → N lines of backlog, then live tail; `data:` is the unchanged
  TextHandler line, so `parseLine` in the UI does exactly what it did over the file
- `?since=N` and `Last-Event-ID: N` → only `id > N`; the header wins over the
  query, since EventSource sends the header itself on its own reconnect
- `: heartbeat` every 20s passes through both proxy hops (Railway edge + Next
  relay must not kill an idle connection)
- a slow reader is not waited for: when its buffer (256) fills, the channel is
  closed and the client comes back with its last id — the logger writes on the
  engine's tick loop, so blocking is not an option
- an engine restart resets the id space; a resume with an old (higher) id would
  otherwise wait for the new process to count up to it → the server serves a
  fresh tail instead

Measured (2026-09-19):

- `?tail=3` → backlog id 13–15 immediately, then live id 16, 17… every 2s
  (engine snapshot cadence); `?since=15` → first line id 16; `Last-Event-ID: 23`
  with `?tail=800` → first line id 24 (header wins)
- operator action visible in the stream through the relay:
  `POST /v1/replicas/<id>/restart` at 14:06:33 → `reconcile -> rule fired
  rule=anyHostlessReplicas intents=[assign_host]` 14:06:33.987 → `engine -> pass
  applied` 14:06:34.003
- 30s capture: 16 lines (~32 lines/min at idle DEBUG) and exactly 1 heartbeat →
  a 5000-line ring ≈ 2.5h of idle history, ~600 KB
- Logs page (headless Chrome): 240 rows on open, oldest `storage: connected`
  from 8min earlier (so replay, not just live), status "streaming from the
  engine", +3 rows in 6s
- `docker restart conductor-engine` at +6.4s → "stream down" banner at +7.9s →
  "streaming from the engine" at +9.5s with a fresh tail from the new process
  (14 → 19 rows), no page reload

## Through chaos-ui (localhost:3000)

The Chaos tab covers every action, by target:

- **Replica**: Crash (terminal `failed`), Crash loop (restart_count grows until
  the budget breaks), Stall health checks (progress-deadline path), Heal (clear
  chaos), Delete row (orphan simulation — the only direct DB write)
- **Deployment**: Crash deployment / Stall rollout — fan-out of the same agent
  action to every live replica of the deployment
- **Host**: Kill host (agent goes silent → `unhealthy` at ~30s → dead at 2min),
  Recover (first heartbeat restores `healthy`), Cordon and Drain (operator
  desired state through the apiserver). The Hosts panel carries two badges:
  health (`healthy|unhealthy`) and status (`cordoned|draining`; `open` is not
  shown), plus replica count and "draining since …" while a drain runs
- **Volume** (row below the replicas of a stateful service; MOUNT | HOST | SIZE |
  ON DISK | STATUS): Grow… (inline GiB, control plane; hint "host short N GiB —
  parked as resize_pending" when there is no room), Revert (only
  `resize_pending`, control plane), Stall resize / Heal (agentsim
  `volume_stall_resize` / `volume_heal`). Steps and measurements in §11 "Through
  chaos-ui".

Every agent-observable action is forwarded by the UI to the agentsim control API
(`AGENTSIM_URL`, `http://agentsim:7780` in the stack) — chaos travels over the
real transport (the agent lies or goes silent over gRPC), so the next report
cannot overwrite it. curl equivalent: `POST :7780/chaos` with the same `action`
field. Watch transitions on the Topology tab and in Logs
(`watchdog -> stale hosts out of scheduling`, `watchdog -> host down`,
`reconcile -> rule fired`).
