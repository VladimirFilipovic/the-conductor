-- +goose Up

-- Topology / reference ------------------------------------------------------

-- The closed set of regions the fleet spans. Modeled as a table (not free text)
-- so every region column below can be FK-guarded: a typo like 'us-east1' fails
-- the write instead of silently creating a phantom region nothing can schedule
-- onto. Seeded here, not in db/seeds, because the region set is fixed infra —
-- the FKs must be satisfiable on a plain `migrate`, before any dev fixtures.
CREATE TABLE regions (
	name       text        PRIMARY KEY,
	created_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO regions (name) VALUES
	('us-east-1'),
	('us-west-2'),
	('eu-west-1');

-- Identity / intent ---------------------------------------------------------

CREATE TABLE projects (
	name       text        PRIMARY KEY, -- i know its weird but i look at this projec t like non multinant, each project is unique
	created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE environments (
	id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
	project_name text        NOT NULL REFERENCES projects(name) ON DELETE CASCADE,
	name         text        NOT NULL,
	created_at   timestamptz NOT NULL DEFAULT now(),
	UNIQUE (project_name, name)
);

CREATE TABLE services (
	id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
	project_name text        NOT NULL REFERENCES projects(name) ON DELETE CASCADE,
	name         text        NOT NULL,
	stateful     boolean     NOT NULL DEFAULT false,   -- §4 strategy flag
	created_at   timestamptz NOT NULL DEFAULT now(),
	UNIQUE (project_name, name)
);

-- A service's presence in an environment. The free-form source ({repo,image,
-- builder}) lives here because it is per-environment, not per-service.
CREATE TABLE environment_services (
	id             uuid  PRIMARY KEY DEFAULT gen_random_uuid(),
	environment_id uuid  NOT NULL REFERENCES environments ON DELETE CASCADE,
	service_id     uuid  NOT NULL REFERENCES services     ON DELETE CASCADE,
	source         jsonb NOT NULL DEFAULT '{}',
	UNIQUE (environment_id, service_id)
);

-- PG indexes only the leading column of the UNIQUE above; the service_id side
-- needs its own for FK cascades and reverse lookups.
CREATE INDEX environment_services_service_id_idx ON environment_services (service_id);

-- Desired state — append-only versioned commits (§13) -----------------------

CREATE TABLE deployments (
	id                     uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
	environment_service_id uuid        NOT NULL REFERENCES environment_services ON DELETE CASCADE,
	version                int         NOT NULL,                  -- monotonic per service env
	is_current             boolean     NOT NULL DEFAULT false,    -- the active commit
	image_ref              text        NOT NULL,
	cpu_millicores         int         NOT NULL CHECK (cpu_millicores > 0),
	mem_bytes              bigint      NOT NULL CHECK (mem_bytes > 0),
	env                    jsonb       NOT NULL DEFAULT '{}',
	healthcheck            jsonb       NOT NULL DEFAULT '{}',     -- {path,timeout_s}
	drain_seconds          int         NOT NULL DEFAULT 30,
	restart_max            int         NOT NULL DEFAULT 5,
	commit_message         text,
	created_by             text,
	status                 text        NOT NULL DEFAULT 'pending'
	                       CHECK (status IN ('pending','active','draining','failed','rolledback','superseded')),
	created_at             timestamptz NOT NULL DEFAULT now(),
	UNIQUE (environment_service_id, version)
);

-- At most one current commit per service env (§13 rollback flips the flag).
CREATE UNIQUE INDEX one_current_deployment_per_service
	ON deployments (environment_service_id) WHERE is_current;

-- multiRegionConfig { region -> numReplicas }
CREATE TABLE deployment_regions (
	deployment_id uuid NOT NULL REFERENCES deployments ON DELETE CASCADE,
	region        text NOT NULL REFERENCES regions(name),
	replicas      int  NOT NULL CHECK (replicas >= 0),
	PRIMARY KEY (deployment_id, region)
);

-- Runtime / observed --------------------------------------------------------

