import { NextRequest, NextResponse } from "next/server";
import { getTopology } from "@/lib/api";
import { failed } from "@/lib/route";

export const dynamic = "force-dynamic";

export async function GET(req: NextRequest) {
  const sp = req.nextUrl.searchParams;
  try {
    const topo = await getTopology({
      project: sp.get("project") ?? undefined,
      environment: sp.get("environment") ?? undefined,
      region: sp.get("region") ?? undefined,
    });
    return NextResponse.json(topo);
  } catch (err) {
    return failed(err);
  }
}
