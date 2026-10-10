"use client";

import { Suspense, useState } from "react";
import { useSearchParams, useRouter } from "next/navigation";
import { useWorkloads, useWorkload } from "@/lib/api";
import { fmtMs, fmtAgo } from "@/lib/format";
import { Panel, Empty, Meter } from "@/components/ui";
import { StatusBadge } from "@/components/status";
import type { CandidateReport } from "@/lib/types";
import { X } from "lucide-react";

export default function WorkloadsPage() {
  return (
    <Suspense fallback={null}>
      <WorkloadsInner />
    </Suspense>
  );
}

function WorkloadsInner() {
  const params = useSearchParams();
  const router = useRouter();
  const selId = params.get("id");
  const [filter, setFilter] = useState("");
  const { data: workloads } = useWorkloads(filter || undefined);

  return (
    <div className="mx-auto max-w-[1200px] px-6 py-6">
      <header className="mb-6 flex items-end justify-between">
        <div>
          <h1 className="text-[19px] font-semibold tracking-tight">Workloads</h1>
          <p className="mt-0.5 text-[12.5px] text-[var(--color-text-dim)]">
            Inference requests and routing decisions
          </p>
        </div>
        <div className="flex gap-1.5">
          {["", "running", "queued", "completed", "failed", "cancelled"].map((f) => (
            <button
              key={f || "all"}
              className={`btn btn-sm ${filter === f ? "btn-primary" : ""}`}
              onClick={() => setFilter(f)}
            >
              {f || "all"}
            </button>
          ))}
        </div>
      </header>

      <Panel className="fade-up">
        {(!workloads || workloads.length === 0) ? (
          <Empty
            title="No workloads"
            hint="Send a request to POST /v1/chat/completions on the control plane to see routing here."
          />
        ) : (
          <table className="w-full text-left">
            <thead>
              <tr className="border-b text-[11px]" style={{ borderColor: "var(--color-border)" }}>
                <Th>Model</Th>
                <Th>Node</Th>
                <Th>Status</Th>
                <Th>TTFT</Th>
                <Th>Total</Th>
                <Th>Queue wait</Th>
                <Th>Tokens</Th>
                <Th>Age</Th>
              </tr>
            </thead>
            <tbody>
              {workloads.map((w) => (
                <tr
                  key={w.id}
                  className={`row-line cursor-pointer transition-colors hover:bg-white/[0.03] ${w.id === selId ? "bg-white/[0.04]" : ""}`}
                  style={{ borderColor: "var(--color-border)" }}
                  onClick={() => router.push(`/workloads?id=${w.id}`)}
                >
                  <Td><span className="mono text-[12.5px] font-medium">{w.model}</span>
                    {w.streaming && <span className="ml-2 badge badge-neutral !py-0">stream</span>}
                  </Td>
                  <Td><span className="text-[12px]">{w.node_name || "—"}</span></Td>
                  <Td><StatusBadge status={w.status} /></Td>
                  <Td><span className="mono text-[11.5px]">{fmtMs(w.ttft_ms)}</span></Td>
                  <Td><span className="mono text-[11.5px]">{fmtMs(w.total_ms)}</span></Td>
                  <Td>
                    <span className="mono text-[11.5px]">
                      {w.decision?.queued_ms != null && w.decision.queued_ms > 0
                        ? fmtMs(w.decision.queued_ms)
                        : "—"}
                    </span>
                  </Td>
                  <Td>
                    <span className="mono text-[11.5px] text-[var(--color-text-dim)]">
                      {w.prompt_tokens != null ? `${w.prompt_tokens}→${w.completion_tokens ?? 0}` : "—"}
                    </span>
                  </Td>
                  <Td><span className="text-[11px] text-[var(--color-text-faint)]">{fmtAgo(w.queued_at)}</span></Td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>

      {selId && <WorkloadDetail id={selId} />}
    </div>
  );
}

function Th({ children }: { children?: React.ReactNode }) {
  return <th className="label px-4 py-2.5 font-medium">{children}</th>;
}
function Td({ children }: { children?: React.ReactNode }) {
  return <td className="px-4 py-3 align-middle">{children}</td>;
}

// ---------------------------------------------------------------------------

function WorkloadDetail({ id }: { id: string }) {
  const router = useRouter();
  const { data: w } = useWorkload(id);
  if (!w) return null;
  const d = w.decision;

  return (
    <div className="fixed inset-0 z-40 bg-black/50 backdrop-blur-[2px]"
      onClick={() => router.push("/workloads")}>
      <div className="panel absolute right-0 top-0 h-full w-[520px] max-w-[94vw] overflow-y-auto border-l"
        style={{ borderColor: "var(--color-border)", borderRadius: 0 }}
        onClick={(e) => e.stopPropagation()}>
        <div className="flex items-start justify-between border-b px-5 py-4" style={{ borderColor: "var(--color-border)" }}>
          <div>
            <div className="flex items-center gap-2.5">
              <h2 className="mono text-[14px] font-semibold">{w.model}</h2>
              <StatusBadge status={w.status} />
            </div>
            <div className="mono mt-1 text-[11px] text-[var(--color-text-faint)]">{w.id}</div>
          </div>
          <a href="/workloads" className="btn btn-sm"><X size={14} /></a>
        </div>

        <div className="space-y-5 px-5 py-5">
          <section className="grid grid-cols-3 gap-3">
            <DetailStat label="Node" value={w.node_name || "—"} />
            <DetailStat label="TTFT" value={fmtMs(w.ttft_ms)} />
            <DetailStat label="Total" value={fmtMs(w.total_ms)} />
            <DetailStat
              label="Queue wait"
              value={d?.queued_ms != null && d.queued_ms > 0 ? fmtMs(d.queued_ms) : "—"}
            />
            <DetailStat label="Attempt" value={d?.attempt != null ? String(d.attempt) : "—"} />
            <DetailStat label="Stream" value={w.streaming ? "yes" : "no"} />
            <DetailStat label="Prompt tokens" value={w.prompt_tokens?.toString() ?? "—"} />
            <DetailStat label="Completion" value={w.completion_tokens?.toString() ?? "—"} />
          </section>

          {w.error && (
            <section>
              <div className="label mb-1.5">Error</div>
              <div className="rounded-md border border-red-500/30 bg-red-500/10 px-3 py-2 mono text-[12px] text-red-300">
                {w.error}
              </div>
            </section>
          )}

          {/* Scheduling explanation */}
          {d && (
            <section>
              <div className="label mb-2">Routing decision</div>
              <div className="rounded-md border border-[var(--color-accent)]/25 bg-[var(--color-accent-soft)] px-3 py-2.5 text-[12.5px] leading-relaxed">
                {d.reason}
              </div>
              <div className="mt-3 space-y-2">
                <div className="label">Candidates</div>
                {(d.candidates ?? []).map((c) => (
                  <CandidateRow key={c.node_id} c={c} chosen={d.chosen_node_id || d.chosen} />
                ))}
                {(d.candidates ?? []).length === 0 && (
                  <div className="text-[11px] text-[var(--color-text-faint)]">
                    No candidate reports recorded.
                  </div>
                )}
              </div>
            </section>
          )}

          <section className="text-[10.5px] text-[var(--color-text-faint)]">
            Queued {fmtAgo(w.queued_at)}
            {w.started_at && ` · started ${fmtAgo(w.started_at)}`}
            {w.finished_at && ` · finished ${fmtAgo(w.finished_at)}`}
            <div className="mt-1">Prompts and responses are never stored — only metadata.</div>
          </section>
        </div>
      </div>
    </div>
  );
}

function DetailStat({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <div className="text-[10.5px] text-[var(--color-text-faint)]">{label}</div>
      <div className="mono mt-0.5 text-[13px] font-medium">{value}</div>
    </div>
  );
}

function CandidateRow({ c, chosen }: { c: CandidateReport; chosen?: string }) {
  const isChosen = c.node_id === chosen;
  const maxScore = 12; // soft cap for the meter
  return (
    <div className={`rounded-md border px-3 py-2.5 ${isChosen ? "border-[var(--color-online)]/30 bg-[var(--color-online-soft)]" : "border-[var(--color-border)]"}`}>
      <div className="flex items-center gap-2.5">
        <span className={`h-1.5 w-1.5 rounded-full ${isChosen ? "bg-[var(--color-online)]" : c.eligible ? "bg-[var(--color-text-faint)]" : "bg-[var(--color-danger)]"}`} />
        <span className="text-[12.5px] font-medium">{c.node_name}</span>
        {isChosen && <span className="badge badge-ok !py-0">selected</span>}
        {!c.eligible && <span className="badge badge-danger !py-0">filtered</span>}
        {c.eligible && <span className="mono ml-auto text-[11.5px] text-[var(--color-text-dim)]">score {c.score.toFixed(2)}</span>}
      </div>
      {c.eligible ? (
        <>
          <Meter className="mt-2" value={c.score} max={maxScore} />
          <div className="mt-1.5 text-[10.5px] leading-relaxed text-[var(--color-text-faint)]">
            {(c.reasons ?? []).join(" · ")}
          </div>
        </>
      ) : (
        <div className="mt-1.5 text-[10.5px] text-[var(--color-danger)]/80">{c.filtered_reason}</div>
      )}
    </div>
  );
}
