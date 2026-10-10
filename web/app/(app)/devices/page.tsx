"use client";

import { Suspense, useEffect, useRef, useState } from "react";
import { useSearchParams, useRouter } from "next/navigation";
import {
  useNodes, useNode, useEnrollments, useDiscoveryStatus, useOverview,
  useNodeEvents, useVersion, useReleases, useAgentUpdates, api,
} from "@/lib/api";
import { fmtBytes, fmtAgo, fmtMs, fmtUptime, fmtVersion, agentVersion, osLabel, versionGt } from "@/lib/format";
import { Panel, Meter, Empty } from "@/components/ui";
import { NodeBadge, StatusBadge } from "@/components/status";
import { UpdatePhaseStepper } from "@/components/update-phase";
import type { Node, Release } from "@/lib/types";
import {
  X, Cpu, HardDrive, Zap, RefreshCw, Plus, Copy, Check, Wifi, WifiOff,
  Package, Wrench, ArrowUpCircle,
} from "lucide-react";
import { useSWRConfig } from "swr";

export default function DevicesPage() {
  return (
    <Suspense fallback={null}>
      <DevicesInner />
    </Suspense>
  );
}

function DevicesInner() {
  const params = useSearchParams();
  const selectedId = params.get("id");
  const { data: nodes } = useNodes();
  const [addOpen, setAddOpen] = useState(false);

  return (
    <div className="mx-auto max-w-[1200px] px-6 py-6">
      <header className="mb-6 flex items-end justify-between">
        <div>
          <h1 className="text-[19px] font-semibold tracking-tight">Devices</h1>
          <p className="mt-0.5 text-[12.5px] text-[var(--color-text-dim)]">
            Machines connected to your cluster
          </p>
        </div>
        <button className="btn btn-primary btn-sm" onClick={() => setAddOpen(true)}>
          <Plus size={13} /> Add device
        </button>
      </header>

      {(!nodes || nodes.length === 0) ? (
        <Panel>
          <Empty
            title="No devices connected"
            hint="Generate an enrollment token, then run localmesh-agent join on the machine — it appears here after you approve it."
          >
            <button className="btn btn-primary btn-sm" onClick={() => setAddOpen(true)}>
              <Plus size={13} /> Add device
            </button>
          </Empty>
        </Panel>
      ) : (
        <div className="grid gap-3">
          {nodes.map((n) => (
            <NodeRow key={n.id} node={n} selected={n.id === selectedId} />
          ))}
        </div>
      )}

      <UpdateHistory />

      {addOpen && <AddDeviceDialog onClose={() => setAddOpen(false)} />}
      {selectedId && <DeviceDrawer id={selectedId} />}
    </div>
  );
}

// Fleet-wide agent-update history (v0.7+) — newest 10 rows. The whole
// panel self-hides when the endpoint is absent (404) or empty.
function UpdateHistory() {
  const { data: updates } = useAgentUpdates({ limit: 10 });
  if (!updates || updates.length === 0) return null;
  return (
    <Panel className="fade-up mt-3">
      <div className="border-b px-4 py-3" style={{ borderColor: "var(--color-border)" }}>
        <div className="label">Update history</div>
      </div>
      {updates
        .slice()
        .sort((a, b) => b.created_at.localeCompare(a.created_at))
        .slice(0, 10)
        .map((u) => (
        <div
          key={u.id}
          className="row-line flex flex-wrap items-center gap-3 px-4 py-2"
          style={{ borderColor: "var(--color-border)" }}
        >
          <span className="text-[12px] font-medium">{u.node_name || u.node_id.slice(0, 8)}</span>
          <span className="mono text-[11px] text-[var(--color-text-dim)]">
            {u.from_version || "?"} → {u.to_version || u.release_version || "?"}
          </span>
          <StatusBadge status={u.phase} />
          {u.error && (
            <span
              className="max-w-[320px] truncate text-[11px] text-[var(--color-danger)]"
              title={u.error}
            >
              {u.error}
            </span>
          )}
          <span className="ml-auto text-[10.5px] text-[var(--color-text-faint)]">
            {fmtAgo(u.finished_at || u.updated_at || u.created_at)}
          </span>
        </div>
      ))}
    </Panel>
  );
}

// Newest approved release strictly newer than `agent` (semver-ish, best
// effort). null when the registry is absent/empty or nothing is newer.
function newerRelease(releases: Release[] | undefined, agent: string): Release | null {
  if (!agent) return null;
  let best: Release | null = null;
  for (const r of releases ?? []) {
    if (r.status !== "approved") continue;
    if (!versionGt(r.version, agent)) continue;
    if (!best || versionGt(r.version, best.version)) best = r;
  }
  return best;
}

