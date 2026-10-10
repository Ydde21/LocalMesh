"use client";

import React, { useEffect, useMemo, useState } from "react";
import { useModels, useModelOps, useNodes, usePlacement, api } from "@/lib/api";
import { fmtBytes, fmtAgo } from "@/lib/format";
import { Panel, Empty, Meter } from "@/components/ui";
import { StatusBadge } from "@/components/status";
import { ConfirmDialog } from "@/components/confirm";
import { useSWRConfig } from "swr";
import { ChevronDown, ChevronRight, Download, Trash2, X } from "lucide-react";
import type { ModelOperation, Node, PlacementEntry } from "@/lib/types";

// Ops still in flight on a node. "pending" is included defensively — the
// control plane currently emits dispatched/running.
const ACTIVE_STATUSES = new Set<ModelOperation["status"]>([
  "dispatched",
  "pending",
  "running",
]);
const isActive = (o: ModelOperation) => ACTIVE_STATUSES.has(o.status);

interface RemoveTarget {
  model: string;
  nodeIDs: string[];
  nodeNames: string[];
  // Set when the control plane refused a last-replica delete (409) and
  // the admin must confirm the destructive action explicitly.
  lastReplica?: boolean;
}

export default function ModelsPage() {
  const { data: models } = useModels();
  const { data: ops } = useModelOps();
  const { data: nodes } = useNodes();
  const { data: placement } = usePlacement();
  const { mutate } = useSWRConfig();

  const [installOpen, setInstallOpen] = useState(false);
  const [removing, setRemoving] = useState<RemoveTarget | null>(null);
  const [removeBusy, setRemoveBusy] = useState(false);
  const [removeErr, setRemoveErr] = useState("");
  const [opErr, setOpErr] = useState("");
  const [cancelling, setCancelling] = useState<string | null>(null);

  const refresh = () => {
    mutate("/models");
    mutate("/models/placement");
    mutate("/model-operations?limit=30");
    mutate("/nodes");
  };

  const placementByName = useMemo(
    () => new Map((placement ?? []).map((p) => [p.name, p])),
    [placement]
  );
  const [expanded, setExpanded] = useState<string | null>(null);

  const nodeById = useMemo(
    () => new Map((nodes ?? []).map((n) => [n.id, n])),
    [nodes]
  );

  // The catalog endpoint aggregates a subset of per-model fields; pull
  // quantization + capabilities out of each node's own model list.
  const modelMeta = useMemo(() => {
    const out = new Map<string, { quantization?: string; capabilities: string[] }>();
    for (const n of nodes ?? []) {
      for (const mo of n.models ?? []) {
        const cur = out.get(mo.name) ?? { capabilities: [] };
        if (mo.quantization && !cur.quantization) cur.quantization = mo.quantization;
        for (const c of mo.capabilities ?? []) {
          if (!cur.capabilities.includes(c)) cur.capabilities.push(c);
        }
        out.set(mo.name, cur);
      }
    }
    return out;
  }, [nodes]);

  const allOps = ops ?? [];
  const activeOps = allOps.filter(isActive);
  const pastOps = allOps.filter((o) => !isActive(o));

  const confirmRemove = async () => {
    if (!removing) return;
    setRemoveBusy(true);
    setRemoveErr("");
    try {
      await api("POST", "/models/delete", {
        model: removing.model,
        node_ids: removing.nodeIDs,
        ...(removing.lastReplica ? { force: true } : {}),
      });
      setRemoving(null);
      refresh();
    } catch (e) {
      // 409 = last-replica guard: re-show the dialog in its destructive
      // state so the admin confirms knowingly rather than by accident.
      if (e instanceof Error && "status" in e && (e as { status: number }).status === 409) {
        setRemoving({ ...removing, lastReplica: true });
        setRemoveErr("");
      } else {
        setRemoveErr(e instanceof Error ? e.message : String(e));
      }
    } finally {
      setRemoveBusy(false);
    }
  };

  const cancelOp = async (id: string) => {
    setCancelling(id);
    setOpErr("");
    try {
      await api("POST", `/model-operations/${id}/cancel`);
      mutate("/model-operations?limit=30");
    } catch (e) {
      setOpErr(e instanceof Error ? e.message : String(e));
    } finally {
      setCancelling(null);
    }
  };

  return (
    <div className="mx-auto max-w-[1200px] px-6 py-6">
      <header className="mb-6 flex items-end justify-between">
        <div>
          <h1 className="text-[19px] font-semibold tracking-tight">Models</h1>
          <p className="mt-0.5 text-[12.5px] text-[var(--color-text-dim)]">
            Cluster-wide model inventory and placement
          </p>
        </div>
        <button
          className="btn btn-primary btn-sm"
          onClick={() => setInstallOpen(true)}
        >
          <Download size={13} /> Install model
        </button>
      </header>

      {/* Operations in progress */}
      {activeOps.length > 0 && (
        <Panel className="fade-up mb-3">
          <div className="border-b px-4 py-3" style={{ borderColor: "var(--color-border)" }}>
            <div className="label">Operations in progress ({activeOps.length})</div>
          </div>
          {activeOps.map((o) => {
            const pct = o.total > 0 ? Math.min(100, (o.completed / o.total) * 100) : 0;
            return (
              <div key={o.id} className="row-line px-4 py-3" style={{ borderColor: "var(--color-border)" }}>
                <div className="flex flex-wrap items-center gap-3">
                  <span className="mono text-[12px] font-medium">{o.model}</span>
                  <span className="text-[11px] text-[var(--color-text-faint)]">
                    {o.op} → {o.node_name || "node"}
                  </span>
                  <StatusBadge status={o.status} />
                  <span className="mono ml-auto text-[11px] text-[var(--color-text-dim)]">
                    {o.total > 0
                      ? `${fmtBytes(o.completed)} / ${fmtBytes(o.total)}`
                      : o.detail || "starting"}
                  </span>
                  <button
                    className="btn btn-sm"
                    onClick={() => cancelOp(o.id)}
                    disabled={cancelling === o.id}
                    aria-label={`Cancel ${o.op} of ${o.model}`}
                    title="Cancel operation"
                  >
                    <X size={12} />
                  </button>
                </div>
                {o.total > 0 && (
                  <div className="mt-2 flex items-center gap-3">
                    <Meter value={pct} className="flex-1" color="var(--color-accent)" />
                    <span className="mono w-[34px] text-right text-[10.5px] text-[var(--color-text-dim)]">
                      {Math.round(pct)}%
                    </span>
                  </div>
                )}
                {o.total > 0 && o.detail && (
                  <div className="mt-1 text-[10.5px] text-[var(--color-text-faint)]">{o.detail}</div>
                )}
              </div>
            );
          })}
          {opErr && (
            <div className="px-4 py-2.5 text-[11.5px] text-[var(--color-danger)]">{opErr}</div>
          )}
        </Panel>
      )}

      {/* Model inventory */}
      <Panel className="fade-up">
        <div className="border-b px-4 py-3" style={{ borderColor: "var(--color-border)" }}>
          <div className="label">Installed models ({models?.length ?? 0})</div>
        </div>
        {models === undefined ? (
          <SkeletonRows />
        ) : (models ?? []).length === 0 ? (
          <Empty
            title="No models installed"
            hint="Install a model to push it to online nodes, or pull one directly on a node via `ollama pull`."
          >
            <button className="btn btn-primary btn-sm" onClick={() => setInstallOpen(true)}>
              <Download size={13} /> Install model
            </button>
          </Empty>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[760px] text-left">
              <thead>
                <tr className="border-b text-[11px]" style={{ borderColor: "var(--color-border)" }}>
                  <Th>Model</Th>
                  <Th>Engine</Th>
                  <Th>Size</Th>
                  <Th>Digest</Th>
                  <Th>Capabilities</Th>
                  <Th>Replicas</Th>
                  <Th>Needs</Th>
                  <Th>Nodes</Th>
                  <Th>Loaded</Th>
                  <Th></Th>
                </tr>
              </thead>
              <tbody>
                {models.map((m) => {
                  const meta = modelMeta.get(m.name);
                  const pl = placementByName.get(m.name);
                  const ids = m.node_ids ?? [];
                  const names = m.node_names ?? [];
                  const loadedOn = m.loaded_on ?? [];
                  const open = expanded === m.name;
                  return (
                    <React.Fragment key={m.name}>
                    <tr className="row-line" style={{ borderColor: "var(--color-border)" }}>
                      <Td>
                        <button
                          className="flex items-center gap-1"
                          onClick={() => setExpanded(open ? null : m.name)}
                          aria-expanded={open}
                        >
                          {open ? <ChevronDown size={12} /> : <ChevronRight size={12} />}
                          <div className="mono text-[12.5px] font-medium">{m.name}</div>
                        </button>
                        <div className="pl-4 text-[10.5px] text-[var(--color-text-faint)]">
                          {[m.family, m.parameter_size, meta?.quantization]
                            .filter(Boolean)
                            .join(" · ")}
                        </div>
                      </Td>
                      <Td><span className="badge badge-neutral">{m.engine}</span></Td>
                      <Td><span className="mono text-[12px]">{fmtBytes(m.size_bytes)}</span></Td>
                      <Td>
                        {m.digest ? (
                          <code
                            className="mono cursor-pointer text-[10.5px] text-[var(--color-text-faint)] transition-colors hover:text-[var(--color-text-dim)]"
                            title={`${m.digest} — click to copy`}
                            onClick={() => navigator.clipboard.writeText(m.digest!)}
                          >
                            {m.digest.replace(/^sha256:/, "").slice(0, 12)}
                          </code>
                        ) : (
                          <span className="text-[11px] text-[var(--color-text-faint)]">—</span>
                        )}
                      </Td>
                      <Td>
                        {(meta?.capabilities.length ?? 0) > 0 ? (
                          <div className="flex max-w-[180px] flex-wrap gap-1">
                            {meta!.capabilities.map((c) => (
                              <span key={c} className="badge badge-neutral !px-1.5 !py-0">{c}</span>
                            ))}
                          </div>
                        ) : (
                          <span className="text-[11px] text-[var(--color-text-faint)]">—</span>
                        )}
                      </Td>
                      <Td>
                        {pl ? (
                          <span
                            className={`badge ${pl.last_replica ? "badge-warn" : "badge-neutral"}`}
                            title={pl.last_replica ? "single replica — deleting removes cluster availability" : `${pl.replicas} copies in the cluster`}
                          >
                            {pl.replicas} {pl.last_replica ? "⚠" : ""}
                          </span>
                        ) : (
                          <span className="badge badge-neutral">{ids.length}</span>
                        )}
                      </Td>
                      <Td>
                        {pl ? (
                          <span className="mono text-[11.5px] text-[var(--color-text-dim)]" title="estimated memory requirement">
                            ~{pl.est_mem_mb >= 1024 ? `${(pl.est_mem_mb / 1024).toFixed(1)}G` : `${pl.est_mem_mb}M`}
                          </span>
                        ) : (
                          <span className="text-[11px] text-[var(--color-text-faint)]">—</span>
                        )}
                      </Td>
                      <Td>
                        <div className="flex max-w-[240px] flex-wrap gap-1.5">
                          {ids.map((id, i) => {
                            const node = nodeById.get(id);
                            const label = names[i] ?? node?.name ?? id.slice(0, 8);
                            const offline = node ? !node.online : false;
                            return (
                              <span
                                key={id}
                                className={`badge ${offline ? "badge-neutral opacity-50" : "badge-neutral"}`}
                                title={node ? (node.online ? "online" : "offline") : "node"}
                              >
                                {label}
                                <button
                                  className="-mr-1 rounded p-0.5 text-[var(--color-text-faint)] transition-colors hover:text-[var(--color-danger)]"
                                  aria-label={`Remove ${m.name} from ${label}`}
                                  title={`Remove from ${label}`}
                                  onClick={() =>
                                    setRemoving({ model: m.name, nodeIDs: [id], nodeNames: [label] })
                                  }
                                >
                                  <X size={10} />
                                </button>
                              </span>
                            );
                          })}
                        </div>
                      </Td>
                      <Td>
                        {loadedOn.length > 0 ? (
                          <span className="badge badge-ok"><span className="dot" />loaded on {loadedOn.length}</span>
                        ) : (
                          <span className="badge badge-neutral">idle</span>
                        )}
                      </Td>
                      <Td className="text-right">
                        <button
                          className="btn btn-sm"
                          onClick={() =>
                            setRemoving({
                              model: m.name,
                              nodeIDs: ids,
                              nodeNames: names.length ? names : ids.map((id) => nodeById.get(id)?.name ?? id.slice(0, 8)),
                            })
                          }
                        >
                          <Trash2 size={12} /> Remove
                        </button>
                      </Td>
                    </tr>
                    {open && (
                      <tr className="row-line" style={{ borderColor: "var(--color-border)" }}>
                        <td colSpan={8} className="px-4 py-3">
                          <PlacementDetail pl={pl} />
                        </td>
                      </tr>
                    )}
                    </React.Fragment>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </Panel>

      {/* Operation history */}
      {pastOps.length > 0 && (
        <Panel className="fade-up mt-3">
          <div className="border-b px-4 py-3" style={{ borderColor: "var(--color-border)" }}>
            <div className="label">Operation history</div>
          </div>
          {pastOps.map((o) => (
            <div
              key={o.id}
              className="row-line flex flex-wrap items-center gap-3 px-4 py-2"
              style={{ borderColor: "var(--color-border)" }}
            >
              <span className="mono text-[12px]">{o.model}</span>
              <span className="text-[11px] text-[var(--color-text-faint)]">
                {o.op} → {o.node_name || "—"}
              </span>
              <StatusBadge status={o.status} />
              {o.error && (
                <span className="text-[11px] text-[var(--color-danger)]">{o.error}</span>
              )}
              <span className="ml-auto text-[10.5px] text-[var(--color-text-faint)]">
                {fmtAgo(o.updated_at || o.created_at)}
              </span>
            </div>
          ))}
        </Panel>
      )}

      {installOpen && (
        <InstallDialog
          nodes={nodes ?? []}
          onClose={() => setInstallOpen(false)}
          onDone={refresh}
        />
      )}

      {removing && (
        <ConfirmDialog
          title="Remove model"
          confirmLabel="Remove"
          requireText={removing.model}
          busy={removeBusy}
          error={removeErr}
          onConfirm={confirmRemove}
          onClose={() => !removeBusy && setRemoving(null)}
        >
          Remove <span className="mono text-[var(--color-text)]">{removing.model}</span>{" "}
          {removing.nodeIDs.length <= 1 ? (
            <>from <span className="text-[var(--color-text)]">{removing.nodeNames[0] ?? "this node"}</span></>
          ) : (
            <>from <span className="text-[var(--color-text)]">{removing.nodeIDs.length} nodes</span> ({removing.nodeNames.join(", ")})</>
          )}
          ? The delete is dispatched to every online target; offline nodes keep
          their copy until removed separately.
          {removing.lastReplica && (
            <span className="mt-2 block text-[11.5px] text-[var(--color-danger)]">
              This removes the model&apos;s last replica — it becomes unschedulable
              cluster-wide until re-installed. Confirm only if intended.
            </span>
          )}
        </ConfirmDialog>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------

function InstallDialog({
  nodes,
  onClose,
  onDone,
}: {
  nodes: Node[];
  onClose: () => void;
  onDone: () => void;
}) {
  const [model, setModel] = useState("");
  const [targets, setTargets] = useState<Set<string>>(new Set());
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  const online = nodes.filter((n) => n.online && n.status === "approved");

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  const submit = async () => {
    if (!model.trim() || busy) return;
    setBusy(true);
    setErr("");
    try {
      // Empty node_ids = all online approved nodes (server-side default).
      await api("POST", "/models/pull", {
        model: model.trim(),
        node_ids: targets.size ? [...targets] : [],
      });
      onDone();
      onClose();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      setBusy(false);
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
        aria-label="Install model"
        className="panel fade-up w-full max-w-[460px] px-5 py-5"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-start justify-between">
          <div>
            <h2 className="text-[15px] font-semibold">Install model</h2>
            <p className="mt-0.5 text-[12px] text-[var(--color-text-dim)]">
              Pull a model onto cluster nodes.
            </p>
          </div>
          <button className="btn btn-sm -mr-1 -mt-1" onClick={onClose} aria-label="Close dialog">
            <X size={14} />
          </button>
        </div>

        <div className="mt-4">
          <div className="label mb-1.5">Model</div>
          <input
            className="input mono"
            placeholder="e.g. llama3.2:1b, mistral:7b"
            value={model}
            onChange={(e) => setModel(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && submit()}
            autoFocus
            spellCheck={false}
          />
        </div>

        <div className="mt-4">
          <div className="label mb-1.5">Target nodes</div>
          <div className="flex flex-wrap items-center gap-1.5">
            <button
              className={`btn btn-sm ${targets.size === 0 ? "btn-primary" : ""}`}
              onClick={() => setTargets(new Set())}
            >
              All online nodes ({online.length})
            </button>
            {online.map((n) => (
              <button
                key={n.id}
                className={`btn btn-sm ${targets.has(n.id) ? "btn-primary" : ""}`}
                onClick={() =>
                  setTargets((s) => {
                    const next = new Set(s);
                    if (next.has(n.id)) next.delete(n.id);
                    else next.add(n.id);
                    return next;
                  })
                }
              >
                {n.name}
              </button>
            ))}
          </div>
          {online.length === 0 && (
            <p className="mt-2 text-[11.5px] text-[var(--color-warn)]">
              No online approved nodes — nothing can receive the pull.
            </p>
          )}
        </div>

        {err && (
          <div className="mt-3 rounded-md border border-red-500/30 bg-red-500/10 px-3 py-2 text-[12px] text-red-400">
            {err}
          </div>
        )}

        <div className="mt-4 flex justify-end gap-2">
          <button className="btn btn-sm" onClick={onClose} disabled={busy}>
            Cancel
          </button>
          <button
            className="btn btn-sm btn-primary"
            onClick={submit}
            disabled={busy || !model.trim() || online.length === 0}
          >
            <Download size={12} /> {busy ? "Dispatching…" : "Install"}
          </button>
        </div>
      </div>
    </div>
  );
}

// Per-model placement breakdown: which nodes host it (loaded state,
// resident size, digest) and which online nodes could take another
// replica — each with an honest eligibility reason.
function PlacementDetail({ pl }: { pl?: PlacementEntry }) {
  if (!pl) {
    return (
      <div className="text-[11.5px] text-[var(--color-text-faint)]">
        Placement data not available yet.
      </div>
    );
  }
  return (
    <div className="grid gap-4 sm:grid-cols-2">
      <div>
        <div className="label mb-2">Hosted on ({pl.nodes?.length ?? 0})</div>
        <div className="space-y-1.5">
          {(pl.nodes ?? []).map((n) => (
            <div key={n.id} className="flex items-center gap-2 text-[12px]">
              <span className={n.online ? "text-[var(--color-text)]" : "text-[var(--color-text-faint)]"}>
                {n.name}
              </span>
              {!n.online && <span className="badge badge-neutral">offline</span>}
              {n.loaded ? (
                <span className="badge badge-ok">
                  <span className="dot" />
                  {n.loaded_mb ? `resident ${n.loaded_mb}M` : "loaded"}
                </span>
              ) : (
                <span className="badge badge-neutral">cold</span>
              )}
              {n.digest && (
                <code className="mono text-[10px] text-[var(--color-text-faint)]">
                  {n.digest.replace(/^sha256:/, "").slice(0, 10)}
                </code>
              )}
            </div>
          ))}
        </div>
        {pl.active_ops && pl.active_ops.length > 0 && (
          <div className="mt-2 text-[11px] text-[var(--color-warn)]">
            active: {pl.active_ops.join(", ")}
          </div>
        )}
      </div>
      <div>
        <div className="label mb-2">Can also host ({(pl.candidates ?? []).length})</div>
        {(pl.candidates ?? []).length === 0 ? (
          <div className="text-[11.5px] text-[var(--color-text-faint)]">
            No other online nodes — single-replica model.
          </div>
        ) : (
          <div className="space-y-1.5">
            {(pl.candidates ?? []).map((c) => (
              <div key={c.id} className="flex items-center gap-2 text-[12px]">
                <span className={c.eligible ? "text-[var(--color-text)]" : "text-[var(--color-text-faint)]"}>
                  {c.name}
                </span>
                {c.eligible ? (
                  <span className="badge badge-ok">eligible</span>
                ) : (
                  <span className="badge badge-neutral">ineligible</span>
                )}
                {c.free_mb != null && (
                  <span className="mono text-[10.5px] text-[var(--color-text-dim)]">
                    {(c.free_mb / 1024).toFixed(1)}G free
                  </span>
                )}
                <span className="text-[10.5px] text-[var(--color-text-faint)]">{c.reason}</span>
              </div>
            ))}
          </div>
        )}
        <div className="mt-2 text-[10.5px] text-[var(--color-text-faint)]">
          est. memory need ~{pl.est_mem_mb >= 1024 ? `${(pl.est_mem_mb / 1024).toFixed(1)}G` : `${pl.est_mem_mb}M`}
          {" · "}{pl.loaded_on} loaded replica{pl.loaded_on === 1 ? "" : "s"}
        </div>
      </div>
    </div>
  );
}

function SkeletonRows() {
  return (
    <div>
      {Array.from({ length: 4 }).map((_, i) => (
        <div key={i} className="row-line flex items-center gap-5 px-4 py-4">
          <div className="h-3 w-44 animate-pulse rounded bg-white/[0.06]" />
          <div className="h-3 w-14 animate-pulse rounded bg-white/[0.06]" />
          <div className="h-3 w-20 animate-pulse rounded bg-white/[0.06]" />
          <div className="h-3 flex-1 animate-pulse rounded bg-white/[0.06]" />
          <div className="h-3 w-16 animate-pulse rounded bg-white/[0.06]" />
        </div>
      ))}
    </div>
  );
}

function Th({ children }: { children?: React.ReactNode }) {
  return <th className="label px-4 py-2.5 font-medium">{children}</th>;
}
function Td({ children, className }: { children?: React.ReactNode; className?: string }) {
  return <td className={`px-4 py-3 align-middle ${className ?? ""}`}>{children}</td>;
}
