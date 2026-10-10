// Types mirroring the LocalMesh control-plane API.

export interface GPU {
  name: string;
  vendor?: string;
  vram_bytes?: number;
  shared_vram?: boolean;
  driver?: string;
}

export interface HardwareInfo {
  hostname: string;
  os: string;
  arch: string;
  cpu_model: string;
  cpu_cores: number;
  memory_bytes: number;
  gpus?: GPU[];
  agent_version: string;
}

export interface Metrics {
  cpu_util_percent: number;
  memory_used_bytes: number;
  memory_total_bytes: number;
  memory_available_bytes?: number;
  gpu_util_percent?: number;
  gpu_vram_used_bytes?: number;
  active_requests: number;
  loaded_models?: string[];
  uptime_seconds: number;
  // Measured flags: when not true the corresponding field is UNKNOWN —
  // the agent could not produce a real measurement that cycle. Never
  // render an unmeasured value as a real number (e.g. 0%).
  cpu_util_measured?: boolean;
  mem_avail_measured?: boolean;
  gpu_util_measured?: boolean;
  vram_used_measured?: boolean;
  loaded_measured?: boolean;
}

export interface EngineInfo {
  type: string;
  version: string;
  healthy: boolean;
  address?: string;
}

export interface Model {
  name: string;
  engine: string;
  family?: string;
  parameter_size?: string;
  quantization?: string;
  size_bytes: number;
  digest?: string;
  capabilities?: string[];
  modified_at?: string;
  loaded: boolean;
  // Real resident footprint reported by the engine while loaded
  // (Ollama /api/ps `size`/`size_vram`). 0 when cold or unmeasurable.
  loaded_bytes?: number;
  loaded_vram_bytes?: number;
  runner?: string;
}

export type NodeStatus = "pending" | "approved" | "revoked";

export interface Node {
  id: string;
  name: string;
  status: NodeStatus;
  online: boolean;
  stale_heartbeat: boolean;
  hardware: HardwareInfo;
  // Top-level agent version (v0.5+). Older servers report it only inside
  // `hardware.agent_version` — read `node.agent_version ?? hardware.agent_version`.
  agent_version?: string;
  engines?: EngineInfo[];
  models?: Model[];
  metrics?: Metrics | null;
  labels: Record<string, string>;
  max_concurrent: number;
  in_flight: number;
  latency_ms?: number;
  last_seen_at?: string;
  created_at: string;
  // v0.7+ maintenance/drain state. Absent on older servers — the field's
  // presence is the capability probe for the maintenance endpoints.
  //   ''           in service
  //   'draining'   no new work, in-flight requests finishing
  //   'maintenance' drained — admin may act on the node
  //   'updating'   set by the rollout orchestrator during agent upgrades
  maintenance?: "" | "draining" | "maintenance" | "updating";
  maintenance_reason?: string;
  maintenance_since?: string;
}

export interface Overview {
  nodes_total: number;
  nodes_online: number;
  cpu_cores_total: number;
  memory_bytes: number;
  memory_used: number;
  gpu_count: number;
  vram_bytes: number;
  active_requests: number;
  queued_or_active: number;
  models_distinct: number;
  // v0.5+: control-plane base URL for agent join commands. Absent on older
  // servers — fall back to a placeholder in the UI.
  server_url?: string;
  // v0.5+: server-side counts of model ops in flight / waiting. Absent on
  // older servers — consumers compute them from useModelOps instead.
  model_ops_active?: number;
  model_ops_queued?: number;
}

// Cluster catalog entries are aggregated server-side from node model
// lists; Go nil slices serialize as null, so array fields may be null.
export interface ClusterModel {
  name: string;
  engine: string;
  family?: string;
  parameter_size?: string;
  size_bytes: number;
  digest?: string;
  node_ids: string[] | null;
  node_names: string[] | null;
  loaded_on: string[] | null;
  nodes: number;
  // v0.7+ availability rollup — absent on older servers; render as today.
  availability?: "available" | "loaded" | "offline_only" | "unavailable";
  eligible_nodes?: number;
  loaded_nodes?: number;
}

// GET /models/placement — cluster-wide placement view per model.
export interface PlacementNode {
  id: string;
  name: string;
  online: boolean;
  digest?: string;
  loaded: boolean;
  loaded_mb?: number;
}
export interface PlacementCandidate {
  id: string;
  name: string;
  reason: string;
  free_mb?: number;
  eligible: boolean;
}
export interface PlacementEntry {
  name: string;
  engine: string;
  family?: string;
  size_mb: number;
  est_mem_mb: number;
  replicas: number;
  loaded_on: number;
  nodes: PlacementNode[] | null;
  candidates: PlacementCandidate[] | null;
  active_ops?: string[] | null;
  last_replica: boolean;
}

export interface CandidateReport {
  node_id: string;
  node_name: string;
  eligible: boolean;
  score: number;
  selected: boolean;
  reasons?: string[];
  filtered_reason?: string;
}

