"use client";

import { useStore, selectionQuery } from "@/lib/store";
import { usePoll } from "@/lib/hooks";
import { Badge } from "@/components/Badge";
import {
  phaseClass,
  deployStatusClass,
  hostStatusClass,
  shortId,
  relativeTime,
} from "@/lib/ui";
import type { Topology, ServiceNode, HostRow, ServedRow } from "@/lib/db";

export default function DashboardPage() {
  const s = useStore();
  const { data, error, loading } = usePoll<Topology>(
    `/api/topology${selectionQuery(s)}`,
    2000,
  );

  return (
    <div className="grid grid-cols-1 gap-5 lg:grid-cols-[1fr_360px]">
      <section className="space-y-4">
        <div className="flex items-center justify-between">
          <h1 className="text-lg font-semibold">Topology</h1>
          <LiveDot loading={loading} error={error} />
        </div>
        {error && <ErrorCard error={error} />}
        {data?.tree.length === 0 && (
          <div className="panel px-5 py-10 text-center text-sm text-[var(--color-muted)]">
            No projects match the current selection. Create one on the{" "}
            <span className="text-[var(--color-accent-fg)]">Desired</span> tab.
          </div>
        )}
        {data?.tree.map((p) => (
          <div key={p.project} className="space-y-3">
            <div className="flex items-center gap-2">
              <h2 className="mono text-[0.95rem] font-semibold text-[var(--color-fg)]">
                {p.project}
              </h2>
              <span className="text-xs text-[var(--color-faint)]">
                {p.environments.length} env
              </span>
            </div>
            {p.environments.map((env) => (
              <div key={env.id} className="panel overflow-hidden">
                <div className="flex items-center gap-2 border-b border-[var(--color-border-soft)] bg-[var(--color-panel-2)] px-4 py-2">
                  <span className="text-[0.7rem] uppercase tracking-wide text-[var(--color-faint)]">
                    environment
                  </span>
                  <span className="mono text-sm font-medium">{env.name}</span>
                </div>
                {env.services.length === 0 ? (
                  <div className="px-4 py-5 text-xs text-[var(--color-faint)]">
                    no services bound
                  </div>
                ) : (
                  <div className="divide-y divide-[var(--color-border-soft)]">
                    {env.services.map((svc) => (
                      <ServiceRow key={svc.es_id} svc={svc} />
                    ))}
                  </div>
                )}
              </div>
            ))}
          </div>
        ))}
      </section>

      <aside className="space-y-5">
        <HostsPanel hosts={data?.hosts ?? []} />
        <ServedPanel served={data?.served ?? []} />
      </aside>
    </div>
  );
}

function ServiceRow({ svc }: { svc: ServiceNode }) {
  const d = svc.deployment;
  return (
    <div className="px-4 py-3.5">
      <div className="flex flex-wrap items-center gap-2.5">
        <span className="text-sm font-medium">{svc.service}</span>
        {svc.stateful && (
          <Badge className="border-violet-500/30 bg-violet-500/10 text-violet-300">
            stateful
          </Badge>
        )}
        {d ? (
          <>
            <span className="mono text-xs text-[var(--color-muted)]">
              v{d.version}
            </span>
            <Badge className={deployStatusClass(d.status)}>{d.status}</Badge>
            <span
              className="mono truncate text-xs text-[var(--color-faint)]"
              title={d.image_ref}
            >
              {d.image_ref}
            </span>
          </>
        ) : (
          <span className="text-xs text-[var(--color-faint)]">no deployment</span>
        )}
      </div>

      {svc.regions.length > 0 && (
        <div className="mt-2.5 flex flex-wrap gap-1.5">
          {svc.regions.map((r) => {
            const ok = r.healthy >= r.desired && r.desired > 0;
            return (
              <span
                key={r.region}
                className="badge border-[var(--color-border)] bg-[var(--color-panel-2)]"
              >
                <span className="mono text-[var(--color-muted)]">{r.region}</span>
                <span
                  className={ok ? "text-emerald-300" : "text-amber-300"}
                >
                  {r.healthy}/{r.desired} healthy
                </span>
                {r.observed !== r.desired && (
                  <span className="text-[var(--color-faint)]">
                    ({r.observed} live)
                  </span>
                )}
              </span>
            );
          })}
        </div>
      )}

      {svc.replicas.length > 0 && <ReplicaTable svc={svc} />}
    </div>
  );
}

