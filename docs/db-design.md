# Database Design — Control-Plane State Store

> Status: **draft for review.** Reconciles the relational schema against the domain model in `orchestration-erd.html`
> (§3 Domain, §6 Scheduler, §7 Lifecycle, §13 Public Interface). The DDL below is the proposed canonical schema. Once
> approved it becomes goose migrations + sqlc queries.
>
> **No migration is generated yet** — this doc is the thing to review.

## Guiding principle (from the ERD)

> _"Each entity stores desired vs observed state. The reconciler's whole job is closing that gap."_ — ERD §3

## ERD entity → table mapping

| ERD entity (§3)             | Table(s)                             | Notes                                                            |
| --------------------------- | ------------------------------------ | ---------------------------------------------------------------- |
| Project / Environment (§13) | `projects`, `environments`           | identity tree                                                    |
| Service                     | `services`                           | `stateful boolean` drives placement/rollout (§4)                 |
| Deployment                  | `deployments` + `deployment_regions` | immutable, append-only commit; `is_current` marks the active one |
| Replica (Task)              | `replicas`                           | running atom; allocation folded in                               |
| Host (Node)                 | `hosts`                              | capacity + region + status + heartbeat                           |
| Volume                      | `volumes` + `volume_leases`          | lease is its own table = single-writer invariant (§4)            |

## No circular FKs — by design

Two relationships _look_ circular; both are resolved by **not** storing a back-pointer:

**Service Enviroment ↔ Deployment.** A deployment belongs to a service env(`deployments.enviroments_services_id`).
Instead of `enviroments_services_id.current_deployment_id` pointing back, the active commit is a flag on the deployment:

```sql
ALTER TABLE deployments ADD COLUMN is_current boolean NOT NULL DEFAULT false;
CREATE UNIQUE INDEX one_current_deployment_per_service
  ON deployments (service_id) WHERE is_current;   -- at most one current per service
```

"Current desired state" = `SELECT … FROM deployments WHERE service_id = $1 AND is_current`. Rollback = clear the flag on
the old current, set it on the target — still append-only.

**Volume ↔ Replica.** A replica is pinned to its volume (`replicas.volume_id`). The single-writer lease is _not_ a
column on `volumes` pointing back at a replica — it's its own table, created after both, so no cycle:

```sql
volume_leases (
  volume_id   uuid PRIMARY KEY REFERENCES volumes  ON DELETE CASCADE,  -- PK ⇒ one writer
  replica_id  uuid NOT NULL    REFERENCES replicas ON DELETE CASCADE,
  expires_at  timestamptz NOT NULL,                                    -- §8 fencing
  acquired_at timestamptz NOT NULL DEFAULT now()
);
```

The `volume_id` primary key _is_ the single-writer guarantee — the DB physically cannot hold two leases for one volume.
Fencing = delete the row or let `expires_at` lapse. This is cleaner than a nullable back-reference: the lease becomes a
first-class, queryable, expirable entity.

Result: every table can be created in dependency order in one pass — no trailing `ALTER TABLE … ADD CONSTRAINT`.

## Schema

### 1. Identity / intent

```sql
projects (
  id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  name       text        NOT NULL UNIQUE,
  created_at timestamptz NOT NULL DEFAULT now()
);

environments (
  id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid        NOT NULL REFERENCES projects ON DELETE CASCADE,
  name       text        NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (project_id, name)
);

enviroments_services {
	id,
    environment_id uuid        NOT NULL REFERENCES environments ON DELETE CASCADE,
        service_id uuid        NOT NULL REFERENCES services ON DELETE CASCADE,
     source         jsonb       NOT NULL DEFAULT '{}',     -- {repo,image,builder} free-form  NOT SURE ABOUT WHERE TO PUT THIS
     UNIQUE (environment_id, service_id)

}

services (
  id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),,
  name           text        NOT NULL,
  stateful       boolean     NOT NULL DEFAULT false,   -- §4 strategy flag
  created_at     timestamptz NOT NULL DEFAULT now(),
);
```

### 2. Desired state — append-only versioned commits (§13)

```sql
deployments (
  id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  enviroments_services_id     uuid        NOT NULL REFERENCES enviroments_services ON DELETE CASCADE,
  version        int         NOT NULL,                  -- monotonic per service
  is_current     boolean     NOT NULL DEFAULT false,    -- the active commit
  image_ref      text        NOT NULL,
  cpu_millicores int         NOT NULL,
  mem_bytes      bigint      NOT NULL,
  env            jsonb       NOT NULL DEFAULT '{}',
  healthcheck    jsonb       NOT NULL DEFAULT '{}',     -- {path,timeout_s}
  drain_seconds  int         NOT NULL DEFAULT 30,
  restart_max    int         NOT NULL DEFAULT 5,
  commit_message text,
  created_by     text,
  status         text        NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending','active','draining','failed','rolledback','superseded')),
  created_at     timestamptz NOT NULL DEFAULT now(),
  UNIQUE (service_id, version)
);
CREATE UNIQUE INDEX one_current_deployment_per_service
  ON deployments (service_id) WHERE is_current;

-- multiRegionConfig { region -> numReplicas }
deployment_regions (
  deployment_id uuid NOT NULL REFERENCES deployments ON DELETE CASCADE,
  region        text NOT NULL,
  replicas      int  NOT NULL CHECK (replicas >= 0),
  PRIMARY KEY (deployment_id, region)
);
```

