import { NextRequest } from "next/server";

export const dynamic = "force-dynamic";

// The engine's log stream is private, like every other backend the UI talks to:
// the browser opens an EventSource against this route and the body is passed
// through untouched, so nothing outside needs to reach the engine directly.
const ENGINE_LOG_URL = process.env.ENGINE_LOG_URL ?? "http://localhost:7090";

export async function GET(req: NextRequest) {
  const url = new URL("/logs/stream", ENGINE_LOG_URL);
  for (const key of ["since", "tail"]) {
    const v = req.nextUrl.searchParams.get(key);
    if (v) url.searchParams.set(key, v);
  }

  // EventSource resends Last-Event-ID on its own reconnects; forwarding it is
  // what makes the engine resume the stream rather than replay from scratch.
  const headers: Record<string, string> = { Accept: "text/event-stream" };
  const lastEventId = req.headers.get("last-event-id");
  if (lastEventId) headers["Last-Event-ID"] = lastEventId;

  let upstream: Response;
  try {
    upstream = await fetch(url, { headers, cache: "no-store", signal: req.signal });
  } catch {
    return Response.json(
      { error: `engine log stream unreachable at ${ENGINE_LOG_URL} — is the engine running?` },
      { status: 502 },
    );
  }

  if (!upstream.ok || !upstream.body) {
    return Response.json(
      { error: `engine log stream responded ${upstream.status}` },
      { status: 502 },
    );
  }

  return new Response(upstream.body, {
    headers: {
      "Content-Type": "text/event-stream",
      // no-transform keeps proxies from buffering a response that is only
      // useful line by line.
      "Cache-Control": "no-cache, no-transform",
      Connection: "keep-alive",
      "X-Accel-Buffering": "no",
    },
  });
}
