# Conductor

A container orchestrator, written from scratch in Go. You declare what you want
running — *5 replicas of this image in us-east-1, 500m CPU, healthcheck on
`/health`* — and a closed control loop makes the fleet match, one tick at a
time, and keeps it matching while hosts die underneath it.

## What it does

The whole system is one idea: **desired state** and **observed state** are
separate, and a loop closes the gap between them.

- **Desired state** is what you asked for. `conductor up` (or the UI) commits it
  to Postgres. Nothing else writes it.
- **Observed state** is what the fleet reports. Host agents heartbeat their
  replicas in over gRPC. Nothing else writes it.
- **The engine** reads both and makes reality move.

Each tick, in order:

1. **Watchdog** — sweeps heartbeats. A host silent past the threshold is marked
   down, and its replicas are freed for re-placement.
2. **Reconciler** — diffs desired against observed and emits *intents* (create,
   drain, destroy), placing new replicas with best-fit-decreasing bin-packing
   against a capacity ledger, honouring anti-affinity and volume pinning.
3. **Actuator** — applies each intent in its own transaction. Conflicts are
   dropped, not retried; the next tick re-derives them from fresh state.

The loop is **level-triggered**: a pass runs to completion, then waits. A slow,
failed, or skipped pass isn't an outage — the next pass sees the same gap and
closes it. That property is what the rest is built on: blue/green rollouts with
an atomic traffic switch, stateful services held to a single writer by volume
leases, restart budgets that fail a bad deploy instead of crash-looping forever,
and progress deadlines for rollouts that never go healthy.

`agentsim` exists to break all of it on purpose, and `chaos-ui` to watch it
recover.

This is a learning project. The interesting part is the engine, not production
readiness.

## Quick start

Needs Go, Docker, and [goose](https://github.com/pressly/goose).

```bash
make db-up migrate seed         # Postgres + schema + a seeded host fleet
make build                      # → ./build/conductor

./build/conductor init -n demo  # create project, link this directory
./build/conductor add --service --name web --image nginx:alpine
./build/conductor up -s web     # commit desired state
./build/conductor status        # watch the loop converge
```

Identity (project / environment / service) comes from the folder link written by
`init`, or from `-p/-e/-s`, or from `CONDUCTOR_*` env vars — in that order of
precedence. Build and deploy settings come from a `config.toml` next to your
service; see `example/config.toml`.

Or run everything — engine, apiserver, simulated agent fleet, UI — in containers:

```bash
make stack-up                   # UI on http://localhost:3000
```

| Port | What |
|---|---|
| `3000` | chaos UI (`CHAOS_UI_PORT` to move it) |
| `7080` | OperatorAPI — HTTP control plane |
| `7443` | AgentAPI — gRPC, agents dial in |
| `7090` | engine log stream (SSE): `curl -N localhost:7090/logs/stream` |
| `7780` | agentsim chaos control API |
| `5432` | Postgres |

## Components

| Path | What |
|---|---|
| `cmd/` | `conductor` CLI — init, add, up, scale, down, volume, status… (`cmd/README.md`) |
| `internal/engine/` | the loop: watchdog → reconciler → actuator, plus placement, volume leases, supervisor |
| `internal/api/` | apiserver. `AgentAPI` (gRPC) writes `ObservedState`; `OperatorAPI` (HTTP `/v1/…`) writes `DesiredState` |
| `internal/project/` | the rules — every desired-state write, from CLI or UI, goes through here |
| `internal/storage/` | Postgres control plane; sqlc-generated queries, goose migrations in `db/` |
| `internal/deployspec/` | `config.toml` parsing, with `[environments.NAME.*]` overrides |
| `builder/` | source-to-image extension point; the default no-op synthesizes a ref so the control plane works with no build pipeline |
| `agentsim/` | simulated host agents — the deliberate chaos injection point |
| `chaos-ui/` | Next.js dashboard |
| `proto/agentpb/` | the agent wire protocol |
| `docs/scenarios.md` | failure scenarios as repeatable steps, with measured results |

Chaos is routed on purpose. Agent-observable failure (a host going silent, an
agent lying about a replica) goes to agentsim so it travels the real gRPC
transport. Operator actions (cordon, drain, delete a replica) go to the control
plane. The UI holds no database credentials at all — it reads topology and
writes desired state over `CONTROL_PLANE_URL`, so UI and CLI commit through the
same project layer and the same rules.

## Development

```bash
make test           # unit + e2e (e2e drives real ticks against an in-memory store)
make lint           # golangci-lint
make sqlc           # regenerate queries after editing db/queries/
make migrate-fresh  # wipe + re-apply migrations (stack equivalent: make stack-fresh)
make ui-dev         # Next.js dev server against a running apiserver
make stack-logs     # follow every stack service
```
