"use client";

import { useState } from "react";
import { useStore, selectionQuery } from "@/lib/store";
import { usePoll } from "@/lib/hooks";
import { postJson } from "@/lib/api";
import { Badge } from "@/components/Badge";
import { ActivityLog } from "@/components/ActivityLog";
import { ActionMenu, type MenuItem } from "@/components/ActionMenu";
import {
  CreateMenu,
  CreateForm,
  DeployForm,
  ScaleForm,
  type CreateKind,
} from "@/components/DesiredForms";
import {
  phaseClass,
  deployStatusClass,
  hostStatusClass,
  shortId,
  relativeTime,
} from "@/lib/ui";
import type { Topology, ServiceNode, HostRow, ServedRow } from "@/lib/db";

interface Act {
  key: string;
  label: string;
  hint: string;
  danger?: boolean;
}

type ChaosFn = (act: Act, id: string, target: string) => Promise<void>;

// Agent-observable chaos goes through agentsim (the agent lies or goes silent
// over the real gRPC transport); only operator desired state and synthetic row
// accidents touch the database directly.
const REPLICA_ACTS: Act[] = [
  {
    key: "replica_crash",
    label: "Crash",
    danger: true,
    hint: "Agent reports the container dead; reconciler reaps and replaces.",
  },
  {
    key: "replica_crashloop",
    label: "Crash loop",
    danger: true,
    hint: "Dies and restarts every tick until the crash-loop rule fails it.",
  },
  {
    key: "replica_stall_health",
    label: "Stall health check",
    hint: "Up but never passes probes; feeds the progress-deadline path.",
  },
  {
    key: "replica_heal",
    label: "Heal",
    hint: "Clears chaos mode; container resumes its normal lifecycle.",
  },
  {
    key: "delete_replica",
    label: "Orphan (delete row)",
    danger: true,
    hint: "Deletes the replica row so the engine never learns it was lost.",
  },
];

const DEPLOYMENT_ACTS: Act[] = [
  {
    key: "crash_deployment",
    label: "Crash all replicas",
    danger: true,
    hint: "Every live replica dies at once, like a bad image taking out the fleet.",
  },
  {
    key: "stall_rollout",
    label: "Stall rollout",
    hint: "All live replicas stay up but never healthy.",
  },
];

const HOST_ACTS: Act[] = [
  {
    key: "host_kill",
    label: "Kill",
    danger: true,
    hint: "Agent goes silent. ~30s out of scheduling, ~2min declared dead.",
  },
  {
    key: "host_recover",
    label: "Recover",
    hint: "Agent resumes heartbeating; host returns to ready.",
  },
  {
    key: "cordon_host",
    label: "Cordon",
    hint: "Existing replicas stay; nothing new is placed here.",
  },
  {
    key: "drain_host",
    label: "Drain",
    hint: "Engine evacuates replicas off this host.",
  },
];

function toItems(acts: Act[], fire: (a: Act) => void): MenuItem[] {
  return acts.map((a) => ({
    key: a.key,
    label: a.label,
    hint: a.hint,
    danger: a.danger,
    onSelect: () => fire(a),
  }));
}

