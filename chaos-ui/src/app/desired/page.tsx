"use client";

import { useEffect, useMemo, useState } from "react";
import { useStore, selectionQuery } from "@/lib/store";
import { usePoll } from "@/lib/hooks";
import { postJson } from "@/lib/api";
import { Card, Field } from "@/components/Card";
import type { Topology } from "@/lib/db";

interface FlatEs {
  esId: string;
  project: string;
  environment: string;
  service: string;
  deploymentId: string | null;
  version: number | null;
  status: string | null;
  regions: { region: string; desired: number }[];
}

export default function DesiredPage() {
  const s = useStore();
  const [note, setNote] = useState<{ ok: boolean; text: string } | null>(null);

  const { data } = usePoll<Topology>(`/api/topology${selectionQuery(s)}`, 3000);

  const flat: FlatEs[] = useMemo(() => {
    const out: FlatEs[] = [];
    for (const p of data?.tree ?? []) {
      for (const e of p.environments) {
        for (const svc of e.services) {
          out.push({
            esId: svc.es_id,
            project: p.project,
            environment: e.name,
            service: svc.service,
            deploymentId: svc.deployment?.id ?? null,
            version: svc.deployment?.version ?? null,
            status: svc.deployment?.status ?? null,
            regions: svc.regions.map((r) => ({
              region: r.region,
              desired: r.desired,
            })),
          });
        }
      }
    }
    return out;
  }, [data]);

  async function submit(body: Record<string, unknown>, label: string) {
    const { ok, data: res } = await postJson("/api/desired", body);
    if (ok) {
      setNote({ ok: true, text: `${label} ok${res.version ? ` (v${res.version})` : ""}` });
      s.refreshMeta();
    } else {
      setNote({ ok: false, text: `${label}: ${res.error ?? "failed"}` });
    }
  }

  return (
    <div className="space-y-5">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-lg font-semibold">Desired state</h1>
          <p className="text-sm text-[var(--color-muted)]">
            Mutate intent directly in Postgres. The engine converges on its next
            tick.
          </p>
        </div>
      </div>

      {note && (
        <div
          className={`panel px-4 py-2.5 text-sm ${
            note.ok
              ? "border-emerald-500/30 bg-emerald-500/5 text-emerald-300"
              : "border-red-500/30 bg-red-500/5 text-red-300"
          }`}
        >
          {note.text}
        </div>
      )}

      <div className="grid grid-cols-1 gap-5 xl:grid-cols-2">
        <ProjectForm onSubmit={submit} />
        <EnvironmentForm onSubmit={submit} />
        <ServiceForm onSubmit={submit} />
        <EnvironmentServiceForm onSubmit={submit} />
      </div>

      <DeploymentForm flat={flat} regions={s.meta.regions} onSubmit={submit} />
      <ScaleForm flat={flat} regions={s.meta.regions} onSubmit={submit} />
    </div>
  );
}

type Submit = (body: Record<string, unknown>, label: string) => Promise<void>;

function ProjectForm({ onSubmit }: { onSubmit: Submit }) {
  const [name, setName] = useState("");
  const [env, setEnv] = useState("production");
  return (
    <Card title="Create project" subtitle="Creates the project and its first environment.">
      <div className="grid grid-cols-2 gap-3">
        <Field label="project name">
          <input className="input mono" value={name} onChange={(e) => setName(e.target.value)} placeholder="acme" />
        </Field>
        <Field label="first environment">
          <input className="input mono" value={env} onChange={(e) => setEnv(e.target.value)} />
        </Field>
      </div>
      <button
        className="btn btn-accent mt-3"
        disabled={!name}
        onClick={() => onSubmit({ action: "create_project", name, environment: env }, "create project")}
      >
        Create project
      </button>
    </Card>
  );
}

function EnvironmentForm({ onSubmit }: { onSubmit: Submit }) {
  const s = useStore();
  const [project, setProject] = useState("");
  const [name, setName] = useState("");
  useEffect(() => {
    if (s.project !== "all") setProject(s.project);
  }, [s.project]);
  return (
    <Card title="Create environment" subtitle="Adds an environment to an existing project.">
      <div className="grid grid-cols-2 gap-3">
        <Field label="project">
          <ProjectSelect value={project} onChange={setProject} />
        </Field>
        <Field label="environment name">
          <input className="input mono" value={name} onChange={(e) => setName(e.target.value)} placeholder="staging" />
        </Field>
      </div>
      <button
        className="btn btn-accent mt-3"
        disabled={!project || !name}
        onClick={() => onSubmit({ action: "create_environment", project, name }, "create environment")}
      >
        Create environment
      </button>
    </Card>
  );
}

