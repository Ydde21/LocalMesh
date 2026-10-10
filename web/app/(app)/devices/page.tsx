"use client";

import { Suspense, useState } from "react";
import { useSearchParams, useRouter } from "next/navigation";
import { useNodes, useNode, api } from "@/lib/api";
import { fmtBytes, fmtAgo, fmtMs, fmtUptime, osLabel } from "@/lib/format";
import { Panel, Meter, Empty } from "@/components/ui";
import { NodeBadge } from "@/components/status";
import type { Node } from "@/lib/types";
import { X, Cpu, HardDrive, Zap, RefreshCw } from "lucide-react";
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

  return (
    <div className="mx-auto max-w-[1200px] px-6 py-6">
      <header className="mb-6">
        <h1 className="text-[19px] font-semibold tracking-tight">Devices</h1>
        <p className="mt-0.5 text-[12.5px] text-[var(--color-text-dim)]">
          Machines connected to your cluster
        </p>
      </header>

      {(!nodes || nodes.length === 0) ? (
        <Panel>
          <Empty
            title="No devices connected"
            hint="Enroll machines from Settings → Enrollment tokens, then run localmesh-agent join on each device."
          />
        </Panel>
      ) : (
        <div className="grid gap-3">
          {nodes.map((n) => (
            <NodeRow key={n.id} node={n} selected={n.id === selectedId} />
          ))}
        </div>
      )}

      {selectedId && <DeviceDrawer id={selectedId} />}
    </div>
  );
}

function NodeRow({ node, selected }: { node: Node; selected: boolean }) {
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