export default function ConsolePage() {
  const s = useStore();
  const { data, error, loading } = usePoll<Topology>(
    `/api/topology${selectionQuery(s)}`,
    2000,
  );
  const [creating, setCreating] = useState<CreateKind | null>(null);

  const chaos: ChaosFn = async (act, id, target) => {
    const { ok, data: res } = await postJson("/api/chaos", {
      action: act.key,
      id,
    });
    s.logChaos(
      act.key,
      ok
        ? `${act.label} → ${target}`
        : `${act.label} on ${target} failed: ${res.error ?? "unknown"}`,
      ok,
    );
  };

  const servedFor = (esId: string): ServedRow[] =>
    (data?.served ?? []).filter((sr) => sr.environment_service_id === esId);

  return (
    <div className="grid grid-cols-1 gap-5 lg:grid-cols-[1fr_300px]">
      <section className="space-y-4">
        <div className="flex items-center gap-3">
          <h1 className="text-lg font-semibold">Topology</h1>
          <LiveDot loading={loading} error={error} />
          <span className="ml-auto">
            <CreateMenu onPick={setCreating} />
          </span>
        </div>
        {error && <ErrorCard error={error} />}
        {creating && (
          <CreateForm kind={creating} onDone={() => setCreating(null)} />
        )}

        {data?.tree.length === 0 && (
          <div className="panel px-5 py-10 text-center text-sm text-[var(--color-muted)]">
            Nothing here yet. Start with{" "}
            <span className="text-[var(--color-accent-fg)]">+ New</span>.
          </div>
        )}
        {data?.tree.map((p) =>
          p.environments.map((env) => (
            <div key={env.id} className="panel overflow-hidden">
              <div className="mono flex items-baseline gap-1.5 border-b border-[var(--color-border-soft)] px-4 py-2.5 text-sm">
                <span className="text-[var(--color-faint)]">{p.project} /</span>
                <span className="font-semibold">{env.name}</span>
              </div>
              {env.services.length === 0 ? (
                <div className="px-4 py-5 text-xs text-[var(--color-faint)]">
                  no services bound
                </div>
              ) : (
                <div className="divide-y divide-[var(--color-border-soft)]">
                  {env.services.map((svc) => (
                    <ServiceRow
                      key={svc.es_id}
                      svc={svc}
                      served={servedFor(svc.es_id)}
                      path={`${p.project}/${env.name}/${svc.service}`}
                      chaos={chaos}
                    />
                  ))}
                </div>
              )}
            </div>
          )),
        )}
      </section>

      <aside className="space-y-5">
        <HostsPanel hosts={data?.hosts ?? []} chaos={chaos} />
        <ActivityLog />
      </aside>
    </div>
  );
}

function ServiceRow({
  svc,
  served,
  path,
  chaos,
}: {
  svc: ServiceNode;
  served: ServedRow[];
  path: string;
  chaos: ChaosFn;
}) {
  const d = svc.deployment;
  const [form, setForm] = useState<"deploy" | "scale" | null>(null);
  const toggle = (f: "deploy" | "scale") => setForm(form === f ? null : f);

  const desired = svc.regions.reduce((n, r) => n + r.desired, 0);
  const healthy = svc.regions.reduce((n, r) => n + r.healthy, 0);
  const converged = desired > 0 && healthy >= desired;

  // Traffic pointing at a version other than the current deployment is the
  // interesting case (mid-rollout or rolled back); otherwise it's just noise.
  const servedVersions = [...new Set(served.map((sr) => sr.dep_version))];
  const trafficLagging =
    d && servedVersions.some((v) => v != null && v !== d.version);

  const menu: MenuItem[] = [
    {
      key: "deploy",
      label: "Deploy new version",
      hint: "Commit a pending version; engine converges on next tick.",
      onSelect: () => toggle("deploy"),
    },
    ...(d
      ? [
          {
            key: "scale",
            label: "Scale",
            hint: "Patch per-region replica counts.",
            onSelect: () => toggle("scale"),
          },
        ]
      : []),
    ...(d && svc.replicas.length > 0
      ? toItems(DEPLOYMENT_ACTS, (a) => chaos(a, d.id, `${path} v${d.version}`))
      : []),
  ];

  return (
    <div className="px-4 py-3">
      <div className="flex flex-wrap items-center gap-2.5">
        <span className="text-sm font-medium">{svc.service}</span>
        {svc.stateful && (
          <span className="text-[0.68rem] text-[var(--color-faint)]">
            stateful
          </span>
        )}
        {d ? (
          <>
            <Badge className={deployStatusClass(d.status)}>
              v{d.version} · {d.status}
            </Badge>
            <span
              className="mono truncate text-xs text-[var(--color-faint)]"
              title={d.image_ref}
            >
              {d.image_ref}
            </span>
            {trafficLagging && (
              <Badge
                className="border-amber-400/40 bg-amber-500/10 text-amber-700"
                title="Traffic pointer per region"
              >
                serving{" "}
                {served
                  .map((sr) => `${sr.region} v${sr.dep_version ?? "?"}`)
                  .join(", ")}
              </Badge>
            )}
          </>
        ) : (
          <span className="text-xs text-[var(--color-faint)]">
            no deployment
          </span>
        )}

        <span className="ml-auto flex items-center gap-3">
          {desired > 0 && (
            <span
              className={`text-xs ${converged ? "text-emerald-700" : "text-amber-700"}`}
              title={svc.regions
                .map((r) => `${r.region}: ${r.healthy}/${r.desired} healthy, ${r.observed} live`)
                .join("\n")}
            >
              {healthy}/{desired} healthy
              {svc.regions.length > 1 && (
                <span className="text-[var(--color-faint)]">
                  {" "}
                  · {svc.regions.length} regions
                </span>
              )}
            </span>
          )}
          <ActionMenu items={menu} />
        </span>
      </div>

      {form === "deploy" && (
        <DeployForm
          esId={svc.es_id}
          label={path}
          defaultImage={d?.image_ref}
          current={svc.regions}
          onDone={() => setForm(null)}
        />
      )}
      {form === "scale" && d && (
        <ScaleForm
          deploymentId={d.id}
          label={path}
          current={svc.regions}
          onDone={() => setForm(null)}
        />
      )}

      {svc.replicas.length > 0 && <ReplicaTable svc={svc} chaos={chaos} />}
    </div>
  );
}

