import { NextRequest, NextResponse } from "next/server";
import {
  createProject,
  createEnvironment,
  createService,
  createEnvironmentService,
  createDeployment,
  scaleDeployment,
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
      case "create_environment_service":
        await createEnvironmentService(
          str(body.environmentId),
          str(body.serviceId),
          str(body.source) || "{}",
        );
        break;
      case "create_deployment": {
        const res = await createDeployment({
          esId: str(body.esId),
          imageRef: str(body.imageRef),
          cpuMillicores: num(body.cpuMillicores, 500),
          memBytes: num(body.memBytes, 536870912),
          drainSeconds: num(body.drainSeconds, 30),
          restartMax: num(body.restartMax, 5),
          progressDeadline: num(body.progressDeadline, 600),
          commitMessage: str(body.commitMessage),
          createdBy: str(body.createdBy),
          regions: regions(body.regions),
        });
        return NextResponse.json({ ok: true, ...res });
      }
      case "scale":
        await scaleDeployment(str(body.deploymentId), regions(body.regions));
        break;
      default:
        return NextResponse.json({ error: `unknown action ${action}` }, { status: 400 });
    }
    return NextResponse.json({ ok: true });
  } catch (err) {
    return NextResponse.json({ error: String(err) }, { status: 500 });
  }
}

function str(v: unknown): string {
  return typeof v === "string" ? v.trim() : "";
}

function num(v: unknown, def: number): number {
  const n = typeof v === "number" ? v : parseInt(String(v), 10);
  return Number.isFinite(n) ? n : def;
}

function regions(v: unknown): { region: string; replicas: number }[] {
  if (!Array.isArray(v)) return [];
  return v
    .map((r) => ({ region: str((r as Record<string, unknown>).region), replicas: num((r as Record<string, unknown>).replicas, 0) }))
    .filter((r) => r.region);
}
