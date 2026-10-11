"use client";

import { ReactNode } from "react";
import Link from "next/link";
import { clsx } from "clsx";
import { twMerge } from "tailwind-merge";
import { RefreshCw, ArrowRight } from "lucide-react";
import { guidanceFor } from "@/lib/errors";

export function cn(...args: Parameters<typeof clsx>) {
  return twMerge(clsx(...args));
}

export function Panel({
  children,
  className,
  hover,
}: {
  children: ReactNode;
  className?: string;
  hover?: boolean;
}) {
  return (
    <div className={cn("panel", hover && "panel-hover", className)}>{children}</div>
  );
}

export function Stat({
  label,
  value,
  sub,
  accent,
}: {
  label: string;
  value: ReactNode;
  sub?: string;
  accent?: string;
}) {
  return (
    <div>
      <div className="label">{label}</div>
      <div
        className="mono mt-1.5 text-[26px] font-medium leading-none tracking-tight"
        style={accent ? { color: accent } : undefined}
      >
        {value}
      </div>
      {sub && (
        <div className="mt-1 text-[11px] text-[var(--color-text-faint)]">{sub}</div>
      )}
    </div>
  );
}

export function Meter({
  value,
  max = 100,
  className,
  color,
}: {
  value: number;
  max?: number;
  className?: string;
  color?: string;
}) {
  const pct = Math.min(100, Math.max(0, (value / max) * 100));
  const c =
    color || (pct > 90 ? "var(--color-danger)" : pct > 70 ? "var(--color-warn)" : "var(--color-accent)");
  return (
    <div className={cn("meter", className)}>
      <div style={{ width: `${pct}%`, background: c }} />
    </div>
  );
}

export function Empty({
  title,
  hint,
  children,
}: {
  title: string;
  hint?: string;
  children?: ReactNode;
}) {
  return (
    <div className="flex flex-col items-center justify-center py-14 text-center">
      <div className="text-[13px] font-medium text-[var(--color-text-dim)]">{title}</div>
      {hint && (
        <div className="mt-1 max-w-[340px] text-[12px] text-[var(--color-text-faint)]">{hint}</div>
      )}
      {children && <div className="mt-4">{children}</div>}
    </div>
  );
}

export function Copyable({ text, copy, mono = true }: { text: string; copy?: string; mono?: boolean }) {
  // A real <button> — click-to-copy must be keyboard-operable. `copy`
  // overrides what lands on the clipboard for displays that truncate
  // (e.g. show "lme_abc…" but copy the clean prefix, not the ellipsis).
  return (
    <button
      type="button"
      className={`${mono ? "mono" : ""} cursor-pointer rounded bg-white/[0.06] px-1.5 py-0.5 text-[11px] text-[var(--color-text-dim)] transition-colors hover:bg-white/[0.1]`}
      title="Click to copy"
      onClick={() => navigator.clipboard.writeText(copy ?? text)}
    >
      {text}
    </button>
  );
}

// Loading placeholder for list/table panels — pulse bars in place of rows.
export function LoadingRows({ rows = 4 }: { rows?: number }) {
  return (
    <div role="status" aria-label="Loading">
      <span className="sr-only">Loading…</span>
      {Array.from({ length: rows }).map((_, i) => (
        <div key={i} className="row-line flex items-center gap-5 px-4 py-4">
          <div className="h-3 w-44 animate-pulse rounded bg-white/[0.06]" />
          <div className="h-3 w-14 animate-pulse rounded bg-white/[0.06]" />
          <div className="h-3 flex-1 animate-pulse rounded bg-white/[0.06]" />
          <div className="h-3 w-16 animate-pulse rounded bg-white/[0.06]" />
        </div>
      ))}
    </div>
  );
}

// Fetch-failure state with a retry affordance — pass SWR's mutate() so a
// click revalidates instead of reloading the page. The mapped guidance
// keeps even an opaque error actionable (never a bare "couldn't load").
export function ErrorState({
  error,
  onRetry,
}: {
  error: unknown;
  onRetry?: () => void;
}) {
  const msg = error instanceof Error ? error.message : String(error ?? "request failed");
  const g = guidanceFor(msg, "load");
  return (
    <div className="flex flex-col items-center justify-center gap-3 py-10 text-center">
      <div className="text-[12.5px] text-[var(--color-danger)]">
        Couldn&apos;t load — {msg}
      </div>
      <div className="max-w-[380px] text-[11.5px] leading-relaxed text-[var(--color-text-faint)]">
        {g.detail}
        {g.action && (
          <>
            {" "}
            <Link
              href={g.action.href}
              className="text-[var(--color-accent)] hover:underline"
            >
              {g.action.label}
            </Link>
          </>
        )}
      </div>
      {onRetry && (
        <button className="btn btn-sm" onClick={onRetry}>
          <RefreshCw size={12} /> Retry
        </button>
      )}
    </div>
  );
}

// Actionable error box for operation/request failures (not data loads —
// those use ErrorState). Shows the raw error verbatim, then maps it via
// lib/errors to a named failure class plus a concrete next step and a
// link to where the fix lives. Unknown errors still get generic
// guidance — never a bare "something went wrong".
export function ErrorNote({
  error,
  title,
  className,
}: {
  error: unknown;
  /** Optional override for the mapped headline (e.g. "No capacity"). */
  title?: string;
  className?: string;
}) {
  const raw = error instanceof Error ? error.message : String(error ?? "request failed");
  const g = guidanceFor(raw);
  return (
    <div
      className={cn(
        "rounded-md border border-[var(--color-danger)]/30 bg-[var(--color-danger-soft)] px-3 py-2",
        className
      )}
    >
      <div className="text-[12px] font-medium text-[var(--color-danger)]">
        {title ?? g.title}
      </div>
      <div className="mono mt-1 break-words text-[11.5px] text-[var(--color-danger)]/80">
        {raw}
      </div>
      <div className="mt-1.5 text-[11.5px] leading-relaxed text-[var(--color-text-dim)]">
        {g.detail}
        {g.action && (
          <>
            {" "}
            <Link
              href={g.action.href}
              className="inline-flex items-center gap-0.5 text-[var(--color-accent)] hover:underline"
            >
              {g.action.label}
              <ArrowRight size={10} />
            </Link>
          </>
        )}
      </div>
    </div>
  );
}
