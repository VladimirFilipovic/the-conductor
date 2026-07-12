import { NextRequest, NextResponse } from "next/server";
import { tail } from "@/lib/logs";

export const dynamic = "force-dynamic";

const DEFAULT_LOG_FILE = "/var/log/conductor/engine.log";

export async function GET(req: NextRequest) {
  const sp = req.nextUrl.searchParams;
  const path = process.env.LOG_FILE || DEFAULT_LOG_FILE;
  const sinceParam = sp.get("since");
  const since = sinceParam != null ? parseInt(sinceParam, 10) : null;
  const maxLines = Math.min(parseInt(sp.get("tail") || "500", 10) || 500, 5000);

  try {
    const res = await tail(path, maxLines, Number.isFinite(since) ? since : null);
    return NextResponse.json({ ...res, path });
  } catch (err) {
    return NextResponse.json({ error: String(err), path }, { status: 500 });
  }
}
