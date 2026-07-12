import { NextRequest, NextResponse } from "next/server";
import {
  killReplica,
  crashDeployment,
  hostDown,
  hostRecover,
  cordonHost,
  drainHost,
  flapHealth,
  stallRollout,
  deleteReplica,
} from "@/lib/db";

export const dynamic = "force-dynamic";

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

  const handlers: Record<string, (id: string) => Promise<void>> = {
    kill_replica: killReplica,
    crash_deployment: crashDeployment,
    host_down: hostDown,
    host_recover: hostRecover,
    cordon_host: cordonHost,
    drain_host: drainHost,
    flap_health: flapHealth,
    stall_rollout: stallRollout,
    delete_replica: deleteReplica,
  };

  const handler = handlers[action];
  if (!handler) {
    return NextResponse.json({ error: `unknown action ${action}` }, { status: 400 });
  }

  try {
    await handler(id);
    return NextResponse.json({ ok: true });
  } catch (err) {
    return NextResponse.json({ error: String(err) }, { status: 500 });
  }
}
