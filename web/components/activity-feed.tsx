"use client";

import { useEvents } from "@/components/shell";
import { fmtAgo } from "@/lib/format";
import { Panel } from "@/components/ui";

const LABELS: Record<string, string> = {
  "node.connected": "Node connected",
  "node.disconnected": "Node disconnected",
  "node.capability": "Capability report",
  "node.approved": "Node approved",
  "node.revoked": "Node revoked",
  "node.deleted": "Node removed",
  "node.updated": "Node updated",
  "workload.queued": "Request queued",
  "workload.started": "Request started",
  "workload.finished": "Request finished",
  "model.operation": "Model operation",
  "enrollment.requested": "Enrollment requested",
  "enrollment.denied": "Enrollment denied",
  "enrollment_token.created": "Enroll token created",
  "api_key.created": "API key created",
  "api_key.revoked": "API key revoked",
  "settings.updated": "Settings updated",
  "node.maintenance": "Maintenance changed",
  "agent.update": "Agent update",
  "rollout": "Rollout",
  "release.registered": "Release registered",
  "release.revoked": "Release revoked",
  "release_key.registered": "Signing key registered",
  "release_key.revoked": "Signing key revoked",
  "control-plane": "Control-plane event",
};

export function ActivityFeed() {
  const { events } = useEvents();
  const shown = events
    .filter((e) => e.type !== "node.metrics")
    .slice(0, 18);

  return (
    <Panel className="flex flex-col">
      <div className="flex items-center justify-between border-b px-4 py-3" style={{ borderColor: "var(--color-border)" }}>
        <div className="label">Recent activity</div>
      </div>
      <div className="flex-1 overflow-y-auto" style={{ maxHeight: 420 }}>
        {shown.length === 0 && (
          <div className="px-4 py-8 text-center text-[12px] text-[var(--color-text-faint)]">
            Waiting for cluster events…
          </div>
        )}
        {shown.map((e, i) => (
          <div key={i} className="row-line flex items-baseline gap-3 px-4 py-2">
            <span className="text-[12px] font-medium text-[var(--color-text)]">
              {LABELS[e.type] || e.type}
            </span>
            <span className="mono truncate text-[11px] text-[var(--color-text-faint)]">
              {summarize(e.data)}
            </span>
            <span className="ml-auto shrink-0 text-[10px] text-[var(--color-text-faint)]">
              {fmtAgo(new Date(e.at).toISOString())}
            </span>
          </div>
        ))}
      </div>
    </Panel>
  );
}

function summarize(data: unknown): string {
  if (data && typeof data === "object") {
    const d = data as Record<string, unknown>;
    if (d.name) return String(d.name);
    if (d.model) return String(d.model);
    if (d.id) return String(d.id).slice(0, 8);
    if (d.node_id) return String(d.node_id).slice(0, 8);
  }
  return "";
}
