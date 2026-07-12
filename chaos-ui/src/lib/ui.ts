// Badge palettes shared across client views. Values are Tailwind utility
// strings; unknown keys fall back to a neutral chip.

const NEUTRAL = "bg-white/5 text-zinc-400 border-white/10";

export function phaseClass(phase: string): string {
  const map: Record<string, string> = {
    pending: "bg-zinc-500/15 text-zinc-300 border-zinc-500/30",
    scheduling: "bg-blue-500/15 text-blue-300 border-blue-500/30",
    starting: "bg-cyan-500/15 text-cyan-300 border-cyan-500/30",
    health_check: "bg-amber-500/15 text-amber-300 border-amber-500/30",
    healthy: "bg-emerald-500/15 text-emerald-300 border-emerald-500/30",
    shifting: "bg-violet-500/15 text-violet-300 border-violet-500/30",
    active: "bg-green-500/15 text-green-300 border-green-500/30",
    draining: "bg-orange-500/15 text-orange-300 border-orange-500/30",
    reaped: "bg-zinc-700/30 text-zinc-500 border-zinc-700/40",
    failed: "bg-red-500/15 text-red-300 border-red-500/30",
  };
  return map[phase] ?? NEUTRAL;
}

export function deployStatusClass(status: string): string {
  const map: Record<string, string> = {
    pending: "bg-zinc-500/15 text-zinc-300 border-zinc-500/30",
    active: "bg-emerald-500/15 text-emerald-300 border-emerald-500/30",
    draining: "bg-orange-500/15 text-orange-300 border-orange-500/30",
    failed: "bg-red-500/15 text-red-300 border-red-500/30",
    rolledback: "bg-amber-500/15 text-amber-300 border-amber-500/30",
    superseded: "bg-zinc-700/30 text-zinc-500 border-zinc-700/40",
  };
  return map[status] ?? NEUTRAL;
}

export function hostStatusClass(status: string): string {
  const map: Record<string, string> = {
    ready: "bg-emerald-500/15 text-emerald-300 border-emerald-500/30",
    notready: "bg-red-500/15 text-red-300 border-red-500/30",
    draining: "bg-orange-500/15 text-orange-300 border-orange-500/30",
    cordoned: "bg-zinc-500/15 text-zinc-400 border-zinc-500/30",
  };
  return map[status] ?? NEUTRAL;
}

export function levelClass(level: string): string {
  const map: Record<string, string> = {
    DEBUG: "bg-zinc-500/15 text-zinc-400 border-zinc-500/30",
    INFO: "bg-blue-500/15 text-blue-300 border-blue-500/30",
    WARN: "bg-amber-500/15 text-amber-300 border-amber-500/30",
    ERROR: "bg-red-500/15 text-red-300 border-red-500/30",
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
