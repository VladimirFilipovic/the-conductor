import { NextRequest, NextResponse } from "next/server";
import { resizeVolume } from "@/lib/api";
import { failed } from "@/lib/route";

export const dynamic = "force-dynamic";

// Grow is the one chaos-adjacent action with a body instead of an id: the
// project layer addresses the volume by (target, mount_path) and needs the new
// size. The advisory ({waiting_for_space, shortfall_bytes}) is relayed as is so
// the row can say "parked" before the engine's next tick does.
export async function POST(req: NextRequest) {
  let body: Record<string, unknown>;
  try {
    body = await req.json();
  } catch {
    return NextResponse.json({ error: "invalid json" }, { status: 400 });
  }

  const target = {
    project: str(body.project),
    environment: str(body.environment),
    service: str(body.service),
  };
  const mountPath = str(body.mount_path);
  const sizeBytes = typeof body.size_bytes === "number" ? body.size_bytes : NaN;
  if (!target.project || !target.environment || !target.service || !mountPath) {
    return NextResponse.json(
      { error: "project, environment, service and mount_path are required" },
      { status: 400 },
    );
  }
  if (!Number.isFinite(sizeBytes) || sizeBytes <= 0) {
    return NextResponse.json({ error: "size_bytes must be positive" }, { status: 400 });
  }

  try {
    const res = await resizeVolume(target, mountPath, sizeBytes);
    return NextResponse.json(res);
  } catch (err) {
    return failed(err);
  }
}

function str(v: unknown): string {
  return typeof v === "string" ? v.trim() : "";
}
