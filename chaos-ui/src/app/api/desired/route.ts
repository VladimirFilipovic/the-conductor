import { NextRequest, NextResponse } from "next/server";
import {
  createProject,
  createEnvironment,
  createService,
  bindService,
  deploy,
  scale,
  type ServiceTarget,
} from "@/lib/api";
import { failed } from "@/lib/route";

export const dynamic = "force-dynamic";

// Desired-state forms post here; every action is forwarded to the apiserver,
// which runs it through the same project service the CLI drives.
export async function POST(req: NextRequest) {
  let body: Record<string, unknown>;
  try {
    body = await req.json();
  } catch {
    return NextResponse.json({ error: "invalid json" }, { status: 400 });
  }

  const action = body.action as string;
  try {
    switch (action) {
      case "create_project":
        await createProject(str(body.name), str(body.environment) || "production");
        break;
      case "create_environment":
        await createEnvironment(str(body.project), str(body.name));
        break;
      case "create_service":
        await createService(str(body.project), str(body.name), Boolean(body.stateful));
        break;
      case "bind_service":
        await bindService(str(body.environmentId), str(body.serviceId), {
          image: str(body.image),
          repo: str(body.repo),
        });
        break;
      case "deploy": {
        const res = await deploy({
          ...target(body),
          image_ref: str(body.imageRef),
          cpu_millicores: num(body.cpuMillicores, 500),
          mem_bytes: num(body.memBytes, 536870912),
          drain_seconds: num(body.drainSeconds, 30),
          restart_max: num(body.restartMax, 5),
          progress_deadline: num(body.progressDeadline, 600),
          commit_message: str(body.commitMessage),
          created_by: str(body.createdBy),
          replicas: replicas(body.replicas),
        });
        return NextResponse.json({ ok: true, ...res });
      }
      case "scale":
        await scale(target(body), replicas(body.replicas));
        break;
      default:
        return NextResponse.json({ error: `unknown action ${action}` }, { status: 400 });
    }
    return NextResponse.json({ ok: true });
  } catch (err) {
    return failed(err);
  }
}

function target(body: Record<string, unknown>): ServiceTarget {
  return {
    project: str(body.project),
    environment: str(body.environment),
    service: str(body.service),
  };
}

function str(v: unknown): string {
  return typeof v === "string" ? v.trim() : "";
}

function num(v: unknown, def: number): number {
  const n = typeof v === "number" ? v : parseInt(String(v), 10);
  return Number.isFinite(n) ? n : def;
}

// Replica counts arrive keyed by region; drop the regions the form left blank
// so a deploy never commits a region the operator did not ask for.
function replicas(v: unknown): Record<string, number> {
  if (typeof v !== "object" || v === null) return {};
  const out: Record<string, number> = {};
  for (const [region, count] of Object.entries(v as Record<string, unknown>)) {
    if (region) out[region] = num(count, 0);
  }
  return out;
}
