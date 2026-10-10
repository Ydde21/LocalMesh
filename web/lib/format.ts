// Formatting helpers.

export function fmtBytes(b?: number): string {
  if (b === undefined || b === null || isNaN(b)) return "—";
  if (b === 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.min(Math.floor(Math.log(b) / Math.log(1024)), units.length - 1);
  return `${(b / Math.pow(1024, i)).toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

export function fmtMs(ms?: number): string {
  if (ms === undefined || ms === null) return "—";
  if (ms < 1) return "<1ms";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  return `${(ms / 1000).toFixed(2)}s`;
}

export function fmtAgo(iso?: string): string {
  if (!iso) return "—";
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 5) return "just now";
  if (s < 60) return `${Math.floor(s)}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

export function fmtUptime(sec?: number): string {
  if (sec === undefined) return "—";
  if (sec < 60) return `${sec}s`;
  if (sec < 3600) return `${Math.floor(sec / 60)}m`;
  if (sec < 86400) return `${(sec / 3600).toFixed(1)}h`;
  return `${(sec / 86400).toFixed(1)}d`;
}

// Wall-clock duration between two ISO timestamps: "34s", "3m12s", "1h5m".
// Used for op runtimes (finished_at - created_at).
export function fmtDur(from?: string, to?: string): string {
  if (!from || !to) return "—";
  const ms = new Date(to).getTime() - new Date(from).getTime();
  if (!isFinite(ms) || ms < 0) return "—";
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return s % 60 ? `${m}m${s % 60}s` : `${m}m`;
  const h = Math.floor(m / 60);
  return m % 60 ? `${h}h${m % 60}m` : `${h}h`;
}

// Countdown to a future ISO timestamp ("8s", "2m5s"); "now" once elapsed.
// Used for op retry backoffs (next_attempt_at).
export function fmtIn(iso?: string): string {
  if (!iso) return "—";
  const ms = new Date(iso).getTime() - Date.now();
  if (!isFinite(ms)) return "—";
  if (ms <= 0) return "now";
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return s % 60 ? `${m}m${s % 60}s` : `${m}m`;
  const h = Math.floor(m / 60);
  return m % 60 ? `${h}h${m % 60}m` : `${h}h`;
}

// Bare semver gets a "v" prefix ("0.5.0" → "v0.5.0"); already-prefixed or
// non-semver strings ("dev") pass through untouched.
export function fmtVersion(v?: string): string {
  if (!v) return "—";
  return /^\d/.test(v) ? `v${v}` : v;
}

// A node's agent version — emitted top-level on v0.5+ servers, only inside
// the hardware snapshot on older ones. Empty = pre-version agent ("legacy").
export function agentVersion(n: { agent_version?: string; hardware?: { agent_version?: string } }): string {
  return n.agent_version || n.hardware?.agent_version || "";
}

export function osLabel(os: string): string {
  return os === "darwin" ? "macOS" : os;
}