// Agent version/compat tag: mono version for reporting agents, a "legacy"
// pill for pre-v0.5 ones, a quiet warn pill when the agent build differs
// from the server, and an "update" pill when the registry holds a newer
// approved release. serverVersion is absent on pre-v0.5 servers — no
// comparison is made then.
function AgentVersionTag({ node, serverVersion }: { node: Node; serverVersion?: string }) {
  const { data: releases } = useReleases();
  const av = agentVersion(node);
  const newer = newerRelease(releases, av);
  if (!av) {
    return (
      <span
        className="badge badge-neutral !px-1.5 !py-0"
        title="agent predates version reporting (pre-v0.5) — still compatible"
      >
        legacy
      </span>
    );
  }
  const mismatch = !!serverVersion && av !== serverVersion;
  return (
    <>
      <span
        className="mono text-[11px] text-[var(--color-text-faint)]"
        title={`agent ${av}`}
      >
        {fmtVersion(av)}
      </span>
      {mismatch && (
        <span
          className="badge badge-warn !px-1.5 !py-0"
          title={`agent ${av} differs from server ${serverVersion} — still protocol-compatible; upgrade when convenient`}
        >
          mismatch
        </span>
      )}
      {newer && (
        <span
          className="badge badge-accent !px-1.5 !py-0"
          title={`approved release ${fmtVersion(newer.version)} is newer than this agent — update from the device drawer or Releases`}
        >
          update {fmtVersion(newer.version)}
        </span>
      )}
    </>
  );
}

function NodeRow({ node, selected }: { node: Node; selected: boolean }) {
  const { data: ver } = useVersion();
  const m = node.metrics;
  const memUsed = m?.memory_used_bytes ?? 0;
  const memTotal = m?.memory_total_bytes ?? node.hardware.memory_bytes;
  const memPct = memTotal > 0 ? (memUsed / memTotal) * 100 : 0;
  const hasGPU = (node.hardware.gpus?.length ?? 0) > 0;
  const loadedCount = (node.models ?? []).filter((mo) => mo.loaded).length;
  const saturated = node.max_concurrent > 0 && node.in_flight >= node.max_concurrent;

  return (
    <a
      href={`/devices?id=${node.id}`}
      className={`panel panel-hover fade-up grid grid-cols-12 items-center gap-4 px-5 py-4 ${
        selected ? "border-[var(--color-accent)]" : ""
      }`}
    >
      <div className="col-span-4">
        <div className="flex items-center gap-2.5">
          <div className="text-[14px] font-medium">{node.name}</div>
          <NodeBadge node={node} />
          <AgentVersionTag node={node} serverVersion={ver?.server_version} />
        </div>
        <div className="mt-0.5 text-[11.5px] text-[var(--color-text-faint)]">
          {osLabel(node.hardware.os)} {node.hardware.arch} · {node.hardware.hostname}
        </div>
        {(node.engines ?? []).length > 0 && (
          <div className="mt-1.5 flex flex-wrap gap-1">
            {(node.engines ?? []).map((e) => (
              <span
                key={e.type}
                className={`badge !px-1.5 !py-0 ${e.healthy ? "badge-ok" : "badge-danger"}`}
                title={e.address ? `${e.type} at ${e.address}` : e.type}
              >
                <span className="dot" />
                {e.type} {e.version}
              </span>
            ))}
          </div>
        )}
      </div>

      <div className="col-span-3">
        <div className="text-[12px] text-[var(--color-text-dim)]">{node.hardware.cpu_model}</div>
        <div className="text-[11px] text-[var(--color-text-faint)]">
          {node.hardware.cpu_cores} cores · {fmtBytes(node.hardware.memory_bytes)}
          {hasGPU &&
            ` · ${node.hardware.gpus!.map((g) => g.name).join(", ")}`}
        </div>
      </div>

      <div className="col-span-3">
        {!node.online ? (
          <span className="text-[11px] text-[var(--color-text-faint)]">
            {node.last_seen_at ? `last seen ${fmtAgo(node.last_seen_at)}` : "offline"}
          </span>
        ) : m ? (
          <div className="space-y-1.5">
            {/* A missing *_measured flag means the value is unknown —
                render "—", never a fake 0%. */}
            <MeterRow
              label="CPU"
              value={m.cpu_util_measured === true ? m.cpu_util_percent : null}
            />
            <MeterRow label="MEM" value={memPct} />
            {hasGPU && (
              <MeterRow
                label="GPU"
                value={
                  m.gpu_util_measured === true && m.gpu_util_percent !== undefined
                    ? m.gpu_util_percent
                    : null
                }
              />
            )}
          </div>
        ) : (
          <span className="text-[11px] text-[var(--color-text-faint)]">no metrics</span>
        )}
      </div>

      <div className="col-span-2 text-right">
        <div
          className={`mono text-[12px] ${
            saturated ? "text-[var(--color-warn)]" : "text-[var(--color-text-dim)]"
          }`}
        >
          {node.in_flight}/{node.max_concurrent} slots
        </div>
        <div className="text-[11px] text-[var(--color-text-faint)]">
          {(node.models ?? []).length} models{loadedCount > 0 ? ` · ${loadedCount} loaded` : ""}
        </div>
      </div>
    </a>
  );
}

