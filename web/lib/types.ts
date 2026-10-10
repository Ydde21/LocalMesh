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
  engines?: EngineInfo[];
  models?: Model[];
  metrics?: Metrics | null;
  labels: Record<string, string>;
  max_concurrent: number;
  in_flight: number;
  latency_ms?: number;
  last_seen_at?: string;
  created_at: string;
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
export type ModelOpStatus =
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
  error?: string;
  created_at: string;
  updated_at: string;
}

export interface AuditEntry {
  id: number;
  actor: string;
  action: string;
  target: string;
  meta: Record<string, string>;
  created_at: string;
}
