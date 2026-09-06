"use client";

import { useEffect, useState } from "react";
import { useStore } from "@/lib/store";
import { postJson } from "@/lib/api";
import { ActionMenu } from "@/components/ActionMenu";

// All desired-state mutations report into the shared activity log instead of a
// local banner, so chaos and intent changes read as one timeline.
function useSubmit() {
  const s = useStore();
  return async (
    body: Record<string, unknown>,
    label: string,
  ): Promise<boolean> => {
    const { ok, data } = await postJson("/api/desired", body);
    s.logChaos(
      label,
      ok
        ? `${label} ok${data.version ? ` (v${data.version})` : ""}`
        : `${label}: ${data.error ?? "failed"}`,
      ok,
    );
    if (ok) s.refreshMeta();
    return ok;
  };
}

const KINDS = [
  { key: "project", label: "Project" },
  { key: "environment", label: "Environment" },
  { key: "service", label: "Service" },
  { key: "bind", label: "Bind service to environment" },
] as const;

export type CreateKind = (typeof KINDS)[number]["key"];

export function CreateMenu({ onPick }: { onPick: (k: CreateKind) => void }) {
  return (
    <ActionMenu
      label="+ New"
      items={KINDS.map((k) => ({
        key: k.key,
        label: k.label,
        onSelect: () => onPick(k.key),
      }))}
    />
  );
}

export function CreateForm({
  kind,
  onDone,
}: {
  kind: CreateKind;
  onDone: () => void;
}) {
  return (
    <div className="panel bg-[var(--color-panel-2)] px-4 py-3">
      {kind === "project" && <ProjectForm onDone={onDone} />}
      {kind === "environment" && <EnvironmentForm onDone={onDone} />}
      {kind === "service" && <ServiceForm onDone={onDone} />}
      {kind === "bind" && <BindForm onDone={onDone} />}
    </div>
  );
}

function F({
  label,
  wide,
  children,
}: {
  label: string;
  wide?: boolean;
  children: React.ReactNode;
}) {
  return (
    <label className={`block ${wide ? "w-72" : "w-44"}`}>
      <span className="label">{label}</span>
      {children}
    </label>
  );
}

function ProjectForm({ onDone }: { onDone: () => void }) {
  const submit = useSubmit();
  const [name, setName] = useState("");
  const [env, setEnv] = useState("production");
  return (
    <div className="flex flex-wrap items-end gap-3">
      <F label="project name">
        <input
          className="input mono"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="acme"
        />
      </F>
      <F label="first environment">
        <input
          className="input mono"
          value={env}
          onChange={(e) => setEnv(e.target.value)}
        />
      </F>
      <button
        className="btn btn-accent"
        disabled={!name}
        onClick={async () => {
          if (
            await submit(
              { action: "create_project", name, environment: env },
              "create project",
            )
          )
            onDone();
        }}
      >
        Create project
      </button>
    </div>
  );
}

function EnvironmentForm({ onDone }: { onDone: () => void }) {
  const s = useStore();
  const [project, setProject] = useState("");
  const [name, setName] = useState("");
  const submit = useSubmit();
  useEffect(() => {
    if (s.project !== "all") setProject(s.project);
  }, [s.project]);
  return (
    <div className="flex flex-wrap items-end gap-3">
      <F label="project">
        <ProjectSelect value={project} onChange={setProject} />
      </F>
      <F label="environment name">
        <input
          className="input mono"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="staging"
        />
      </F>
      <button
        className="btn btn-accent"
        disabled={!project || !name}
        onClick={async () => {
          if (
            await submit(
              { action: "create_environment", project, name },
              "create environment",
            )
          )
            onDone();
        }}
      >
        Create environment
      </button>
    </div>
  );
}

