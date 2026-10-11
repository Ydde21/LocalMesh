"use client";

// Inference playground: send one chat completion through the real
// gateway (/v1/chat/completions via the /api/lm/openai BFF prefix), watch
// tokens stream in, and see which node the scheduler picked — zero code
// required. Requests land in Workloads like any other inference call;
// nothing is persisted client-side (privacy rule: no prompt history).
import { useEffect, useMemo, useRef, useState } from "react";
import Link from "next/link";
import { useModels, useNodes, ApiError } from "@/lib/api";
import { useSWRConfig } from "swr";
import { useEvents } from "@/components/shell";
import { Panel, Empty, ErrorNote, LoadingRows } from "@/components/ui";
import { StatusBadge } from "@/components/status";
import { fmtBytes, fmtMs } from "@/lib/format";
import type { ClusterModel, Node, Workload } from "@/lib/types";
import { FlaskConical, Send, Square, ArrowRight } from "lucide-react";

type Phase = "idle" | "sending" | "streaming" | "done" | "error" | "cancelled";

interface Usage {
  prompt_tokens?: number;
  completion_tokens?: number;
  total_tokens?: number;
}

// The gateway stamps SSE chunks with "chatcmpl-" + workload.id.slice(0,24)
// — a strict prefix of the workload UUID, so a prefix match against
// workload rows (SSE events or the admin list) is the honest way to
// attribute this request to a node. No fabricated attribution.
const CHAT_ID_PREFIX = "chatcmpl-";

function schedulable(n: Node): boolean {
  // Mirrors the gateway's /v1/models rule: approved, connected, fresh
  // heartbeat, and not under any maintenance state.
  return (
    n.status === "approved" &&
    n.online &&
    !n.stale_heartbeat &&
    !n.maintenance
  );
}

