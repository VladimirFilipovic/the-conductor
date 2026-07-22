import { promises as fs } from "fs";

export interface LogLine {
  raw: string;
  time: string | null;
  level: string | null;
  msg: string | null;
  attrs: { key: string; value: string }[];
}

// slog TextHandler double-quotes values containing spaces/specials; respect
// quotes so msg="reconcile pass done" is one token, not three.
function tokenize(line: string): string[] {
  const tokens: string[] = [];
  let cur = "";
  let inQuote = false;
  for (let i = 0; i < line.length; i++) {
    const ch = line[i];
    if (ch === '"') {
      inQuote = !inQuote;
      cur += ch;
    } else if (ch === " " && !inQuote) {
      if (cur) tokens.push(cur);
      cur = "";
    } else {
      cur += ch;
    }
  }
  if (cur) tokens.push(cur);
  return tokens;
}

function unquote(v: string): string {
  if (v.length >= 2 && v.startsWith('"') && v.endsWith('"')) {
    try {
      return JSON.parse(v);
    } catch {
      return v.slice(1, -1);
    }
  }
  return v;
}

export function parseLine(raw: string): LogLine {
  const out: LogLine = { raw, time: null, level: null, msg: null, attrs: [] };
  for (const tok of tokenize(raw)) {
    const eq = tok.indexOf("=");
    if (eq === -1) continue;
    const key = tok.slice(0, eq);
    const value = unquote(tok.slice(eq + 1));
    switch (key) {
      case "time":
        out.time = value;
        break;
      case "level":
        out.level = value;
        break;
      case "msg":
        out.msg = value;
        break;
      default:
        out.attrs.push({ key, value });
    }
  }
  return out;
}

export interface TailResult {
  exists: boolean;
  offset: number;
  lines: LogLine[];
}

// Returns the new end offset so the client can poll incrementally (pass it back
// as `since`) instead of re-reading the whole file each tick.
export async function tail(
  path: string,
  maxLines: number,
  since: number | null,
): Promise<TailResult> {
  let stat;
  try {
    stat = await fs.stat(path);
  } catch {
    return { exists: false, offset: 0, lines: [] };
  }

  const size = stat.size;
  // A truncated/rotated file (size shrank) resets the client to a fresh tail.
  const fresh = since == null || since > size;
  // Fresh tail reads only a trailing window: a long-running DEBUG engine log
  // grows unbounded and slurping it whole would balloon per-request memory.
  const FRESH_TAIL_WINDOW = 1 << 20;
  const start = fresh ? Math.max(0, size - FRESH_TAIL_WINDOW) : (since as number);

  const fh = await fs.open(path, "r");
  try {
    const len = size - start;
    if (len <= 0) return { exists: true, offset: size, lines: [] };
    const buf = Buffer.alloc(len);
    await fh.read(buf, 0, len, start);

    let raw = buf
      .toString("utf8")
      .split("\n")
      .filter((l) => l.trim().length > 0);
    // A windowed fresh read almost surely starts mid-line; drop the partial.
    if (fresh && start > 0) raw = raw.slice(1);
    // Full tail (no `since`) keeps only the last N; incremental returns all new.
    if (fresh && raw.length > maxLines) raw = raw.slice(-maxLines);
    return { exists: true, offset: size, lines: raw.map(parseLine) };
  } finally {
    await fh.close();
  }
}