function ServiceForm({ onDone }: { onDone: () => void }) {
  const s = useStore();
  const [project, setProject] = useState("");
  const [name, setName] = useState("");
  const [stateful, setStateful] = useState(false);
  const submit = useSubmit();
  useEffect(() => {
    if (s.project !== "all") setProject(s.project);
  }, [s.project]);
  return (
    <div className="flex flex-wrap items-end gap-3">
      <F label="project">
        <ProjectSelect value={project} onChange={setProject} />
      </F>
      <F label="service name">
        <input
          className="input mono"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="api"
        />
      </F>
      <label className="flex items-center gap-2 pb-2 text-sm text-[var(--color-muted)]">
        <input
          type="checkbox"
          checked={stateful}
          onChange={(e) => setStateful(e.target.checked)}
        />
        stateful
      </label>
      <button
        className="btn btn-accent"
        disabled={!project || !name}
        onClick={async () => {
          if (
            await submit(
              { action: "create_service", project, name, stateful },
              "create service",
            )
          )
            onDone();
        }}
      >
        Create service
      </button>
    </div>
  );
}

function BindForm({ onDone }: { onDone: () => void }) {
  const s = useStore();
  const submit = useSubmit();
  const [project, setProject] = useState("");
  const [environmentId, setEnvironmentId] = useState("");
  const [serviceId, setServiceId] = useState("");
  const [source, setSource] = useState('{"image":"nginx:latest"}');
  const [services, setServices] = useState<{ id: string; name: string }[]>([]);

  useEffect(() => {
    if (s.project !== "all") setProject(s.project);
  }, [s.project]);

  useEffect(() => {
    if (!project) return setServices([]);
    fetch(`/api/services?project=${encodeURIComponent(project)}`)
      .then((r) => r.json())
      .then((d) => setServices(d.services ?? []))
      .catch(() => setServices([]));
  }, [project]);

  const envs = s.meta.environments.filter((e) => e.project_name === project);

  return (
    <div className="flex flex-wrap items-end gap-3">
      <F label="project">
        <ProjectSelect value={project} onChange={setProject} />
      </F>
      <F label="environment">
        <select
          className="input mono"
          value={environmentId}
          onChange={(e) => setEnvironmentId(e.target.value)}
        >
          <option value="">select…</option>
          {envs.map((e) => (
            <option key={e.id} value={e.id}>
              {e.name}
            </option>
          ))}
        </select>
      </F>
      <F label="service">
        <select
          className="input mono"
          value={serviceId}
          onChange={(e) => setServiceId(e.target.value)}
        >
          <option value="">select…</option>
          {services.map((sv) => (
            <option key={sv.id} value={sv.id}>
              {sv.name}
            </option>
          ))}
        </select>
      </F>
      <F label="source (jsonb)" wide>
        <input
          className="input mono"
          value={source}
          onChange={(e) => setSource(e.target.value)}
        />
      </F>
      <button
        className="btn btn-accent"
        disabled={!environmentId || !serviceId}
        onClick={async () => {
          if (
            await submit(
              {
                action: "create_environment_service",
                environmentId,
                serviceId,
                source,
              },
              "bind service",
            )
          )
            onDone();
        }}
      >
        Bind
      </button>
    </div>
  );
}

const MB = 1024 * 1024;

