"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { X } from "lucide-react";
import {
  useVersion, useRollouts, useAgentUpdates, useNodes, useDiagnostics,
  useControlPlane, useModelOps,
} from "@/lib/api";
import { fmtVersion } from "@/lib/format";

type Severity = "danger" | "warn";

interface Alert {
  id: string;
  sev: Severity;
  text: string;
  title?: string; // full text when `text` truncates
  href?: string;
  linkLabel?: string;
}

// Terminal agent-update phases that mean "human should look".
const FAILED_UPDATE_PHASES = new Set(["failed", "rolled_back", "interrupted"]);

// Operational alerts strip — one slim line per real problem, rendered at
// the top of the app shell above every page. Every row is derived from a
// live endpoint (nothing fabricated): pre-v0.6/0.7 servers 404 the newer
// hooks and those alerts simply never appear. Dismissals are per-session
// (component state inside the Shell — a full reload brings them back).
export function OperationalAlerts({ connected }: { connected: boolean }) {
  const { data: ver } = useVersion();
  const { data: rollouts } = useRollouts();
  const { data: updates } = useAgentUpdates({ limit: 20 });
  const { data: nodes } = useNodes();
  const { data: diag } = useDiagnostics();
  const { data: cp } = useControlPlane();
  const { data: ops } = useModelOps();
  const [dismissed, setDismissed] = useState<ReadonlySet<string>>(new Set());

  // SSE dropouts are usually momentary (EventSource auto-reconnects) —
  // only flag the stream after 8s of continuous outage. Reconnect resets
  // via the documented render-phase adjustment (no effect needed for
  // prop-derived state).
  const [sseStale, setSseStale] = useState(false);
  const [prevConnected, setPrevConnected] = useState(connected);
  if (prevConnected !== connected) {
    setPrevConnected(connected);
    if (connected && sseStale) setSseStale(false);
  }
  useEffect(() => {
    if (connected) return;
    const t = setTimeout(() => setSseStale(true), 8000);
    return () => clearTimeout(t);
  }, [connected]);

  const alerts: Alert[] = [];

  // Standby: the version payload only carries `role` on a standby — the
  // admin API 503s there, so this may be the only alert that can render.
  if (ver?.role === "standby") {
    alerts.push({
      id: "standby",
      sev: "warn",
      text: "You are viewing a standby control plane — it is not serving cluster operations. Connect to the active instance.",
    });
  }

  // Live event stream down — panels keep showing the last poll but
  // nothing invalidates; honest "data may be stale" signal.
  if (sseStale) {
    alerts.push({
      id: "sse-down",
      sev: "warn",
      text: "Live event stream interrupted — data may be stale. Retrying connection…",
    });
  }

  // Ownership heartbeat runs every 2s (server-side); a row this stale
  // means the owner's DB writes are failing or it is dead — a standby
  // could take over at any moment.
  const hbAge = cp?.owner?.heartbeat_age_seconds;
  if (hbAge !== undefined && hbAge > 15) {
    alerts.push({
      id: "cp-heartbeat",
      sev: "warn",
      text: `Control-plane owner heartbeat is ${hbAge}s stale — database connectivity may be failing and a standby could promote`,
      href: "/",
      linkLabel: "Overview",
    });
  }

  // Rollouts needing attention. Awaiting approval is actionable (link
  // lands on the drawer's Approve button); failed/cancelled are records —
  // a cancelled rollout may have left nodes mid-update.
  const rs = (rollouts ?? [])
    .slice()
    .sort((a, b) => (b.updated_at || b.created_at).localeCompare(a.updated_at || a.created_at));
  const needsAttention = (r: (typeof rs)[number]) =>
    r.state === "awaiting_approval" || r.state === "failed" || r.state === "cancelled";
  const rolloutAlerts = rs.filter(needsAttention).slice(0, 4);
  const extraRollouts = rs.filter(needsAttention).length - rolloutAlerts.length;
  for (const r of rolloutAlerts) {
    const rv = fmtVersion(r.release_version || "");
    if (r.state === "awaiting_approval") {
      alerts.push({
        id: `rollout-await-${r.id}`,
        sev: "warn",
        text: `Rollout ${rv} is waiting for approval to continue`,
        href: `/releases?id=${r.id}`,
        linkLabel: "Review",
      });
    } else if (r.state === "cancelled") {
      alerts.push({
        id: `rollout-cancelled-${r.id}`,
        sev: "warn",
        text: `Rollout ${rv} was cancelled — ${r.succeeded}/${r.total} nodes updated`,
        href: `/releases?id=${r.id}`,
        linkLabel: "View",
      });
    } else {
      alerts.push({
        id: `rollout-failed-${r.id}`,
        sev: "danger",
        text: `Rollout ${rv} failed${r.error ? ` — ${r.error}` : ""}`,
        title: r.error,
        href: `/releases?id=${r.id}`,
        linkLabel: "View",
      });
    }
  }
  if (extraRollouts > 0) {
    alerts.push({
      id: "rollouts-more",
      sev: "warn",
      text: `${extraRollouts} more rollout${extraRollouts === 1 ? "" : "s"} need attention`,
      href: "/releases",
      linkLabel: "View",
    });
  }

  // Recent failed/rolled-back/interrupted agent updates (last 20 rows).
  const failedUps = (updates ?? []).filter((u) => FAILED_UPDATE_PHASES.has(u.phase));
  if (failedUps.length > 0) {
    const names = [...new Set(failedUps.map((u) => u.node_name || u.node_id.slice(0, 8)))];
    alerts.push({
      id: "agent-updates-failed",
      sev: "warn",
      text: `${failedUps.length} recent agent update${failedUps.length === 1 ? "" : "s"} failed or rolled back (${names.join(", ")})`,
      title: failedUps
        .map((u) => `${u.node_name || u.node_id}: ${u.phase}${u.error ? ` — ${u.error}` : ""}`)
        .join("\n"),
      href: "/devices",
      linkLabel: "View",
    });
  }

  // Recent failed/interrupted model operations (shared 60-row ops feed).
  const failedOps = (ops ?? []).filter(
    (o) => o.status === "failed" || o.status === "interrupted"
  );
  if (failedOps.length > 0) {
    const allNames = [...new Set(failedOps.map((o) => o.model))];
    const names = allNames.slice(0, 3);
    alerts.push({
      id: "modelops-failed",
      sev: "warn",
      text: `${failedOps.length} model operation${failedOps.length === 1 ? "" : "s"} failed or interrupted (${names.join(", ")}${allNames.length > names.length ? "…" : ""})`,
      title: failedOps
        .map((o) => {
          const e = o.last_error || o.error;
          return `${o.op} ${o.model} → ${o.node_name || o.node_id}: ${o.status}${e ? ` — ${e}` : ""}`;
        })
        .join("\n"),
      href: "/models",
      linkLabel: "View",
    });
  }

  // Nodes out of service (approved nodes only — pending/revoked aren't
  // expected to serve). Offline is danger; any maintenance state is warn.
  const approved = (nodes ?? []).filter((n) => n.status === "approved");
  const offline = approved.filter((n) => !n.online);
  if (offline.length > 0) {
    const names = offline.slice(0, 3).map((n) => n.name).join(", ");
    alerts.push({
      id: "nodes-offline",
      sev: "danger",
      text: `${offline.length} node${offline.length === 1 ? "" : "s"} offline: ${names}${offline.length > 3 ? "…" : ""}`,
      title: offline.map((n) => n.name).join("\n"),
      href: "/devices",
      linkLabel: "View",
    });
  }
  const maint = approved.filter((n) => !!n.maintenance);
  if (maint.length > 0) {
    const by = (s: string) => maint.filter((n) => n.maintenance === s).length;
    const parts = (["draining", "maintenance", "updating"] as const)
      .filter((s) => by(s) > 0)
      .map((s) => `${by(s)} ${s}`);
    alerts.push({
      id: "nodes-maintenance",
      sev: "warn",
      text: `${maint.length} node${maint.length === 1 ? "" : "s"} out of service — ${parts.join(", ")}`,
      href: "/devices",
      linkLabel: "View",
    });
  }

  // Engine failures block scheduling for every model hosted on that
  // engine — surfaced verbatim from the node's own health report.
  const badEngine = approved.filter(
    (n) => n.online && (n.engines ?? []).some((e) => !e.healthy)
  );
  if (badEngine.length > 0) {
    const names = badEngine.slice(0, 3).map((n) => n.name).join(", ");
    alerts.push({
      id: "engine-unhealthy",
      sev: "warn",
      text: `Inference engine unhealthy on ${badEngine.length} node${badEngine.length === 1 ? "" : "s"}: ${names}${badEngine.length > 3 ? "…" : ""}`,
      title: badEngine
        .map(
          (n) =>
            `${n.name}: ${(n.engines ?? [])
              .filter((e) => !e.healthy)
              .map((e) => e.type)
              .join(", ")}`
        )
        .join("\n"),
      href: "/devices",
      linkLabel: "View",
    });
  }

  // Memory pressure — the scheduler gates on memory_available_bytes; a
  // node under ~10% free will start refusing new work. Only nodes whose
  // agent actually measured availability count (never infer from used%).
  const memPressure = approved.filter((n) => {
    const m = n.metrics;
    if (!n.online || !m || m.mem_avail_measured !== true) return false;
    const avail = m.memory_available_bytes ?? m.memory_total_bytes - m.memory_used_bytes;
    return m.memory_total_bytes > 0 && avail / m.memory_total_bytes < 0.1;
  });
  if (memPressure.length > 0) {
    const names = memPressure.slice(0, 3).map((n) => n.name).join(", ");
    alerts.push({
      id: "mem-pressure",
      sev: "warn",
      text: `Memory nearly exhausted on ${memPressure.length} node${memPressure.length === 1 ? "" : "s"}: ${names}${memPressure.length > 3 ? "…" : ""}`,
      title: memPressure
        .map((n) => {
          const m = n.metrics!;
          const avail = m.memory_available_bytes ?? m.memory_total_bytes - m.memory_used_bytes;
          return `${n.name}: ${Math.round((avail / m.memory_total_bytes) * 100)}% free`;
        })
        .join("\n"),
      href: "/devices",
      linkLabel: "View",
    });
  }

  // Server TLS leaf expiry — same threshold the settings page uses.
  const certDays = diag?.tls?.server_days_remaining;
  if (certDays !== undefined && certDays < 30) {
    alerts.push({
      id: "cert-expiry",
      sev: certDays <= 0 ? "danger" : "warn",
      text:
        certDays <= 0
          ? `Server TLS certificate expired ${-certDays}d ago — agents can no longer verify the control plane`
          : `Server TLS certificate expires in ${certDays}d — renew before agents can no longer verify the control plane`,
      title: diag?.tls?.server_expires_at
        ? `expires ${new Date(diag.tls.server_expires_at).toLocaleString()}`
        : undefined,
      href: "/settings",
      linkLabel: "Diagnostics",
    });
  }

  const visible = alerts.filter((a) => !dismissed.has(a.id));
  if (visible.length === 0) return null;

  const dismiss = (id: string) =>
    setDismissed((s) => {
      const next = new Set(s);
      next.add(id);
      return next;
    });

  return (
    <div className="mx-auto max-w-[1200px] px-6 pt-5">
      <div role="status" aria-label="Operational alerts" className="space-y-2">
        {visible.map((a) => (
          <AlertRow key={a.id} alert={a} onDismiss={() => dismiss(a.id)} />
        ))}
      </div>
    </div>
  );
}

