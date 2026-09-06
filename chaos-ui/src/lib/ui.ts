// Badge palettes shared across client views; values are Tailwind utility strings.

const NEUTRAL = "bg-zinc-500/10 text-zinc-600 border-zinc-400/40";

export function phaseClass(phase: string): string {
  const map: Record<string, string> = {
    pending: "bg-zinc-500/10 text-zinc-600 border-zinc-400/40",
    scheduling: "bg-blue-500/10 text-blue-700 border-blue-400/40",
    starting: "bg-cyan-500/10 text-cyan-700 border-cyan-400/40",
    health_check: "bg-amber-500/10 text-amber-700 border-amber-400/40",
    healthy: "bg-emerald-500/10 text-emerald-700 border-emerald-400/40",
    shifting: "bg-violet-500/10 text-violet-700 border-violet-400/40",
    active: "bg-green-500/10 text-green-700 border-green-400/40",
    draining: "bg-orange-500/10 text-orange-700 border-orange-400/40",
    reaped: "bg-zinc-400/10 text-zinc-400 border-zinc-300/60",
    failed: "bg-red-500/10 text-red-700 border-red-400/40",
  };
  return map[phase] ?? NEUTRAL;
}

export function deployStatusClass(status: string): string {
  const map: Record<string, string> = {
    pending: "bg-zinc-500/10 text-zinc-600 border-zinc-400/40",
    active: "bg-emerald-500/10 text-emerald-700 border-emerald-400/40",
    draining: "bg-orange-500/10 text-orange-700 border-orange-400/40",
    failed: "bg-red-500/10 text-red-700 border-red-400/40",
    rolledback: "bg-amber-500/10 text-amber-700 border-amber-400/40",
    superseded: "bg-zinc-400/10 text-zinc-400 border-zinc-300/60",
  };
  return map[status] ?? NEUTRAL;
}

export function hostStatusClass(status: string): string {
  const map: Record<string, string> = {
    ready: "bg-emerald-500/10 text-emerald-700 border-emerald-400/40",
    notready: "bg-red-500/10 text-red-700 border-red-400/40",
    draining: "bg-orange-500/10 text-orange-700 border-orange-400/40",
    cordoned: "bg-zinc-500/10 text-zinc-600 border-zinc-400/40",
  };
  return map[status] ?? NEUTRAL;
}

export function levelClass(level: string): string {
  const map: Record<string, string> = {
    DEBUG: "bg-zinc-500/10 text-zinc-500 border-zinc-400/40",
    INFO: "bg-blue-500/10 text-blue-700 border-blue-400/40",
    WARN: "bg-amber-500/10 text-amber-700 border-amber-400/40",
    ERROR: "bg-red-500/10 text-red-700 border-red-400/40",
  };
  return map[level?.toUpperCase()] ?? NEUTRAL;
}

export function shortId(id: string): string {
  return id.slice(0, 8);
}

export function relativeTime(iso: string | null): string {
  if (!iso) return "—";
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return iso;
  const diff = Date.now() - then;
  const s = Math.round(diff / 1000);
  if (s < 0) return "just now";
  if (s < 60) return `${s}s ago`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.round(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.round(h / 24)}d ago`;
}