### 3. Runtime / observed

```sql
hosts (
  id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  region         text        NOT NULL,
  hostname       text        NOT NULL UNIQUE,
  cpu_millicores int         NOT NULL,
  mem_bytes      bigint      NOT NULL,
  disk_bytes     bigint      NOT NULL,                  -- stateful volume placement (§6)
  labels         jsonb       NOT NULL DEFAULT '{}',
  status         text        NOT NULL DEFAULT 'ready'
                 CHECK (status IN ('ready','notready','draining','cordoned')),
  last_heartbeat timestamptz,
  revision       bigint      NOT NULL DEFAULT 0,        -- §6 optimistic-concurrency bind
  created_at     timestamptz NOT NULL DEFAULT now()
);

volumes (
  id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  service_id          uuid        NOT NULL REFERENCES services ON DELETE RESTRICT,
  name                text        NOT NULL,             -- stable identity, e.g. pg-0
  region              text        NOT NULL,
  host_id             uuid        REFERENCES hosts,      -- where the disk lives (null until provisioned)
  backing             text        NOT NULL DEFAULT 'local'
                      CHECK (backing IN ('local','networked')),   -- §4b failover story
  desired_size_bytes  bigint      NOT NULL,
  observed_size_bytes bigint,                            -- §4b resize drift (grow-only)
  status              text        NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending','attached','detached','resizing','failed')),
  revision            bigint      NOT NULL DEFAULT 0,
  created_at          timestamptz NOT NULL DEFAULT now(),
  UNIQUE (service_id, name)
);

-- The running atom (§3). Allocation folded in: host_id + reserved cpu/mem + reason.
replicas (
  id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
  deployment_id    uuid        NOT NULL REFERENCES deployments ON DELETE CASCADE,
  region           text        NOT NULL,
  ordinal          int         NOT NULL,                -- §4b stable identity index
  identity         text,                                -- stable name, e.g. pg-0.internal
  host_id          uuid        REFERENCES hosts,          -- null until placed
  volume_id        uuid        REFERENCES volumes,        -- stateful: the pinned volume
  cpu_millicores   int         NOT NULL,                 -- reserved (folded allocation)
  mem_bytes        bigint      NOT NULL,
  alloc_reason     text,                                 -- scheduler explanation
  desired_status   text        NOT NULL DEFAULT 'running'
                   CHECK (desired_status IN ('running','stopped')),
  phase            text        NOT NULL DEFAULT 'pending'
                   CHECK (phase IN ('pending','scheduling','starting','health_check',
                                    'healthy','shifting','active','draining','reaped','failed')),  -- §7
  healthy          boolean     NOT NULL DEFAULT false,
  restart_count    int         NOT NULL DEFAULT 0,        -- §8 backoff cap
  last_exit_reason text,
  revision         bigint      NOT NULL DEFAULT 0,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (deployment_id, ordinal)
);

-- Single-writer lease (§4). PK on volume_id is the invariant. Created last → no cycle.
volume_leases (
  volume_id   uuid        PRIMARY KEY REFERENCES volumes  ON DELETE CASCADE,
  replica_id  uuid        NOT NULL    REFERENCES replicas ON DELETE CASCADE,
  expires_at  timestamptz NOT NULL,                       -- §8 fencing
  acquired_at timestamptz NOT NULL DEFAULT now()
);
```

Table creation order (one pass, no `ALTER`): projects → environments → services → deployments → deployment_regions →
hosts → volumes → replicas → volume_leases.

## Conventions

- **PKs:** `uuid DEFAULT gen_random_uuid()` (needs `pgcrypto`); app may supply ids.
- **Natural keys** as `UNIQUE`; **FKs cascade** down the project→env→service tree; `ON DELETE RESTRICT` where a child
  must outlive a careless delete (volumes).
- **`timestamptz`** everywhere; `created_at`/`updated_at`.
- **Status as `text` + `CHECK`**, not Postgres `enum` (enums are painful to alter as the lifecycle grows).
- **jsonb only for free-form** (`source`, `env`, `healthcheck`, `labels`); real columns for anything
  filtered/joined/constrained.
- **Optimistic concurrency** via `revision` on `hosts`/`replicas`/`volumes`.

## Postgres vs Temporal

This store holds **desired + observed state**. The **rollout state machine** (§7, surge/drain sequencing) is a
**Temporal workflow** (§5, §10) — `deployments.status` records the outcome; the in-flight choreography is Temporal's
history, not a table.

## Relationship to current code

This supersedes the generic `records`/jsonb store for these entities; the `storage.Client` (collection/key/blob) gives
way to per-entity sqlc repositories. `project.Project` maps to `projects`+`environments`+`services` rows. The `records`
t
