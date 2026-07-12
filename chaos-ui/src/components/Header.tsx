"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useStore } from "@/lib/store";

const NAV = [
  { href: "/", label: "Topology" },
  { href: "/desired", label: "Desired" },
  { href: "/chaos", label: "Chaos" },
  { href: "/logs", label: "Logs" },
];

export function Header() {
  const pathname = usePathname();
  const s = useStore();

  const envs = s.meta.environments
    .filter((e) => s.project === "all" || e.project_name === s.project)
    .map((e) => e.name);
  const uniqueEnvs = [...new Set(envs)];

  return (
    <header className="sticky top-0 z-20 border-b border-[var(--color-border)] bg-[rgba(11,13,14,0.82)] backdrop-blur-md">
      <div className="mx-auto flex max-w-[1400px] flex-wrap items-center gap-x-6 gap-y-3 px-5 py-3">
        <div className="flex items-center gap-2.5">
          <span
            className="grid h-7 w-7 place-items-center rounded-lg text-sm font-bold text-white"
            style={{ background: "linear-gradient(180deg,#9a4de0,#7a34bd)" }}
          >
            C
          </span>
          <div className="leading-tight">
            <div className="text-sm font-semibold">chaos-ui</div>
            <div className="text-[0.65rem] text-[var(--color-faint)]">
              conductor console
            </div>
          </div>
        </div>

        <nav className="flex items-center gap-1">
          {NAV.map((n) => {
            const active =
              n.href === "/" ? pathname === "/" : pathname.startsWith(n.href);
            return (
              <Link
                key={n.href}
                href={n.href}
                className={`rounded-lg px-3 py-1.5 text-[0.82rem] font-medium transition-colors ${
                  active
                    ? "bg-[rgba(133,59,206,0.16)] text-[var(--color-accent-fg)]"
                    : "text-[var(--color-muted)] hover:text-[var(--color-fg)]"
                }`}
              >
                {n.label}
              </Link>
            );
          })}
        </nav>

        <div className="ml-auto flex flex-wrap items-center gap-2.5">
          <Selector
            label="project"
            value={s.project}
            onChange={s.setProject}
            options={s.meta.projects.map((p) => p.name)}
          />
          <Selector
            label="env"
            value={s.environment}
            onChange={s.setEnvironment}
            options={uniqueEnvs}
          />
          <Selector
            label="region"
            value={s.region}
            onChange={s.setRegion}
            options={s.meta.regions}
          />
          <span
            className="badge mono border-[var(--color-border)] bg-[var(--color-panel-2)] text-[var(--color-muted)]"
            title="Client session id (localStorage)"
          >
            {s.session || "…"}
          </span>
        </div>
      </div>
    </header>
  );
}

function Selector({
  label,
  value,
  onChange,
  options,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  options: string[];
}) {
  return (
    <label className="flex items-center gap-1.5 rounded-lg border border-[var(--color-border)] bg-[var(--color-panel-2)] pl-2.5 pr-1 py-1">
      <span className="text-[0.65rem] uppercase tracking-wide text-[var(--color-faint)]">
        {label}
      </span>
      <select
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="mono cursor-pointer border-none bg-transparent text-[0.78rem] text-[var(--color-fg)] outline-none"
      >
        <option value="all">all</option>
        {options.map((o) => (
          <option key={o} value={o}>
            {o}
          </option>
        ))}
      </select>
    </label>
  );
}
