"use client";

import { useMemo, useState } from "react";
import { useStore, selectionQuery } from "@/lib/store";
import { usePoll } from "@/lib/hooks";
import { postJson } from "@/lib/api";
import { Badge } from "@/components/Badge";
import {
  ActionModal,
  type ActionDef,
  type ActionTarget,
} from "@/components/ActionModal";
import { phaseClass, hostStatusClass, shortId, relativeTime } from "@/lib/ui";
import type { Topology, HostRow, ReplicaRow } from "@/lib/db";

interface DeploymentTarget {
  id: string;
  label: string;
  version: number;
  status: string;
  replicaCount: number;
}

const REPLICA_ACTIONS: ActionDef[] = [
  {
    key: "kill_replica",
    name: "Kill replica",
    description:
      "Simulates a hard crash: healthy=false, phase=failed, last_exit_reason='chaos: killed', restart_count incremented.",
    danger: true,
  },
  {
    key: "flap_health",
    name: "Flap health",
    description:
      "Simulates a flapping health probe: healthy=false while the phase stays intact — no crash, just a failed check.",
  },
  {
    key: "delete_replica",
    name: "Delete row (orphan)",
    description:
      "Deletes the replica row outright, simulating an orphaned allocation the engine never heard about losing.",
    danger: true,
  },
];

const DEPLOYMENT_ACTIONS: ActionDef[] = [
  {
    key: "crash_deployment",
    name: "Crash deployment",
    description:
      "Fails every replica of this deployment at once (failed / unhealthy / restart_count++), like a bad image taking out the whole fleet.",
    danger: true,
  },
  {
    key: "stall_rollout",
    name: "Stall rollout",
    description:
      "Clears the health high-water mark (healthy=false, health_checks_passed_at=NULL) so replicas look 'up but never healthy' — the progress_deadline stalled-rollout signal.",
  },
];

const HOST_ACTIONS: ActionDef[] = [
  {
    key: "host_down",
    name: "Host down",
    description:
      "Simulates a dead node: status=notready and last_heartbeat pushed 10 minutes into the past.",
    danger: true,
  },
  {
    key: "host_recover",
    name: "Recover",
    description: "Brings the host back: status=ready with a fresh heartbeat.",
  },
  {
    key: "cordon_host",
    name: "Cordon",
    description:
      "status=cordoned — existing replicas keep running but the scheduler places nothing new here.",
  },
  {
    key: "drain_host",
    name: "Drain",
    description:
      "status=draining — the engine should evacuate replicas off this host.",
  },
];

export default function ChaosPage() {
  const s = useStore();
  const { data } = usePoll<Topology>(`/api/topology${selectionQuery(s)}`, 2000);
  const [target, setTarget] = useState<ActionTarget | null>(null);

  const { replicas, deployments } = useMemo(() => {
    const reps: (ReplicaRow & { label: string })[] = [];
    const depMap = new Map<string, DeploymentTarget>();
    for (const p of data?.tree ?? []) {
      for (const e of p.environments) {
        for (const svc of e.services) {
          const label = `${p.project}/${e.name}/${svc.service}`;
          if (svc.deployment) {
            depMap.set(svc.deployment.id, {
              id: svc.deployment.id,
              label,
              version: svc.deployment.version,
              status: svc.deployment.status,
              replicaCount: 0,
            });
          }
          for (const r of svc.replicas) {
            reps.push({ ...r, label });
            const d = depMap.get(r.deployment_id);
            if (d) d.replicaCount++;
            else
              depMap.set(r.deployment_id, {
                id: r.deployment_id,
                label,
                version: r.dep_version,
                status: r.is_current ? "current" : "superseded",
                replicaCount: 1,
              });
          }
        }
      }
    }
    return { replicas: reps, deployments: [...depMap.values()] };
  }, [data]);

  const runAction = async (action: ActionDef, id: string) => {
    const { ok, data: res } = await postJson("/api/chaos", {
      action: action.key,
      id,
    });
    const detail = ok
      ? `${action.name} → ${target?.subtitle ?? shortId(id)}`
      : `${action.name} failed: ${res.error ?? "unknown"}`;
    s.logChaos(action.key, detail, ok);
  };

  return (
    <div className="space-y-5">
      <div>
        <h1 className="text-lg font-semibold">Chaos</h1>
        <p className="text-sm text-[var(--color-muted)]">
          Pick a target, choose a failure to inject, confirm. Watch the engine
          react on Topology and Logs.
        </p>
      </div>

      <div className="grid grid-cols-1 gap-5 lg:grid-cols-[1fr_340px]">
        <div className="space-y-5">
          <Section title="Replicas">
            {replicas.length === 0 ? (
              <Empty>no replicas placed yet</Empty>
            ) : (
              <div className="max-h-[420px] divide-y divide-[var(--color-border-soft)] overflow-y-auto">
                {replicas.map((r) => (
                  <TargetRow
                    key={r.id}
                    primary={shortId(r.id)}
                    primaryMono
                    secondary={`${r.label} · ${r.region}${r.hostname ? ` · ${r.hostname}` : ""}`}
                    badge={<Badge className={phaseClass(r.phase)}>{r.phase}</Badge>}
                    onOpen={() =>
                      setTarget({
                        id: r.id,
                        title: "Replica chaos",
                        subtitle: `${shortId(r.id)} · ${r.label} · ${r.region}`,
                        actions: REPLICA_ACTIONS,
                      })
                    }
                  />
                ))}
              </div>
            )}
          </Section>

          <Section title="Deployments">
            {deployments.length === 0 ? (
              <Empty>no deployments with replicas</Empty>
            ) : (
              <div className="divide-y divide-[var(--color-border-soft)]">
                {deployments.map((d) => (
                  <TargetRow
                    key={d.id}
                    primary={`${d.label} v${d.version}`}
                    secondary={`${d.replicaCount} replica(s) · ${d.status}`}
                    onOpen={() =>
                      setTarget({
                        id: d.id,
                        title: "Deployment chaos",
                        subtitle: `${d.label} v${d.version}`,
                        actions: DEPLOYMENT_ACTIONS,
                      })
                    }
                  />
                ))}
              </div>
            )}
          </Section>

          <Section title="Hosts">
            {(data?.hosts ?? []).length === 0 ? (
              <Empty>no hosts (run seed)</Empty>
            ) : (
              <div className="divide-y divide-[var(--color-border-soft)]">
                {(data?.hosts ?? []).map((h: HostRow) => (
                  <TargetRow
                    key={h.id}
                    primary={h.hostname}
                    primaryMono
                    secondary={`${h.region} · hb ${relativeTime(h.last_heartbeat)}`}
                    badge={
                      <Badge className={hostStatusClass(h.status)}>{h.status}</Badge>
                    }
                    onOpen={() =>
                      setTarget({
                        id: h.id,
                        title: "Host chaos",
                        subtitle: `${h.hostname} · ${h.region}`,
                        actions: HOST_ACTIONS,
                      })
                    }
                  />
                ))}
              </div>
            )}
          </Section>
        </div>

        <ChaosLog />
      </div>

      <ActionModal
        target={target}
        onClose={() => setTarget(null)}
        onRun={runAction}
      />
    </div>
  );
}

