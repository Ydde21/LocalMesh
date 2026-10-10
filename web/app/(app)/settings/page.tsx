"use client";

import { useState } from "react";
import {
  useEnrollTokens, useEnrollments, useAPIKeys, useSettings, useAudit, api,
} from "@/lib/api";
import { fmtAgo } from "@/lib/format";
import { Panel, Copyable } from "@/components/ui";
import { useSWRConfig } from "swr";
import { Plus, KeyRound, Ticket, Check, X, Copy } from "lucide-react";

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
                  onClick={() => api("DELETE", `/enrollment-tokens/${t.id}`).then(() => mutTokens())}
                  disabled={!!t.revoked_at}>
                  Revoke
                </button>
              </div>
            ))}
          </div>
        </div>
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
              onClick={() => api("DELETE", `/api-keys/${k.id}`).then(() => mutate())}>
              Revoke
            </button>
          </div>
        ))}
        {(keys ?? []).length === 0 && (
          <div className="text-[12px] text-[var(--color-text-faint)]">No keys yet.</div>
        )}
      </div>
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