function AlertRow({ alert, onDismiss }: { alert: Alert; onDismiss: () => void }) {
  const danger = alert.sev === "danger";
  return (
    <div
      className={`flex items-center gap-2.5 rounded-md border px-3 py-2 ${
        danger
          ? "border-[var(--color-danger)]/25 bg-[var(--color-danger-soft)]"
          : "border-[var(--color-warn)]/25 bg-[var(--color-warn-soft)]"
      }`}
    >
      <span
        className={`badge shrink-0 !px-1.5 !py-0 ${danger ? "badge-danger" : "badge-warn"}`}
      >
        {danger ? "critical" : "warning"}
      </span>
      <span
        className={`min-w-0 flex-1 truncate text-[12px] ${
          danger ? "text-[var(--color-danger)]" : "text-[var(--color-warn)]"
        }`}
        title={alert.title ?? alert.text}
      >
        {alert.text}
      </span>
      {alert.href && (
        <Link
          href={alert.href}
          className="shrink-0 text-[11.5px] text-[var(--color-accent)] hover:underline"
        >
          {alert.linkLabel ?? "View"} →
        </Link>
      )}
      <button
        className="shrink-0 rounded p-0.5 text-[var(--color-text-faint)] transition-colors hover:bg-white/[0.06] hover:text-[var(--color-text)]"
        onClick={onDismiss}
        aria-label="Dismiss alert"
        title="Dismiss for this session"
      >
        <X size={12} />
      </button>
    </div>
  );
}
