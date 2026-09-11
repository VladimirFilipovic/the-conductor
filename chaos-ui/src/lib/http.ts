// Browser-side helper for this app's own /api routes. Kept out of lib/api.ts
// so the control-plane client (and its server-only URL) never reaches a client
// bundle.
export async function postJson(
  url: string,
  body: unknown,
): Promise<{ ok: boolean; data: Record<string, unknown> }> {
  const res = await fetch(url, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
  let data: Record<string, unknown> = {};
  try {
    data = await res.json();
  } catch {
    /* empty body */
  }
  return { ok: res.ok, data };
}