function ServiceForm({ onSubmit }: { onSubmit: Submit }) {
  const s = useStore();
  const [project, setProject] = useState("");
  const [name, setName] = useState("");
  const [stateful, setStateful] = useState(false);
  useEffect(() => {
    if (s.project !== "all") setProject(s.project);
  }, [s.project]);
  return (
    <Card title="Create service" subtitle="A service is project-scoped; bind it to an environment below.">
      <div className="grid grid-cols-2 gap-3">
        <Field label="project">
          <ProjectSelect value={project} onChange={setProject} />
        </Field>
        <Field label="service name">
          <input className="input mono" value={name} onChange={(e) => setName(e.target.value)} placeholder="api" />
        </Field>
      </div>
      <label className="mt-3 flex items-center gap-2 text-sm text-[var(--color-muted)]">
        <input type="checkbox" checked={stateful} onChange={(e) => setStateful(e.target.checked)} />
        stateful (single instance, volume-backed)
      </label>
      <button
        className="btn btn-accent mt-3"
        disabled={!project || !name}
        onClick={() => onSubmit({ action: "create_service", project, name, stateful }, "create service")}
      >
        Create service
      </button>
    </Card>
  );
}

function EnvironmentServiceForm({ onSubmit }: { onSubmit: Submit }) {
  const s = useStore();
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
    <Card title="Bind service to environment" subtitle="Creates the environment_service link with its source.">
      <div className="grid grid-cols-3 gap-3">
        <Field label="project">
          <ProjectSelect value={project} onChange={setProject} />
        </Field>
        <Field label="environment">
          <select className="input mono" value={environmentId} onChange={(e) => setEnvironmentId(e.target.value)}>
            <option value="">select…</option>
            {envs.map((e) => (
              <option key={e.id} value={e.id}>{e.name}</option>
            ))}
          </select>
        </Field>
        <Field label="service">
          <select className="input mono" value={serviceId} onChange={(e) => setServiceId(e.target.value)}>
            <option value="">select…</option>
            {services.map((sv) => (
              <option key={sv.id} value={sv.id}>{sv.name}</option>
            ))}
          </select>
        </Field>
      </div>
      <div className="mt-3">
        <Field label="source (jsonb)">
          <input className="input mono" value={source} onChange={(e) => setSource(e.target.value)} />
        </Field>
      </div>
      <button
        className="btn btn-accent mt-3"
        disabled={!environmentId || !serviceId}
        onClick={() => onSubmit({ action: "create_environment_service", environmentId, serviceId, source }, "bind service")}
      >
        Bind
      </button>
    </Card>
  );
}

const MB = 1024 * 1024;