export default function PlaygroundPage() {
  const { data: models, error: modelsErr } = useModels();
  const { data: nodes, error: nodesErr } = useNodes();
  const { events } = useEvents();
  const { mutate } = useSWRConfig();

  const [model, setModel] = useState("");
  const [preferNode, setPreferNode] = useState("");
  const [prompt, setPrompt] = useState("");
  const [maxTokens, setMaxTokens] = useState("");
  const [temperature, setTemperature] = useState("");

  const [phase, setPhase] = useState<Phase>("idle");
  const [output, setOutput] = useState("");
  const [errText, setErrText] = useState("");
  const [usage, setUsage] = useState<Usage | null>(null);
  const [finishReason, setFinishReason] = useState("");
  // Workload attribution: the SSE chunk id carries the workload-id
  // prefix; the matching row resolves via the shared events feed, or a
  // one-shot list fetch as fallback.
  const [wlPrefix, setWlPrefix] = useState("");
  const [fallbackWl, setFallbackWl] = useState<Workload | null>(null);
  // Ref mirror of wlPrefix — the async send()/resolveWorkload
  // continuation can't see state set mid-stream.
  const wlPrefixRef = useRef("");

  // Timing — performance.now() refs (wall-clock deltas, not render state).
  const t0Ref = useRef(0);
  const ttftRef = useRef<number | null>(null);
  const endRef = useRef<number | null>(null);
  const [tick, setTick] = useState(0);

  const abortRef = useRef<AbortController | null>(null);

  const busy = phase === "sending" || phase === "streaming";

  // --- model inventory ----------------------------------------------------
  const onlineSched = useMemo(
    () => new Map((nodes ?? []).filter(schedulable).map((n) => [n.id, n])),
    [nodes]
  );

  interface Usable {
    m: ClusterModel;
    hosts: Node[]; // schedulable nodes carrying this model
  }
  const { usable, unusableNames } = useMemo(() => {
    const usable: Usable[] = [];
    const unusableNames: string[] = [];
    for (const m of models ?? []) {
      const hosts = (m.node_ids ?? [])
        .map((id) => onlineSched.get(id))
        .filter((n): n is Node => !!n);
      if (hosts.length > 0) usable.push({ m, hosts });
      else unusableNames.push(m.name);
    }
    return { usable, unusableNames };
  }, [models, onlineSched]);

  const selected = usable.find((u) => u.m.name === model);
  // If a refreshed inventory drops the selected model, clear the stale
  // pick via render-phase adjustment (no effect needed).
  const [prevModels, setPrevModels] = useState(models);
  if (prevModels !== models) {
    setPrevModels(models);
    if (model && models && !usable.some((u) => u.m.name === model)) setModel("");
  }
  // The node pin is only honored while it hosts the selected model —
  // derived, so inventory changes can never send a bogus pin upstream.
  const pinValid =
    preferNode !== "" && !!selected?.hosts.some((n) => n.id === preferNode);

  // Loaded-state lookup for host chips (node's own model list is
  // authoritative — catalog rows aggregate).
  const hostModelState = (n: Node, name: string) =>
    (n.models ?? []).find((mo) => mo.name === name);

  // --- workload attribution via the shared SSE feed -----------------------
  // Pure derivation over the events context — no setState, no effect.
  const eventWl = useMemo(() => {
    if (!wlPrefix) return null;
    let found: Workload | null = null;
    for (const e of events) {
      if (!e.type.startsWith("workload.")) continue;
      const w = e.data as Workload | undefined;
      if (!w || typeof w.id !== "string" || !w.id.startsWith(wlPrefix)) continue;
      // The finished row carries the terminal numbers; prefer it over
      // the queued/started duplicates of the same workload.
      if (!found || e.type === "workload.finished") found = w;
    }
    return found;
  }, [events, wlPrefix]);

  const wl = eventWl ?? fallbackWl;
  const workloadId = wl?.id ?? "";
  const nodeName = wl?.node_name ?? "";

  // Fallback when the events feed missed it (dropped connection): one
  // list fetch after the stream settles, still prefix-matched. The row
  // can lag the stream end — the backend finalizes asynchronously — so
  // a non-terminal row gets one retry before we take it on faith.
  const fetchWorkloadRow = async (prefix: string) => {
    const rows = (await fetch("/api/lm/workloads?limit=25", { cache: "no-store" })
      .then((r) => (r.ok ? r.json() : []))
      .catch(() => [])) as Workload[];
    return rows.find((x) => typeof x.id === "string" && x.id.startsWith(prefix));
  };
  const resolveWorkload = async () => {
    const prefix = wlPrefixRef.current;
    if (!prefix) return;
    try {
      let w = await fetchWorkloadRow(prefix);
      if (w && (w.status === "queued" || w.status === "running")) {
        await new Promise((r) => setTimeout(r, 1500));
        w = (await fetchWorkloadRow(prefix)) ?? w;
      }
      if (w) setFallbackWl(w);
    } catch {
      /* attribution is best-effort */
    }
  };

  // Elapsed ticker while a request is live.
  useEffect(() => {
    if (!busy) return;
    const t = setInterval(() => setTick((x) => x + 1), 100);
    return () => clearInterval(t);
  }, [busy]);

  // --- send / stream ------------------------------------------------------

  const resetRun = () => {
    setPhase("sending");
    setOutput("");
    setErrText("");
    setUsage(null);
    setFinishReason("");
    setWlPrefix("");
    setFallbackWl(null);
    wlPrefixRef.current = "";
    ttftRef.current = null;
    endRef.current = null;
    t0Ref.current = performance.now();
  };

  const send = async () => {
    if (!model || !prompt.trim() || busy) return;
    resetRun();
    const ctrl = new AbortController();
    abortRef.current = ctrl;

    try {
      const res = await fetch("/api/lm/openai/chat/completions", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Accept: "text/event-stream",
        },
        body: JSON.stringify({
          model,
          stream: true,
          stream_options: { include_usage: true },
          messages: [{ role: "user", content: prompt }],
          ...(maxTokens.trim() ? { max_tokens: Number(maxTokens) } : {}),
          ...(temperature.trim() ? { temperature: Number(temperature) } : {}),
          // LocalMesh extension — pin scheduling to one node.
          ...(pinValid ? { x_localmesh_node: preferNode } : {}),
        }),
        signal: ctrl.signal,
      });

      if (!res.ok) {
        const body = await res.json().catch(() => ({}));
        throw new ApiError(
          res.status,
          body?.error?.message || `request failed (${res.status})`
        );
      }
      if (!res.body) throw new ApiError(0, "empty response stream");
      setPhase("streaming");

      const reader = res.body.getReader();
      const dec = new TextDecoder();
      let buf = "";
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let idx;
        while ((idx = buf.indexOf("\n\n")) >= 0) {
          consumeSSE(buf.slice(0, idx));
          buf = buf.slice(idx + 2);
        }
      }
      buf += dec.decode();
      if (buf.trim()) consumeSSE(buf);

      endRef.current ??= performance.now();
      setPhase((p) => (p === "streaming" ? "done" : p));
      void resolveWorkload();
      // Refresh the workloads surfaces (list + onboarding probe).
      mutate((key) => typeof key === "string" && key.startsWith("/workloads"));
    } catch (e) {
      endRef.current ??= performance.now();
      if (e instanceof DOMException && e.name === "AbortError") {
        setPhase("cancelled");
        void resolveWorkload();
      } else {
        setErrText(e instanceof Error ? e.message : String(e));
        setPhase("error");
        void resolveWorkload();
      }
    } finally {
      abortRef.current = null;
    }
  };

  // One SSE event block (possibly several data: lines joined).
  const consumeSSE = (block: string) => {
    const data = block
      .split("\n")
      .filter((l) => l.startsWith("data:"))
      .map((l) => l.slice(5).trim())
      .join("\n");
    if (!data || data === "[DONE]") return;
    let obj: {
      id?: string;
      choices?: { delta?: { content?: string }; finish_reason?: string | null }[];
      usage?: Usage | null;
      error?: { message?: string; type?: string };
    };
    try {
      obj = JSON.parse(data);
    } catch {
      return;
    }
    if (obj.error) {
      endRef.current ??= performance.now();
      setErrText(obj.error.message || "stream error");
      setPhase("error");
      return;
    }
    if (obj.id && !wlPrefixRef.current) {
      wlPrefixRef.current = obj.id.startsWith(CHAT_ID_PREFIX)
        ? obj.id.slice(CHAT_ID_PREFIX.length)
        : obj.id;
      setWlPrefix(wlPrefixRef.current);
    }
    const delta = obj.choices?.[0]?.delta?.content;
    if (delta) {
      if (ttftRef.current === null) ttftRef.current = performance.now() - t0Ref.current;
      setOutput((o) => o + delta);
    }
    const fr = obj.choices?.[0]?.finish_reason;
    if (fr) setFinishReason(fr);
    if (obj.usage) setUsage(obj.usage);
  };

  const cancel = () => {
    endRef.current ??= performance.now();
    setPhase("cancelled");
    abortRef.current?.abort();
  };

  // --- derived metrics -----------------------------------------------------
  const now = busy ? performance.now() : (endRef.current ?? t0Ref.current);
  const elapsed = phase === "idle" ? null : now - t0Ref.current;
  const ttft = ttftRef.current;
  const genSecs =
    ttft !== null && endRef.current !== null ? (endRef.current - t0Ref.current - ttft) / 1000 : null;
  const tokPerSec =
    usage?.completion_tokens != null && genSecs && genSecs > 0.05
      ? usage.completion_tokens / genSecs
      : null;
  void tick; // ticker state — value itself is only a re-render trigger

  // The workload row is authoritative once it reaches a terminal state.
  // A stream that was cut off can still end with reader `done` — a lost
  // error chunk, a proxy drop, a node gone mid-decode — so "done" only
  // stands while the row agrees. A finished row reporting failed/
  // cancelled makes the verdict honest instead of a fake "completed".
  const wlFailed = phase === "done" && wl?.status === "failed";
  const wlCancelled = phase === "done" && wl?.status === "cancelled";

  const phaseStatus =
    wlFailed || phase === "error"
      ? "failed"
      : wlCancelled || phase === "cancelled"
        ? "cancelled"
        : phase === "sending"
          ? "queued"
          : phase === "streaming"
            ? "running"
            : phase === "done"
              ? "completed"
              : "";

  return (
    <div className="mx-auto max-w-[1200px] px-6 py-6">
      <header className="mb-6 flex items-end justify-between">
        <div>
          <h1 className="flex items-center gap-2 text-[19px] font-semibold tracking-tight">
            <FlaskConical size={17} className="text-[var(--color-accent)]" />
            Playground
          </h1>
          <p className="mt-0.5 text-[12.5px] text-[var(--color-text-dim)]">
            Send one request through the real gateway — verify the cluster end to end, no code.
          </p>
        </div>
      </header>

      <div className="grid grid-cols-1 gap-3 lg:grid-cols-5">
        {/* -------- composer -------- */}
        <Panel className="fade-up self-start px-4 py-4 lg:col-span-2">
          <div className="label mb-2.5">Model</div>
          {modelsErr || nodesErr ? (
            <ErrorNote error={modelsErr ?? nodesErr} />
          ) : models === undefined || nodes === undefined ? (
            <LoadingRows rows={1} />
          ) : usable.length === 0 ? (
            <Empty
              title="No usable models"
              hint="Install a model on an online node first — the picker only lists models the scheduler can actually serve."
            >
              <div className="flex flex-col items-center gap-2">
                {unusableNames.length > 0 && (
                  <div className="max-w-[340px] text-center text-[11px] text-[var(--color-warn)]">
                    Installed but unschedulable (offline or out of service):{" "}
                    {unusableNames.join(", ")}
                  </div>
                )}
                <Link href="/models" className="btn btn-primary btn-sm">
                  Install a model
                </Link>
              </div>
            </Empty>
          ) : (
            <>
              <select
                className="input mono w-full"
                aria-label="Model"
                value={model}
                onChange={(e) => {
                  setModel(e.target.value);
                  setPreferNode(""); // pin is per-model
                }}
                disabled={busy}
              >
                <option value="" disabled>
                  pick a model…
                </option>
                {usable.map((u) => (
                  <option key={u.m.name} value={u.m.name}>
                    {u.m.name}
                  </option>
                ))}
              </select>

              {selected && (
                <div className="mt-2 flex flex-wrap items-center gap-1.5">
                  <span className="text-[10.5px] text-[var(--color-text-faint)]">lives on</span>
                  {selected.hosts.map((n) => {
                    const loaded = !!hostModelState(n, selected.m.name)?.loaded;
                    return (
                      <span
                        key={n.id}
                        className={`badge ${loaded ? "badge-ok" : "badge-neutral"} !px-1.5 !py-0`}
                        title={loaded ? "resident in engine memory" : "installed, cold"}
                      >
                        {loaded && <span className="dot" />}
                        {n.name}
                      </span>
                    );
                  })}
                  <span className="mono text-[10.5px] text-[var(--color-text-faint)]">
                    {fmtBytes(selected.m.size_bytes)}
                  </span>
                </div>
              )}

              {unusableNames.length > 0 && (
                <div className="mt-2 text-[10.5px] leading-relaxed text-[var(--color-text-faint)]">
                  Also installed but not schedulable right now:{" "}
                  <span className="text-[var(--color-warn)]">{unusableNames.join(", ")}</span>
                </div>
              )}
            </>
          )}

          {selected && selected.hosts.length > 1 && (
            <div className="mt-4">
              <div className="label mb-1.5">Route</div>
              <select
                className="input w-full"
                aria-label="Route — pin to a node or let the scheduler pick"
                value={pinValid ? preferNode : ""}
                onChange={(e) => setPreferNode(e.target.value)}
                disabled={busy}
              >
                <option value="">auto — scheduler picks</option>
                {selected.hosts.map((n) => (
                  <option key={n.id} value={n.id}>
                    pin to {n.name}
                  </option>
                ))}
              </select>
            </div>
          )}

          <div className="label mb-1.5 mt-4">Prompt</div>
          <textarea
            className="input min-h-[130px] w-full resize-y"
            aria-label="Prompt"
            placeholder="Ask the cluster something…"
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            disabled={busy}
            onKeyDown={(e) => {
              if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) void send();
            }}
          />
          <div className="mt-1 flex items-center justify-between text-[10.5px] text-[var(--color-text-faint)]">
            <span>{prompt.length > 0 ? `${prompt.length} chars` : ""}</span>
            <span>⌘/Ctrl+Enter to send</span>
          </div>

          <div className="mt-3 grid grid-cols-2 gap-2">
            <div>
              <div className="label mb-1">Max tokens</div>
              <input
                className="input mono w-full"
                type="number"
                min={1}
                aria-label="Max tokens"
                placeholder="engine default"
                value={maxTokens}
                onChange={(e) => setMaxTokens(e.target.value)}
                disabled={busy}
              />
            </div>
            <div>
              <div className="label mb-1">Temperature</div>
              <input
                className="input mono w-full"
                type="number"
                min={0}
                max={2}
                step={0.1}
                aria-label="Temperature"
                placeholder="engine default"
                value={temperature}
                onChange={(e) => setTemperature(e.target.value)}
                disabled={busy}
              />
            </div>
          </div>

          <div className="mt-4 flex items-center gap-2">
            {busy ? (
              <button className="btn btn-danger btn-sm" onClick={cancel}>
                <Square size={12} /> Cancel
              </button>
            ) : (
              <button
                className="btn btn-primary btn-sm"
                disabled={!model || !prompt.trim() || usable.length === 0}
                onClick={() => void send()}
              >
                <Send size={12} /> Send
              </button>
            )}
            {phase !== "idle" && <StatusBadge status={phaseStatus} />}
            {workloadId && (
              <Link
                href={`/workloads?id=${workloadId}`}
                className="ml-auto inline-flex items-center gap-1 text-[11px] text-[var(--color-accent)] hover:underline"
              >
                workload <ArrowRight size={10} />
              </Link>
            )}
          </div>
          <p className="mt-2.5 text-[10.5px] leading-relaxed text-[var(--color-text-faint)]">
            Goes through the same /v1/chat/completions gateway as any client — shows up
            in Workloads. Prompts are never stored.
          </p>
        </Panel>

        {/* -------- response -------- */}
        <Panel className="fade-up flex min-h-[420px] flex-col lg:col-span-3">
          <div
            className="flex flex-wrap items-center gap-x-4 gap-y-1 border-b px-4 py-3"
            style={{ borderColor: "var(--color-border)" }}
          >
            <div className="label">Response</div>
            {phase !== "idle" && (
              <div className="mono ml-auto flex items-center gap-3 text-[11px] text-[var(--color-text-dim)]">
                <span title="wall-clock elapsed">
                  {elapsed !== null ? fmtMs(elapsed) : "—"}
                </span>
                <span title="time to first token (client-measured)">
                  ttft {ttft !== null ? fmtMs(ttft) : "—"}
                </span>
                <span title="completion tokens per second of decode time (server usage)">
                  {tokPerSec !== null ? `${tokPerSec.toFixed(1)} tok/s` : "—"}
                </span>
                {usage && (
                  <span title="prompt → completion tokens (server-reported)">
                    {usage.prompt_tokens ?? "?"}→{usage.completion_tokens ?? "?"} tok
                  </span>
                )}
                {nodeName && (
                  <span
                    className="badge badge-neutral !px-1.5 !py-0"
                    title="node the scheduler dispatched to (from the workload row)"
                  >
                    {nodeName}
                  </span>
                )}
              </div>
            )}
          </div>

          <div className="flex-1 overflow-y-auto px-4 py-3">
            {phase === "idle" && (
              <div className="flex h-full items-center justify-center text-center">
                <div className="max-w-[300px] text-[12px] text-[var(--color-text-faint)]">
                  Output streams here token by token — timing, token usage, and the
                  node that served it appear above.
                </div>
              </div>
            )}
            {phase === "sending" && (
              <div className="flex h-full items-center justify-center text-[12px] text-[var(--color-text-faint)]">
                <span className="pulse">scheduling…</span>
              </div>
            )}
            {(phase === "streaming" || phase === "done" || phase === "cancelled" ||
              (phase === "error" && output)) && (
              <div className="whitespace-pre-wrap break-words text-[13px] leading-relaxed text-[var(--color-text)]">
                {output}
                {phase === "streaming" && (
                  <span className="pulse text-[var(--color-accent)]">▍</span>
                )}
              </div>
            )}
            {phase === "error" && <ErrorNote error={errText} className="mt-2" />}
            {wlFailed && (
              <ErrorNote
                error={
                  wl?.error ||
                  "the response stream ended before the workload finished"
                }
                title="Interrupted — the stream was cut short"
                className="mt-2"
              />
            )}
            {wlCancelled && (
              <div className="mt-2 rounded-md border border-[var(--color-border)] bg-white/[0.03] px-3 py-2 text-[12px] text-[var(--color-text-dim)]">
                Cancelled — the control plane marked this workload{" "}
                <span className="mono">cancelled</span> (client disconnect).
              </div>
            )}
            {phase === "cancelled" && (
              <div className="mt-2 rounded-md border border-[var(--color-border)] bg-white/[0.03] px-3 py-2 text-[12px] text-[var(--color-text-dim)]">
                Cancelled — the request was aborted client-side; the server marks this
                workload <span className="mono">cancelled</span>.
              </div>
            )}
          </div>

          {(phase === "done" || phase === "cancelled" || phase === "error") && (
            <div
              className="flex items-center gap-3 border-t px-4 py-2 text-[10.5px] text-[var(--color-text-faint)]"
              style={{ borderColor: "var(--color-border)" }}
            >
              {finishReason && <span>finish: {finishReason}</span>}
              {nodeName && <span>served by {nodeName}</span>}
              {workloadId && (
                <Link
                  href={`/workloads?id=${workloadId}`}
                  className="ml-auto inline-flex items-center gap-1 text-[var(--color-accent)] hover:underline"
                >
                  open workload <ArrowRight size={10} />
                </Link>
              )}
            </div>
          )}
        </Panel>
      </div>
    </div>
  );
}
