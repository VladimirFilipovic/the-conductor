// The apiserver control plane is the UI's only view of the control plane:
// every read and every desired-state write goes over HTTP so the browser, the
// CLI and the engine all see state through the same rules. Nothing here talks
// to Postgres — the UI has no database credentials at all.

const CONTROL_PLANE_URL =
  process.env.CONTROL_PLANE_URL ?? "http://localhost:7080";

// --- Wire shapes (mirror internal/api/topology.go and hosts.go) ----------------

export interface Meta {
  projects: { name: string }[];
  environments: { id: string; project_name: string; name: string }[];
  regions: string[];
}

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
  deployment_version: number;
  is_current: boolean;
  environment_service_id: string;
  deployment_id: string;
}

export interface ServiceNode {
  environment_service_id: string;
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
  mem_bytes: number;
  disk_bytes: number;
}

export interface ServedRow {
  environment_service_id: string;
  region: string;
  deployment_id: string;
  deployment_version: number;
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

export interface ServiceRef {
  id: string;
  name: string;
  stateful: boolean;
}

// Identity of a deploy/scale target — the same triple the CLI takes from its
// folder link and -p/-e/-s flags.
export interface ServiceTarget {
  project: string;
  environment: string;
  service: string;
}

// --- Transport --------------------------------------------------------------

// ApiError carries the control plane's own status through the Next route, so a
// rejected write reads as 409 "already exists" in the UI, not a blanket 500.
export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

async function call<T>(path: string, init?: RequestInit): Promise<T> {
  const url = `${CONTROL_PLANE_URL}${path}`;
  let res: Response;
  try {
    res = await fetch(url, { ...init, cache: "no-store" });
  } catch {
    throw new ApiError(502, `apiserver unreachable at ${url} — is it running?`);
  }
  const body = await res.json().catch(() => ({}));
  if (!res.ok) {
    const err = (body as { error?: string }).error;
    throw new ApiError(res.status, err ?? `apiserver responded ${res.status}`);
  }
  return body as T;
}

function post<T>(path: string, body: unknown): Promise<T> {
  return call<T>(path, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
}

function query(params: Record<string, string | undefined>): string {
  const sp = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    // "all" is the selector's word for "unfiltered"; the control plane treats
    // it as absent too, but there is no point sending it.
    if (v && v !== "all") sp.set(k, v);
  }
  const q = sp.toString();
  return q ? `?${q}` : "";
}

// --- Reads -------------------------------------------------------------------

export function getMeta(): Promise<Meta> {
  return call<Meta>("/v1/meta");
}

export function getTopology(f: {
  project?: string;
  environment?: string;
  region?: string;
}): Promise<Topology> {
  return call<Topology>(`/v1/topology${query(f)}`);
}

export async function listServices(
  project: string,
  environment?: string,
): Promise<ServiceRef[]> {
  const res = await call<{ services: ServiceRef[] }>(
    `/v1/services${query({ project, environment })}`,
  );
  return res.services;
}

// Fan-out targets for deployment-wide agent chaos.
export async function deploymentReplicaIds(
  deploymentId: string,
): Promise<string[]> {
  const res = await call<{ replica_ids: string[] }>(
    `/v1/deployments/${deploymentId}/replicas`,
  );
  return res.replica_ids;
}

// --- Desired-state writes ----------------------------------------------------

export function createProject(name: string, environment: string) {
  return post<{ name: string; environment: string }>("/v1/projects", {
    name,
    environment,
  });
}

export function createEnvironment(project: string, name: string) {
  return post<{ id: string }>("/v1/environments", { project, name });
}

export function createService(
  project: string,
  name: string,
  stateful: boolean,
) {
  return post<ServiceRef>("/v1/services", { project, name, stateful });
}

export function bindService(
  environmentId: string,
  serviceId: string,
  source: { image?: string; repo?: string },
) {
  return post<{ id: string }>("/v1/environment-services", {
    environment_id: environmentId,
    service_id: serviceId,
    source,
  });
}

export interface DeployInput extends ServiceTarget {
  image_ref: string;
  cpu_millicores: number;
  mem_bytes: number;
  drain_seconds: number;
  restart_max: number;
  progress_deadline: number;
  commit_message: string;
  created_by: string;
  replicas: Record<string, number>;
}

export function deploy(input: DeployInput) {
  return post<{ version: number; replicas: Record<string, number> }>(
    "/v1/deployments",
    input,
  );
}

export function scale(target: ServiceTarget, replicas: Record<string, number>) {
  return post<{ ok: boolean }>("/v1/deployments/scale", {
    ...target,
    replicas,
  });
}

// --- Operator chaos ----------------------------------------------------------
// Desired-state chaos with no agent to report it: cordon/drain a host, or make
// a replica row vanish so the engine never learns it was lost.

export function cordonHost(id: string) {
  return call<{ ok: boolean }>(`/v1/hosts/${id}/cordon`, { method: "POST" });
}

export function drainHost(id: string) {
  return call<{ ok: boolean }>(`/v1/hosts/${id}/drain`, { method: "POST" });
}

export function deleteReplica(id: string) {
  return call<{ ok: boolean }>(`/v1/replicas/${id}`, { method: "DELETE" });
}