export interface Decision {
  model: string;
  chosen_node_id?: string;
  chosen?: string;
  reason: string;
  candidates: CandidateReport[];
  queued_ms?: number;
  attempt: number;
}

export interface Workload {
  id: string;
  model: string;
  node_id?: string;
  node_name?: string;
  api_key_id?: string;
  status: "queued" | "running" | "completed" | "failed" | "cancelled";
  streaming: boolean;
  prompt_chars: number;
  decision?: Decision;
  error?: string;
  prompt_tokens?: number;
  completion_tokens?: number;
  ttft_ms?: number;
  total_ms?: number;
  queued_at: string;
  started_at?: string;
  finished_at?: string;
}

export interface EnrollmentToken {
  id: string;
  label: string;
  prefix: string;
  max_uses: number;
  uses: number;
  expires_at: string;
  revoked_at?: string;
  created_at: string;
}

export interface EnrollmentRequest {
  id: string;
  token_id: string;
  name: string;
  hardware: HardwareInfo;
  status: "pending" | "approved" | "denied";
  node_id?: string;
  created_at: string;
  decided_at?: string;
}

export interface APIKey {
  id: string;
  name: string;
  prefix: string;
  role: "admin" | "inference" | "readonly";
  last_used_at?: string;
  revoked_at?: string;
  created_at: string;
}

// Model pull/delete operations dispatched to nodes. Progress rides in on
// `detail` (engine status line) plus completed/total byte counters.
// cancelled/interrupted are produced when an op is aborted or a node
// drops mid-operation.
//
// v0.5 adds a queue: `queued` = waiting for a concurrency slot (or retry
// backoff when `next_attempt_at` is set); `pending`/`completed` are legacy
// spellings of `queued`/`succeeded` on old rows.
export type ModelOpStatus =
  | "queued"
  | "dispatched"
  | "pending"
  | "running"
  | "succeeded"
  | "completed"
  | "failed"
  | "cancelled"
  | "interrupted";

export interface ModelOperation {
  id: string;
  node_id: string;
  node_name?: string;
  model: string;
  op: "pull" | "delete";
  status: ModelOpStatus;
  detail: string;
  completed: number;
  total: number;
  // `error` is the legacy field name; v0.5 emits `last_error`. Read
  // `last_error || error` for display.
  error?: string;
  last_error?: string;
  created_at: string;
  updated_at: string;
  // v0.5 queue/retry/progress fields (all optional — absent on old rows):
  attempt?: number;         // attempts made so far (0 = first try)
  max_attempts?: number;    // configured retry ceiling
  next_attempt_at?: string; // queued for retry — dispatch after this time
  phase?: string;           // engine-reported phase ("downloading", "verifying")
  rate_bps?: number;        // measured transfer rate, bytes/sec, while running
  indeterminate?: boolean;  // total bytes unknown — never render 0/0
  priority?: number;
  finished_at?: string;     // terminal timestamp (duration = finished_at - created_at)
}

export interface AuditEntry {
  id: number;
  actor: string;
  action: string;
  target: string;
  meta: Record<string, string>;
  created_at: string;
}

// GET /api/v1/version — unauthenticated control-plane identity. Not present
// on pre-v0.5 servers; consumers must tolerate a 404.
export interface VersionInfo {
  server_version: string;
  protocol: number;
  min_agent_protocol: number;
  // Emitted only by standby instances ("standby") — the active instance's
  // version payload has no role key at all, so presence is the signal.
  role?: string;
}

// GET /api/v1/discovery/status — LAN discovery (mDNS) state. The endpoint
// may 404 entirely on servers built without the feature.
export interface DiscoveryStatus {
  enabled: boolean;
  advertising: boolean;
  cluster_name: string;
  ca_fingerprint?: string;
}

// settings["modelops"] — v0.5 model-operation queue tuning. The whole
// object is PUT back under /settings/modelops.
export interface ModelOpsSettings {
  global_concurrent?: number;  // max ops in flight cluster-wide (1-16)
  per_node_concurrent?: number; // max ops in flight per node (1-4)
  queue_max?: number;          // max queued ops before rejection (8-256)
  max_attempts?: number;       // retry attempts per op (0-8)
}

// GET /api/v1/nodes/{id}/events — connection lifecycle history, newest
// first. v0.6+; a 404 means the node is gone or the server predates the
// endpoint. `reason` is empty on "connected"; on "disconnected" it is e.g.
// "connection closed by agent" / "replaced by new connection" /
// "control plane shutting down" / a revoke reason.
export interface NodeEvent {
  id: number;
  node_id: string;
  event: "connected" | "disconnected" | string;
  reason: string;
  agent_version: string;
  created_at: string;
}

// ---------------------------------------------------------------------------
// v0.7+ release registry & agent update orchestration. All of these
// endpoints 404 on older servers — hooks use shouldRetryOnError:false
// and consumers gate UI on data presence.
// ---------------------------------------------------------------------------