function ReplicaTable({ svc, chaos }: { svc: ServiceNode; chaos: ChaosFn }) {
  const multiRegion = svc.regions.length > 1;
  return (
    <div className="mt-2.5 overflow-x-auto">
      <table className="w-full border-collapse text-left text-xs">
        <tbody className="text-[var(--color-muted)]">
          {svc.replicas.map((r) => (
            <tr key={r.id} className="border-t border-[var(--color-border-soft)]">
              <td className="mono w-24 py-1.5 pr-3 text-[var(--color-fg)]">
                {shortId(r.id)}
              </td>
              <td className="mono py-1.5 pr-3">
                {r.hostname ?? "unplaced"}
                {multiRegion && (
                  <span className="text-[var(--color-faint)]"> · {r.region}</span>
                )}
              </td>
              <td className="py-1.5 pr-3">
                <span className="flex items-center gap-1.5">
                  <Badge
                    className={phaseClass(r.phase)}
                    title={r.last_exit_reason ?? undefined}
                  >
                    {r.phase}
                  </Badge>
                  {!r.is_current && (
                    <span className="text-[var(--color-faint)]">
                      v{r.dep_version}
                    </span>
                  )}
                  {r.restart_count > 0 && (
                    <span className="text-amber-700">
                      {r.restart_count} restarts
                    </span>
                  )}
                </span>
              </td>
              <td className="py-1.5 pr-3 whitespace-nowrap text-right text-[var(--color-faint)]">
                {relativeTime(r.updated_at)}
              </td>
              <td className="w-8 py-1 text-right">
                <ActionMenu
                  items={toItems(REPLICA_ACTS, (a) =>
                    chaos(a, r.id, `${shortId(r.id)} · ${r.region}`),
                  )}
                />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function HostsPanel({ hosts, chaos }: { hosts: HostRow[]; chaos: ChaosFn }) {
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
          <div key={h.id} className="flex items-center gap-2 px-4 py-2.5">
            <div className="min-w-0 flex-1">
              <div className="mono truncate text-sm">{h.hostname}</div>
              <div className="mono text-[0.68rem] text-[var(--color-faint)]">
                {h.region} · hb {relativeTime(h.last_heartbeat)}
              </div>
            </div>
            <Badge className={hostStatusClass(h.status)}>{h.status}</Badge>
            <ActionMenu
              items={toItems(HOST_ACTS, (a) => chaos(a, h.id, h.hostname))}
            />
          </div>
        ))}
      </div>
    </div>
  );
}

function PanelHead({ title, count }: { title: string; count: number }) {
  return (
    <div className="flex items-center justify-between border-b border-[var(--color-border-soft)] px-4 py-2.5">
      <h3 className="text-sm font-semibold">{title}</h3>
      <span className="mono text-xs text-[var(--color-faint)]">{count}</span>
    </div>
  );
}

function LiveDot({
  loading,
  error,
}: {
  loading: boolean;
  error: string | null;
}) {
  const color = error ? "bg-red-500" : "bg-emerald-500";
  return (
    <span className="flex items-center gap-1.5 text-xs text-[var(--color-muted)]">
      <span
        className={`h-2 w-2 rounded-full ${color} ${!error && "animate-pulse"}`}
      />
      {error ? "error" : loading ? "syncing" : "live"}
    </span>
  );
}

function ErrorCard({ error }: { error: string }) {
  return (
    <div className="panel border-red-400/40 bg-red-500/5 px-4 py-3 text-sm text-red-700">
      {error}
    </div>
  );
}
