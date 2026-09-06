import { NextRequest, NextResponse } from "next/server";
import {
  cordonHost,
  drainHost,
  deleteReplica,
  deploymentReplicaIds,
} from "@/lib/db";

export const dynamic = "force-dynamic";

// Agent-observable chaos is proxied to the agentsim control API so it travels
// the real transport (agent lies/goes silent over gRPC) instead of editing the
// database behind the engine's back — a live agent's next report would just
// overwrite a DB edit anyway.
const AGENTSIM_URL = process.env.AGENTSIM_URL ?? "http://localhost:7780";

async function agentChaos(action: string, target: "host" | "replica", id: string) {
  let res: Response;
  try {
    res = await fetch(`${AGENTSIM_URL}/chaos`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action, [target]: id }),
      cache: "no-store",
    });
  } catch {
    throw new Error(`agentsim unreachable at ${AGENTSIM_URL} — is it running?`);
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

const handlers: Record<string, (id: string) => Promise<void>> = {
  // host agent chaos
  host_kill: (id) => agentChaos("host_kill", "host", id),
  host_recover: (id) => agentChaos("host_recover", "host", id),
  // replica (container) chaos on the agent
  replica_crash: (id) => agentChaos("replica_crash", "replica", id),
  replica_crashloop: (id) => agentChaos("replica_crashloop", "replica", id),
  replica_stall_health: (id) => agentChaos("replica_stall_health", "replica", id),
  replica_heal: (id) => agentChaos("replica_heal", "replica", id),
  // deployment-wide fan-out of agent chaos
  crash_deployment: (id) => fanOut("replica_crash", id),
  stall_rollout: (id) => fanOut("replica_stall_health", id),
  // operator desired state + synthetic row accidents stay DB-side
  cordon_host: cordonHost,
  drain_host: drainHost,
  delete_replica: deleteReplica,
};

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
    await handler(id);
    return NextResponse.json({ ok: true });
  } catch (err) {
    return NextResponse.json(
      { error: err instanceof Error ? err.message : String(err) },
      { status: 500 },
    );
  }
}
