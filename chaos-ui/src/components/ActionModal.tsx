"use client";

import { useEffect, useState } from "react";

export interface ActionDef {
  key: string;
  name: string;
  description: string;
  danger?: boolean;
}

export interface ActionTarget {
  id: string;
  title: string;
  subtitle: string;
  actions: ActionDef[];
}

export function ActionModal({
  target,
  onClose,
  onRun,
}: {
  target: ActionTarget | null;
  onClose: () => void;
  onRun: (action: ActionDef, targetId: string) => Promise<void>;
}) {
  const [selected, setSelected] = useState<string | null>(null);
  const [running, setRunning] = useState(false);

  // Reset the picked action whenever the modal re-opens for a new target, so a
  // dangerous choice never carries over between rows.
  useEffect(() => {
    setSelected(null);
    setRunning(false);
  }, [target?.id]);

  useEffect(() => {
    if (!target) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [target, onClose]);

  if (!target) return null;

  const chosen = target.actions.find((a) => a.key === selected) ?? null;

  const run = async () => {
    if (!chosen || running) return;
    setRunning(true);
    try {
      await onRun(chosen, target.id);
      onClose();
    } finally {
      setRunning(false);
    }
  };

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4 backdrop-blur-sm"
      onClick={onClose}
    >
      <div
        className="panel w-full max-w-md overflow-hidden shadow-2xl shadow-black/50"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="border-b border-[var(--color-border-soft)] bg-[var(--color-panel-2)] px-5 py-3.5">
          <div className="flex items-start justify-between gap-3">
            <div>
              <h3 className="text-sm font-semibold">{target.title}</h3>
              <p className="mono mt-0.5 text-xs text-[var(--color-faint)]">
                {target.subtitle}
              </p>
            </div>
            <button
              onClick={onClose}
              className="rounded-md px-2 py-0.5 text-[var(--color-faint)] transition-colors hover:bg-white/5 hover:text-[var(--color-fg)]"
              aria-label="Close"
            >
              ✕
            </button>
          </div>
        </div>

        <div className="max-h-[55vh] space-y-1.5 overflow-y-auto p-3">
          {target.actions.map((a) => {
            const active = a.key === selected;
            return (
              <button
                key={a.key}
                onClick={() => setSelected(a.key)}
                className={`w-full rounded-lg border px-3.5 py-2.5 text-left transition-all ${
                  active
                    ? a.danger
                      ? "border-red-500/50 bg-red-500/10"
                      : "border-[var(--color-accent)]/60 bg-[rgba(133,59,206,0.12)]"
                    : "border-[var(--color-border-soft)] bg-[var(--color-panel-2)] hover:border-[var(--color-border)]"
                }`}
              >
                <div className="flex items-center gap-2">
                  <span
                    className={`h-3 w-3 shrink-0 rounded-full border ${
                      active
                        ? a.danger
                          ? "border-red-400 bg-red-400"
                          : "border-[var(--color-accent-fg)] bg-[var(--color-accent)]"
                        : "border-[var(--color-faint)]"
                    }`}
                  />
                  <span
                    className={`text-sm font-medium ${
                      a.danger ? "text-red-300" : "text-[var(--color-fg)]"
                    }`}
                  >
                    {a.name}
                  </span>
                </div>
                <p className="mt-1 pl-5 text-xs leading-relaxed text-[var(--color-muted)]">
                  {a.description}
                </p>
              </button>
            );
          })}
        </div>

        <div className="flex items-center justify-end gap-2 border-t border-[var(--color-border-soft)] bg-[var(--color-panel-2)] px-4 py-3">
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button
            className={chosen?.danger ? "btn btn-danger" : "btn btn-accent"}
            disabled={!chosen || running}
            onClick={run}
          >
            {running ? "Running…" : chosen ? `Run: ${chosen.name}` : "Select an action"}
          </button>
        </div>
      </div>
    </div>
  );
}
