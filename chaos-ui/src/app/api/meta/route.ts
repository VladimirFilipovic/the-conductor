import { NextResponse } from "next/server";
import { getMeta } from "@/lib/api";
import { failed } from "@/lib/route";

export const dynamic = "force-dynamic";

export async function GET() {
  try {
    return NextResponse.json(await getMeta());
  } catch (err) {
    return failed(err);
  }
}
