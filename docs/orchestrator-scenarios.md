# Orchestrator — what it must cover

> What the reconcile engine (`orchestration-engine.md`) has to handle, scenario by
> scenario, grounded in the schema (`db/migrations/00001_create_control_plane.sql`)
> and the CLI that already writes desired state (`init` / `add` / `up`).

## The one idea

Every entity stores **desired** vs **observed** state. The engine's whole job is
closing that gap, **level-triggered**: on every tick it reads "what should be"
and "what is", computes the diff, and takes the next step toward convergence —
never assuming it remembers anything from last tick. That property is what makes
it survive control-plane restarts.

```
desired (committed by the CLI)            observed (written by the engine)
  deployments (is_current) ............... replicas (phase, healthy, host_id)
  deployment_regions {region: N} ......... hosts (capacity, heartbeat, status)
  volumes (desired_size) ................. volume_leases (single writer)
```

## Vocabulary → tables

| Term            | Table                                  | Who writes it          |
| --------------- | -------------------------------------- | ---------------------- |
| desired commit  | `deployments` + `deployment_regions`   | CLI (`up`, `scale`)    |
| running atom    | `replicas` (`phase`, `healthy`)        | **engine**             |
| fleet node      | `hosts` (capacity, `last_heartbeat`)   | agent / heartbeat      |
| persistent disk | `volumes` + `volume_leases`            | **engine**             |

`replicas.phase`: `pending → scheduling → starting → health_check → healthy →
shifting → active → draining → reaped` (also `failed`).
`deployments.status`: `pending → active → draining / failed / rolledback / superseded`.

---

## Scenarios

Each: **trigger → the gap → what the engine does → tables touched**. Tagged
`[v1]` (stateless first slice), `[next]`, or `[stateful]`.

### 1. Initial deploy: 0 → N replicas `[v1]`
- **Trigger:** `up` writes a `current` deployment + `deployment_regions {us-east-1: 3}`.
- **Gap:** desired 3 replicas in us-east-1, observed 0.
- **Engine:** create 3 `replicas` (ordinals 0–2) → **schedule** each (bin-pack onto
  a `ready` host in-region with free cpu/mem) → set `host_id` + reserved cpu/mem →
  start → health-gate → `active`. Mark `deployments.status='active'` when all healthy.
- **Tables:** `replicas` (create + phase), capacity read from `hosts` − Σ replicas.

### 2. Scale up: N → N+M `[v1]`
- **Trigger:** `scale us-east-1=5` (or an `up` with a higher `num_replicas`).
- **Gap:** desired 5, observed 3.
- **Engine:** create 2 more replicas, schedule + start them. Existing 3 untouched.

### 3. Scale to zero / `down`: N → 0 `[v1]`
- **Trigger:** `down`, or `scale region=0`.
- **Gap:** desired 0, observed 3.
- **Engine:** drain then reap all replicas. **Volumes are preserved** (stateful
  data survives) — only compute stops. Service + deployment rows remain.

### 4. New-version rollout (stateless) `[v1 / next]`
- **Trigger:** `up` with a new image/config → new `deployments` row, old → `superseded`.
- **Gap:** observed replicas point at the old deployment; desired is the new one.
- **Engine (surge):** start NEW replicas alongside old → health-gate → **shift**
  traffic → **drain** old (`drain_seconds`) → **reap** old. If new never goes
  healthy within timeout → stop, leave old serving, `status='failed'`.
- **Why it's the hard part (#7):** ordered, timed, retryable. Doable in the loop
  with `phase` + a deadline column; a candidate for Temporal if it grows.

### 5. Rollback — desired-state half DONE `[engine: next]`
- **Trigger:** `conductor rollback [--to vN]` re-points `is_current` to an older
  deployment (old current → `rolledback`, target → `pending`). Implemented today:
  no rebuild, the target row's image/env/sizing are reused verbatim (never reads
  config.toml). Default target = the version before current; `--to vN` picks one.
- **Gap:** same as a rollout, but the target image already exists.
- **Engine:** identical convergence as #4, toward the re-pointed version (pending).

### 6. Replica crash / unhealthy → self-heal `[v1]`
- **Trigger:** a replica exits or fails its health check (`healthy=false`).
- **Engine:** restart it with backoff, incrementing `restart_count`, capped at
  `restart_max`. Past the cap → `phase='failed'`, surface it; don't hot-loop.
