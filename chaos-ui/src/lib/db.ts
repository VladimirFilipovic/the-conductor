import { Pool, type PoolClient } from "pg";

// Stash the pool on the global: Next dev recompiles modules per edit, leaking
// a new Pool (and its sockets) per reload until Postgres refuses connections.
const globalForPg = globalThis as unknown as { conductorPool?: Pool };

const DEFAULT_DSN = "postgres://conductor:conductor@localhost:5432/conductor";

export function pool(): Pool {
  if (!globalForPg.conductorPool) {
    globalForPg.conductorPool = new Pool({
      connectionString: process.env.DATABASE_URL || DEFAULT_DSN,
      max: 8,
    });
  }
  return globalForPg.conductorPool;
}

async function query<T = Record<string, unknown>>(
  sql: string,
  params: unknown[] = [],
): Promise<T[]> {
  const res = await pool().query(sql, params);
  return res.rows as T[];
}

async function withTx<T>(fn: (c: PoolClient) => Promise<T>): Promise<T> {
  const client = await pool().connect();
  try {
    await client.query("BEGIN");
    const out = await fn(client);
    await client.query("COMMIT");
    return out;
  } catch (err) {
    await client.query("ROLLBACK");
    throw err;
  } finally {
    client.release();
  }
}

// --- Read: meta for the global selectors ----------------------------------

export interface Meta {
  projects: { name: string }[];
  environments: { id: string; project_name: string; name: string }[];
  regions: string[];
}

export async function getMeta(): Promise<Meta> {
  const [projects, environments, regions] = await Promise.all([
    query<{ name: string }>("SELECT name FROM projects ORDER BY name"),
    query<{ id: string; project_name: string; name: string }>(
      "SELECT id, project_name, name FROM environments ORDER BY project_name, name",
    ),
    query<{ name: string }>("SELECT name FROM regions ORDER BY name"),
  ]);
  return {
    projects,
    environments,
    regions: regions.map((r) => r.name),
  };
}

// --- Read: topology snapshot ------------------------------------------------

export interface ReplicaRow {
  id: string;
  region: string;
  hostname: string | null;
  host_id: string | null;
  phase: string;
  healthy: boolean;
  desired_status: string;
  restart_count: number;
  last_exit_reason: string | null;
  updated_at: string;
  dep_version: number;
  is_current: boolean;
  es_id: string;
  deployment_id: string;
}

export interface ServiceNode {
  es_id: string;
  environment_id: string;
  service: string;
  stateful: boolean;
  deployment: {
    id: string;
    version: number;
    status: string;
    image_ref: string;
    created_at: string;
    created_by: string | null;
    commit_message: string | null;
  } | null;
  regions: { region: string; desired: number; observed: number; healthy: number }[];
  replicas: ReplicaRow[];
}

export interface HostRow {
  id: string;
  region: string;
  hostname: string;
  status: string;
  last_heartbeat: string | null;
  cpu_millicores: number;
  mem_bytes: string;
}

export interface ServedRow {
  environment_service_id: string;
  region: string;
  deployment_id: string;
  dep_version: number | null;
  service: string;
  environment: string;
  updated_at: string;
}

export interface Topology {
  tree: {
    project: string;
    environments: { id: string; name: string; services: ServiceNode[] }[];
  }[];
  hosts: HostRow[];
  served: ServedRow[];
}

interface Filter {
  project?: string;
  environment?: string;
  region?: string;
}

