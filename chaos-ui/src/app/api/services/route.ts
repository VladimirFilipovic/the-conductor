import { NextRequest, NextResponse } from "next/server";
import { listServices } from "@/lib/api";
import { failed } from "@/lib/route";

export const dynamic = "force-dynamic";

export async function GET(req: NextRequest) {
  const sp = req.nextUrl.searchParams;
  const project = sp.get("project");
  if (!project) return NextResponse.json({ services: [] });
  try {
    const services = await listServices(
      project,
      sp.get("environment") ?? undefined,
    );
    return NextResponse.json({ services });
  } catch (err) {
    return failed(err);
  }
}