- **Note:** this is what proves the loop is level-triggered — delete a replica row
  and the next tick recreates it.

### 7. Host failure / unreachable → reschedule `[next]`
- **Trigger:** `hosts.last_heartbeat` goes stale → mark `status='notready'`.
- **Engine:** its replicas are presumed dead → reschedule them onto other hosts
  (new placement). Stateful needs the volume reachable first (see #11–12).

### 8. Host drain / cordon (maintenance) `[next]`
- **Trigger:** operator sets `hosts.status='draining'`/`'cordoned'`.
- **Engine:** `cordoned` = no new placements here; `draining` = evict existing
  replicas (reschedule elsewhere) then it's safe to pull the box.

### 9. No capacity / unschedulable `[v1]`
- **Trigger:** desired replicas exceed free fleet capacity in the region.
- **Engine:** replica stays `pending` with an `alloc_reason` ("no host with 500m
  free in us-east-1"). Do **not** overcommit. Place automatically once capacity frees.

### 10. Multi-region spread `[next]`
- **Trigger:** `deployment_regions {us-east-1: 3, eu-west-1: 2}`.
- **Engine:** reconcile each region independently against in-region hosts;
  a region with no capacity stays pending without blocking the others.

### 11. Stateful placement + volume provisioning `[stateful]`
- **Trigger:** `add --database` then `up` for a `stateful` service.
- **Gap:** each ordinal needs a `volume` (e.g. `pg-0`) before the replica can run.
- **Engine:** provision a `volume` per ordinal, place it on a host by `disk_bytes`,
  pin `replicas.volume_id`, and co-locate (local) or attach (networked). Placement
  is disk-aware, not just cpu/mem.

### 12. Stateful rollout / failover → volume handoff `[stateful]`
- **Trigger:** new version, or the host holding `pg-0` dies.
- **Constraint:** exactly one writer per volume (`volume_leases` PK = physical guarantee).
- **Engine (ordered, NO surge):** drain old `pg-0` → release/expire its lease
  (`expires_at` fencing ⇒ old can't write) → new `pg-0` acquires the lease →
  attach volume → start → health-gate → only then `pg-1`. One ordinal at a time.

### 13. Volume resize (grow-only) `[stateful]`
- **Trigger:** `desired_size_bytes` raised.
- **Engine:** grow the disk, reconcile `observed_size_bytes` up. Never shrink.

### 14. Control-plane restart / concurrency `[v1]`
- **Trigger:** the engine process restarts mid-work, or two ticks overlap.
- **Engine:** re-derive everything from DB each tick (no in-memory truth). Guard
  every observed-state write with the `revision` column (optimistic concurrency)
  so a stale actor can't clobber a newer one. All steps idempotent.

### 15. Config-only change vs image change `[v1]`
- Both are just a new `deployments` row → both trigger a rollout (#4). The engine
  doesn't special-case "only env changed" in v1; the version bump is the signal.

---

## Stateless vs stateful — the split that drives everything

| | Stateless | Stateful |
| --- | --- | --- |
| Placement | cpu/mem bin-pack, any host | + `disk_bytes`, pinned to its volume's host (local) |
| Rollout | **surge** (new before old) | **handoff**, one ordinal at a time, no overlap |
| Identity | fungible | stable `ordinal` (`pg-0`) tied to a volume |
| Writer safety | n/a | single-writer `volume_lease` + fencing |
| Scale to zero | drop all | drop compute, **keep volumes** |

## Build order

1. **`[v1]` stateless steady-state** — scenarios 1, 2, 3, 6, 9, 14: replica CRUD
   queries, host-capacity query, bin-packer, poll loop, simulated executor. Proves
   convergence + self-heal; makes `status` show real numbers.
2. **`[next]` rollout + fleet** — scenarios 4, 5, 7, 8, 10: version transitions
   (surge/drain), host liveness, multi-region.
3. **`[stateful]`** — scenarios 11, 12, 13: volumes, leases, handoff. The hard tail.

## Decisions still open

- **Reconcile loop = plain Go (A) vs Temporal (B).** Recommended: **A** for
  steady-state (it's already level-triggered via Postgres); reach for **B** only if
  the rollout state machine (#4/#12) gets unwieldy — kick off one workflow keyed by
  `deployment_id` (idempotent: Temporal dedupes by id).
- **Executor:** simulated (flip phases on a timer) for v1; real container runtime
  + per-host agent later. The loop logic is identical either way.