// A signed agent release accepted into the registry. Registration verifies
// the detached signature against a registered release key server-side —
// unsigned releases can never enter the registry.
export interface Release {
  id: string;
  version: string;
  channel: string;
  base_url: string;
  manifest_sha256: string;
  signer_key_id: string;
  protocol_version: number;
  min_agent_protocol: number;
  platforms: string[];
  notes: string;
  status: "approved" | "revoked";
  created_by: string;
  created_at: string;
}

// A trusted release-signing public key (minisign or openssl PEM).
export interface ReleaseKey {
  id: string;
  key_id: string;
  kind: string; // minisign | openssl
  public_key: string;
  comment: string;
  created_at: string;
  revoked_at?: string;
}

export type RolloutState =
  | "pending"
  | "running"
  | "awaiting_approval" // canary finished; next batch needs admin OK
  | "succeeded"
  | "failed"
  | "cancelled";

export type RolloutStrategy =
  | "single"
  | "selected"
  | "sequential"
  | "canary"
  | "parallel";

// One rollout campaign. total/succeeded/failed/pending are the per-node
// agent_update counters, populated server-side.
export interface Rollout {
  id: string;
  release_id: string;
  release_version?: string;
  strategy: RolloutStrategy | string;
  state: RolloutState;
  canary_count: number;
  batch_size: number;
  error?: string;
  created_by: string;
  created_at: string;
  updated_at: string;
  // Durable canary approval (v0.8+): recorded atomically with the
  // awaiting_approval→running transition. Absent on older servers and on
  // rollouts that never waited for approval.
  approved_by?: string;
  approved_at?: string;
  total: number;
  succeeded: number;
  failed: number;
  pending: number;
}

// Agent update phases — one node moving to one release. Phases before the
// terminal five are "live" (finished_at unset).
export type AgentUpdatePhase =
  | "pending"
  | "draining"
  | "dispatched"
  | "verifying"
  | "downloading"
  | "staging"
  | "installing"
  | "restarting"
  | "verify_health"
  | "succeeded"
  | "failed"
  | "rolled_back"
  | "cancelled"
  | "interrupted";

export interface AgentUpdate {
  id: string;
  rollout_id?: string;
  release_id: string;
  release_version?: string;
  node_id: string;
  node_name?: string;
  phase: AgentUpdatePhase | string;
  attempt: number;
  error?: string;
  detail?: string;
  from_version?: string;
  to_version?: string;
  created_at: string;
  dispatched_at?: string;
  updated_at: string;
  finished_at?: string;
}

// One row of control-plane ownership history (ownership_events table),
// embedded in the /control-plane payload. `owner_id` is the ephemeral
// instance UUID — display it only truncated, if at all.
export interface ControlPlaneEvent {
  id: number;
  owner_id?: string;
  epoch: number;
  event: string; // acquired|released|lost|promoted|refused
  detail?: string;
  at: string;
}

// GET /control-plane — single-active ownership/HA status. 404 on older
// servers. A promoted standby reports role "active" (the role it holds
// now) with `promoted_from_standby` recording how it got there. `events`
// is the recent ownership-event history (newest first); Go nil slices
// serialize as null.
export interface ControlPlaneStatus {
  role: "active" | "standby" | "unknown" | string;
  standby_capable?: boolean;
  promoted_from_standby?: boolean;
  uptime_seconds?: number;
  // This instance's ephemeral UUID + fencing epoch — present once the
  // instance holds (or held) ownership.
  instance_id?: string;
  epoch?: number;
  // The live owner row, when the store has one.
  owner?: {
    instance_id: string;
    epoch: number;
    started_at: string;
    heartbeat_age_seconds: number;
  } | null;
  events?: ControlPlaneEvent[] | null;
}

// Node summary inside the diagnostics bundle — same fields the hub
// snapshot exposes, minus labels/capacity knobs.
export interface DiagnosticsNode {
  id: string;
  name: string;
  status: NodeStatus;
  online: boolean;
  in_flight: number;
  latency_ms?: number;
  stale_heartbeat: boolean;
  hardware?: HardwareInfo;
  engines?: EngineInfo[] | null;
  models?: Model[] | null;
  metrics?: Metrics | null;
  last_seen_at?: string;
}

// GET /api/v1/diagnostics — v0.6+ redacted operational snapshot for
// troubleshooting/bug reports. 404 on older servers — gate UI on
// presence. Go serializes nil slices as null: guard arrays with `?? []`.
export interface Diagnostics {
  generated_at: string;
  server_version: string;
  protocol: number;
  min_agent_protocol: number;
  uptime_seconds: number;
  cluster_name: string;
  discovery?: DiscoveryStatus;
  control_plane?: {
    owner_id: string;
    epoch: number;
    started_at: string;
    heartbeat_at: string;
  };
  database?: {
    reachable: boolean;
    latency_ms: number;
  };
  tls?: {
    ca_fingerprint?: string;
    ca_expires_at?: string;
    server_expires_at?: string;
    server_days_remaining?: number;
  };
  nodes?: DiagnosticsNode[] | null;
  modelops_config?: ModelOpsSettings;
  modelops_recent_status?: Record<string, number> | null;
  model_operations?: ModelOperation[] | null;
  recent_audit?: AuditEntry[] | null;
}
