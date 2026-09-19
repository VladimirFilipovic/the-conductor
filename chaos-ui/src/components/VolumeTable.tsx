"use client";

import { useState } from "react";
import { useStore } from "@/lib/store";
import { postJson } from "@/lib/http";
import { Badge } from "@/components/Badge";
import { ActionMenu, type MenuItem } from "@/components/ActionMenu";
import { volumeStatusClass, formatGiB, GiB } from "@/lib/ui";
import type { ServiceNode, ServiceTarget, VolumeRow } from "@/lib/api";

// Volumes of a stateful service, under its replica table. Two lanes, as for
// replicas: grow and revert are operator intent and go to the control plane
// (the project guards answer 409 with their own words); stall and heal are
// agent-observable and go through agentsim, so the engine sees a disk that
// never grows over the real transport.

interface VolumeAct {
  key: string;
  label: string;
  hint: string;
  danger?: boolean;
}

const GROW_ACT: VolumeAct = {
  key: "resize_volume",
  label: "Grow…",
  hint: "Raise desired size; engine approves next tick if the host has room, else parks it as resize_pending.",
};

const REVERT_ACT: VolumeAct = {
  key: "revert_volume",
  label: "Revert",
  hint: "Take back a parked grow; desired returns to the previous size, attached next tick.",
};

const STALL_ACT: VolumeAct = {
  key: "volume_stall_resize",
  label: "Stall resize",
  danger: true,
  hint: "Agent accepts the next grow but the disk never changes; volume stays resizing.",
};

const HEAL_ACT: VolumeAct = {
  key: "volume_heal",
  label: "Heal",
  hint: "Clears chaos mode; a stalled grow lands and the engine settles the volume.",
};

// A grow may be requested only where the project layer would accept it: an
// unplaced volume, or an attached one with nothing outstanding (observed equals
// desired, or the agent has never reported). resize_pending is the one state
// revert applies to. Stall is offered only where it will bite the *next* grow,
// so the operator sees a clean attached → resizing → stuck sequence.
function volumeActs(v: VolumeRow): VolumeAct[] {
  const converged =
    v.observed_size_bytes === null || v.observed_size_bytes === v.desired_size_bytes;
  const acts: VolumeAct[] = [];
  if ((v.status === "pending" || v.status === "attached") && converged) acts.push(GROW_ACT);
  if (v.status === "resize_pending") acts.push(REVERT_ACT);
  if (v.status === "attached" && converged) acts.push(STALL_ACT);
  acts.push(HEAL_ACT);
  return acts;
}

export function VolumeTable({
  svc,
  target,
}: {
  svc: ServiceNode;
  target: ServiceTarget;
}) {
  const s = useStore();
  const [growing, setGrowing] = useState<string | null>(null);
  // One-line advisory per mount after a grow the host could not hold; cleared
  // by the next grow or revert on that mount.
  const [hints, setHints] = useState<Record<string, string>>({});
  const setHint = (mount: string, hint: string | null) =>
    setHints((h) => {
      const next = { ...h };
      if (hint) next[mount] = hint;
      else delete next[mount];
      return next;
    });

  const fire = async (act: VolumeAct, v: VolumeRow) => {
    if (act === GROW_ACT) {
      setGrowing(growing === v.mount_path ? null : v.mount_path);
      return;
    }
    const { ok, data } = await postJson("/api/chaos", {
      action: act.key,
      id: v.id,
      target,
      mount_path: v.mount_path,
    });
    const what =
      act === REVERT_ACT
        ? `revert ${v.mount_path} → ${formatGiB(v.previous_desired_size_bytes ?? v.desired_size_bytes)}`
        : `${act.label.toLowerCase()} ${v.mount_path}`;
    s.logChaos(act.key, ok ? what : `${what} failed: ${data.error ?? "unknown"}`, ok);
    if (ok && act === REVERT_ACT) setHint(v.mount_path, null);
  };

  const grow = async (v: VolumeRow, gib: number): Promise<boolean> => {
    const { ok, data } = await postJson("/api/volumes/resize", {
      ...target,
      mount_path: v.mount_path,
      size_bytes: gib * GiB,
    });
    const what = `resize ${v.mount_path} → ${gib}GiB`;
    if (!ok) {
      s.logChaos(GROW_ACT.key, `${what} failed: ${data.error ?? "unknown"}`, false);
      return false;
    }
    const hint = data.waiting_for_space
      ? `host short ${formatGiB(Number(data.shortfall_bytes ?? 0))} — parked as resize_pending`
      : null;
    setHint(v.mount_path, hint);
    s.logChaos(GROW_ACT.key, hint ? `${what} (${hint})` : what, true);
    return true;
  };

  const items = (v: VolumeRow): MenuItem[] =>
    volumeActs(v).map((a) => ({
      key: a.key,
      label: a.label,
      hint: a.hint,
      danger: a.danger,
      onSelect: () => fire(a, v),
    }));

  return (
    <div className="mt-2.5 overflow-x-auto">
      <table className="w-full border-collapse text-left text-xs">
        <thead className="text-[0.65rem] uppercase tracking-wide text-[var(--color-faint)]">
          <tr>
            <th className="py-1 pr-3 font-medium">mount</th>
            <th className="py-1 pr-3 font-medium">host</th>
            <th className="py-1 pr-3 font-medium">size</th>
            <th className="py-1 pr-3 font-medium">on disk</th>
            <th className="py-1 pr-3 font-medium">status</th>
            <th className="w-8" />
          </tr>
        </thead>
        <tbody className="text-[var(--color-muted)]">
          {svc.volumes.map((v) => (
            <VolumeRowView
              key={v.id}
              v={v}
              hint={hints[v.mount_path]}
              items={items(v)}
              growing={growing === v.mount_path}
              onGrow={(gib) => grow(v, gib)}
              onClose={() => setGrowing(null)}
            />
          ))}
        </tbody>
      </table>
    </div>
  );
}

