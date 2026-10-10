"use client";

import { ReactNode, useEffect, useState } from "react";
import { X } from "lucide-react";

// ConfirmDialog is a modal confirmation step for destructive actions.
// Set `requireText` to force the operator to type a string (e.g. the
// model name) before the confirm button enables.
export function ConfirmDialog({
  title,
  children,
  confirmLabel = "Confirm",
  requireText,
  busy,
  error,
  onConfirm,
  onClose,
}: {
  title: string;
  children?: ReactNode;
  confirmLabel?: string;
  requireText?: string;
  busy?: boolean;
  error?: string;
  onConfirm: () => void;
  onClose: () => void;
}) {
  const [typed, setTyped] = useState("");
  const canConfirm = requireText ? typed.trim() === requireText : true;

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 px-4 backdrop-blur-[2px]"
      onClick={onClose}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-label={title}
        className="panel fade-up w-full max-w-[420px] px-5 py-5"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-start justify-between">
          <h2 className="text-[15px] font-semibold">{title}</h2>
          <button
            className="btn btn-sm -mr-1 -mt-1"
            onClick={onClose}
            aria-label="Close dialog"
          >
            <X size={14} />
          </button>
        </div>

        <div className="mt-2 text-[12.5px] leading-relaxed text-[var(--color-text-dim)]">
          {children}
        </div>

        {requireText && (
          <div className="mt-3.5">
            <div className="label mb-1.5">
              Type <span className="mono normal-case">{requireText}</span> to confirm
            </div>
            <input
              className="input mono"
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && canConfirm && !busy && onConfirm()}
              placeholder={requireText}
              autoFocus
              spellCheck={false}
            />
          </div>
        )}

        {error && (
          <div className="mt-3 rounded-md border border-red-500/30 bg-red-500/10 px-3 py-2 text-[12px] text-red-400">
            {error}
          </div>
        )}

        <div className="mt-4 flex justify-end gap-2">
          <button className="btn btn-sm" onClick={onClose} disabled={busy}>
            Cancel
          </button>
          <button
            className="btn btn-sm btn-danger"
            onClick={onConfirm}
            disabled={busy || !canConfirm}
          >
            {busy ? "Working…" : confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );
}
