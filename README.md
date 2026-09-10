# Conductor

A Railway-style deployment platform, built from scratch in Go — CLI, control plane, and a reconciliation engine that turns *"I want 5 replicas of this in us-east-1"* into reality, one tick at a time.

## What it does

You describe a service (image or repo, replicas, resources, health checks) and `conductor up` commits that as desired state in Postgres. The engine's closed loop does the rest:

- **Sensor** — ingests host heartbeats and replica observations; marks dead hosts down and frees their replicas for re-placement
- **Reconciler** — diffs desired vs observed state, plans intents (create, drain, destroy), and bin-packs replicas onto hosts (best-fit decreasing, capacity ledger, anti-affinity)
- **Actuator** — applies each intent in its own transaction; conflicts are dropped and self-heal next tick

On top of that loop: blue/green rollouts with atomic traffic switch, stateful services with single-writer volume leases, restart budgets, and a chaos UI for watching it all break and recover.

This is a learning project — the interesting part is the engine, not production readiness.

## Layout

| Path | What |
|---|---|
| `cmd/` | `conductor` CLI (init, add, up, scale, status…) — see `cmd/README.md` |
| `internal/engine/` | sensor → reconciler → actuator loop, placement, supervisor |
| `internal/storage/` | Postgres control plane (sqlc, goose migrations in `db/`) |
| `agentsim/` | simulated host agents — the deliberate chaos injection point |
| `chaos-ui/` | Next.js dashboard over the control plane + engine log |

## Quick start

Requires Go, Docker, and [goose](https://github.com/pressly/goose).

```bash
make db-up migrate seed        # Postgres + schema + a seeded host fleet
make build                     # → ./build/conductor

./build/conductor init -n demo # create project, link this dir
./build/conductor add --service --name web --image nginx:alpine
./build/conductor up -s web    # commit desired state
./build/conductor status       # watch the loop converge
```

Or run the whole thing — engine + chaos UI — in containers:

```bash
make stack-up                  # http://localhost:3000
```

Deploy settings live in a `config.toml` next to your service (see `example/config.toml`); identity (project/env/service) comes from the folder link or `-p/-e/-s` flags.

## Development

```bash
make test          # unit + e2e (e2e drives real ticks against an in-memory store)
make lint          # golangci-lint
make sqlc          # regenerate queries after editing db/queries/
make migrate-fresh # wipe + re-apply migrations
```