function TargetRow({
  primary,
  primaryMono,
  secondary,
  badge,
  onOpen,
}: {
  primary: string;
  primaryMono?: boolean;
  secondary: string;
  badge?: React.ReactNode;
  onOpen: () => void;
}) {
  return (
    <div className="flex items-center gap-3 px-4 py-2.5">
      <div className="min-w-0 flex-1">
        <div className={`truncate text-sm ${primaryMono ? "mono" : ""}`}>
          {primary}
        </div>
        <div className="truncate text-[0.68rem] text-[var(--color-faint)]">
          {secondary}
        </div>
      </div>
      {badge}
      <button className="btn !px-2.5 !py-1 text-xs" onClick={onOpen}>
        Actions…
      </button>
    </div>
  );
}

function Section({
  title,
  children,
}: {
  title: string;
  children: React.ReactNode;
}) {
  return (
    <div className="panel overflow-hidden">
      <div className="border-b border-[var(--color-border-soft)] bg-[var(--color-panel-2)] px-4 py-2.5">
        <h3 className="text-sm font-semibold">{title}</h3>
      </div>
      <div>{children}</div>
    </div>
  );
}

function ChaosLog() {
  const s = useStore();
  return (
    <div className="panel sticky top-20 flex max-h-[calc(100vh-6rem)] flex-col overflow-hidden">
      <div className="flex items-center justify-between border-b border-[var(--color-border-soft)] bg-[var(--color-panel-2)] px-4 py-2.5">
        <h3 className="text-sm font-semibold">Chaos log</h3>
        <button
          className="btn !py-1 !px-2 text-xs"
          onClick={s.clearChaos}
          disabled={s.chaosLog.length === 0}
        >
          Clear
        </button>
      </div>
      <div className="flex-1 overflow-y-auto">
        {s.chaosLog.length === 0 ? (
          <Empty>no actions this session</Empty>
        ) : (
          s.chaosLog.map((e) => (
            <div
              key={e.id}
              className="border-b border-[var(--color-border-soft)] px-4 py-2"
            >
              <div className="flex items-center gap-2">
                <span
                  className={`h-1.5 w-1.5 rounded-full ${e.ok ? "bg-emerald-400" : "bg-red-400"}`}
                />
                <span className="mono text-xs text-[var(--color-accent-fg)]">
                  {e.action}
                </span>
                <span className="ml-auto text-[0.65rem] text-[var(--color-faint)]">
                  {relativeTime(e.ts)}
                </span>
              </div>
              <div className="mt-0.5 pl-3.5 text-xs text-[var(--color-muted)]">
                {e.detail}
              </div>
              <div className="mono pl-3.5 text-[0.6rem] text-[var(--color-faint)]">
                {e.session}
              </div>
            </div>
          ))
        )}
      </div>
    </div>
  );
}

function Empty({ children }: { children: React.ReactNode }) {
  return (
    <div className="px-4 py-5 text-xs text-[var(--color-faint)]">{children}</div>
  );
}
