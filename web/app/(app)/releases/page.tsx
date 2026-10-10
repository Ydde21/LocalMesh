"use client";

import { Suspense, useEffect, useRef, useState } from "react";
import Link from "next/link";
import { useSearchParams, useRouter } from "next/navigation";
import {
  useReleases, useReleaseKeys, useRollouts, useRollout, useAgentUpdates,
  useNodes, api,
} from "@/lib/api";
import { fmtAgo, fmtVersion, agentVersion, versionGt } from "@/lib/format";
import { Panel, Meter, Empty } from "@/components/ui";
import { StatusBadge } from "@/components/status";
import { UpdatePhaseStepper } from "@/components/update-phase";
import { ConfirmDialog } from "@/components/confirm";
import type { Node, Release, ReleaseKey, Rollout } from "@/lib/types";
import {
  X, Plus, Rocket, KeyRound, PackageCheck, Ban, Check,
  ChevronRight, Upload,
} from "lucide-react";
import { useSWRConfig } from "swr";

export default function ReleasesPage() {
  return (
    <Suspense fallback={null}>
      <ReleasesInner />
    </Suspense>
  );
}

// The three v0.7 endpoints ship together — when every one of them errors
// (404 on pre-v0.7 servers) the page collapses to a single empty state.
function ReleasesInner() {
  const params = useSearchParams();
  const selRollout = params.get("id");
  const releases = useReleases();
  const keys = useReleaseKeys();
  const rollouts = useRollouts();
  const [registerOpen, setRegisterOpen] = useState(false);
  const [rolloutOpen, setRolloutOpen] = useState(false);

  const unavailable = !!(releases.error && keys.error && rollouts.error);
  const approved = (releases.data ?? []).filter((r) => r.status === "approved");

  return (
    <div className="mx-auto max-w-[1200px] px-6 py-6">
      <header className="mb-6 flex items-end justify-between">
        <div>
          <h1 className="text-[19px] font-semibold tracking-tight">Releases</h1>
          <p className="mt-0.5 text-[12.5px] text-[var(--color-text-dim)]">
            Signed agent releases and update rollouts
          </p>
        </div>
        <div className="flex gap-2">
          <button
            className="btn btn-sm"
            onClick={() => setRolloutOpen(true)}
            disabled={approved.length === 0}
            title={approved.length === 0 ? "register an approved release first" : undefined}
          >
            <Rocket size={13} /> New rollout
          </button>
          <button className="btn btn-primary btn-sm" onClick={() => setRegisterOpen(true)}>
            <Plus size={13} /> Register release
          </button>
        </div>
      </header>

      {unavailable ? (
        <Panel>
          <Empty
            title="Release registry unavailable"
            hint="This control plane doesn't expose the v0.7 release API — upgrade the server to manage signed releases and agent rollouts here."
          />
        </Panel>
      ) : (
        <div className="space-y-3">
          <RolloutsPanel rollouts={rollouts.data} selectedId={selRollout} />
          <ReleasesPanel releases={releases.data} />
          <KeysPanel keysData={keys.data} />
        </div>
      )}

      {registerOpen && <RegisterDialog onClose={() => setRegisterOpen(false)} />}
      {rolloutOpen && <RolloutDialog onClose={() => setRolloutOpen(false)} />}
      {selRollout && <RolloutDrawer id={selRollout} />}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Rollouts

function RolloutsPanel({
  rollouts,
  selectedId,
}: {
  rollouts?: Rollout[];
  selectedId: string | null;
}) {
  const { mutate: gm } = useSWRConfig();
  const [busyId, setBusyId] = useState("");
  const [err, setErr] = useState("");
  const [cancelling, setCancelling] = useState<Rollout | null>(null);

  if (!rollouts) return null;

  // Row-level actions mirror the drawer's: approve is a safe advance,
  // cancel is destructive and gets a confirm. Buttons live inside the
  // row's <a> so they must swallow the click before it navigates.
  const act = async (r: Rollout, fn: () => Promise<unknown>) => {
    setBusyId(r.id);
    setErr("");
    try {
      await fn();
      gm(
        (key) =>
          typeof key === "string" &&
          (key.startsWith("/rollouts") || key.startsWith("/agent-updates"))
      );
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusyId("");
    }
  };

  const cancellable = (r: Rollout) =>
    r.state === "pending" || r.state === "running" || r.state === "awaiting_approval";

  return (
    <Panel className="fade-up">
      <div className="border-b px-4 py-3" style={{ borderColor: "var(--color-border)" }}>
        <div className="label">Rollouts ({rollouts.length})</div>
      </div>
      {err && (
        <div className="border-b border-red-500/30 bg-red-500/10 px-4 py-2 text-[12px] text-red-400">
          {err}
        </div>
      )}
      {rollouts.length === 0 ? (
        <div className="px-4 py-8 text-center text-[12px] text-[var(--color-text-faint)]">
          No rollouts yet — pick a release and start one.
        </div>
      ) : (
        <div>
          {rollouts.map((r) => {
            const pct = r.total > 0 ? (r.succeeded / r.total) * 100 : 0;
            const active =
              r.state === "running" || r.state === "pending" || r.state === "awaiting_approval";
            return (
              <Link
                key={r.id}
                href={`/releases?id=${r.id}`}
                className={`row-line flex flex-wrap items-center gap-3 px-4 py-3 transition-colors hover:bg-white/[0.03] ${
                  r.id === selectedId ? "bg-white/[0.04]" : ""
                }`}
                style={{ borderColor: "var(--color-border)" }}
              >
                <span className="mono text-[12.5px] font-medium">
                  {fmtVersion(r.release_version || "")}
                </span>
                <span className="badge badge-neutral">{r.strategy}</span>
                <StatusBadge status={r.state} />
                {r.approved_by && (
                  <span
                    className="badge badge-neutral !px-1.5 !py-0"
                    title={`approved by ${r.approved_by}${r.approved_at ? ` · ${fmtAgo(r.approved_at)}` : ""}`}
                  >
                    approved
                  </span>
                )}
                {r.error && (
                  <span
                    className="max-w-[260px] truncate text-[11px] text-[var(--color-danger)]"
                    title={r.error}
                  >
                    {r.error}
                  </span>
                )}
                <span className="ml-auto flex items-center gap-3">
                  {r.state === "awaiting_approval" && (
                    <button
                      className="btn btn-primary btn-sm"
                      disabled={busyId === r.id}
                      aria-label={`Approve rollout ${r.release_version || r.id}`}
                      onClick={(e) => {
                        e.preventDefault();
                        e.stopPropagation();
                        act(r, () => api("POST", `/rollouts/${r.id}/approve`));
                      }}
                    >
                      <Check size={12} /> Approve
                    </button>
                  )}
                  {cancellable(r) && (
                    <button
                      className="btn btn-danger btn-sm"
                      disabled={busyId === r.id}
                      aria-label={`Cancel rollout ${r.release_version || r.id}`}
                      onClick={(e) => {
                        e.preventDefault();
                        e.stopPropagation();
                        setErr("");
                        setCancelling(r);
                      }}
                    >
                      <Ban size={12} /> Cancel
                    </button>
                  )}
                  <span className="mono text-[11px] text-[var(--color-text-dim)]">
                    {r.succeeded}/{r.total} nodes
                    {r.failed > 0 && (
                      <span className="text-[var(--color-danger)]"> · {r.failed} failed</span>
                    )}
                  </span>
                  <Meter
                    value={pct}
                    className="w-[90px]"
                    color={
                      r.state === "failed"
                        ? "var(--color-danger)"
                        : active
                          ? "var(--color-accent)"
                          : "var(--color-online)"
                    }
                  />
                  <span className="text-[10.5px] text-[var(--color-text-faint)]">
                    {fmtAgo(r.updated_at || r.created_at)}
                  </span>
                  <ChevronRight size={12} className="text-[var(--color-text-faint)]" />
                </span>
              </Link>
            );
          })}
        </div>
      )}

      {cancelling && (
        <ConfirmDialog
          title="Cancel rollout"
          confirmLabel="Cancel rollout"
          busy={busyId === cancelling.id}
          error={err}
          onClose={() => {
            if (!busyId) {
              setCancelling(null);
              setErr("");
            }
          }}
          onConfirm={() =>
            act(cancelling, async () => {
              await api("POST", `/rollouts/${cancelling.id}/cancel`);
              setCancelling(null);
            })
          }
        >
          Stop rollout to{" "}
          <span className="mono text-[var(--color-text)]">
            {fmtVersion(cancelling.release_version || "")}
          </span>
          ? Nodes mid-update finish their current phase; queued nodes are never
          dispatched.
        </ConfirmDialog>
      )}
    </Panel>
  );
}

// Detail drawer: campaign progress + per-node update rows. Primary data
// comes from GET /rollouts/{id}; when that endpoint is absent we fall
// back to the row already in the list cache.
function RolloutDrawer({ id }: { id: string }) {
  const router = useRouter();
  const { data: detail } = useRollout(id);
  const { data: rollouts } = useRollouts();
  const { data: updates } = useAgentUpdates({ rollout_id: id });
  const { mutate: gm } = useSWRConfig();
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [confirmCancel, setConfirmCancel] = useState(false);

  const r = detail ?? (rollouts ?? []).find((x) => x.id === id);
  if (!r) return null;

  const rows = (updates ?? []).slice().sort((a, b) => a.created_at.localeCompare(b.created_at));
  const cancellable = r.state === "pending" || r.state === "running" || r.state === "awaiting_approval";

  const act = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    setErr("");
    try {
      await fn();
      gm(
        (key) =>
          typeof key === "string" &&
          (key.startsWith("/rollouts") || key.startsWith("/agent-updates"))
      );
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div
      className="fixed inset-0 z-40 bg-black/50 backdrop-blur-[2px]"
      onClick={() => router.push("/releases")}
    >
      <div
        className="panel absolute right-0 top-0 h-full w-[520px] max-w-[94vw] overflow-y-auto border-l"
        style={{ borderColor: "var(--color-border)", borderRadius: 0 }}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-start justify-between border-b px-5 py-4" style={{ borderColor: "var(--color-border)" }}>
          <div>
            <div className="flex items-center gap-2.5">
              <h2 className="mono text-[15px] font-semibold">
                {fmtVersion(r.release_version || "")}
              </h2>
              <span className="badge badge-neutral">{r.strategy}</span>
              <StatusBadge status={r.state} />
            </div>
            <div className="mono mt-1 text-[11px] text-[var(--color-text-faint)]">{r.id}</div>
          </div>
          <Link href="/releases" className="btn btn-sm" aria-label="Close rollout details">
            <X size={14} />
          </Link>
        </div>

        <div className="space-y-5 px-5 py-5">
          {err && (
            <div className="rounded-md border border-red-500/30 bg-red-500/10 px-3 py-2 text-[12px] text-red-400">
              {err}
            </div>
          )}

          {/* Progress */}
          <section>
            <div className="mb-1.5 flex items-baseline justify-between">
              <div className="label">Progress</div>
              <div className="mono text-[11.5px] text-[var(--color-text-dim)]">
                {r.succeeded}/{r.total} succeeded
                {r.failed > 0 && (
                  <span className="text-[var(--color-danger)]"> · {r.failed} failed</span>
                )}
                {r.pending > 0 && <span> · {r.pending} pending</span>}
              </div>
            </div>
            <Meter
              value={r.total > 0 ? (r.succeeded / r.total) * 100 : 0}
              color={r.state === "failed" ? "var(--color-danger)" : "var(--color-online)"}
            />
            <div className="mt-2 grid grid-cols-2 gap-2 text-[11.5px]">
              <KV2 label="Created" value={fmtAgo(r.created_at)} />
              <KV2 label="By" value={r.created_by || "—"} />
              {r.approved_by && (
                <KV2
                  label="Approved"
                  value={`${r.approved_by}${r.approved_at ? ` · ${fmtAgo(r.approved_at)}` : ""}`}
                />
              )}
              {r.strategy === "canary" && (
                <>
                  <KV2 label="Canary" value={String(r.canary_count)} />
                  <KV2 label="Batch size" value={String(r.batch_size)} />
                </>
              )}
              {r.strategy === "parallel" && <KV2 label="Batch size" value={String(r.batch_size)} />}
            </div>
            {r.error && (
              <div className="mt-2.5 rounded-md border border-red-500/30 bg-red-500/10 px-3 py-2 text-[12px] text-red-400">
                {r.error}
              </div>
            )}
          </section>

          {/* Actions */}
          {(r.state === "awaiting_approval" || cancellable) && (
            <section className="flex gap-2">
              {r.state === "awaiting_approval" && (
                <button
                  className="btn btn-primary btn-sm"
                  disabled={busy}
                  onClick={() => act(() => api("POST", `/rollouts/${r.id}/approve`))}
                >
                  <Check size={12} /> Approve next batch
                </button>
              )}
              {cancellable && (
                <button
                  className="btn btn-danger btn-sm"
                  disabled={busy}
                  onClick={() => {
                    setErr("");
                    setConfirmCancel(true);
                  }}
                >
                  <Ban size={12} /> Cancel rollout
                </button>
              )}
            </section>
          )}

          {/* Per-node updates */}
          <section>
            <div className="label mb-2.5">Nodes ({rows.length})</div>
            {rows.length === 0 ? (
              <div className="text-[12px] text-[var(--color-text-faint)]">
                {updates ? "No node updates recorded for this rollout." : "Loading…"}
              </div>
            ) : (
              <div className="space-y-2.5">
                {rows.map((u) => (
                  <div
                    key={u.id}
                    className="rounded-md bg-white/[0.03] px-3 py-2.5"
                  >
                    <div className="mb-1.5 flex items-center gap-2">
                      <span className="text-[12.5px] font-medium">
                        {u.node_name || u.node_id.slice(0, 8)}
                      </span>
                    </div>
                    <UpdatePhaseStepper update={u} />
                  </div>
                ))}
              </div>
            )}
          </section>
        </div>
      </div>

      {confirmCancel && (
        <ConfirmDialog
          title="Cancel rollout"
          confirmLabel="Cancel rollout"
          busy={busy}
          error={err}
          onClose={() => setConfirmCancel(false)}
          onConfirm={() =>
            act(async () => {
              await api("POST", `/rollouts/${r.id}/cancel`);
              setConfirmCancel(false);
            })
          }
        >
          Stop this rollout? Nodes mid-update finish their current phase; queued
          nodes are never dispatched.
        </ConfirmDialog>
      )}
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
// Releases table

function ReleasesPanel({ releases }: { releases?: Release[] }) {
  const { mutate } = useSWRConfig();
  const [revoking, setRevoking] = useState<Release | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  if (!releases) return null;

  const confirmRevoke = async () => {
    if (!revoking) return;
    setBusy(true);
    setErr("");
    try {
      await api("DELETE", `/releases/${revoking.id}`);
      setRevoking(null);
      mutate("/releases");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Panel className="fade-up">
      <div className="border-b px-4 py-3" style={{ borderColor: "var(--color-border)" }}>
        <div className="label">Releases ({releases.length})</div>
      </div>
      {releases.length === 0 ? (
        <Empty
          title="No releases registered"
          hint="Register a signed release (manifest + SHA256SUMS + signatures) — the server verifies the signature before accepting it."
        />
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[720px] text-left">
            <thead>
              <tr className="border-b text-[11px]" style={{ borderColor: "var(--color-border)" }}>
                <Th>Version</Th>
                <Th>Channel</Th>
                <Th>Signed by</Th>
                <Th>Protocol</Th>
                <Th>Platforms</Th>
                <Th>Status</Th>
                <Th>Registered</Th>
                <Th></Th>
              </tr>
            </thead>
            <tbody>
              {releases.map((r) => (
                <tr key={r.id} className="row-line" style={{ borderColor: "var(--color-border)" }}>
                  <Td>
                    <div className="mono text-[12.5px] font-medium">{fmtVersion(r.version)}</div>
                    {r.notes && (
                      <div
                        className="max-w-[220px] truncate text-[10.5px] text-[var(--color-text-faint)]"
                        title={r.notes}
                      >
                        {r.notes}
                      </div>
                    )}
                  </Td>
                  <Td><span className="badge badge-neutral">{r.channel}</span></Td>
                  <Td>
                    <span
                      className="mono text-[10.5px] text-[var(--color-text-faint)]"
                      title={`signer key ${r.signer_key_id} · manifest sha256 ${r.manifest_sha256}`}
                    >
                      {r.signer_key_id.length > 14
                        ? `${r.signer_key_id.slice(0, 12)}…`
                        : r.signer_key_id}
                    </span>
                  </Td>
                  <Td>
                    <span
                      className="mono text-[11px] text-[var(--color-text-dim)]"
                      title={`release protocol ${r.protocol_version}; requires agent protocol ≥ ${r.min_agent_protocol}`}
                    >
                      {r.protocol_version} / ≥{r.min_agent_protocol}
                    </span>
                  </Td>
                  <Td>
                    <span
                      className="mono text-[11px] text-[var(--color-text-dim)]"
                      title={(r.platforms ?? []).join("\n")}
                    >
                      {(r.platforms ?? []).length}
                    </span>
                  </Td>
                  <Td><StatusBadge status={r.status} /></Td>
                  <Td>
                    <span className="text-[11px] text-[var(--color-text-faint)]">
                      {fmtAgo(r.created_at)}
                    </span>
                  </Td>
                  <Td className="text-right">
                    {r.status === "approved" && (
                      <button
                        className="btn btn-sm btn-danger"
                        onClick={() => {
                          setErr("");
                          setRevoking(r);
                        }}
                        title="Revoke — stops new rollouts and agent dispatches of this release"
                      >
                        Revoke
                      </button>
                    )}
                  </Td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {revoking && (
        <ConfirmDialog
          title="Revoke release"
          confirmLabel="Revoke"
          busy={busy}
          error={err}
          onClose={() => !busy && setRevoking(null)}
          onConfirm={confirmRevoke}
        >
          Revoke <span className="mono text-[var(--color-text)]">{fmtVersion(revoking.version)}</span>
          ? It stays in the registry for audit but can no longer be dispatched to agents;
          in-flight updates are unaffected.
        </ConfirmDialog>
      )}
    </Panel>
  );
}

// ---------------------------------------------------------------------------
// Signing keys

function KeysPanel({ keysData }: { keysData?: ReleaseKey[] }) {
  const { mutate } = useSWRConfig();
  const [pub, setPub] = useState("");
  const [comment, setComment] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [revoking, setRevoking] = useState<ReleaseKey | null>(null);

  if (!keysData) return null;

  const register = async () => {
    if (!pub.trim() || busy) return;
    setBusy(true);
    setErr("");
    try {
      await api("POST", "/release-keys", { public_key: pub.trim(), comment: comment.trim() });
      setPub("");
      setComment("");
      mutate("/release-keys");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const confirmRevoke = async () => {
    if (!revoking) return;
    setBusy(true);
    setErr("");
    try {
      await api("DELETE", `/release-keys/${revoking.id}`);
      setRevoking(null);
      mutate("/release-keys");
    } catch (e) {
      // Keep the dialog open so the server's message stays in view.
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Panel className="fade-up">
      <div className="border-b px-4 py-3" style={{ borderColor: "var(--color-border)" }}>
        <div className="flex items-center gap-2">
          <KeyRound size={13} className="text-[var(--color-accent)]" />
          <div className="label">Signing keys ({keysData.length})</div>
        </div>
      </div>
      <div className="px-4 py-4">
        <div className="flex flex-col gap-2 sm:flex-row">
          <div className="flex-1">
            <FileArea
              value={pub}
              onChange={setPub}
              placeholder="release public key — minisign .pub contents or PEM"
              rows={2}
            />
          </div>
          <div className="flex flex-col gap-2 sm:w-[220px]">
            <input
              className="input"
              placeholder="comment (e.g. maintainer key)"
              value={comment}
              onChange={(e) => setComment(e.target.value)}
            />
            <button
              className="btn btn-primary btn-sm"
              disabled={busy || !pub.trim()}
              onClick={register}
            >
              <Plus size={12} /> Register key
            </button>
          </div>
        </div>
        {err && (
          <div className="mt-2.5 rounded-md border border-red-500/30 bg-red-500/10 px-3 py-2 text-[12px] text-red-400">
            {err}
          </div>
        )}

        <div className="mt-3 space-y-1.5">
          {keysData.map((k) => (
            <div
              key={k.id}
              className="row-line flex flex-wrap items-center gap-3 py-2"
              style={{ borderColor: "var(--color-border)" }}
            >
              <span className="mono text-[11.5px] text-[var(--color-text)]" title={k.key_id}>
                {k.key_id.length > 16 ? `${k.key_id.slice(0, 16)}…` : k.key_id}
              </span>
              <span className="badge badge-neutral !px-1.5 !py-0">{k.kind}</span>
              {k.comment && (
                <span className="text-[11.5px] text-[var(--color-text-dim)]">{k.comment}</span>
              )}
              <span className="text-[10.5px] text-[var(--color-text-faint)]">
                {fmtAgo(k.created_at)}
              </span>
              {k.revoked_at && <span className="badge badge-neutral">revoked</span>}
              {!k.revoked_at && (
                <button
                  className="btn btn-sm btn-danger ml-auto"
                  onClick={() => {
                    setErr("");
                    setRevoking(k);
                  }}
                >
                  Revoke
                </button>
              )}
            </div>
          ))}
          {keysData.length === 0 && (
            <div className="py-2 text-[12px] text-[var(--color-text-faint)]">
              No keys — register a trusted signing public key before registering releases.
            </div>
          )}
        </div>
      </div>

      {revoking && (
        <ConfirmDialog
          title="Revoke signing key"
          confirmLabel="Revoke key"
          busy={busy}
          error={err}
          onClose={() => !busy && setRevoking(null)}
          onConfirm={confirmRevoke}
        >
          Revoke key <span className="mono text-[var(--color-text)]">{revoking.key_id}</span>? New
          releases signed by it can no longer be registered. Existing releases are unaffected.
        </ConfirmDialog>
      )}
    </Panel>
  );
}

// ---------------------------------------------------------------------------
// Register release dialog

// Textarea with a file-picker button — the release inputs are long
// base64/JSON blobs that almost always come from files on disk.
function FileArea({
  value,
  onChange,
  placeholder,
  rows = 4,
  mono = true,
}: {
  value: string;
  onChange: (v: string) => void;
  placeholder: string;
  rows?: number;
  mono?: boolean;
}) {
  const fileRef = useRef<HTMLInputElement>(null);
  return (
    <div>
      <textarea
        className={`input ${mono ? "mono" : ""} w-full resize-y !text-[11.5px] leading-relaxed`}
        rows={rows}
        placeholder={placeholder}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        spellCheck={false}
      />
      <div className="mt-1 flex items-center gap-2">
        <button className="btn btn-sm" onClick={() => fileRef.current?.click()}>
          <Upload size={11} /> Load file
        </button>
        {value && (
          <span className="text-[10.5px] text-[var(--color-text-faint)]">
            {value.length} chars
          </span>
        )}
        <input
          ref={fileRef}
          type="file"
          className="hidden"
          onChange={async (e) => {
            const f = e.target.files?.[0];
            if (f) onChange(await f.text());
            e.target.value = "";
          }}
        />
      </div>
    </div>
  );
}

function RegisterDialog({ onClose }: { onClose: () => void }) {
  const { mutate } = useSWRConfig();
  const [manifest, setManifest] = useState("");
  const [sums, setSums] = useState("");
  const [sigs, setSigs] = useState("");
  const [baseUrl, setBaseUrl] = useState("");
  const [channel, setChannel] = useState("stable");
  const [notes, setNotes] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  // Client-side pre-checks only — the server does the real signature
  // verification and returns the failure reason verbatim.
  const precheck = (): string => {
    if (!manifest.trim()) return "release-manifest.json is required";
    try {
      JSON.parse(manifest);
    } catch {
      return "manifest is not valid JSON";
    }
    if (!sums.trim()) return "SHA256SUMS is required";
    if (!sigs.trim()) return "signatures JSON is required";
    try {
      const s = JSON.parse(sigs);
      if (!s || typeof s !== "object" || Array.isArray(s))
        return "signatures must be a JSON object of {filename: signature}";
    } catch {
      return "signatures is not valid JSON";
    }
    if (!baseUrl.trim()) return "base_url is required (where agents fetch artifacts)";
    return "";
  };

  const submit = async () => {
    const problem = precheck();
    if (problem) {
      setErr(problem);
      return;
    }
    setBusy(true);
    setErr("");
    try {
      await api("POST", "/releases", {
        manifest_json: manifest,
        sha256sums: sums,
        signatures: JSON.parse(sigs),
        base_url: baseUrl.trim(),
        channel: channel.trim() || "stable",
        notes: notes.trim(),
      });
      mutate("/releases");
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
        aria-label="Register release"
        className="panel fade-up max-h-[92vh] w-full max-w-[640px] overflow-y-auto px-5 py-5"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-start justify-between">
          <div>
            <h2 className="text-[15px] font-semibold">Register release</h2>
            <p className="mt-0.5 text-[12px] text-[var(--color-text-dim)]">
              The control plane verifies the signature against a registered key
              before the release enters the registry.
            </p>
          </div>
          <button className="btn btn-sm -mr-1 -mt-1" onClick={onClose} aria-label="Close dialog">
            <X size={14} />
          </button>
        </div>

        <div className="mt-4 space-y-4">
          <div>
            <div className="label mb-1.5">release-manifest.json</div>
            <FileArea value={manifest} onChange={setManifest} rows={4}
              placeholder='{"version":"0.7.0","protocol":1,"artifacts":[…]}' />
          </div>
          <div>
            <div className="label mb-1.5">SHA256SUMS</div>
            <FileArea value={sums} onChange={setSums} rows={3}
              placeholder={"<sha256>  localmesh-agent-<ver>-<os>-<arch>\n…"} />
          </div>
          <div>
            <div className="label mb-1.5">Signatures</div>
            <FileArea value={sigs} onChange={setSigs} rows={3}
              placeholder='{"SHA256SUMS.minisig": "…", "release-manifest.json.minisig": "…"}' />
          </div>
          <div className="grid gap-3 sm:grid-cols-3">
            <div className="sm:col-span-2">
              <div className="label mb-1.5">Base URL</div>
              <input className="input mono" placeholder="https://releases.example.com/v0.7.0"
                value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)} spellCheck={false} />
            </div>
            <div>
              <div className="label mb-1.5">Channel</div>
              <input className="input mono" value={channel}
                onChange={(e) => setChannel(e.target.value)} spellCheck={false} />
            </div>
          </div>
          <div>
            <div className="label mb-1.5">Notes (optional)</div>
            <input className="input" placeholder="what's in this release"
              value={notes} onChange={(e) => setNotes(e.target.value)} />
          </div>
        </div>

        {err && (
          <div className="mt-3.5 rounded-md border border-red-500/30 bg-red-500/10 px-3 py-2 text-[12px] text-red-400">
            {err}
          </div>
        )}

        <div className="mt-4 flex justify-end gap-2">
          <button className="btn btn-sm" onClick={onClose} disabled={busy}>
            Cancel
          </button>
          <button className="btn btn-sm btn-primary" onClick={submit} disabled={busy}>
            <PackageCheck size={12} /> {busy ? "Verifying…" : "Register"}
          </button>
        </div>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// New rollout dialog

const STRATEGIES: { key: string; hint: string }[] = [
  { key: "single", hint: "one node" },
  { key: "selected", hint: "chosen nodes, together" },
  { key: "sequential", hint: "one at a time, in order" },
  { key: "canary", hint: "canaries first, then batches on approval" },
  { key: "parallel", hint: "waves of batch_size" },
];

function RolloutDialog({ onClose }: { onClose: () => void }) {
  const router = useRouter();
  const { mutate } = useSWRConfig();
  const { data: releases } = useReleases();
  const { data: nodes } = useNodes();
  const [relId, setRelId] = useState("");
  const [strategy, setStrategy] = useState("selected");
  const [targets, setTargets] = useState<Set<string>>(new Set());
  const [canary, setCanary] = useState("1");
  const [batch, setBatch] = useState("2");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  const approved = (releases ?? []).filter((r) => r.status === "approved");
  const newest = approved.reduce<Release | null>(
    (b, r) => (b && versionGt(b.version, r.version) ? b : r),
    null
  );
  const sel = relId || newest?.id || "";

  // A node is a valid update target only when it can actually take one.
  const eligible = (n: Node) =>
    n.status === "approved" && n.online && !n.maintenance;
  const needsPick = strategy === "single" || strategy === "selected";
  const needsBatch = strategy === "canary" || strategy === "parallel";
  const needCount = strategy === "single" ? 1 : 0;

  const canSubmit =
    !!sel &&
    !busy &&
    (!needsPick || (needCount ? targets.size === needCount : targets.size > 0));

  const submit = async () => {
    if (!canSubmit) return;
    setBusy(true);
    setErr("");
    try {
      const body: Record<string, unknown> = {
        release_id: sel,
        strategy,
        node_ids: [...targets],
      };
      if (strategy === "canary") {
        body.canary_count = Number(canary) || 1;
        body.batch_size = Number(batch) || 1;
      } else if (strategy === "parallel") {
        body.batch_size = Number(batch) || 1;
      }
      const res = await api<Rollout>("POST", "/rollouts", body);
      mutate("/rollouts");
      mutate(
        (key) =>
          typeof key === "string" &&
          (key.startsWith("/agent-updates") || key === "/nodes")
      );
      onClose();
      if (res?.id) router.push(`/releases?id=${res.id}`);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      setBusy(false);
    }
  };

  const toggle = (id: string) =>
    setTargets((s) => {
      const next = new Set(s);
      if (strategy === "single") {
        next.clear();
        next.add(id);
      } else if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 px-4 backdrop-blur-[2px]"
      onClick={onClose}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-label="New rollout"
        className="panel fade-up max-h-[92vh] w-full max-w-[560px] overflow-y-auto px-5 py-5"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-start justify-between">
          <div>
            <h2 className="text-[15px] font-semibold">New rollout</h2>
            <p className="mt-0.5 text-[12px] text-[var(--color-text-dim)]">
              Dispatch a signed release to cluster agents.
            </p>
          </div>
          <button className="btn btn-sm -mr-1 -mt-1" onClick={onClose} aria-label="Close dialog">
            <X size={14} />
          </button>
        </div>

        <div className="mt-4 space-y-4">
          <div>
            <div className="label mb-1.5">Release</div>
            <select className="input mono w-full" value={sel} onChange={(e) => setRelId(e.target.value)}>
              {approved.map((r) => (
                <option key={r.id} value={r.id}>
                  {fmtVersion(r.version)} · {r.channel}
                </option>
              ))}
            </select>
          </div>

          <div>
            <div className="label mb-1.5">Strategy</div>
            <div className="flex flex-wrap gap-1.5">
              {STRATEGIES.map((s) => (
                <button
                  key={s.key}
                  className={`btn btn-sm ${strategy === s.key ? "btn-primary" : ""}`}
                  title={s.hint}
                  onClick={() => {
                    setStrategy(s.key);
                    if (s.key === "single") setTargets((t) => new Set([...t].slice(0, 1)));
                  }}
                >
                  {s.key}
                </button>
              ))}
            </div>
            <div className="mt-1 text-[10.5px] text-[var(--color-text-faint)]">
              {STRATEGIES.find((s) => s.key === strategy)?.hint}
            </div>
          </div>

          {needsBatch && (
            <div className="flex gap-3">
              {strategy === "canary" && (
                <div>
                  <div className="label mb-1.5">Canaries</div>
                  <input
                    className="input mono w-[90px]"
                    type="number" min={1} max={64}
                    value={canary} onChange={(e) => setCanary(e.target.value)}
                  />
                </div>
              )}
              <div>
                <div className="label mb-1.5">Batch size</div>
                <input
                  className="input mono w-[90px]"
                  type="number" min={1} max={64}
                  value={batch} onChange={(e) => setBatch(e.target.value)}
                />
              </div>
            </div>
          )}

          <div>
            <div className="label mb-1.5">
              Target nodes {targets.size > 0 && `(${targets.size} selected)`}
            </div>
            <div className="flex flex-wrap gap-1.5">
              {(nodes ?? []).map((n) => {
                const ok = eligible(n);
                const av = agentVersion(n);
                return (
                  <button
                    key={n.id}
                    className={`btn btn-sm ${targets.has(n.id) ? "btn-primary" : ""} ${
                      !ok ? "opacity-40" : ""
                    }`}
                    disabled={!ok}
                    title={
                      !ok
                        ? n.status !== "approved"
                          ? `not eligible — status ${n.status}`
                          : !n.online
                            ? "not eligible — offline"
                            : "not eligible — in maintenance"
                        : `${av ? `agent ${av}` : "legacy agent"} — click to ${targets.has(n.id) ? "remove" : "target"}`
                    }
                    onClick={() => toggle(n.id)}
                  >
                    {n.name}
                    <span className="mono text-[10px] opacity-70">
                      {av ? fmtVersion(av) : "legacy"}
                    </span>
                  </button>
                );
              })}
              {(nodes ?? []).length === 0 && (
                <span className="text-[12px] text-[var(--color-text-faint)]">
                  No nodes in the cluster.
                </span>
              )}
            </div>
            {!needsPick && (
              <p className="mt-1.5 text-[11px] text-[var(--color-text-faint)]">
                No selection = all eligible nodes (online, approved, not in maintenance).
              </p>
            )}
          </div>
        </div>

        {err && (
          <div className="mt-3.5 rounded-md border border-red-500/30 bg-red-500/10 px-3 py-2 text-[12px] text-red-400">
            {err}
          </div>
        )}

        <div className="mt-4 flex justify-end gap-2">
          <button className="btn btn-sm" onClick={onClose} disabled={busy}>
            Cancel
          </button>
          <button className="btn btn-sm btn-primary" onClick={submit} disabled={!canSubmit}>
            <Rocket size={12} /> {busy ? "Starting…" : "Start rollout"}
          </button>
        </div>
      </div>
    </div>
  );
}

function Th({ children }: { children?: React.ReactNode }) {
  return <th className="label px-4 py-2.5 font-medium">{children}</th>;
}
function Td({ children, className }: { children?: React.ReactNode; className?: string }) {
  return <td className={`px-4 py-3 align-middle ${className ?? ""}`}>{children}</td>;
}
