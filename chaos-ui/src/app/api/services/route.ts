import { NextRequest, NextResponse } from "next/server";
import { listServices } from "@/lib/db";

export const dynamic = "force-dynamic";

export async function GET(req: NextRequest) {
  const project = req.nextUrl.searchParams.get("project");
  if (!project) return NextResponse.json({ services: [] });
  try {
    return NextResponse.json({ services: await listServices(project) });
  } catch (err) {
    return NextResponse.json({ error: String(err) }, { status: 500 });
  }
}
