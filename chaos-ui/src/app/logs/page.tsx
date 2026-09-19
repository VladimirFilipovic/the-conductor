"use client";

import { useEffect, useRef, useState } from "react";
import { Badge } from "@/components/Badge";
import { levelClass, relativeTime } from "@/lib/ui";
import { parseLine, type LogLine } from "@/lib/logs";

const LEVELS = ["DEBUG", "INFO", "WARN", "ERROR"];
const MAX_BUFFER = 3000;
// The engine emits bursts at DEBUG; one setState per line would re-render the
// page hundreds of times a second for no visible gain.
const FLUSH_INTERVAL = 250;

type Connection = "connecting" | "live" | "down";

export default function LogsPage() {
  const [lines, setLines] = useState<LogLine[]>([]);
  const [connection, setConnection] = useState<Connection>("connecting");
  const [levelFilter, setLevelFilter] = useState<Record<string, boolean>>({
    DEBUG: true,
    INFO: true,
    WARN: true,
    ERROR: true,
  });
  const [search, setSearch] = useState("");
  const [follow, setFollow] = useState(true);

  const scroller = useRef<HTMLDivElement>(null);

  useEffect(() => {
    // EventSource reconnects on its own and resends Last-Event-ID, so a dropped
    // connection resumes at the line it stopped on instead of replaying.
    const es = new EventSource("/api/logs/stream?tail=800");
    const pending: LogLine[] = [];

    es.onopen = () => setConnection("live");
    es.onerror = () => setConnection("down");
    es.onmessage = (e) => pending.push(parseLine(e.data));

    const flush = setInterval(() => {
      if (pending.length === 0) return;
      const batch = pending.splice(0, pending.length);
      setLines((prev) => {
        const next = [...prev, ...batch];
        return next.length > MAX_BUFFER ? next.slice(-MAX_BUFFER) : next;
      });
    }, FLUSH_INTERVAL);

    return () => {
      es.close();
      clearInterval(flush);
    };
  }, []);

  useEffect(() => {
    if (follow && scroller.current) {
      scroller.current.scrollTop = scroller.current.scrollHeight;
    }
  }, [lines, follow]);

  const q = search.toLowerCase();
  const visible = lines.filter((l) => {
    const lvl = (l.level ?? "INFO").toUpperCase();
    if (!levelFilter[lvl]) return false;
    if (!q) return true;
    return l.raw.toLowerCase().includes(q);
  });

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-3">
        <div>
          <h1 className="text-lg font-semibold">Engine logs</h1>
          <p className="mono text-xs text-[var(--color-faint)]">
            {connection === "live" ? "streaming from the engine" : `stream ${connection}`}
          </p>
        </div>
        <div className="ml-auto flex flex-wrap items-center gap-2">
          {LEVELS.map((lvl) => (
            <button
              key={lvl}
              onClick={() => setLevelFilter((f) => ({ ...f, [lvl]: !f[lvl] }))}
              className={`badge cursor-pointer ${
                levelFilter[lvl] ? levelClass(lvl) : "border-[var(--color-border)] bg-transparent text-[var(--color-faint)]"
              }`}
            >
              {lvl}
            </button>
          ))}
          <input
            className="input mono !w-48"
            placeholder="search…"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
          <label className="flex items-center gap-1.5 text-xs text-[var(--color-muted)]">
            <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} />
            follow
          </label>
        </div>
      </div>

      {connection === "down" && (
        <div className="panel border-amber-400/40 bg-amber-500/5 px-4 py-3 text-sm text-amber-700">
          Log stream disconnected — retrying. Lines already received stay on
          screen, and the stream resumes where it stopped.
        </div>
      )}

      <div
        ref={scroller}
        className="panel h-[calc(100vh-13rem)] overflow-y-auto p-3 font-mono text-xs"
      >
        {visible.length === 0 ? (
          <div className="px-2 py-4 text-[var(--color-faint)]">
            {lines.length === 0 ? "waiting for log lines…" : "no lines match the current filter"}
          </div>
        ) : (
          visible.map((l, i) => <LogRow key={i} line={l} />)
        )}
      </div>
    </div>
  );
}

function LogRow({ line }: { line: LogLine }) {
  const level = (line.level ?? "INFO").toUpperCase();
  return (
    <div className="flex items-start gap-2.5 border-b border-[var(--color-border-soft)]/60 px-2 py-1 hover:bg-black/[0.03]">
      <span
        className="w-16 shrink-0 pt-0.5 text-right text-[0.65rem] text-[var(--color-faint)]"
        title={line.time ?? ""}
      >
        {line.time ? relativeTime(line.time) : "—"}
      </span>
      <Badge className={`${levelClass(level)} w-14 shrink-0 justify-center`}>{level}</Badge>
      <div className="min-w-0 flex-1">
        <span className="text-[var(--color-fg)]">{line.msg ?? line.raw}</span>
        {line.attrs.length > 0 && (
          <span className="ml-2 inline-flex flex-wrap gap-1.5 align-middle">
            {line.attrs.map((a, i) => (
              <span
                key={i}
                className="rounded bg-black/5 px-1.5 py-0.5 text-[0.68rem] text-[var(--color-muted)]"
              >
                <span className="text-[var(--color-faint)]">{a.key}=</span>
                {a.value}
              </span>
            ))}
          </span>
        )}
      </div>
    </div>
  );
}
