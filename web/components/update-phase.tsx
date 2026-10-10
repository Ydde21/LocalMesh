"use client";

import type { AgentUpdate } from "@/lib/types";
import { fmtAgo, fmtDur } from "@/lib/format";
import { StatusBadge } from "@/components/status";

// Agent-side progress order reported by update ops (v0.7+). The
// pre-flight phases (pending/draining/dispatched) happen server-side
// before the agent starts; the terminal phases replace the final step.
const STEPS: { key: string; label: string }[] = [
  { key: "verifying", label: "verify" },
  { key: "downloading", label: "download" },
  { key: "staging", label: "stage" },
  { key: "installing", label: "install" },
  { key: "restarting", label: "restart" },
  { key: "verify_health", label: "health" },
  { key: "succeeded", label: "done" },
];

const TERMINAL_FAIL = new Set(["failed", "rolled_back", "cancelled", "interrupted"]);

// Index into STEPS: -1 = pre-flight (queued/draining/dispatched), -2 =
// terminal failure/abort (we don't know which step it stopped at).
function stepIndex(phase: string): number {
  switch (phase) {
    case "pending":
    case "draining":
    case "dispatched":
      return -1;
    case "succeeded":
      return STEPS.length - 1;
    default:
      if (TERMINAL_FAIL.has(phase)) return -2;
      return STEPS.findIndex((s) => s.key === phase);
  }
}

// Compact stepper for one agent_update row: dots for the agent-side
// phases, a status badge + detail line, and the error verbatim on
// failure/rollback.
export function UpdatePhaseStepper({ update }: { update: AgentUpdate }) {
  const idx = stepIndex(update.phase);
  const doneAll = update.phase === "succeeded";
  const failed = idx === -2;
  // Wall-clock only makes sense once the update has finished.
  const duration = update.finished_at
    ? fmtDur(update.dispatched_at || update.created_at, update.finished_at)
    : null;

  return (
    <div>
      <div className="flex items-start">
        {STEPS.map((s, i) => {
          const reached = doneAll || (idx >= 0 && i < idx);
          const current = idx >= 0 && i === idx && !doneAll;
          const successEnd = doneAll && i === STEPS.length - 1;
          const failEnd = failed && i === STEPS.length - 1;
          const dotColor = successEnd
            ? "var(--color-online)"
            : failEnd
              ? "var(--color-danger)"
              : reached || current
                ? "var(--color-accent)"
                : "var(--color-border-strong)";
          return (
            <div key={s.key} className="flex min-w-0 flex-1 flex-col items-center">
              <div className="flex w-full items-center">
                <div
                  className="h-px flex-1"
                  style={{
                    background:
                      i === 0
                        ? "transparent"
                        : reached || current || doneAll
                          ? "var(--color-accent)"
                          : "var(--color-border)",
                  }}
                />
                <div
                  className={`h-2 w-2 shrink-0 rounded-full ${current ? "pulse" : ""}`}
                  style={{
                    background: dotColor,
                    border: `1px solid ${dotColor}`,
                  }}
                  title={s.key}
                />
                <div
                  className="h-px flex-1"
                  style={{
                    background:
                      i === STEPS.length - 1
                        ? "transparent"
                        : doneAll || (idx >= 0 && i < idx)
                          ? "var(--color-accent)"
                          : "var(--color-border)",
                  }}
                />
              </div>
              <span
                className={`mt-1 whitespace-nowrap text-[9px] uppercase tracking-wide ${
                  failEnd
                    ? "text-[var(--color-danger)]"
                    : current
                      ? "text-[var(--color-accent)]"
                      : reached || doneAll
                        ? "text-[var(--color-text-dim)]"
                        : "text-[var(--color-text-faint)]"
                }`}
              >
                {failEnd ? update.phase.replace(/_/g, " ") : s.label}
              </span>
            </div>
          );
        })}
      </div>

      <div className="mt-2 flex flex-wrap items-center gap-2">
        <StatusBadge status={update.phase} />
        {update.attempt > 0 && (
          <span className="badge badge-warn !px-1.5 !py-0">retry {update.attempt}</span>
        )}
        {(update.from_version || update.to_version) && (
          <span className="mono text-[10.5px] text-[var(--color-text-faint)]">
            {update.from_version || "?"} → {update.to_version || update.release_version || "?"}
          </span>
        )}
        <span className="ml-auto text-[10.5px] text-[var(--color-text-faint)]">
          {duration ? `${duration} · ` : ""}
          {fmtAgo(update.finished_at || update.updated_at || update.created_at)}
        </span>
      </div>
      {update.detail && idx !== -2 && (
        <div className="mt-1 truncate text-[11px] text-[var(--color-text-faint)]" title={update.detail}>
          {update.detail}
        </div>
      )}
      {update.error && (
        <div className="mt-1.5 text-[11.5px] leading-snug text-[var(--color-danger)]">
          {update.error}
        </div>
      )}
    </div>
  );
}
