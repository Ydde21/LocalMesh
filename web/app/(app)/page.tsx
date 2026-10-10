"use client";

import Link from "next/link";
import { useOverview, useNodes, useWorkloads, useModelOps } from "@/lib/api";
import { fmtBytes, fmtAgo, osLabel } from "@/lib/format";
import { Panel, Stat, Meter, Empty } from "@/components/ui";
import { NodeBadge } from "@/components/status";
import { ActivityFeed } from "@/components/activity-feed";
import { MetricsChart } from "@/components/metrics-chart";
import { ArrowRight } from "lucide-react";

export default function OverviewPage() {
  const { data: ov } = useOverview();
  const { data: nodes } = useNodes();
  const { data: activeWls } = useWorkloads("running");
  const { data: ops } = useModelOps();

  const memPct = ov && ov.memory_bytes > 0 ? (ov.memory_used / ov.memory_bytes) * 100 : 0;
  const activeOps = (ops ?? []).filter(
    (o) => o.status === "dispatched" || o.status === "running" || o.status === "pending"
  ).length;

  return (
    <div className="mx-auto max-w-[1200px] px-6 py-6">
      <header className="mb-6 flex items-end justify-between">
        <div>
          <h1 className="text-[19px] font-semibold tracking-tight">Cluster overview</h1>
          <p className="mt-0.5 text-[12.5px] text-[var(--color-text-dim)]">
            Your hardware. One AI network.
          </p>
        </div>
      </header>

      {/* Top stats */}
      <div className={`grid grid-cols-2 gap-3 ${activeOps > 0 ? "md:grid-cols-5" : "md:grid-cols-4"}`}>
        <Panel className="fade-up px-4 py-4">
          <Stat
            label="Devices online"
            value={
              <>
                {ov?.nodes_online ?? "—"}
                <span className="text-[14px] text-[var(--color-text-faint)]">
                  {" "}/ {ov?.nodes_total ?? "—"}
                </span>
              </>
            }
            sub="connected to cluster"
          />
        </Panel>
        <Panel className="fade-up px-4 py-4" >
          <Stat label="Active requests" value={ov?.queued_or_active ?? "—"} sub="queued or running" />
        </Panel>
        <Panel className="fade-up px-4 py-4">
          <Stat label="Models installed" value={ov?.models_distinct ?? "—"} sub="distinct across cluster" />
        </Panel>
        <Panel className="fade-up px-4 py-4">
          <Stat
            label="Memory pool"
            value={fmtBytes(ov?.memory_bytes)}
            sub={`${fmtBytes(ov?.memory_used)} in use`}
          />
          <Meter className="mt-2" value={memPct} />
        </Panel>
        {activeOps > 0 && (
          <Panel className="fade-up px-4 py-4">
            <Stat
              label="Model ops"
              value={activeOps}
              sub="pulls/deletes in progress"
              accent="var(--color-idle)"
            />
          </Panel>
        )}
      </div>

      {/* Live utilization */}
      <Panel className="fade-up mt-3">
        <div className="flex items-center justify-between border-b px-4 py-3" style={{ borderColor: "var(--color-border)" }}>
          <div className="label">Cluster utilization</div>
          <div className="flex items-center gap-4 text-[10.5px] text-[var(--color-text-faint)]">
            <Legend swatch="#4f7cff" label="CPU" />
            <Legend swatch="#34d399" label="Memory" />
            <Legend swatch="#a78bfa" label="Active requests" />
          </div>
        </div>
        <div className="px-3 py-3">
          <MetricsChart />
        </div>
      </Panel>

      {/* Mid row: compute + activity */}
      <div className="mt-3 grid grid-cols-1 gap-3 lg:grid-cols-3">
        <Panel className="fade-up col-span-1 px-4 py-4 lg:col-span-1">
          <div className="label mb-3">Compute</div>
          <div className="space-y-3">
            <Row label="CPU cores" value={String(ov?.cpu_cores_total ?? 0)} />
            <Row label="GPUs" value={String(ov?.gpu_count ?? 0)} />
            <Row label="VRAM (dedicated)" value={fmtBytes(ov?.vram_bytes)} />
            <Row label="Inference slots busy" value={String(ov?.active_requests ?? 0)} />
          </div>
        </Panel>

        {/* Running workloads */}
        <Panel className="fade-up lg:col-span-1">
          <div className="flex items-center justify-between border-b px-4 py-3" style={{ borderColor: "var(--color-border)" }}>
            <div className="label">In flight</div>
            <Link href="/workloads" className="text-[11px] text-[var(--color-accent)] hover:underline">
              View all →
            </Link>
          </div>
          <div className="max-h-[300px] overflow-y-auto">
            {(!activeWls || activeWls.length === 0) && (
              <div className="px-4 py-8 text-center text-[12px] text-[var(--color-text-faint)]">
                No requests currently executing
              </div>
            )}
            {activeWls?.map((w) => (
              <Link
                key={w.id}
                href={`/workloads?id=${w.id}`}
                className="row-line flex items-center gap-3 px-4 py-2.5 transition-colors hover:bg-white/[0.03]"
              >
                <span className="mono text-[12px] text-[var(--color-text)]">{w.model}</span>
                <span className="text-[11px] text-[var(--color-text-faint)]">→ {w.node_name || "…"}</span>
                <ArrowRight size={11} className="ml-auto text-[var(--color-text-faint)]" />
              </Link>
            ))}
          </div>
        </Panel>

        <div className="fade-up lg:col-span-1">
          <ActivityFeed />
        </div>
      </div>

      {/* Nodes table */}
      <Panel className="fade-up mt-3">
        <div className="flex items-center justify-between border-b px-4 py-3" style={{ borderColor: "var(--color-border)" }}>
          <div className="label">Devices</div>
          <Link href="/devices" className="text-[11px] text-[var(--color-accent)] hover:underline">
            Manage →
          </Link>
        </div>
        {(!nodes || nodes.length === 0) ? (
          <Empty
            title="No devices yet"
            hint="Generate an enrollment token in Settings, then run `localmesh-agent join` on another machine."
          >
            <Link href="/settings" className="btn btn-primary btn-sm">Create enrollment token</Link>
          </Empty>
        ) : (
          <div className="divide-y" style={{ borderColor: "var(--color-border)" }}>
            {nodes.map((n) => (
              <Link
                key={n.id}
                href={`/devices?id=${n.id}`}
                className="row-line flex items-center gap-4 px-4 py-3 transition-colors hover:bg-white/[0.03]"
                style={{ borderColor: "var(--color-border)" }}
              >
                <div className="min-w-[140px]">
                  <div className="text-[13px] font-medium">{n.name}</div>
                  <div className="text-[11px] text-[var(--color-text-faint)]">
                    {osLabel(n.hardware.os)} · {n.hardware.arch}
                  </div>
                </div>
                <NodeBadge node={n} />
                <div className="hidden text-[11.5px] text-[var(--color-text-dim)] md:block">
                  {n.hardware.cpu_model} · {n.hardware.cpu_cores} cores
                  {(n.hardware.gpus?.length ?? 0) > 0 && ` · ${n.hardware.gpus!.length} GPU`}
                </div>
                <div className="ml-auto text-right">
                  <div className="mono text-[11.5px] text-[var(--color-text-dim)]">
                    {(n.models ?? []).length} models · {n.in_flight}/{n.max_concurrent} slots
                  </div>
                  <div className="text-[10.5px] text-[var(--color-text-faint)]">
                    {n.online ? `seen ${fmtAgo(n.last_seen_at)}` : "offline"}
                  </div>
                </div>
              </Link>
            ))}
          </div>
        )}
      </Panel>
    </div>
  );
}

function Legend({ swatch, label }: { swatch: string; label: string }) {
  return (
    <span className="flex items-center gap-1.5">
      <span className="h-[7px] w-[7px] rounded-[2px]" style={{ background: swatch }} />
      {label}
    </span>
  );
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-baseline justify-between">
      <span className="text-[12px] text-[var(--color-text-dim)]">{label}</span>
      <span className="mono text-[13px] font-medium">{value}</span>
    </div>
  );
}
