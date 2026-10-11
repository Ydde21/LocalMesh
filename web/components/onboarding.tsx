// First-run checklist on the Overview page. Shows while the cluster is
// still being set up (any step incomplete), hides itself once everything
// is done, and can be dismissed permanently via localStorage.
"use client";

import Link from "next/link";
import { useState, useSyncExternalStore } from "react";
import {
  useOverview, useNodes, useEnrollTokens, useEnrollments, useModelOps,
  useHasWorkloads,
} from "@/lib/api";
import { Panel } from "@/components/ui";
import { Rocket, Check, ChevronDown, ChevronRight, X } from "lucide-react";

const DISMISS_KEY = "localmesh:onboarding-dismissed";

// useSyncExternalStore plumbing: no subscription is needed (the flag is
// only written by this component's dismiss button), the client snapshot
// reads localStorage, and the server snapshot is "dismissed" so the card
// is never in the SSR/initial HTML.
const subscribeNoop = () => () => {};
const getDismissed = () => {
  try {
    return localStorage.getItem(DISMISS_KEY) === "1";
  } catch {
    return false;
  }
};
const getDismissedServer = () => true;

interface Step {
  title: string;
  done: boolean;
  // Rendered only while the step is incomplete — a pointer to where the
  // action lives, or a hint when the action happens off-dashboard.
  action?: { href: string; label: string };
  hint?: string;
}

export function OnboardingChecklist() {
  const { data: ov } = useOverview();
  const { data: nodes } = useNodes();
  const { data: tokens } = useEnrollTokens();
  const { data: pending } = useEnrollments("pending");
  const { data: ops } = useModelOps();
  const { data: anyWl } = useHasWorkloads();

  // Dismiss flag lives in localStorage — read it via useSyncExternalStore
  // so SSR/first paint never flashes the card (server snapshot: hidden),
  // and there is no post-mount setState cascade. `sessionDismissed` covers
  // the click itself since nothing else notifies the store.
  const storedDismissed = useSyncExternalStore(
    subscribeNoop,
    getDismissed,
    getDismissedServer
  );
  const [sessionDismissed, setSessionDismissed] = useState(false);
  const dismissed = storedDismissed || sessionDismissed;
  const [collapsed, setCollapsed] = useState(false);

  const nodeList = nodes ?? [];
  const hasNode = nodeList.length > 0;
  const hasApproved = nodeList.some(
    (n) => n.status === "approved" || n.status === "revoked" // revoked ⇒ was approved once
  );
  // A healthy engine on any approved node — models on a node imply the
  // engine worked at report time, so count them too.
  const hasEngine = nodeList.some(
    (n) =>
      n.status === "approved" &&
      ((n.engines ?? []).some((e) => e.healthy) || (n.models ?? []).length > 0)
  );
  const hasModels =
    (ov?.models_distinct ?? 0) > 0 ||
    (ops ?? []).some((o) => o.status === "succeeded" || o.status === "completed");

  const steps: Step[] = [
    { title: "Deploy the control plane", done: true },
    {
      title: "Create an enrollment token",
      done: (tokens ?? []).length > 0 || hasNode,
      action: { href: "/settings", label: "Settings →" },
    },
    {
      title: "Join an agent (localmesh-agent join)",
      done: hasNode || (pending ?? []).length > 0,
      action: { href: "/devices?add=1", label: "Add device →" },
      hint: "or `join --discover` on the same LAN",
    },
    {
      title: "Approve the enrollment",
      done: hasApproved,
      action: { href: "/settings", label: "Pending approvals →" },
    },
    {
      title: "Inference engine detected (e.g. Ollama)",
      done: hasEngine,
      action: { href: "/devices", label: "Check node →" },
      hint: "install & start Ollama on the node",
    },
    {
      title: "Pull a model",
      done: hasModels,
      action: { href: "/models", label: "Models →" },
    },
    {
      title: "Send your first inference",
      done: (anyWl ?? []).length > 0,
      action: { href: "/playground", label: "Playground →" },
      hint: "or point any OpenAI client at the gateway",
    },
  ];

  const doneCount = steps.filter((s) => s.done).length;
  const allDone = doneCount === steps.length;
  // Index of the step to work on next — its hint stays visible.
  const nextIdx = steps.findIndex((s) => !s.done);

  // Stay hidden while any source is still loading — a half-loaded
  // checklist would flash wrong states on an established cluster.
  const loaded =
    ov !== undefined && nodes !== undefined && tokens !== undefined &&
    pending !== undefined && anyWl !== undefined;

  if (dismissed || !loaded || allDone) return null;

  const dismiss = () => {
    try {
      localStorage.setItem(DISMISS_KEY, "1");
    } catch {
      /* private mode — hide for this session only */
    }
    setSessionDismissed(true);
  };

  return (
    <Panel className="fade-up mb-3">
      <div
        className="flex items-center justify-between border-b px-4 py-3"
        style={{ borderColor: "var(--color-border)" }}
      >
        <div className="flex items-center gap-2">
          <Rocket size={13} className="text-[var(--color-accent)]" />
          <div className="label">Get started</div>
          <span className="text-[10.5px] text-[var(--color-text-faint)]">
            {doneCount}/{steps.length}
          </span>
        </div>
        <div className="flex items-center gap-1">
          <button
            className="rounded p-1 text-[var(--color-text-faint)] transition-colors hover:bg-white/[0.06] hover:text-[var(--color-text)]"
            onClick={() => setCollapsed((c) => !c)}
            title={collapsed ? "Expand checklist" : "Collapse checklist"}
            aria-label={collapsed ? "Expand checklist" : "Collapse checklist"}
          >
            {collapsed ? <ChevronRight size={13} /> : <ChevronDown size={13} />}
          </button>
          <button
            className="rounded p-1 text-[var(--color-text-faint)] transition-colors hover:bg-white/[0.06] hover:text-[var(--color-text)]"
            onClick={dismiss}
            title="Dismiss checklist"
            aria-label="Dismiss checklist"
          >
            <X size={13} />
          </button>
        </div>
      </div>
      {!collapsed && (
        <div className="space-y-2.5 px-4 py-3.5">
          {steps.map((s, i) => (
            <div key={s.title} className="flex items-center gap-2.5">
              {s.done ? (
                <span className="flex h-[18px] w-[18px] shrink-0 items-center justify-center rounded-full bg-[var(--color-online-soft)] text-[var(--color-online)]">
                  <Check size={11} />
                </span>
              ) : i === nextIdx ? (
                <span className="flex h-[18px] w-[18px] shrink-0 items-center justify-center rounded-full bg-[var(--color-accent-soft)] text-[10.5px] font-semibold text-[var(--color-accent)]">
                  {i + 1}
                </span>
              ) : (
                <span className="flex h-[18px] w-[18px] shrink-0 items-center justify-center rounded-full border border-[var(--color-border)] text-[10.5px] text-[var(--color-text-faint)]">
                  {i + 1}
                </span>
              )}
              <span
                className={`text-[12.5px] ${
                  s.done
                    ? "text-[var(--color-text-faint)]"
                    : "font-medium text-[var(--color-text)]"
                }`}
              >
                {s.title}
              </span>
              {!s.done && i === nextIdx && s.hint && (
                <span className="text-[11px] text-[var(--color-text-faint)]">
                  · {s.hint}
                </span>
              )}
              {!s.done && s.action && (
                <Link
                  href={s.action.href}
                  className="ml-auto text-[11px] text-[var(--color-accent)] hover:underline"
                >
                  {s.action.label}
                </Link>
              )}
            </div>
          ))}
        </div>
      )}
    </Panel>
  );
}
