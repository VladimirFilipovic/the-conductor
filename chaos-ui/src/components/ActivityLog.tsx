"use client";

import { useStore } from "@/lib/store";
import { relativeTime } from "@/lib/ui";

// Session-local timeline of everything fired from this UI — chaos injections
// and desired-state mutations alike.
export function ActivityLog() {
  const s = useStore();
  return (
    <div className="panel flex max-h-[480px] flex-col overflow-hidden">
      <div className="flex items-center justify-between border-b border-[var(--color-border-soft)] px-4 py-2.5">
        <h3 className="text-sm font-semibold">Activity</h3>
        <button
          className="chip-btn"
          onClick={s.clearChaos}
          disabled={s.chaosLog.length === 0}
        >
          Clear
        </button>
      </div>
      <div className="flex-1 overflow-y-auto">
        {s.chaosLog.length === 0 ? (
          <div className="px-4 py-5 text-xs text-[var(--color-faint)]">
            no actions this session
          </div>
        ) : (
          s.chaosLog.map((e) => (
            <div
              key={e.id}
              className="border-b border-[var(--color-border-soft)] px-4 py-2"
            >
              <div className="flex items-center gap-2">
                <span
                  className={`h-1.5 w-1.5 rounded-full ${e.ok ? "bg-emerald-500" : "bg-red-500"}`}
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
            </div>
          ))
        )}
      </div>
    </div>
  );
}