export async function getTopology(f: Filter): Promise<Topology> {
  const projFilter = f.project && f.project !== "all" ? f.project : null;
  const envFilter = f.environment && f.environment !== "all" ? f.environment : null;

  const [projects, environments, services, regions, replicas, hosts, served] =
    await Promise.all([
      query<{ name: string }>(
        `SELECT name FROM projects WHERE ($1::text IS NULL OR name = $1) ORDER BY name`,
        [projFilter],
      ),
      query<{ id: string; project_name: string; name: string }>(
        `SELECT id, project_name, name FROM environments
         WHERE ($1::text IS NULL OR project_name = $1)
           AND ($2::text IS NULL OR name = $2)
         ORDER BY name`,
        [projFilter, envFilter],
      ),
      query<{
        es_id: string;
        environment_id: string;
        service: string;
        stateful: boolean;
        dep_id: string | null;
        version: number | null;
        status: string | null;
        image_ref: string | null;
        created_at: string | null;
        created_by: string | null;
        commit_message: string | null;
      }>(
        `SELECT es.id AS es_id, es.environment_id, s.name AS service, s.stateful,
                d.id AS dep_id, d.version, d.status, d.image_ref, d.created_at,
                d.created_by, d.commit_message
         FROM environment_services es
         JOIN environments e ON e.id = es.environment_id
         JOIN services s ON s.id = es.service_id
         LEFT JOIN deployments d ON d.environment_service_id = es.id AND d.is_current
         WHERE ($1::text IS NULL OR e.project_name = $1)
           AND ($2::text IS NULL OR e.name = $2)
         ORDER BY s.name`,
        [projFilter, envFilter],
      ),
      query<{ es_id: string; region: string; desired: number }>(
        `SELECT d.environment_service_id AS es_id, dr.region, dr.replicas AS desired
         FROM deployments d
         JOIN deployment_regions dr ON dr.deployment_id = d.id
         WHERE d.is_current`,
      ),
      query<ReplicaRow>(
        `SELECT r.id, r.region, h.hostname, r.host_id, r.phase, r.healthy,
                r.desired_status, r.restart_count, r.last_exit_reason, r.updated_at,
                d.version AS dep_version, d.is_current, d.id AS deployment_id,
                d.environment_service_id AS es_id
         FROM replicas r
         JOIN deployments d ON d.id = r.deployment_id
         LEFT JOIN hosts h ON h.id = r.host_id
         ORDER BY r.region, r.created_at`,
      ),
      query<HostRow>(
        `SELECT id, region, hostname, status, last_heartbeat, cpu_millicores,
                mem_bytes::text AS mem_bytes
         FROM hosts
         WHERE ($1::text IS NULL OR region = $1)
         ORDER BY region, hostname`,
        [f.region && f.region !== "all" ? f.region : null],
      ),
      query<ServedRow>(
        `SELECT sr.environment_service_id, sr.region, sr.deployment_id,
                d.version AS dep_version, s.name AS service, e.name AS environment,
                sr.updated_at
         FROM served_revisions sr
         JOIN deployments d ON d.id = sr.deployment_id
         JOIN environment_services es ON es.id = sr.environment_service_id
         JOIN environments e ON e.id = es.environment_id
         JOIN services s ON s.id = es.service_id
         WHERE ($1::text IS NULL OR e.project_name = $1)
           AND ($2::text IS NULL OR e.name = $2)
         ORDER BY e.name, s.name, sr.region`,
        [projFilter, envFilter],
      ),
    ]);

  const regionFilter = f.region && f.region !== "all" ? f.region : null;
  const replicasByEs = new Map<string, ReplicaRow[]>();
  for (const r of replicas) {
    if (regionFilter && r.region !== regionFilter) continue;
    const list = replicasByEs.get(r.es_id) ?? [];
    list.push(r);
    replicasByEs.set(r.es_id, list);
  }

  const desiredByEs = new Map<string, Map<string, number>>();
  for (const row of regions) {
    if (regionFilter && row.region !== regionFilter) continue;
    const m = desiredByEs.get(row.es_id) ?? new Map();
    m.set(row.region, row.desired);
    desiredByEs.set(row.es_id, m);
  }

  const serviceNodes: ServiceNode[] = services.map((s) => {
    const reps = replicasByEs.get(s.es_id) ?? [];
    const desired = desiredByEs.get(s.es_id) ?? new Map<string, number>();

    const regionSet = new Set<string>([...desired.keys()]);
    // A current replica surviving in a region the current deployment no longer
    // targets (post scale-down, mid-reap) still deserves a row.
    for (const r of reps) if (r.is_current) regionSet.add(r.region);

    const regionSummaries = [...regionSet].sort().map((region) => {
      const currentHere = reps.filter((r) => r.is_current && r.region === region);
      return {
        region,
        desired: desired.get(region) ?? 0,
        observed: currentHere.filter((r) => r.desired_status === "running").length,
        healthy: currentHere.filter((r) => r.healthy).length,
      };
    });

    return {
      es_id: s.es_id,
      environment_id: s.environment_id,
      service: s.service,
      stateful: s.stateful,
      deployment: s.dep_id
        ? {
            id: s.dep_id,
            version: s.version!,
            status: s.status!,
            image_ref: s.image_ref!,
            created_at: s.created_at!,
            created_by: s.created_by,
            commit_message: s.commit_message,
          }
        : null,
      regions: regionSummaries,
      replicas: reps,
    };
  });

  const byEnv = new Map<string, ServiceNode[]>();
  for (const node of serviceNodes) {
    const list = byEnv.get(node.environment_id) ?? [];
    list.push(node);
    byEnv.set(node.environment_id, list);
  }

  const envByProject = new Map<
    string,
    { id: string; name: string; services: ServiceNode[] }[]
  >();
  for (const e of environments) {
    const list = envByProject.get(e.project_name) ?? [];
    list.push({ id: e.id, name: e.name, services: byEnv.get(e.id) ?? [] });
    envByProject.set(e.project_name, list);
  }

  const tree = projects.map((p) => ({
    project: p.name,
    environments: envByProject.get(p.name) ?? [],
  }));

  return { tree, hosts, served };
}

// --- Write: desired-state mutations ----------------------------------------

export async function createProject(name: string, environment: string) {
  // Mirror project.Service.Create: a project is born with a first environment.
  await withTx(async (c) => {
    await c.query("INSERT INTO projects (name) VALUES ($1)", [name]);
    await c.query(
      "INSERT INTO environments (project_name, name) VALUES ($1, $2)",
      [name, environment],
    );
  });
}

export async function createEnvironment(project: string, name: string) {
  await query(
    "INSERT INTO environments (project_name, name) VALUES ($1, $2)",
    [project, name],
  );
}