function ReplicaTable({ svc }: { svc: ServiceNode }) {
  return (
    <div className="mt-3 overflow-x-auto">
      <table className="w-full border-collapse text-left text-xs">
        <thead>
          <tr className="text-[0.65rem] uppercase tracking-wide text-[var(--color-faint)]">
            <th className="py-1 pr-3 font-medium">replica</th>
            <th className="py-1 pr-3 font-medium">rev</th>
            <th className="py-1 pr-3 font-medium">region</th>
            <th className="py-1 pr-3 font-medium">host</th>
            <th className="py-1 pr-3 font-medium">phase</th>
            <th className="py-1 pr-3 font-medium">healthy</th>
            <th className="py-1 pr-3 font-medium">restarts</th>
            <th className="py-1 pr-3 font-medium">last exit</th>
            <th className="py-1 font-medium">updated</th>
          </tr>
        </thead>
        <tbody className="text-[var(--color-muted)]">
          {svc.replicas.map((r) => (
            <tr key={r.id} className="border-t border-[var(--color-border-soft)]">
              <td className="mono py-1.5 pr-3 text-[var(--color-fg)]">
                {shortId(r.id)}
              </td>
              <td className="mono py-1.5 pr-3">
                v{r.dep_version}
                {!r.is_current && (
                  <span className="ml-1 text-[var(--color-faint)]">old</span>
                )}
              </td>
              <td className="mono py-1.5 pr-3">{r.region}</td>
              <td className="mono py-1.5 pr-3">{r.hostname ?? "—"}</td>
              <td className="py-1.5 pr-3">
                <Badge className={phaseClass(r.phase)}>{r.phase}</Badge>
              </td>
              <td className="py-1.5 pr-3">
                <span
                  className={
                    r.healthy ? "text-emerald-300" : "text-[var(--color-faint)]"
                  }
                >
                  {r.healthy ? "yes" : "no"}
                </span>
              </td>
              <td className="mono py-1.5 pr-3">{r.restart_count}</td>
              <td className="py-1.5 pr-3 text-[var(--color-faint)]">
                {r.last_exit_reason ?? "—"}
              </td>
              <td className="py-1.5 whitespace-nowrap text-[var(--color-faint)]">
                {relativeTime(r.updated_at)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function HostsPanel({ hosts }: { hosts: HostRow[] }) {
  return (
    <div className="panel overflow-hidden">
      <PanelHead title="Hosts" count={hosts.length} />
      <div className="divide-y divide-[var(--color-border-soft)]">
        {hosts.length === 0 && (
          <div className="px-4 py-4 text-xs text-[var(--color-faint)]">
            no hosts (run seed)
          </div>
        )}
        {hosts.map((h) => (
          <div
            key={h.id}
            className="flex items-center justify-between px-4 py-2.5"
          >
            <div>
              <div className="mono text-sm">{h.hostname}</div>
              <div className="mono text-[0.68rem] text-[var(--color-faint)]">
                {h.region} · hb {relativeTime(h.last_heartbeat)}
              </div>
            </div>
            <Badge className={hostStatusClass(h.status)}>{h.status}</Badge>
          </div>
        ))}
      </div>
    </div>
  );
}

function ServedPanel({ served }: { served: ServedRow[] }) {
  return (
    <div className="panel overflow-hidden">
      <PanelHead title="Served revisions" count={served.length} />
      <div className="divide-y divide-[var(--color-border-soft)]">
        {served.length === 0 && (
          <div className="px-4 py-4 text-xs text-[var(--color-faint)]">
            no traffic pointers yet
          </div>
        )}
        {served.map((sr) => (
          <div
            key={`${sr.environment_service_id}-${sr.region}`}
            className="flex items-center justify-between px-4 py-2.5"
          >
            <div>
              <div className="text-sm">
                {sr.environment}/{sr.service}
              </div>
              <div className="mono text-[0.68rem] text-[var(--color-faint)]">
                {sr.region} · {relativeTime(sr.updated_at)}
              </div>
            </div>
            <Badge className="border-[var(--color-accent)]/40 bg-[rgba(133,59,206,0.12)] text-[var(--color-accent-fg)]">
              v{sr.dep_version ?? "?"}
            </Badge>
          </div>
        ))}
      </div>
    </div>
  );
}

function PanelHead({ title, count }: { title: string; count: number }) {
  return (
    <div className="flex items-center justify-between border-b border-[var(--color-border-soft)] bg-[var(--color-panel-2)] px-4 py-2.5">
      <h3 className="text-sm font-semibold">{title}</h3>
      <span className="mono text-xs text-[var(--color-faint)]">{count}</span>
    </div>
  );
}

function LiveDot({ loading, error }: { loading: boolean; error: string | null }) {
  const color = error ? "bg-red-400" : "bg-emerald-400";
  return (
    <span className="flex items-center gap-1.5 text-xs text-[var(--color-muted)]">
      <span className={`h-2 w-2 rounded-full ${color} ${!error && "animate-pulse"}`} />
      {error ? "error" : loading ? "syncing" : "live · 2s"}
    </span>
  );
}

function ErrorCard({ error }: { error: string }) {
  return (
    <div className="panel border-red-500/30 bg-red-500/5 px-4 py-3 text-sm text-red-300">
      {error}
    </div>
  );
}