function MeterRow({ label, value }: { label: string; value: number | null }) {
  return (
    <div className="flex items-center gap-2">
      <span className="label w-[30px]">{label}</span>
      {value === null ? (
        <span className="mono flex-1 text-[10.5px] text-[var(--color-text-faint)]">—</span>
      ) : (
        <>
          <Meter value={value} className="flex-1" />
          <span className="mono w-[44px] text-right text-[10.5px] text-[var(--color-text-dim)]">
            {value.toFixed(0)}%
          </span>
        </>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------

function DeviceDrawer({ id }: { id: string }) {
  const router = useRouter();
  const { data: node, mutate } = useNode(id);
  const { data: ver } = useVersion();
  const { mutate: globalMutate } = useSWRConfig();
  const [busy, setBusy] = useState(false);
  const [maxConc, setMaxConc] = useState("");
  const [err, setErr] = useState("");

  if (!node) return null;

  const act = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    setErr("");
    try {
      await fn();
      mutate();
      globalMutate("/nodes");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const m = node.metrics;
  const memAvail = m?.memory_available_bytes ?? ((m?.memory_total_bytes ?? 0) - (m?.memory_used_bytes ?? 0));

  return (
    <div
      className="fixed inset-0 z-40 bg-black/50 backdrop-blur-[2px]"
      onClick={() => router.push("/devices")}
    >
      <div
        className="panel absolute right-0 top-0 h-full w-[440px] max-w-[94vw] overflow-y-auto border-l"
        style={{ borderColor: "var(--color-border)", borderRadius: 0 }}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-start justify-between border-b px-5 py-4" style={{ borderColor: "var(--color-border)" }}>
          <div>
            <div className="flex items-center gap-2.5">
              <h2 className="text-[16px] font-semibold">{node.name}</h2>
              <NodeBadge node={node} />
              <AgentVersionTag node={node} serverVersion={ver?.server_version} />
            </div>
            <div className="mono mt-1 text-[11px] text-[var(--color-text-faint)]">{node.id}</div>
          </div>
          <a href="/devices" className="btn btn-sm"><X size={14} /></a>
        </div>

        <div className="space-y-5 px-5 py-5">
          {err && (
            <div className="rounded-md border border-red-500/30 bg-red-500/10 px-3 py-2 text-[12px] text-red-400">
              {err}
            </div>
          )}

          {/* Hardware */}
          <section>
            <div className="label mb-2.5">Hardware</div>
            <div className="space-y-2">
              <KV icon={Cpu} label="CPU" value={`${node.hardware.cpu_model} (${node.hardware.cpu_cores} cores)`} />
              <KV icon={HardDrive} label="Memory" value={fmtBytes(node.hardware.memory_bytes)} />
              <KV
                icon={Package}
                label="Agent"
                value={
                  agentVersion(node)
                    ? fmtVersion(agentVersion(node))
                    : "legacy — pre-v0.5 (no version report)"
                }
              />
              {(node.hardware.gpus ?? []).map((g, i) => (
                <KV key={i} icon={Zap} label={g.vendor || "GPU"}
                  value={`${g.name}${g.shared_vram ? " (unified memory)" : g.vram_bytes ? ` · ${fmtBytes(g.vram_bytes)}` : ""}`} />
              ))}
            </div>
          </section>

          {/* Live metrics */}
          <section>
            <div className="label mb-2.5">Utilization</div>
            {!node.online && (
              <div className="mb-2 text-[11px] text-[var(--color-text-faint)]">
                Node is offline — values below are the last reported snapshot.
              </div>
            )}
            {m ? (
              <div className="space-y-2.5">
                <div>
                  <div className="mb-1 flex justify-between text-[11.5px]">
                    <span className="text-[var(--color-text-dim)]">CPU</span>
                    <span className="mono">
                      {m.cpu_util_measured === true ? `${m.cpu_util_percent.toFixed(0)}%` : "—"}
                    </span>
                  </div>
                  {m.cpu_util_measured === true && <Meter value={m.cpu_util_percent} />}
                </div>
                <div>
                  <div className="mb-1 flex justify-between text-[11.5px]">
                    <span className="text-[var(--color-text-dim)]">Memory</span>
                    <span className="mono">
                      {fmtBytes(m.memory_used_bytes)} / {fmtBytes(m.memory_total_bytes)}
                    </span>
                  </div>
                  <Meter value={(m.memory_used_bytes / (m.memory_total_bytes || 1)) * 100} />
                </div>
                {(node.hardware.gpus?.length ?? 0) > 0 && (
                  <div>
                    <div className="mb-1 flex justify-between text-[11.5px]">
                      <span className="text-[var(--color-text-dim)]">GPU</span>
                      <span className="mono">
                        {m.gpu_util_measured === true && m.gpu_util_percent !== undefined
                          ? `${m.gpu_util_percent.toFixed(0)}%`
                          : "—"}
                      </span>
                    </div>
                    {m.gpu_util_measured === true && m.gpu_util_percent !== undefined && (
                      <Meter value={m.gpu_util_percent} />
                    )}
                  </div>
                )}
                <div className="grid grid-cols-2 gap-2 pt-1 text-[11.5px]">
                  <KV2
                    label="Available mem"
                    value={m.mem_avail_measured === true ? fmtBytes(memAvail) : "—"}
                  />
                  <KV2 label="Uptime" value={fmtUptime(m.uptime_seconds)} />
                  <KV2 label="Active req" value={String(m.active_requests)} />
                  <KV2
                    label="Latency"
                    value={node.latency_ms != null ? fmtMs(node.latency_ms) : "—"}
                  />
                  {(node.hardware.gpus?.length ?? 0) > 0 && (
                    <KV2
                      label="VRAM used"
                      value={
                        m.vram_used_measured === true && m.gpu_vram_used_bytes !== undefined
                          ? fmtBytes(m.gpu_vram_used_bytes)
                          : "—"
                      }
                    />
                  )}
                </div>
                <div className="text-[11.5px]">
                  <span className="text-[var(--color-text-faint)]">Loaded: </span>
                  <span className="mono text-[var(--color-text-dim)]">
                    {m.loaded_measured === true
                      ? (m.loaded_models ?? []).join(", ") || "none"
                      : "unknown"}
                  </span>
                </div>
              </div>
            ) : (
              <div className="text-[12px] text-[var(--color-text-faint)]">No metrics received yet</div>
            )}
          </section>

          {/* Engines & models */}
          <section>
            <div className="label mb-2.5">Engines</div>
            {(node.engines ?? []).length === 0 ? (
              <div className="text-[12px] text-[var(--color-text-faint)]">None detected</div>
            ) : (
              <div className="space-y-1.5">
                {(node.engines ?? []).map((e) => (
                  <div
                    key={e.type}
                    className="flex items-center gap-2.5 rounded-md bg-white/[0.03] px-2.5 py-1.5"
                  >
                    <span className={`badge !py-0 ${e.healthy ? "badge-ok" : "badge-danger"}`}>
                      <span className="dot" />
                      {e.healthy ? "healthy" : "unhealthy"}
                    </span>
                    <span className="text-[12px] font-medium">{e.type}</span>
                    {e.version && (
                      <span className="mono text-[11px] text-[var(--color-text-faint)]">
                        {e.version}
                      </span>
                    )}
                    {e.address && (
                      <span className="mono ml-auto truncate text-[10.5px] text-[var(--color-text-faint)]">
                        {e.address}
                      </span>
                    )}
                  </div>
                ))}
              </div>
            )}
            <div className="label mb-2 mt-4">Models ({(node.models ?? []).length})</div>
            <div className="space-y-1">
              {(node.models ?? []).map((mo) => (
                <div key={mo.name} className="flex items-center justify-between rounded-md bg-white/[0.03] px-2.5 py-1.5">
                  <span className="mono text-[12px]">{mo.name}</span>
                  <span className="flex items-center gap-2 text-[10.5px] text-[var(--color-text-faint)]">
                    {mo.loaded && <span className="badge badge-ok !px-1.5 !py-0">loaded</span>}
                    {fmtBytes(mo.size_bytes)}
                  </span>
                </div>
              ))}
            </div>
          </section>

          {/* Connection history (v0.6+; absent endpoint renders nothing) */}
          <ConnectionHistory id={node.id} />

          {/* Maintenance + agent update (v0.7+; each self-hides when the
              server doesn't expose the capability) */}
          <MaintenanceSection node={node} busy={busy} act={act} />
          <UpdateSection node={node} busy={busy} act={act} />

          {/* Controls */}
          <section>
            <div className="label mb-2.5">Concurrency</div>
            <div className="flex gap-2">
              <input
                className="input mono w-[90px]"
                type="number" min={1} max={64}
                placeholder={String(node.max_concurrent)}
                value={maxConc}
                onChange={(e) => setMaxConc(e.target.value)}
              />
              <button
                className="btn btn-sm"
                disabled={busy || !maxConc}
                onClick={() =>
                  act(() => api("PATCH", `/nodes/${node.id}`, { max_concurrent: Number(maxConc) }))
                }
              >
                <RefreshCw size={12} /> Update slots
              </button>
            </div>
            <p className="mt-1.5 text-[11px] text-[var(--color-text-faint)]">
              Max concurrent inference requests this node accepts (current: {node.max_concurrent}).
            </p>
          </section>

          <section className="border-t pt-4" style={{ borderColor: "var(--color-border)" }}>
            <div className="label mb-2.5">Danger zone</div>
            <div className="flex gap-2">
              {node.status === "approved" ? (
                <button className="btn btn-danger btn-sm" disabled={busy}
                  onClick={() => act(() => api("POST", `/nodes/${node.id}/revoke`))}>
                  Revoke access
                </button>
              ) : node.status === "revoked" ? (
                <button className="btn btn-sm" disabled={busy}
                  onClick={() => act(() => api("POST", `/nodes/${node.id}/reinstate`))}>
                  Reinstate
                </button>
              ) : null}
              <button className="btn btn-danger btn-sm" disabled={busy}
                onClick={() => {
                  if (confirm(`Remove ${node.name} from the cluster? Its credential stops working immediately.`)) {
                    act(() => api("DELETE", `/nodes/${node.id}`));
                    router.push("/devices");
                  }
                }}>
                Remove node
              </button>
            </div>
          </section>

          <div className="text-[10.5px] text-[var(--color-text-faint)]">
            Joined {fmtAgo(node.created_at)}
            {node.last_seen_at && ` · last seen ${fmtAgo(node.last_seen_at)}`}
          </div>
        </div>
      </div>
    </div>
  );
}

// Recent connect/disconnect rows from node_events — the audit trail for
// "why did this machine drop". Hidden entirely on pre-v0.6 servers (404)
// so the drawer doesn't show a dead section.
function ConnectionHistory({ id }: { id: string }) {
  const { data: events } = useNodeEvents(id);
  if (!events) return null;
  const rows = events.slice(0, 10);
  return (
    <section>
      <div className="label mb-2.5">Connection history</div>
      {rows.length === 0 ? (
        <div className="text-[12px] text-[var(--color-text-faint)]">No events recorded yet</div>
      ) : (
        <div>
          {rows.map((e) => {
            const up = e.event === "connected";
            return (
              <div
                key={e.id}
                className="row-line flex items-baseline gap-2.5 py-1.5"
                style={{ borderColor: "var(--color-border)" }}
                title={e.agent_version ? `agent ${e.agent_version}` : undefined}
              >
                {up ? (
                  <Wifi size={12} className="relative top-px shrink-0 text-[var(--color-online)]" />
                ) : (
                  <WifiOff size={12} className="relative top-px shrink-0 text-[var(--color-text-faint)]" />
                )}
                <span className="mono w-[54px] shrink-0 text-[10.5px] text-[var(--color-text-faint)]">
                  {fmtAgo(e.created_at)}
                </span>
                <span className="text-[12px] text-[var(--color-text)]">{e.event}</span>
                {e.reason && (
                  <span className="truncate text-[11px] text-[var(--color-text-faint)]">
                    {e.reason}
                  </span>
                )}
              </div>
            );
          })}
        </div>
      )}
    </section>
  );
}

// Maintenance/drain controls (v0.7+). Capability probe: the node row
// carries `maintenance` only on servers that have the endpoints; on
// older ones the whole section stays hidden. A 403/404 on the action
// itself surfaces through the drawer's error box like every other call.
function MaintenanceSection({
  node,
  busy,
  act,
}: {
  node: Node;
  busy: boolean;
  act: (fn: () => Promise<unknown>) => Promise<void>;
}) {
  const [reason, setReason] = useState("");
  if (node.maintenance === undefined) return null;
  const st = node.maintenance;
  return (
    <section>
      <div className="label mb-2.5">Maintenance</div>
      {st === "" ? (
        <>
          <div className="flex gap-2">
            <input
              className="input flex-1"
              placeholder="reason (optional)"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              onKeyDown={(e) =>
                e.key === "Enter" &&
                act(() => api("POST", `/nodes/${node.id}/maintenance`, { reason }))
              }
            />
            <button
              className="btn btn-sm"
              disabled={busy}
              onClick={() => act(() => api("POST", `/nodes/${node.id}/maintenance`, { reason }))}
            >
              <Wrench size={12} /> Enter maintenance
            </button>
          </div>
          <p className="mt-1.5 text-[11px] text-[var(--color-text-faint)]">
            Stops new work on this node; in-flight requests finish first.
          </p>
        </>
      ) : (
        <>
          <div className="flex items-center gap-2">
            <NodeBadge node={node} />
            {st === "draining" && (
              <span className="mono text-[11px] text-[var(--color-warn)]">
                …{node.in_flight} in flight
              </span>
            )}
          </div>
          {node.maintenance_reason && (
            <div className="mt-1.5 text-[11.5px] text-[var(--color-text-dim)]">
              {node.maintenance_reason}
            </div>
          )}
          {node.maintenance_since && (
            <div className="mt-0.5 text-[10.5px] text-[var(--color-text-faint)]">
              since {fmtAgo(node.maintenance_since)}
            </div>
          )}
          <button
            className="btn btn-sm mt-2.5"
            disabled={busy}
            onClick={() => act(() => api("POST", `/nodes/${node.id}/resume`))}
          >
            <Check size={12} /> Resume service
          </button>
        </>
      )}
    </section>
  );
}

// Agent update for this node (v0.7+): a live stepper while an update is
// in flight, otherwise a release picker that dispatches a single-node
// rollout, plus the last finished update for context. Hidden when the
// feature endpoints are absent entirely.
function UpdateSection({
  node,
  busy,
  act,
}: {
  node: Node;
  busy: boolean;
  act: (fn: () => Promise<unknown>) => Promise<void>;
}) {
  const { data: releases } = useReleases();
  const { data: updates } = useAgentUpdates({ node_id: node.id, limit: 1 });
  const { mutate: gm } = useSWRConfig();
  const [relId, setRelId] = useState("");

  const approved = (releases ?? []).filter((r) => r.status === "approved");
  // Endpoint ordering isn't contractual — take newest by created_at.
  const latest = (updates ?? [])
    .slice()
    .sort((a, b) => b.created_at.localeCompare(a.created_at))[0];
  const live = !!latest && !latest.finished_at;

  if (!releases && !updates) return null; // pre-v0.7 server
  if (approved.length === 0 && !latest) return null; // nothing to act on

  // Default the picker to the newest approved release (semver-ish).
  const newest = approved.reduce<Release | null>(
    (best, r) => (best && versionGt(best.version, r.version) ? best : r),
    null
  );
  const sel = relId || newest?.id || "";
  const canUpdate =
    !!sel && node.online && node.status === "approved" && !node.maintenance && !live;

  return (
    <section>
      <div className="label mb-2.5">Agent update</div>
      {live && latest ? (
        <UpdatePhaseStepper update={latest} />
      ) : (
        <>
          {approved.length > 0 ? (
            <div className="flex gap-2">
              <select
                className="input mono flex-1"
                value={sel}
                onChange={(e) => setRelId(e.target.value)}
              >
                {approved.map((r) => (
                  <option key={r.id} value={r.id}>
                    {fmtVersion(r.version)} · {r.channel}
                  </option>
                ))}
              </select>
              <button
                className="btn btn-sm btn-primary"
                disabled={busy || !canUpdate}
                title={
                  canUpdate
                    ? "Dispatch a single-node rollout to this release"
                    : !node.online
                      ? "node is offline"
                      : node.maintenance
                        ? "node is in maintenance"
                        : node.status !== "approved"
                          ? "node is not approved"
                          : undefined
                }
                onClick={() =>
                  act(async () => {
                    await api("POST", "/rollouts", {
                      release_id: sel,
                      strategy: "single",
                      node_ids: [node.id],
                    });
                    gm(
                      (key) =>
                        typeof key === "string" &&
                        (key.startsWith("/agent-updates") || key.startsWith("/rollouts"))
                    );
                  })
                }
              >
                <ArrowUpCircle size={12} /> Update this device
              </button>
            </div>
          ) : (
            <div className="text-[12px] text-[var(--color-text-faint)]">
              No approved releases — register one on the Releases page.
            </div>
          )}
          {latest && (
            <div className="mt-2.5 flex flex-wrap items-center gap-2 text-[11px] text-[var(--color-text-faint)]">
              <span>last update:</span>
              <span className="mono text-[var(--color-text-dim)]">
                {latest.from_version || "?"} → {latest.to_version || latest.release_version || "?"}
              </span>
              <StatusBadge status={latest.phase} />
              <span>{fmtAgo(latest.finished_at)}</span>
              {latest.error && (
                <span className="max-w-[220px] truncate text-[var(--color-danger)]" title={latest.error}>
                  {latest.error}
                </span>
              )}
            </div>
          )}
        </>
      )}
    </section>
  );
}

function KV({ icon: Icon, label, value }: { icon: typeof Cpu; label: string; value: string }) {
  return (
    <div className="flex items-center gap-2.5 text-[12.5px]">
      <Icon size={14} className="text-[var(--color-text-faint)]" />
      <span className="w-[70px] text-[var(--color-text-faint)]">{label}</span>
      <span className="text-[var(--color-text)]">{value}</span>
    </div>
  );
}

function KV2({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <div className="text-[10.5px] text-[var(--color-text-faint)]">{label}</div>
      <div className="mono text-[12px]">{value}</div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Add-device onboarding: mint a one-time token → run `localmesh-agent join`
// → approve the pending request. LAN discovery (when the server advertises
// it) offers the shorter `join --discover` path.

function StepHead({ n, title, sub }: { n: number; title: string; sub?: string }) {
  return (
    <div>
      <div className="flex items-center gap-2">
        <span className="flex h-[18px] w-[18px] shrink-0 items-center justify-center rounded-full bg-[var(--color-accent-soft)] text-[10.5px] font-semibold text-[var(--color-accent)]">
          {n}
        </span>
        <span className="text-[12.5px] font-medium">{title}</span>
      </div>
      {sub && (
        <div className="mt-0.5 pl-[26px] text-[11px] text-[var(--color-text-faint)]">{sub}</div>
      )}
    </div>
  );
}

function AddDeviceDialog({ onClose }: { onClose: () => void }) {
  const [token, setToken] = useState("");
  const [genErr, setGenErr] = useState("");
  const [busy, setBusy] = useState(false);
  const [actErr, setActErr] = useState("");
  const [copied, setCopied] = useState("");
  const { data: disco } = useDiscoveryStatus();
  const { data: ov } = useOverview();
  const { data: pending, mutate: mutPending } = useEnrollments("pending");
  const { mutate } = useSWRConfig();
  const didInit = useRef(false);

  const genToken = async () => {
    setBusy(true);
    setGenErr("");
    try {
      // `label` is the field the server reads today; `name` is the alias in
      // the v0.5 contract — send both so either spelling works.
      const res = await api<{ token: string }>("POST", "/enrollment-tokens", {
        label: "",
        name: "",
        max_uses: 1,
      });
      setToken(res.token);
      mutate("/enrollment-tokens");
    } catch (e) {
      setGenErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  useEffect(() => {
    // The ref also stops StrictMode's double-effect in dev from minting two
    // tokens per dialog open.
    if (didInit.current) return;
    didInit.current = true;
    void genToken();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  const serverUrl = ov?.server_url || "https://<control-plane>:<port>";
  const joinCmd = token
    ? `localmesh-agent join --server ${serverUrl} --token ${token}` +
      (disco?.ca_fingerprint ? ` --ca-fingerprint ${disco.ca_fingerprint}` : "")
    : "";

  const copy = (key: string, text: string) => {
    navigator.clipboard.writeText(text);
    setCopied(key);
    setTimeout(() => setCopied((c) => (c === key ? "" : c)), 1500);
  };

  const act = async (path: string) => {
    setActErr("");
    try {
      await api("POST", path);
      mutPending();
      mutate("/nodes");
    } catch (e) {
      setActErr(e instanceof Error ? e.message : String(e));
    }
  };

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 px-4 backdrop-blur-[2px]"
      onClick={onClose}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-label="Add device"
        className="panel fade-up max-h-[92vh] w-full max-w-[560px] overflow-y-auto px-5 py-5"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-start justify-between">
          <div>
            <h2 className="text-[15px] font-semibold">Add device</h2>
            <p className="mt-0.5 text-[12px] text-[var(--color-text-dim)]">
              Enroll a machine — it joins the cluster after your approval.
            </p>
          </div>
          <button className="btn btn-sm -mr-1 -mt-1" onClick={onClose} aria-label="Close dialog">
            <X size={14} />
          </button>
        </div>

        <div className="mt-5 space-y-5">
          {/* Step 1 — one-time enrollment token */}
          <section>
            <StepHead
              n={1}
              title="Enrollment token"
              sub="Single use, expires in 24h, shown only once."
            />
            <div className="mt-2 pl-[26px]">
              {busy && !token && (
                <div className="text-[12px] text-[var(--color-text-faint)]">Generating…</div>
              )}
              {token && (
                <div className="codeblock flex items-center justify-between">
                  <span>{token}</span>
                  <button className="btn btn-sm ml-3" onClick={() => copy("token", token)}>
                    {copied === "token" ? <Check size={12} /> : <Copy size={12} />} Copy
                  </button>
                </div>
              )}
              {genErr && (
                <div className="flex items-center gap-3">
                  <span className="text-[12px] text-[var(--color-danger)]">{genErr}</span>
                  <button className="btn btn-sm" onClick={genToken} disabled={busy}>
                    <RefreshCw size={11} /> Try again
                  </button>
                </div>
              )}
            </div>
          </section>

          {/* Step 2 — join command */}
          <section>
            <StepHead
              n={2}
              title="Run this on the device"
              sub="Replace <control-plane>:<port> with this server's reachable address (e.g. https://192.168.1.10:8443)."
            />
            <div className="mt-2 pl-[26px]">
              {joinCmd ? (
                <div className="codeblock flex items-center justify-between">
                  <span>{joinCmd}</span>
                  <button className="btn btn-sm ml-3" onClick={() => copy("cmd", joinCmd)}>
                    {copied === "cmd" ? <Check size={12} /> : <Copy size={12} />} Copy
                  </button>
                </div>
              ) : (
                <div className="text-[12px] text-[var(--color-text-faint)]">
                  The command appears once the token is generated.
                </div>
              )}
              <p className="mt-2 text-[11px] leading-relaxed text-[var(--color-text-faint)]">
                <code className="mono">--ca-fingerprint sha256:…</code> pins the cluster CA —
                recommended so the agent verifies it is talking to your server.
              </p>
              {disco?.advertising && (
                <div className="mt-2.5 flex items-start gap-2.5 rounded-md border border-[var(--color-accent)]/25 bg-[var(--color-accent-soft)] px-3 py-2.5">
                  <Wifi size={13} className="mt-0.5 shrink-0 text-[var(--color-accent)]" />
                  <div className="text-[11.5px] leading-relaxed text-[var(--color-text-dim)]">
                    LAN discovery is on{disco.cluster_name ? ` (“${disco.cluster_name}”)` : ""} —
                    devices on this network can instead run{" "}
                    <code className="mono text-[var(--color-text)]">localmesh-agent join --discover</code>
                    {disco.ca_fingerprint && (
                      <>
                        {" "}and verify CA fingerprint{" "}
                        <code
                          className="mono text-[var(--color-text)]"
                          title={disco.ca_fingerprint}
                        >
                          {disco.ca_fingerprint.length > 24
                            ? `${disco.ca_fingerprint.slice(0, 24)}…`
                            : disco.ca_fingerprint}
                        </code>
                      </>
                    )}
                    .
                  </div>
                </div>
              )}
            </div>
          </section>

          {/* Step 3 — approve the pending request */}
          <section>
            <StepHead
              n={3}
              title="Approve the device"
              sub="Enrollment requests land here — nothing joins without approval."
            />
            <div className="mt-2 space-y-2 pl-[26px]">
              {(pending ?? []).length === 0 ? (
                <div className="text-[12px] text-[var(--color-text-faint)]">
                  No pending requests — the device appears here after it contacts the control plane.
                </div>
              ) : (
                (pending ?? []).map((r) => (
                  <div
                    key={r.id}
                    className="flex items-center gap-3 rounded-md border border-[var(--color-warn)]/25 bg-[var(--color-warn-soft)] px-3.5 py-2.5"
                  >
                    <div>
                      <div className="text-[13px] font-medium">{r.name}</div>
                      <div className="text-[11px] text-[var(--color-text-dim)]">
                        {r.hardware.cpu_model} · {r.hardware.cpu_cores} cores · {r.hardware.os}/{r.hardware.arch} · {fmtAgo(r.created_at)}
                      </div>
                    </div>
                    <div className="ml-auto flex gap-2">
                      <button
                        className="btn btn-sm btn-primary"
                        onClick={() => act(`/enrollments/${r.id}/approve`)}
                      >
                        <Check size={12} /> Approve
                      </button>
                      <button
                        className="btn btn-sm btn-danger"
                        onClick={() => act(`/enrollments/${r.id}/deny`)}
                      >
                        <X size={12} /> Deny
                      </button>
                    </div>
                  </div>
                ))
              )}
              {actErr && (
                <div className="text-[12px] text-[var(--color-danger)]">{actErr}</div>
              )}
            </div>
          </section>
        </div>
      </div>
    </div>
  );
}