CREATE TABLE hosts (
	id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
	region         text        NOT NULL REFERENCES regions(name),
	hostname       text        NOT NULL UNIQUE,
	cpu_millicores int         NOT NULL CHECK (cpu_millicores > 0),
	mem_bytes      bigint      NOT NULL CHECK (mem_bytes > 0),
	disk_bytes     bigint      NOT NULL CHECK (disk_bytes > 0),  -- stateful volume placement (§6)
	labels         jsonb       NOT NULL DEFAULT '{}',
	status         text        NOT NULL DEFAULT 'ready'
	               CHECK (status IN ('ready','notready','draining','cordoned')),
	last_heartbeat timestamptz,
	revision       bigint      NOT NULL DEFAULT 0,        -- §6 optimistic-concurrency bind
	created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE volumes (
	id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
	service_id          uuid        NOT NULL REFERENCES services ON DELETE RESTRICT,
	name                text        NOT NULL,             -- engine-internal id (slug of mount_path)
	mount_path          text        NOT NULL,             -- user-facing handle: the container mount point
	region              text        NOT NULL REFERENCES regions(name),
	host_id             uuid        REFERENCES hosts,      -- where the disk lives (null until provisioned)
	backing             text        NOT NULL DEFAULT 'local'
	                    CHECK (backing IN ('local','networked')),   -- §4b failover story
	desired_size_bytes  bigint      NOT NULL CHECK (desired_size_bytes > 0),
	observed_size_bytes bigint,                            -- §4b resize drift (grow-only)
	status              text        NOT NULL DEFAULT 'pending'
	                    CHECK (status IN ('pending','attached','detached','resizing','failed')),
	revision            bigint      NOT NULL DEFAULT 0,
	created_at          timestamptz NOT NULL DEFAULT now(),
	UNIQUE (service_id, name),
	UNIQUE (service_id, mount_path)
);

CREATE INDEX volumes_host_id_idx ON volumes (host_id);

-- The running atom (§3). Allocation folded in: host_id + reserved cpu/mem + reason.
-- Railway-style: replicas are fungible (stateless) or a singleton (a volume-backed
-- service runs ONE replica, recreate-on-deploy) — so there is no stable ordinal or
-- per-ordinal identity. Replicated stateful sets (k8s StatefulSet) are out of scope;
-- HA is composed from multiple single-instance services at the app layer.
CREATE TABLE replicas (
	id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
	deployment_id    uuid        NOT NULL REFERENCES deployments ON DELETE CASCADE,
	region           text        NOT NULL,
	host_id          uuid        REFERENCES hosts,         -- null until placed
	volume_id        uuid        REFERENCES volumes,       -- stateful: the pinned volume
	cpu_millicores   int         NOT NULL CHECK (cpu_millicores > 0),  -- reserved (folded allocation)
	mem_bytes        bigint      NOT NULL CHECK (mem_bytes > 0),
	alloc_reason     text,                                 -- scheduler explanation
	desired_status   text        NOT NULL DEFAULT 'running'
	                 CHECK (desired_status IN ('running','stopped')),
	phase            text        NOT NULL DEFAULT 'pending'
	                 CHECK (phase IN ('pending','scheduling','starting','health_check',
	                                  'healthy','shifting','active','draining','reaped','failed',
	                                  'replacing')),  -- §7
	healthy          boolean     NOT NULL DEFAULT false,
	restart_count    int         NOT NULL DEFAULT 0,        -- §8 backoff cap
	last_exit_reason text,
	revision         bigint      NOT NULL DEFAULT 0,
	created_at       timestamptz NOT NULL DEFAULT now(),
	updated_at       timestamptz NOT NULL DEFAULT now(),

	-- A replica may only exist for a (deployment, region) the user actually asked
	-- for. The denormalized region column stays (the scheduler filters on it every
	-- tick) — this composite FK just keeps it honest. Transitively it also forces
	-- region ∈ regions, so no separate FK to regions is needed. Default NO ACTION:
	-- a region entry can't be dropped out from under live replicas — they must be
	-- drained/reaped first (deleting the whole deployment still cascades cleanly).
	FOREIGN KEY (deployment_id, region) REFERENCES deployment_regions (deployment_id, region)
);

CREATE INDEX replicas_host_id_idx   ON replicas (host_id);
CREATE INDEX replicas_volume_id_idx ON replicas (volume_id);

-- Keep updated_at honest without every UPDATE having to remember it.
-- +goose StatementBegin
CREATE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
	NEW.updated_at := now();
	RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER replicas_set_updated_at
	BEFORE UPDATE ON replicas
	FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Single-writer lease (§4). PK on volume_id is the invariant. Created last → no cycle.
CREATE TABLE volume_leases (
	volume_id   uuid        PRIMARY KEY REFERENCES volumes  ON DELETE CASCADE,
	replica_id  uuid        NOT NULL    REFERENCES replicas ON DELETE CASCADE,
	expires_at  timestamptz NOT NULL,                       -- §8 fencing
	acquired_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX volume_leases_replica_id_idx ON volume_leases (replica_id);

-- +goose Down
DROP TABLE volume_leases;
DROP TABLE replicas;
DROP FUNCTION set_updated_at;
DROP TABLE volumes;
DROP TABLE hosts;
DROP TABLE deployment_regions;
DROP TABLE deployments;
DROP TABLE environment_services;
DROP TABLE services;
DROP TABLE environments;
DROP TABLE projects;
DROP TABLE regions;
