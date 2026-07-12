import { NextResponse } from "next/server";
import { getMeta } from "@/lib/db";

export const dynamic = "force-dynamic";

export async function GET() {
  try {
    return NextResponse.json(await getMeta());
  } catch (err) {
    return NextResponse.json({ error: String(err) }, { status: 500 });
  }
}
