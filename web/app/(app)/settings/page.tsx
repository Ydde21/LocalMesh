"use client";

import { useState } from "react";
import {
  useEnrollTokens, useEnrollments, useAPIKeys, useSettings, useAudit,
  useDiagnostics, api,
} from "@/lib/api";
import { fmtAgo, fmtMs, fmtUptime, fmtVersion } from "@/lib/format";
import { Panel, Copyable } from "@/components/ui";
import { ConfirmDialog } from "@/components/confirm";
import { useSWRConfig } from "swr";
import { Plus, KeyRound, Ticket, Check, X, Copy, Info, Download, AlertTriangle } from "lucide-react";
import type { APIKey, Diagnostics, EnrollmentToken } from "@/lib/types";

export default function SettingsPage() {
  return (
    <div className="mx-auto max-w-[1100px] px-6 py-6">
      <header className="mb-6">
        <h1 className="text-[19px] font-semibold tracking-tight">Settings</h1>
        <p className="mt-0.5 text-[12.5px] text-[var(--color-text-dim)]">
          Cluster configuration, enrollment, and credentials
        </p>
      </header>
      <div className="space-y-4">
        <EnrollmentSection />
        <APIKeysSection />
        <SchedulingSection />
        <ModelOpsSection />
        <DiagnosticsSection />
        <AuditSection />
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------

function Section({ icon: Icon, title, desc, children }: {
  icon: typeof KeyRound; title: string; desc: string; children: React.ReactNode;
}) {
  return (
    <Panel className="fade-up">
      <div className="border-b px-5 py-4" style={{ borderColor: "var(--color-border)" }}>
        <div className="flex items-center gap-2">
          <Icon size={15} className="text-[var(--color-accent)]" />
          <h2 className="text-[14px] font-semibold">{title}</h2>
        </div>
        <p className="mt-1 text-[12px] text-[var(--color-text-dim)]">{desc}</p>
      </div>
      <div className="px-5 py-4">{children}</div>
    </Panel>
  );
}

// ---- Enrollment -------------------------------------------------------------

function EnrollmentSection() {
  const { data: tokens, mutate: mutTokens } = useEnrollTokens();
  const { data: pending, mutate: mutPending } = useEnrollments("pending");
  const [label, setLabel] = useState("");
  const [maxUses, setMaxUses] = useState("1");
  const [newToken, setNewToken] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const [revokingTok, setRevokingTok] = useState<EnrollmentToken | null>(null);
  const [tokBusy, setTokBusy] = useState(false);
  const [tokErr, setTokErr] = useState("");

  const confirmRevokeTok = async () => {
    if (!revokingTok) return;
    setTokBusy(true);
    setTokErr("");
    try {
      await api("DELETE", `/enrollment-tokens/${revokingTok.id}`);
      setRevokingTok(null);
      mutTokens();
    } catch (e) {
      setTokErr(e instanceof Error ? e.message : String(e));
    } finally {
      setTokBusy(false);
    }
  };

  const createToken = async () => {
    setBusy(true); setErr("");
    try {
      const res = await api<{ token: string }>("POST", "/enrollment-tokens", {
        label, max_uses: Number(maxUses) || 1,
      });
      setNewToken(res.token);
      mutTokens();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const act = async (path: string) => {
    await api("POST", path);
    mutPending();
    mutTokens();
  };

  return (
    <Section icon={Ticket} title="Device enrollment"
      desc="Generate a token, then run `localmesh-agent join` on the machine. Enrollments require admin approval.">
      <div className="flex flex-wrap items-center gap-2.5">
        <input className="input w-[200px]" placeholder="token label (e.g. my-mac)"
          value={label} onChange={(e) => setLabel(e.target.value)} />
        <input className="input mono w-[80px]" type="number" min={1} max={100}
          value={maxUses} onChange={(e) => setMaxUses(e.target.value)} title="max uses" />
        <button className="btn btn-primary btn-sm" disabled={busy} onClick={createToken}>
          <Plus size={13} /> Create token
        </button>
      </div>
      {err && <p className="mt-2 text-[12px] text-[var(--color-danger)]">{err}</p>}

      {newToken && (
        <div className="mt-3 rounded-md border border-[var(--color-online)]/30 bg-[var(--color-online-soft)] px-3.5 py-3">
          <div className="mb-1.5 flex items-center gap-2 text-[12px] font-medium text-[var(--color-online)]">
            <Check size={13} /> Token created — it is shown only once.
          </div>
          <div className="codeblock flex items-center justify-between">
            <span>{`localmesh-agent join --server <control-plane-url> --token ${newToken}`}</span>
            <button className="btn btn-sm ml-3" onClick={() => navigator.clipboard.writeText(`localmesh-agent join --server <control-plane-url> --token ${newToken}`)}>
              <Copy size={12} /> Copy
            </button>
          </div>
        </div>
      )}

      {/* Pending approvals */}
      {(pending ?? []).length > 0 && (
        <div className="mt-4">
          <div className="label mb-2">Pending approval</div>
          <div className="space-y-2">
            {(pending ?? []).map((r) => (
              <div key={r.id} className="flex items-center gap-3 rounded-md border border-[var(--color-warn)]/25 bg-[var(--color-warn-soft)] px-3.5 py-2.5">
                <div>
                  <div className="text-[13px] font-medium">{r.name}</div>
                  <div className="text-[11px] text-[var(--color-text-dim)]">
                    {r.hardware.cpu_model} · {r.hardware.cpu_cores} cores · {r.hardware.os}/{r.hardware.arch} · {fmtAgo(r.created_at)}
                  </div>
                </div>
                <div className="ml-auto flex gap-2">
                  <button className="btn btn-sm btn-primary" onClick={() => act(`/enrollments/${r.id}/approve`)}>
                    <Check size={12} /> Approve
                  </button>
                  <button className="btn btn-sm btn-danger" onClick={() => act(`/enrollments/${r.id}/deny`)}>
                    <X size={12} /> Deny
                  </button>
                </div>
              </div>
            ))}
          </div>
        </div>
      )}

      {/* Token list */}
      {(tokens ?? []).length > 0 && (
        <div className="mt-4">
          <div className="label mb-2">Tokens</div>
          <div className="space-y-1.5">
            {(tokens ?? []).map((t) => (
              <div key={t.id} className="flex items-center gap-3 rounded-md bg-white/[0.03] px-3.5 py-2">
                <Copyable text={t.prefix + "…"} />
                <span className="text-[12px] text-[var(--color-text-dim)]">{t.label || "—"}</span>
                <span className="mono text-[11px] text-[var(--color-text-faint)]">
                  {t.uses}/{t.max_uses} uses
                </span>
                <span className="text-[10.5px] text-[var(--color-text-faint)]">
                  expires {fmtAgo(t.expires_at).replace(" ago", "")}
                </span>
                {t.revoked_at && <span className="badge badge-danger">revoked</span>}
                {!t.revoked_at && new Date(t.expires_at) < new Date() && (
                  <span className="badge badge-neutral">expired</span>
                )}
                <button className="btn btn-sm btn-danger ml-auto"
                  onClick={() => { setTokErr(""); setRevokingTok(t); }}
                  disabled={!!t.revoked_at}>
                  Revoke
                </button>
              </div>
            ))}
          </div>
        </div>
      )}

      {revokingTok && (
        <ConfirmDialog
          title="Revoke enrollment token"
          confirmLabel="Revoke token"
          busy={tokBusy}
          error={tokErr}
          onClose={() => !tokBusy && setRevokingTok(null)}
          onConfirm={confirmRevokeTok}
        >
          Revoke token{" "}
          <span className="mono text-[var(--color-text)]">
            {revokingTok.label || revokingTok.prefix + "…"}
          </span>
          ? Devices can no longer enroll with it — mint a new token to add more
          machines.
        </ConfirmDialog>
      )}
    </Section>
  );
}

// ---- API keys ---------------------------------------------------------------

function APIKeysSection() {
  const { data: keys, mutate } = useAPIKeys();
  const [name, setName] = useState("");
  const [role, setRole] = useState("inference");
  const [newKey, setNewKey] = useState("");
  const [busy, setBusy] = useState(false);
  const [revoking, setRevoking] = useState<APIKey | null>(null);
  const [revErr, setRevErr] = useState("");

  const confirmRevoke = async () => {
    if (!revoking) return;
    setBusy(true);
    setRevErr("");
    try {
      await api("DELETE", `/api-keys/${revoking.id}`);
      setRevoking(null);
      mutate();
    } catch (e) {
      setRevErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const create = async () => {
    if (!name.trim()) return;
    setBusy(true);
    try {
      const res = await api<{ key: string }>("POST", "/api-keys", { name: name.trim(), role });
      setNewKey(res.key);
      setName("");
      mutate();
    } finally {
      setBusy(false);
    }
  };

  return (
    <Section icon={KeyRound} title="API keys"
      desc="Keys authenticate the OpenAI-compatible gateway and this admin API. Use an `inference` key for applications.">
      <div className="flex flex-wrap items-center gap-2.5">
        <input className="input w-[220px]" placeholder="key name (e.g. my-app)"
          value={name} onChange={(e) => setName(e.target.value)} />
        <div className="flex gap-1">
          {(["inference", "readonly", "admin"] as const).map((r) => (
            <button key={r} className={`btn btn-sm ${role === r ? "btn-primary" : ""}`}
              onClick={() => setRole(r)}>{r}</button>
          ))}
        </div>
        <button className="btn btn-primary btn-sm" disabled={busy || !name.trim()} onClick={create}>
          <Plus size={13} /> Create key
        </button>
      </div>

      {newKey && (
        <div className="mt-3 rounded-md border border-[var(--color-online)]/30 bg-[var(--color-online-soft)] px-3.5 py-3">
          <div className="mb-1.5 flex items-center gap-2 text-[12px] font-medium text-[var(--color-online)]">
            <Check size={13} /> Key created — store it now.
          </div>
          <div className="codeblock flex items-center justify-between">
            <span>{newKey}</span>
            <button className="btn btn-sm ml-3" onClick={() => navigator.clipboard.writeText(newKey)}>
              <Copy size={12} /> Copy
            </button>
          </div>
          <div className="mt-2 text-[11px] text-[var(--color-text-dim)]">
            Use with any OpenAI client: <code className="mono">base_url = {"<control-plane>"}/v1, api_key = this key</code>
          </div>
        </div>
      )}

      <div className="mt-4 space-y-1.5">
        {(keys ?? []).map((k) => (
          <div key={k.id} className="flex items-center gap-3 rounded-md bg-white/[0.03] px-3.5 py-2">
            <Copyable text={k.prefix + "…"} />
            <span className="text-[12.5px] font-medium">{k.name}</span>
            <span className="badge badge-neutral">{k.role}</span>
            {k.revoked_at && <span className="badge badge-danger">revoked</span>}
            <span className="text-[10.5px] text-[var(--color-text-faint)]">
              {k.last_used_at ? `used ${fmtAgo(k.last_used_at)}` : "never used"}
            </span>
            <button className="btn btn-sm btn-danger ml-auto" disabled={!!k.revoked_at}
              onClick={() => { setRevErr(""); setRevoking(k); }}>
              Revoke
            </button>
          </div>
        ))}
        {(keys ?? []).length === 0 && (
          <div className="text-[12px] text-[var(--color-text-faint)]">No keys yet.</div>
        )}
      </div>

      {revoking && (
        <ConfirmDialog
          title="Revoke API key"
          confirmLabel="Revoke key"
          busy={busy}
          error={revErr}
          onClose={() => !busy && setRevoking(null)}
          onConfirm={confirmRevoke}
        >
          Revoke <span className="text-[var(--color-text)]">{revoking.name}</span>{" "}
          (<span className="mono">{revoking.prefix}…</span>, {revoking.role})?
          Every client using this key loses access immediately — revoked keys
          can&apos;t be restored.
        </ConfirmDialog>
      )}
    </Section>
  );
}

// ---- Scheduling ---------------------------------------------------------------

function SchedulingSection() {
  const { data: settings, mutate } = useSettings();
  const { mutate: gm } = useSWRConfig();
  const sched = (settings?.scheduling ?? {}) as Record<string, unknown>;
  const privacy = (settings?.privacy ?? {}) as Record<string, unknown>;
  const weights = (sched.weights ?? {}) as Record<string, number>;

  const [busy, setBusy] = useState(false);
  const [saved, setSaved] = useState(false);

  const [queueMs, setQueueMs] = useState<number | null>(null);
  const [attempts, setAttempts] = useState<number | null>(null);
  const [allowRemote, setAllowRemote] = useState<boolean | null>(null);

  const curQueueMs = queueMs ?? (sched.queue_timeout_ms as number) ?? 30000;
  const curAttempts = attempts ?? (sched.max_attempts as number) ?? 2;
  const curRemote = allowRemote ?? (privacy.allow_remote_nodes as boolean) ?? true;

  const save = async () => {
    setBusy(true); setSaved(false);
    try {
      await api("PUT", "/settings/scheduling", {
        ...sched,
        queue_timeout_ms: curQueueMs,
        max_attempts: curAttempts,
      });
      await api("PUT", "/settings/privacy", {
        ...privacy,
        allow_remote_nodes: curRemote,
      });
      mutate();
      gm("/settings");
      setSaved(true);
      setTimeout(() => setSaved(false), 2000);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Section icon={Ticket} title="Scheduling"
      desc="How the router picks a node for each request. Weights bias the scoring; filters are absolute.">
      <div className="grid max-w-[560px] grid-cols-2 gap-x-6 gap-y-4">
        <Field label="Queue timeout" hint="ms to wait for capacity before failing">
          <input className="input mono" type="number" min={0} step={1000}
            value={curQueueMs} onChange={(e) => setQueueMs(Number(e.target.value))} />
        </Field>
        <Field label="Max attempts" hint="dispatch retries when pre-token failure">
          <input className="input mono" type="number" min={1} max={5}
            value={curAttempts} onChange={(e) => setAttempts(Number(e.target.value))} />
        </Field>
      </div>

      <div className="label mb-2 mt-5">Scoring weights</div>
      <div className="grid max-w-[560px] grid-cols-2 gap-x-6 gap-y-2.5">
        {Object.entries(weights).map(([k, v]) => (
          <WeightInput key={k} name={k} value={v}
            onChange={async (nv) => {
              const next = { ...sched, weights: { ...weights, [k]: nv } };
              await api("PUT", "/settings/scheduling", next);
              mutate();
            }} />
        ))}
        {Object.keys(weights).length === 0 && (
          <div className="text-[12px] text-[var(--color-text-faint)]">Defaults apply — save once to edit.</div>
        )}
      </div>

      <div className="label mb-2 mt-5">Privacy</div>
      <label className="flex max-w-[560px] cursor-pointer items-center gap-2.5 text-[12.5px] text-[var(--color-text-dim)]">
        <input type="checkbox" checked={curRemote} onChange={(e) => setAllowRemote(e.target.checked)}
          className="accent-[var(--color-accent)]" />
        Allow scheduling to nodes reachable over remote networks (Tailscale/Headscale).
        When off, only nodes labeled <code className="mono">localmesh.net=lan</code> are eligible.
      </label>

      <div className="mt-4 flex items-center gap-3">
        <button className="btn btn-primary btn-sm" disabled={busy} onClick={save}>
          {saved ? "Saved" : "Save changes"}
        </button>
      </div>
    </Section>
  );
}

// ---- Model operations -----------------------------------------------------
//
// settings["modelops"] tunes the pull/delete queue. Rendered only when the
// server exposes the key — older servers don't have the queue at all.

function ModelOpsSection() {
  const { data: settings, mutate } = useSettings();
  const { mutate: gm } = useSWRConfig();
  const modelops = (settings?.modelops ?? null) as Record<string, number> | null;

  const [busy, setBusy] = useState(false);
  const [saved, setSaved] = useState(false);
  const [err, setErr] = useState("");

  const [globalConc, setGlobalConc] = useState<number | null>(null);
  const [perNodeConc, setPerNodeConc] = useState<number | null>(null);
  const [queueMax, setQueueMax] = useState<number | null>(null);
  const [maxAttempts, setMaxAttempts] = useState<number | null>(null);

  if (!modelops) return null; // also covers settings still loading

  const curGlobal = globalConc ?? modelops.global_concurrent ?? 2;
  const curPerNode = perNodeConc ?? modelops.per_node_concurrent ?? 1;
  const curQueueMax = queueMax ?? modelops.queue_max ?? 32;
  const curAttempts = maxAttempts ?? modelops.max_attempts ?? 3;

  const save = async () => {
    setBusy(true);
    setErr("");
    setSaved(false);
    try {
      await api("PUT", "/settings/modelops", {
        ...modelops,
        global_concurrent: curGlobal,
        per_node_concurrent: curPerNode,
        queue_max: curQueueMax,
        max_attempts: curAttempts,
      });
      mutate();
      gm("/settings");
      setSaved(true);
      setTimeout(() => setSaved(false), 2000);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Section icon={Ticket} title="Model operations"
      desc="Pull/delete jobs run through a server-side queue — these bound how many run at once, per node and cluster-wide, and how failures retry.">
      <div className="grid max-w-[560px] grid-cols-2 gap-x-6 gap-y-4">
        <Field label="Global concurrency" hint="model ops in flight across the whole cluster (1-16)">
          <input className="input mono" type="number" min={1} max={16} step={1}
            value={curGlobal} onChange={(e) => setGlobalConc(Number(e.target.value))} />
        </Field>
        <Field label="Per-node concurrency" hint="model ops a single node runs at once (1-4)">
          <input className="input mono" type="number" min={1} max={4} step={1}
            value={curPerNode} onChange={(e) => setPerNodeConc(Number(e.target.value))} />
        </Field>
        <Field label="Queue depth" hint="queued ops accepted before new requests are rejected (8-256)">
          <input className="input mono" type="number" min={8} max={256} step={1}
            value={curQueueMax} onChange={(e) => setQueueMax(Number(e.target.value))} />
        </Field>
        <Field label="Max attempts" hint="retry attempts per op before it fails permanently (0-8)">
          <input className="input mono" type="number" min={0} max={8} step={1}
            value={curAttempts} onChange={(e) => setMaxAttempts(Number(e.target.value))} />
        </Field>
      </div>

      {err && (
        <div className="mt-3 rounded-md border border-red-500/30 bg-red-500/10 px-3 py-2 text-[12px] text-red-400">
          {err}
        </div>
      )}

      <div className="mt-4 flex items-center gap-3">
        <button className="btn btn-primary btn-sm" disabled={busy} onClick={save}>
          {saved ? "Saved" : "Save changes"}
        </button>
      </div>
    </Section>
  );
}

function Field({ label, hint, children }: { label: string; hint: string; children: React.ReactNode }) {
  return (
    <div>
      <div className="text-[12.5px] font-medium">{label}</div>
      <div className="mt-0.5 text-[10.5px] text-[var(--color-text-faint)]">{hint}</div>
      <div className="mt-1.5">{children}</div>
    </div>
  );
}

function WeightInput({ name, value, onChange }: { name: string; value: number; onChange: (v: number) => void }) {
  return (
    <label className="flex items-center gap-2.5 text-[12px] text-[var(--color-text-dim)]">
      <span className="mono w-[130px]">{name}</span>
      <input className="input mono w-[70px]" type="number" step={0.1} min={0} defaultValue={value}
        onBlur={(e) => onChange(Number(e.target.value))} />
    </label>
  );
}

// ---- Diagnostics ------------------------------------------------------------
//
// v0.6+ redacted operational snapshot (GET /diagnostics). The whole
// section is gated on the endpoint existing — pre-v0.6 servers 404 and
// nothing renders.

function DiagnosticsSection() {
  const { data: diag } = useDiagnostics();
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  if (!diag) return null; // also covers settings still loading

  const certDays = diag.tls?.server_days_remaining;
  const certWarn = certDays !== undefined && certDays < 30;

  const exportJson = async () => {
    setBusy(true);
    setErr("");
    try {
      // Pull a fresh bundle at click time — the exported file should
      // reflect the cluster right now, not the last render.
      const fresh = await api<Diagnostics>("GET", "/diagnostics");
      const blob = new Blob([JSON.stringify(fresh, null, 2)], {
        type: "application/json",
      });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `localmesh-diagnostics-${new Date()
        .toISOString()
        .replace(/[:.]/g, "-")}.json`;
      a.click();
      URL.revokeObjectURL(url);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Section icon={Info} title="Diagnostics"
      desc="Redacted operational snapshot — versions, ownership, DB health, cert expiry, node/ops state. Attach it to bug reports; credentials and prompts are never included.">
      <div className="grid max-w-[560px] grid-cols-2 gap-x-6 gap-y-2.5">
        <DiagRow label="Server" value={`${fmtVersion(diag.server_version)} · proto ${diag.protocol}`} />
        <DiagRow label="Cluster" value={diag.cluster_name || "—"} />
        <DiagRow label="Uptime" value={fmtUptime(diag.uptime_seconds)} />
        <DiagRow
          label="Database"
          value={
            diag.database
              ? diag.database.reachable
                ? `reachable · ${fmtMs(diag.database.latency_ms)}`
                : "unreachable"
              : "—"
          }
        />
        {diag.control_plane && (
          <DiagRow
            label="Control plane"
            value={`epoch ${diag.control_plane.epoch} · since ${fmtAgo(diag.control_plane.started_at)}`}
          />
        )}
        {diag.discovery && (
          <DiagRow
            label="LAN discovery"
            value={
              diag.discovery.advertising
                ? `advertising${diag.discovery.cluster_name ? ` “${diag.discovery.cluster_name}”` : ""}`
                : diag.discovery.enabled
                  ? "enabled"
                  : "off"
            }
          />
        )}
        {diag.nodes && (
          <DiagRow
            label="Nodes"
            value={`${diag.nodes.filter((n) => n.online).length} online / ${diag.nodes.length} total`}
          />
        )}
      </div>

      {/* Cert expiry — quiet until the leaf is inside 30 days. */}
      {certWarn && (
        <div className="mt-3.5 flex max-w-[560px] items-center gap-2.5 rounded-md border border-[var(--color-warn)]/25 bg-[var(--color-warn-soft)] px-3 py-2 text-[12px] text-[var(--color-warn)]">
          <AlertTriangle size={13} className="shrink-0" />
          <span>
            Server TLS certificate expires in {certDays}d
            {diag.tls?.server_expires_at &&
              ` (${new Date(diag.tls.server_expires_at).toLocaleDateString()})`}
            {" "}— renew before agents can no longer verify the control plane.
          </span>
          <span className="badge badge-warn ml-auto shrink-0">cert expiry</span>
        </div>
      )}

      {err && (
        <div className="mt-3 max-w-[560px] rounded-md border border-red-500/30 bg-red-500/10 px-3 py-2 text-[12px] text-red-400">
          {err}
        </div>
      )}

      <div className="mt-4 flex items-center gap-3">
        <button className="btn btn-sm" disabled={busy} onClick={exportJson}>
          <Download size={12} /> {busy ? "Exporting…" : "Export diagnostics"}
        </button>
        <span className="text-[11px] text-[var(--color-text-faint)]">
          Downloads a JSON snapshot of everything above.
        </span>
      </div>
    </Section>
  );
}

function DiagRow({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-3 text-[12px]">
      <span className="text-[var(--color-text-faint)]">{label}</span>
      <span className="mono text-[var(--color-text-dim)]">{value}</span>
    </div>
  );
}

// ---- Audit ----------------------------------------------------------------

function AuditSection() {
  const { data: audit } = useAudit();
  return (
    <Section icon={Ticket} title="Audit log" desc="Security-relevant events. Prompts and responses are never logged.">
      <div className="max-h-[300px] space-y-0 overflow-y-auto">
        {(audit ?? []).map((a) => (
          <div key={a.id} className="row-line flex items-baseline gap-3 py-1.5 text-[12px]" style={{ borderColor: "var(--color-border)" }}>
            <span className="mono text-[var(--color-text-faint)]">{fmtAgo(a.created_at)}</span>
            <span className="font-medium text-[var(--color-text)]">{a.actor}</span>
            <span className="text-[var(--color-text-dim)]">{a.action}</span>
            <span className="mono truncate text-[var(--color-text-faint)]">{a.target}</span>
          </div>
        ))}
        {(audit ?? []).length === 0 && (
          <div className="py-4 text-[12px] text-[var(--color-text-faint)]">No events yet.</div>
        )}
      </div>
    </Section>
  );
}