function VolumeRowView({
  v,
  hint,
  items,
  growing,
  onGrow,
  onClose,
}: {
  v: VolumeRow;
  hint?: string;
  items: MenuItem[];
  growing: boolean;
  onGrow: (gib: number) => Promise<boolean>;
  onClose: () => void;
}) {
  // Desired ahead of what is on disk: a grow is requested, approved or parked.
  const drifting =
    v.observed_size_bytes !== null && v.observed_size_bytes < v.desired_size_bytes;
  return (
    <>
      <tr className="border-t border-[var(--color-border-soft)]">
        <td className="mono py-1.5 pr-3 text-[var(--color-fg)]">{v.mount_path}</td>
        <td className="mono py-1.5 pr-3">
          {v.hostname ?? "unplaced"}
          <span className="text-[var(--color-faint)]"> · {v.region}</span>
        </td>
        <td className="mono py-1.5 pr-3 whitespace-nowrap">
          {formatGiB(v.desired_size_bytes)}
          {v.previous_desired_size_bytes !== null && (
            <span
              className="text-[var(--color-faint)]"
              title="previous desired size — what revert restores"
            >
              {" "}
              ← {formatGiB(v.previous_desired_size_bytes)}
            </span>
          )}
        </td>
        <td
          className={`mono py-1.5 pr-3 whitespace-nowrap ${drifting ? "text-amber-700" : ""}`}
        >
          {v.observed_size_bytes === null ? "—" : formatGiB(v.observed_size_bytes)}
        </td>
        <td className="py-1.5 pr-3">
          <span className="flex items-center gap-1.5">
            <Badge className={volumeStatusClass(v.status)}>{v.status}</Badge>
            {hint && <span className="text-amber-700">{hint}</span>}
          </span>
        </td>
        <td className="w-8 py-1 text-right">
          <ActionMenu items={items} />
        </td>
      </tr>
      {growing && (
        <tr>
          <td colSpan={6} className="pb-2">
            <GrowForm v={v} onGrow={onGrow} onClose={onClose} />
          </td>
        </tr>
      )}
    </>
  );
}

// Inline, one field: the size in GiB (the CLI's unit); bytes go on the wire.
// Grow-only and "one grow in flight" are the project layer's to refuse — a
// rejection shows up in the activity log with the guard's own message.
function GrowForm({
  v,
  onGrow,
  onClose,
}: {
  v: VolumeRow;
  onGrow: (gib: number) => Promise<boolean>;
  onClose: () => void;
}) {
  const [gib, setGib] = useState(Math.ceil(v.desired_size_bytes / GiB) + 2);
  const [busy, setBusy] = useState(false);
  return (
    <div className="panel-soft mt-1 flex flex-wrap items-end gap-3 p-3">
      <label className="block w-28">
        <span className="label">size (GiB)</span>
        <input
          type="number"
          min={1}
          className="input mono"
          value={gib}
          onChange={(e) => setGib(Math.max(0, +e.target.value))}
        />
      </label>
      <span className="pb-2 text-xs text-[var(--color-faint)]">
        now {formatGiB(v.desired_size_bytes)}
      </span>
      <button
        className="btn btn-accent"
        disabled={busy || !Number.isFinite(gib) || gib <= 0}
        onClick={async () => {
          setBusy(true);
          const ok = await onGrow(gib);
          setBusy(false);
          if (ok) onClose();
        }}
      >
        Grow
      </button>
      <button className="btn" onClick={onClose}>
        Cancel
      </button>
    </div>
  );
}
