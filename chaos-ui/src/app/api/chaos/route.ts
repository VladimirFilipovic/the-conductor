import { NextRequest, NextResponse } from "next/server";
import {
  deploymentReplicaIds,
  cordonHost,
  drainHost,
  deleteReplica,
  restartReplica,
  revertVolume,
  ApiError,
  type ServiceTarget,
} from "@/lib/api";
import { failed } from "@/lib/route";

export const dynamic = "force-dynamic";

// Agent-observable chaos is proxied to the agentsim control API so it travels
// the real transport (agent lies/goes silent over gRPC) instead of editing the
// database behind the engine's back — a live agent's next report would just
// overwrite a DB edit anyway.
const AGENTSIM_URL = process.env.AGENTSIM_URL ?? "http://localhost:7780";

async function agentChaos(
  action: string,
  target: "host" | "replica" | "volume",
  id: string,
) {
  const url = `${AGENTSIM_URL}/chaos`;
  let res: Response;
  try {
    res = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action, [target]: id }),
      cache: "no-store",
    });
  } catch {
    throw new Error(`agentsim unreachable at ${url} — is it running?`);
  }
  if (!res.ok) {
    const body = (await res.json().catch(() => ({}))) as { error?: string };
    throw new Error(body.error ?? `agentsim responded ${res.status}`);
  }
}

async function fanOut(action: string, deploymentId: string) {
  const ids = await deploymentReplicaIds(deploymentId);
  if (ids.length === 0) throw new Error("deployment has no live replicas");
  await Promise.all(ids.map((id) => agentChaos(action, "replica", id)));
}

// Every action names an id; the control-plane volume actions also carry the
// (target, mount_path) the project layer addresses a volume by.
interface ChaosCall {
  id: string;
  target?: ServiceTarget;
  mount_path?: string;
}

const handlers: Record<string, (c: ChaosCall) => Promise<unknown>> = {
  // host agent chaos
  host_kill: ({ id }) => agentChaos("host_kill", "host", id),
  host_recover: ({ id }) => agentChaos("host_recover", "host", id),
  // replica (container) chaos on the agent
  replica_crash: ({ id }) => agentChaos("replica_crash", "replica", id),
  replica_crashloop: ({ id }) => agentChaos("replica_crashloop", "replica", id),
  replica_stall_health: ({ id }) => agentChaos("replica_stall_health", "replica", id),
  replica_heal: ({ id }) => agentChaos("replica_heal", "replica", id),
  // volume (disk) chaos on the agent: the grow is approved but never lands
  volume_stall_resize: ({ id }) => agentChaos("volume_stall_resize", "volume", id),
  volume_heal: ({ id }) => agentChaos("volume_heal", "volume", id),
  // deployment-wide fan-out of agent chaos
  crash_deployment: ({ id }) => fanOut("replica_crash", id),
  stall_rollout: ({ id }) => fanOut("replica_stall_health", id),
  // operator desired state + synthetic row accidents via the control plane
  cordon_host: ({ id }) => cordonHost(id),
  drain_host: ({ id }) => drainHost(id),
  delete_replica: ({ id }) => deleteReplica(id),
  restart_replica: ({ id }) => restartReplica(id),
  // a parked grow goes back to its previous size; grow itself has its own
  // route (/api/volumes/resize) because it needs a body, not an id
  revert_volume: ({ target, mount_path }) => {
    if (!target || !mount_path) {
      throw new ApiError(400, "revert_volume needs target and mount_path");
    }
    return revertVolume(target, mount_path);
  },
};

function serviceTarget(v: unknown): ServiceTarget | undefined {
  if (typeof v !== "object" || v === null) return undefined;
  const t = v as Record<string, unknown>;
  const project = typeof t.project === "string" ? t.project : "";
  const environment = typeof t.environment === "string" ? t.environment : "";
  const service = typeof t.service === "string" ? t.service : "";
  if (!project || !environment || !service) return undefined;
  return { project, environment, service };
}

export async function POST(req: NextRequest) {
  let body: Record<string, unknown>;
  try {
    body = await req.json();
  } catch {
    return NextResponse.json({ error: "invalid json" }, { status: 400 });
  }

  const action = body.action as string;
  const id = typeof body.id === "string" ? body.id : "";
  if (!id) return NextResponse.json({ error: "missing id" }, { status: 400 });

  const handler = handlers[action];
  if (!handler) {
    return NextResponse.json({ error: `unknown action ${action}` }, { status: 400 });
  }

  try {
    await handler({
      id,
      target: serviceTarget(body.target),
      mount_path: typeof body.mount_path === "string" ? body.mount_path : undefined,
    });
    return NextResponse.json({ ok: true });
  } catch (err) {
    return failed(err);
  }
}
