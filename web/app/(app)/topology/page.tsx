"use client";

import { useNodes, useWorkloads } from "@/lib/api";
import { TopologyGraph } from "@/components/topology-graph";
import { Panel } from "@/components/ui";
import { fmtAgo } from "@/lib/format";
import { StatusBadge } from "@/components/status";

export default function TopologyPage() {
  const { data: nodes } = useNodes();
  const { data: running } = useWorkloads("running");
  const { data: queued } = useWorkloads("queued");

  const active = [...(running ?? []), ...(queued ?? [])];

  return (
    <div className="mx-auto max-w-[1200px] px-6 py-6">
      <header className="mb-6">
        <h1 className="text-[19px] font-semibold tracking-tight">Network topology</h1>
        <p className="mt-0.5 text-[12.5px] text-[var(--color-text-dim)]">
          Live view of connected devices and routed workloads
        </p>
      </header>

      <TopologyGraph nodes={nodes ?? []} workloads={active} />

      <div className="mt-3 grid grid-cols-1 gap-3 md:grid-cols-2">
        <Panel>
          <div className="border-b px-4 py-3" style={{ borderColor: "var(--color-border)" }}>
            <div className="label">Active routes</div>
          </div>
          <div className="max-h-[200px] overflow-y-auto">
            {active.length === 0 && (
              <div className="px-4 py-6 text-center text-[12px] text-[var(--color-text-faint)]">
                No requests in flight
              </div>
            )}
            {active.map((w) => (
              <div key={w.id} className="row-line flex items-center gap-3 px-4 py-2.5" style={{ borderColor: "var(--color-border)" }}>
                <span className="mono text-[12px]">{w.model}</span>
                <span className="text-[11px] text-[var(--color-text-faint)]">→ {w.node_name || "pending"}</span>
                <StatusBadge status={w.status} />
                <span className="ml-auto text-[10.5px] text-[var(--color-text-faint)]">{fmtAgo(w.queued_at)}</span>
              </div>
            ))}
          </div>
        </Panel>

        <Panel>
          <div className="border-b px-4 py-3" style={{ borderColor: "var(--color-border)" }}>
            <div className="label">Legend</div>
          </div>
          <div className="space-y-2.5 px-4 py-4 text-[12px]">
            <Legend color="var(--color-online)" label="Online, idle" />
            <Legend color="var(--color-idle)" label="Online, executing work" pulse />
            <Legend color="#3a3a44" label="Offline / unreachable" dashed />
            <div className="pt-1 text-[11px] leading-relaxed text-[var(--color-text-faint)]">
              Nodes connect outbound to the control plane over an encrypted
              channel — no inbound ports required. Work through Tailscale,
              Headscale, or LANs transparently.
            </div>
          </div>
        </Panel>
      </div>
    </div>
  );
}

function Legend({ color, label, pulse, dashed }: { color: string; label: string; pulse?: boolean; dashed?: boolean }) {
  return (
    <div className="flex items-center gap-2.5">
      <svg width="34" height="2">
        <line x1="0" y1="1" x2="34" y2="1" stroke={color} strokeWidth="1.5" strokeDasharray={dashed ? "4 3" : undefined} className={pulse ? "pulse" : ""} />
      </svg>
      <span className="text-[var(--color-text-dim)]">{label}</span>
    </div>
  );
}