export async function createService(
  project: string,
  name: string,
  stateful: boolean,
) {
  await query(
    "INSERT INTO services (project_name, name, stateful) VALUES ($1, $2, $3)",
    [project, name, stateful],
  );
}

export async function createEnvironmentService(
  environmentId: string,
  serviceId: string,
  source: string,
) {
  await query(
    "INSERT INTO environment_services (environment_id, service_id, source) VALUES ($1, $2, $3)",
    [environmentId, serviceId, source],
  );
}

export interface DeployInput {
  esId: string;
  imageRef: string;
  cpuMillicores: number;
  memBytes: number;
  drainSeconds: number;
  restartMax: number;
  progressDeadline: number;
  commitMessage: string;
  createdBy: string;
  regions: { region: string; replicas: number }[];
}

// Mirrors project.Service.Deploy / the `up` path — all in one tx so the
// one-current partial unique index never trips mid-flight.
export async function createDeployment(in_: DeployInput) {
  return withTx(async (c) => {
    const { rows: vrows } = await c.query(
      "SELECT (COALESCE(MAX(version), 0) + 1)::int AS next FROM deployments WHERE environment_service_id = $1",
      [in_.esId],
    );
    const version: number = vrows[0].next;

    await c.query(
      "UPDATE deployments SET is_current = false, status = 'superseded' WHERE environment_service_id = $1 AND is_current",
      [in_.esId],
    );

    const { rows: drows } = await c.query(
      `INSERT INTO deployments (
         environment_service_id, version, is_current, image_ref,
         cpu_millicores, mem_bytes, drain_seconds, restart_max,
         progress_deadline, commit_message, created_by
       ) VALUES ($1, $2, true, $3, $4, $5, $6, $7, $8, $9, $10)
       RETURNING id`,
      [
        in_.esId,
        version,
        in_.imageRef,
        in_.cpuMillicores,
        in_.memBytes,
        in_.drainSeconds,
        in_.restartMax,
        in_.progressDeadline,
        in_.commitMessage || null,
        in_.createdBy || null,
      ],
    );
    const depId: string = drows[0].id;

    for (const r of in_.regions) {
      await c.query(
        `INSERT INTO deployment_regions (deployment_id, region, replicas)
         VALUES ($1, $2, $3)
         ON CONFLICT (deployment_id, region) DO UPDATE SET replicas = EXCLUDED.replicas`,
        [depId, r.region, r.replicas],
      );
    }
    return { version, deploymentId: depId };
  });
}

// Matches CLI `scale` semantics: patch deployment_regions, engine converges.
export async function scaleDeployment(
  deploymentId: string,
  regions: { region: string; replicas: number }[],
) {
  await withTx(async (c) => {
    for (const r of regions) {
      await c.query(
        `INSERT INTO deployment_regions (deployment_id, region, replicas)
         VALUES ($1, $2, $3)
         ON CONFLICT (deployment_id, region) DO UPDATE SET replicas = EXCLUDED.replicas`,
        [deploymentId, r.region, r.replicas],
      );
    }
  });
}

export async function listServices(project: string) {
  return query<{ id: string; name: string; stateful: boolean }>(
    "SELECT id, name, stateful FROM services WHERE project_name = $1 ORDER BY name",
    [project],
  );
}

// --- Write: chaos mutations -------------------------------------------------

export async function killReplica(id: string) {
  await query(
    `UPDATE replicas
     SET healthy = false, phase = 'failed', last_exit_reason = 'chaos: killed',
         restart_count = restart_count + 1
     WHERE id = $1`,
    [id],
  );
}

export async function crashDeployment(deploymentId: string) {
  await query(
    `UPDATE replicas
     SET healthy = false, phase = 'failed', last_exit_reason = 'chaos: crashed',
         restart_count = restart_count + 1
     WHERE deployment_id = $1 AND phase <> 'reaped'`,
    [deploymentId],
  );
}

export async function hostDown(id: string) {
  await query(
    "UPDATE hosts SET status = 'notready', last_heartbeat = now() - interval '10 minutes' WHERE id = $1",
    [id],
  );
}

export async function hostRecover(id: string) {
  await query(
    "UPDATE hosts SET status = 'ready', last_heartbeat = now() WHERE id = $1",
    [id],
  );
}

export async function cordonHost(id: string) {
  await query("UPDATE hosts SET status = 'cordoned' WHERE id = $1", [id]);
}

export async function drainHost(id: string) {
  await query("UPDATE hosts SET status = 'draining' WHERE id = $1", [id]);
}

export async function flapHealth(id: string) {
  // Health drops but the phase is left intact — simulates a flapping probe the
  // reconciler must notice without a hard crash.
  await query("UPDATE replicas SET healthy = false WHERE id = $1", [id]);
}

export async function stallRollout(deploymentId: string) {
  // Wipe the health high-water mark so the progress_deadline rule sees "up but
  // never healthy" — the stalled-rollout signal.
  await query(
    "UPDATE replicas SET healthy = false, health_checks_passed_at = NULL WHERE deployment_id = $1 AND phase <> 'reaped'",
    [deploymentId],
  );
}

export async function deleteReplica(id: string) {
  await query("DELETE FROM replicas WHERE id = $1", [id]);
}
