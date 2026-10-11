"use client";

import Link from "next/link";
import {
  useOverview, useNodes, useWorkloads, useModelOps, useControlPlane,
  useDiagnostics,
} from "@/lib/api";
import { fmtBytes, fmtAgo, fmtMs, fmtUptime, osLabel, fmtVersion, agentVersion } from "@/lib/format";
import { Panel, Stat, Meter, Empty, LoadingRows, ErrorState } from "@/components/ui";
import { NodeBadge } from "@/components/status";
import { ActivityFeed } from "@/components/activity-feed";
import { MetricsChart } from "@/components/metrics-chart";
import { OnboardingChecklist } from "@/components/onboarding";
import { ArrowRight } from "lucide-react";
import type { Node } from "@/lib/types";

export default function OverviewPage() {
  const { data: ov, error: ovErr, mutate: ovMutate } = useOverview();
  const { data: nodes, error: nodesErr, mutate: nodesMutate } = useNodes();
  const { data: activeWls, error: wlsErr, mutate: wlsMutate } = useWorkloads("running");
  const { data: ops } = useModelOps();

  const memPct = ov && ov.memory_bytes > 0 ? (ov.memory_used / ov.memory_bytes) * 100 : 0;
  // Prefer server-computed counts when the overview emits them; otherwise
  // reuse the shared ops cache — queued (waiting/backoff) vs in-flight.
  const allOps = ops ?? [];
  const activeOps =
    ov?.model_ops_active ??
    allOps.filter((o) => o.status === "dispatched" || o.status === "running").length;
  const queuedOps =
    ov?.model_ops_queued ??
    allOps.filter((o) => o.status === "queued" || o.status === "pending").length;
  const opsTotal = activeOps + queuedOps;

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

      {/* First-run setup checklist — self-hides once the cluster is live */}
      <OnboardingChecklist />

      {ovErr && (
        <Panel className="fade-up mb-3">
          <ErrorState error={ovErr} onRetry={() => ovMutate()} />
        </Panel>
      )}

      {/* Top stats */}
      <div className={`grid grid-cols-2 gap-3 ${opsTotal > 0 ? "md:grid-cols-5" : "md:grid-cols-4"}`}>
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
        {opsTotal > 0 && (
          <Panel className="fade-up px-4 py-4">
            <Stat
              label="Model ops"
              value={opsTotal}
              sub={`${activeOps} in flight · ${queuedOps} queued`}
              accent="var(--color-idle)"
            />
          </Panel>
        )}
      </div>

      {/* Fleet health: maintenance breakdown + agent-version spread */}
      {(nodes ?? []).length > 0 && <FleetHealthStrip nodes={nodes!} />}

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
        <div className="fade-up col-span-1 space-y-3 lg:col-span-1">
          <Panel className="px-4 py-4">
            <div className="label mb-3">Compute</div>
            <div className="space-y-3">
              <Row label="CPU cores" value={String(ov?.cpu_cores_total ?? 0)} />
              <Row label="GPUs" value={String(ov?.gpu_count ?? 0)} />
              <Row label="VRAM (dedicated)" value={fmtBytes(ov?.vram_bytes)} />
              <Row label="Inference slots busy" value={String(ov?.active_requests ?? 0)} />
            </div>
          </Panel>
          <ControlPlanePanel />
        </div>

        {/* Running workloads */}
        <Panel className="fade-up lg:col-span-1">
          <div className="flex items-center justify-between border-b px-4 py-3" style={{ borderColor: "var(--color-border)" }}>
            <div className="label">In flight</div>
            <Link href="/workloads" className="text-[11px] text-[var(--color-accent)] hover:underline">
              View all →
            </Link>
          </div>
          <div className="max-h-[300px] overflow-y-auto">
            {wlsErr ? (
              <ErrorState error={wlsErr} onRetry={() => wlsMutate()} />
            ) : !activeWls ? (
              <LoadingRows rows={2} />
            ) : activeWls.length === 0 ? (
              <div className="px-4 py-8 text-center text-[12px] text-[var(--color-text-faint)]">
                No requests currently executing
              </div>
            ) : null}
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
        {nodesErr ? (
          <ErrorState error={nodesErr} onRetry={() => nodesMutate()} />
        ) : !nodes ? (
          <LoadingRows rows={3} />
        ) : nodes.length === 0 ? (
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

// Slim fleet-health strip: online/total plus counts by maintenance state
// and the top agent versions across the fleet (v0.7+ fields are optional —
// the chips just don't render when absent).
function FleetHealthStrip({ nodes }: { nodes: Node[] }) {
  const approved = nodes.filter((n) => n.status === "approved");
  const online = approved.filter((n) => n.online).length;
  const offline = approved.length - online;
  const byMaint = (s: Node["maintenance"]) =>
    approved.filter((n) => n.maintenance === s).length;
  const draining = byMaint("draining");
  const maint = byMaint("maintenance");
  const updating = byMaint("updating");

  // Top-3 agent versions by node count ("legacy" = pre-version agents).
  const verCount = new Map<string, number>();
  for (const n of approved) {
    const v = agentVersion(n) || "legacy";
    verCount.set(v, (verCount.get(v) ?? 0) + 1);
  }
  const vers = [...verCount.entries()].sort((a, b) => b[1] - a[1]).slice(0, 3);

  return (
    <Panel className="fade-up mt-3 flex flex-wrap items-center gap-x-4 gap-y-2 px-4 py-2.5">
      <span className="label shrink-0">Fleet</span>
      <span className="badge badge-ok">
        <span className="dot" />
        {online}/{approved.length} online
      </span>
      {offline > 0 && (
        <span className="badge badge-danger">
          <span className="dot" />
          {offline} offline
        </span>
      )}
      {draining > 0 && (
        <span className="badge badge-warn">
          <span className="dot pulse" />
          {draining} draining
        </span>
      )}
      {maint > 0 && (
        <span className="badge badge-neutral">
          <span className="dot" />
          {maint} maintenance
        </span>
      )}
      {updating > 0 && (
        <span className="badge badge-accent">
          <span className="dot pulse" />
          {updating} updating
        </span>
      )}
      <span className="ml-auto flex flex-wrap items-center gap-1.5">
        {vers.map(([v, c]) => (
          <span
            key={v}
            className="badge badge-neutral mono !px-1.5 !py-0"
            title={`${c} node${c === 1 ? "" : "s"} on agent ${v}`}
          >
            {fmtVersion(v)} ×{c}
          </span>
        ))}
      </span>
    </Panel>
  );
}

// Single-active ownership / HA state (v0.7+). The endpoint 404s on older
// servers — the whole panel stays hidden then. `events` is the ownership
// history (newest first); `owner` is the live lease row. A promoted
// standby still reports role "active" — `promoted_from_standby` says how
// it got there. DB reachability comes from the diagnostics bundle (a real
// Pool.Ping server-side); heartbeat staleness is derived from
// heartbeat_age_seconds — the owner heartbeats every 2s, so >10s means
// the row is going cold.
function ControlPlanePanel() {
  const { data: cp } = useControlPlane();
  const { data: diag } = useDiagnostics();
  if (!cp) return null;
  const events = (cp.events ?? []).slice(0, 6);
  const instanceId = cp.instance_id || cp.owner?.instance_id || "";
  const hbAge = cp.owner?.heartbeat_age_seconds;
  const hbStale = hbAge !== undefined && hbAge > 10;
  const promotedAt = (cp.events ?? []).find((e) => e.event === "promoted")?.at;
  return (
    <Panel className="px-4 py-4">
      <div className="label mb-3">Control plane</div>
      <div className="space-y-3">
        <div className="flex items-baseline justify-between">
          <span className="text-[12px] text-[var(--color-text-dim)]">Role</span>
          <span className="flex items-center gap-1.5">
            {cp.promoted_from_standby && (
              <span
                className="badge badge-accent !px-1.5 !py-0"
                title="this instance was promoted from standby"
              >
                promoted
              </span>
            )}
            {cp.role === "active" ? (
              <span className="badge badge-ok"><span className="dot" />active</span>
            ) : cp.role === "standby" ? (
              <span className="badge badge-warn"><span className="dot" />standby</span>
            ) : (
              <span className="badge badge-neutral"><span className="dot" />{cp.role || "unknown"}</span>
            )}
          </span>
        </div>
        {cp.epoch !== undefined && <Row label="Epoch" value={String(cp.epoch)} />}
        {promotedAt && <Row label="Last promotion" value={fmtAgo(promotedAt)} />}
        {diag?.database && (
          <div className="flex items-baseline justify-between">
            <span className="text-[12px] text-[var(--color-text-dim)]">Database</span>
            {diag.database.reachable ? (
              <span className="mono text-[13px] font-medium">
                reachable · {fmtMs(diag.database.latency_ms)}
              </span>
            ) : (
              <span className="badge badge-danger"><span className="dot" />unreachable</span>
            )}
          </div>
        )}
        {instanceId && (
          <div className="flex items-baseline justify-between">
            <span className="text-[12px] text-[var(--color-text-dim)]">Instance</span>
            <span className="mono text-[12px] font-medium" title={instanceId}>
              {instanceId.length > 12 ? `${instanceId.slice(0, 8)}…` : instanceId}
            </span>
          </div>
        )}
        {cp.owner?.started_at && (
          <Row label="Owner since" value={fmtAgo(cp.owner.started_at)} />
        )}
        {cp.owner && (
          <div className="flex items-baseline justify-between">
            <span className="text-[12px] text-[var(--color-text-dim)]">Heartbeat</span>
            <span className="flex items-center gap-1.5">
              {hbStale && (
                <span
                  className="badge badge-warn !px-1.5 !py-0"
                  title="owner heartbeat is stale — expected every ~2s; database writes may be failing or the owner is dead"
                >
                  stale
                </span>
              )}
              <span
                className={`mono text-[13px] font-medium ${hbStale ? "text-[var(--color-warn)]" : ""}`}
              >
                {hbAge}s ago
              </span>
            </span>
          </div>
        )}
        {cp.uptime_seconds !== undefined && (
          <Row label="Uptime" value={fmtUptime(cp.uptime_seconds)} />
        )}
        {/* Standby presence (v0.10+; absent entirely on older servers —
            the row simply doesn't render then). Heartbeat age is
            optional; >15s means the standby's lease writes are going
            cold, same threshold as the owner heartbeat warning. */}
        {cp.standby != null && (
          <div className="flex items-baseline justify-between">
            <span className="text-[12px] text-[var(--color-text-dim)]">Standby</span>
            {cp.standby.present ? (
              <span className="flex items-center gap-1.5">
                {(cp.standby.heartbeat_age_seconds ?? 0) > 15 && (
                  <span
                    className="badge badge-warn !px-1.5 !py-0"
                    title="standby heartbeat is stale — it may be dead or losing database connectivity"
                  >
                    stale
                  </span>
                )}
                <span className="mono text-[13px] font-medium">
                  live
                  {cp.standby.heartbeat_age_seconds !== undefined &&
                    ` · last seen ${cp.standby.heartbeat_age_seconds}s ago`}
                </span>
              </span>
            ) : (
              <span className="text-[12px] text-[var(--color-text-faint)]">none detected</span>
            )}
          </div>
        )}
        {events.length > 0 && (
          <div>
            <div className="mb-1.5 text-[12px] text-[var(--color-text-dim)]">
              Ownership history
            </div>
            <div>
              {events.map((e) => (
                <div
                  key={e.id}
                  className="row-line flex items-baseline gap-2 py-1"
                  style={{ borderColor: "var(--color-border)" }}
                  title={e.detail || undefined}
                >
                  <span className="text-[12px] text-[var(--color-text)]">{e.event}</span>
                  <span className="mono text-[10.5px] text-[var(--color-text-faint)]">
                    epoch {e.epoch}
                  </span>
                  <span className="ml-auto text-[10.5px] text-[var(--color-text-faint)]">
                    {fmtAgo(e.at)}
                  </span>
                </div>
              ))}
            </div>
          </div>
        )}
      </div>
    </Panel>
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