// Inline per-service deploy — the service row already pins esId, so the form
// only asks for the deployment spec itself.
export function DeployForm({
  esId,
  label,
  defaultImage,
  current,
  onDone,
}: {
  esId: string;
  label: string;
  defaultImage?: string;
  current: { region: string; desired: number }[];
  onDone: () => void;
}) {
  const s = useStore();
  const submit = useSubmit();
  const [imageRef, setImageRef] = useState(defaultImage || "nginx:1.27");
  const [cpu, setCpu] = useState(500);
  const [memMb, setMemMb] = useState(512);
  const [drain, setDrain] = useState(30);
  const [restartMax, setRestartMax] = useState(5);
  const [deadline, setDeadline] = useState(600);
  const [msg, setMsg] = useState("");
  const [counts, setCounts] = useState<Record<string, number>>(() =>
    Object.fromEntries(current.map((r) => [r.region, r.desired])),
  );

  return (
    <div className="panel-soft mt-3 space-y-3 p-3">
      <div className="flex flex-wrap items-end gap-3">
        <F label="image_ref">
          <input
            className="input mono"
            value={imageRef}
            onChange={(e) => setImageRef(e.target.value)}
          />
        </F>
        <NumField label="cpu (m)" value={cpu} onChange={setCpu} />
        <NumField label="mem (MB)" value={memMb} onChange={setMemMb} />
        <NumField label="drain (s)" value={drain} onChange={setDrain} />
        <NumField label="restart_max" value={restartMax} onChange={setRestartMax} />
        <NumField label="deadline (s)" value={deadline} onChange={setDeadline} />
        <F label="commit_message" wide>
          <input
            className="input"
            value={msg}
            onChange={(e) => setMsg(e.target.value)}
            placeholder="deploy via chaos-ui"
          />
        </F>
      </div>
      <RegionCounts counts={counts} setCounts={setCounts} />
      <div className="flex items-center gap-2">
        <button
          className="btn btn-accent"
          disabled={!imageRef}
          onClick={async () => {
            if (
              await submit(
                {
                  action: "create_deployment",
                  esId,
                  imageRef,
                  cpuMillicores: cpu,
                  memBytes: memMb * MB,
                  drainSeconds: drain,
                  restartMax,
                  progressDeadline: deadline,
                  commitMessage: msg,
                  createdBy: `chaos-ui:${s.session}`,
                  regions: Object.entries(counts)
                    .filter(([, v]) => v > 0)
                    .map(([region, replicas]) => ({ region, replicas })),
                },
                `deploy ${label}`,
              )
            )
              onDone();
          }}
        >
          Deploy
        </button>
        <button className="btn" onClick={onDone}>
          Cancel
        </button>
      </div>
    </div>
  );
}

export function ScaleForm({
  deploymentId,
  label,
  current,
  onDone,
}: {
  deploymentId: string;
  label: string;
  current: { region: string; desired: number }[];
  onDone: () => void;
}) {
  const submit = useSubmit();
  const [counts, setCounts] = useState<Record<string, number>>(() =>
    Object.fromEntries(current.map((r) => [r.region, r.desired])),
  );
  return (
    <div className="panel-soft mt-3 space-y-3 p-3">
      <RegionCounts counts={counts} setCounts={setCounts} />
      <div className="flex items-center gap-2">
        <button
          className="btn btn-accent"
          onClick={async () => {
            if (
              await submit(
                {
                  action: "scale",
                  deploymentId,
                  regions: Object.entries(counts).map(([region, replicas]) => ({
                    region,
                    replicas,
                  })),
                },
                `scale ${label}`,
              )
            )
              onDone();
          }}
        >
          Apply scale
        </button>
        <button className="btn" onClick={onDone}>
          Cancel
        </button>
      </div>
    </div>
  );
}

function NumField({
  label,
  value,
  onChange,
}: {
  label: string;
  value: number;
  onChange: (v: number) => void;
}) {
  return (
    <label className="block w-28">
      <span className="label">{label}</span>
      <input
        type="number"
        className="input mono"
        value={value}
        onChange={(e) => onChange(+e.target.value)}
      />
    </label>
  );
}

function RegionCounts({
  counts,
  setCounts,
}: {
  counts: Record<string, number>;
  setCounts: (c: Record<string, number>) => void;
}) {
  const s = useStore();
  return (
    <div>
      <span className="label">replicas per region</span>
      <div className="flex flex-wrap gap-3">
        {s.meta.regions.map((r) => (
          <label key={r} className="flex items-center gap-2">
            <span className="mono text-xs text-[var(--color-muted)]">{r}</span>
            <input
              type="number"
              min={0}
              className="input mono !w-20"
              value={counts[r] ?? 0}
              onChange={(e) =>
                setCounts({ ...counts, [r]: Math.max(0, +e.target.value) })
              }
            />
          </label>
        ))}
      </div>
    </div>
  );
}

function ProjectSelect({
  value,
  onChange,
}: {
  value: string;
  onChange: (v: string) => void;
}) {
  const s = useStore();
  return (
    <select
      className="input mono"
      value={value}
      onChange={(e) => onChange(e.target.value)}
    >
      <option value="">select…</option>
      {s.meta.projects.map((p) => (
        <option key={p.name} value={p.name}>
          {p.name}
        </option>
      ))}
    </select>
  );
}