function DeploymentForm({
  flat,
  regions,
  onSubmit,
}: {
  flat: FlatEs[];
  regions: string[];
  onSubmit: Submit;
}) {
  const [esId, setEsId] = useState("");
  const [imageRef, setImageRef] = useState("nginx:1.27");
  const [cpu, setCpu] = useState(500);
  const [memMb, setMemMb] = useState(512);
  const [drain, setDrain] = useState(30);
  const [restartMax, setRestartMax] = useState(5);
  const [deadline, setDeadline] = useState(600);
  const [msg, setMsg] = useState("");
  const [counts, setCounts] = useState<Record<string, number>>({});

  const s = useStore();

  return (
    <Card
      title="Create deployment"
      subtitle="Commits a new current version (pending) for a bound service, mirroring `conductor up`: supersedes the prior current, bumps version, sets per-region replicas."
    >
      <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
        <Field label="service (env · project)">
          <select className="input mono" value={esId} onChange={(e) => setEsId(e.target.value)}>
            <option value="">select a bound service…</option>
            {flat.map((f) => (
              <option key={f.esId} value={f.esId}>
                {f.project}/{f.environment}/{f.service}
                {f.version ? ` (current v${f.version})` : " (never deployed)"}
              </option>
            ))}
          </select>
        </Field>
        <Field label="image_ref">
          <input className="input mono" value={imageRef} onChange={(e) => setImageRef(e.target.value)} />
        </Field>
      </div>

      <div className="mt-3 grid grid-cols-2 gap-3 md:grid-cols-5">
        <Field label="cpu (millicores)">
          <input type="number" className="input mono" value={cpu} onChange={(e) => setCpu(+e.target.value)} />
        </Field>
        <Field label="mem (MB)">
          <input type="number" className="input mono" value={memMb} onChange={(e) => setMemMb(+e.target.value)} />
        </Field>
        <Field label="drain (s)">
          <input type="number" className="input mono" value={drain} onChange={(e) => setDrain(+e.target.value)} />
        </Field>
        <Field label="restart_max">
          <input type="number" className="input mono" value={restartMax} onChange={(e) => setRestartMax(+e.target.value)} />
        </Field>
        <Field label="progress_deadline (s)">
          <input type="number" className="input mono" value={deadline} onChange={(e) => setDeadline(+e.target.value)} />
        </Field>
      </div>

      <div className="mt-3">
        <Field label="commit_message">
          <input className="input" value={msg} onChange={(e) => setMsg(e.target.value)} placeholder="deploy via chaos-ui" />
        </Field>
      </div>

      <RegionCounts regions={regions} counts={counts} setCounts={setCounts} />

      <button
        className="btn btn-accent mt-4"
        disabled={!esId || !imageRef}
        onClick={() =>
          onSubmit(
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
            "create deployment",
          )
        }
      >
        Deploy
      </button>
    </Card>
  );
}

function ScaleForm({
  flat,
  regions,
  onSubmit,
}: {
  flat: FlatEs[];
  regions: string[];
  onSubmit: Submit;
}) {
  const [esId, setEsId] = useState("");
  const [counts, setCounts] = useState<Record<string, number>>({});
  const deployable = flat.filter((f) => f.deploymentId);
  const selected = deployable.find((f) => f.esId === esId);

  useEffect(() => {
    if (!selected) return;
    const next: Record<string, number> = {};
    for (const r of selected.regions) next[r.region] = r.desired;
    setCounts(next);
  }, [esId]); // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <Card
      title="Scale current deployment"
      subtitle="Patches deployment_regions replica counts on the active deployment (mirrors `conductor scale`)."
    >
      <Field label="service">
        <select className="input mono" value={esId} onChange={(e) => setEsId(e.target.value)}>
          <option value="">select a deployed service…</option>
          {deployable.map((f) => (
            <option key={f.esId} value={f.esId}>
              {f.project}/{f.environment}/{f.service} (v{f.version})
            </option>
          ))}
        </select>
      </Field>

      <RegionCounts regions={regions} counts={counts} setCounts={setCounts} />

      <button
        className="btn btn-accent mt-4"
        disabled={!selected}
        onClick={() =>
          onSubmit(
            {
              action: "scale",
              deploymentId: selected!.deploymentId,
              regions: Object.entries(counts).map(([region, replicas]) => ({
                region,
                replicas,
              })),
            },
            "scale",
          )
        }
      >
        Apply scale
      </button>
    </Card>
  );
}

function RegionCounts({
  regions,
  counts,
  setCounts,
}: {
  regions: string[];
  counts: Record<string, number>;
  setCounts: (c: Record<string, number>) => void;
}) {
  return (
    <div className="mt-4">
      <span className="label">replicas per region</span>
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
        {regions.map((r) => (
          <label key={r} className="flex items-center gap-2">
            <span className="mono w-24 shrink-0 text-xs text-[var(--color-muted)]">{r}</span>
            <input
              type="number"
              min={0}
              className="input mono"
              value={counts[r] ?? 0}
              onChange={(e) => setCounts({ ...counts, [r]: Math.max(0, +e.target.value) })}
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
    <select className="input mono" value={value} onChange={(e) => onChange(e.target.value)}>
      <option value="">select…</option>
      {s.meta.projects.map((p) => (
        <option key={p.name} value={p.name}>{p.name}</option>
      ))}
    </select>
  );
}
