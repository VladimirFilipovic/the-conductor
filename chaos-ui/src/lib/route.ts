import { NextResponse } from "next/server";
import { ApiError } from "@/lib/api";

// failed relays the control plane's own status so a rejected write reads as
// "already exists" / "not found" in the activity log instead of a blanket 500.
export function failed(err: unknown) {
  const status = err instanceof ApiError ? err.status : 500;
  const message = err instanceof Error ? err.message : String(err);
  return NextResponse.json({ error: message }, { status });
}
