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

