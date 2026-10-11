// Package contracts defines the shared domain types and the node<->server
// wire protocol. Both the control plane and the node agent import this
// package; nothing in here may depend on server- or agent-only packages.
package contracts

import (
	"encoding/json"
	"time"
)

// ProtocolVersion is bumped whenever the wire protocol changes in a
// backwards-incompatible way. Server and agent exchange versions during
// the hello handshake; the server rejects agents with a newer major
// version than its own.
const ProtocolVersion = 1

// MinAgentProtocolVersion is the oldest agent protocol this server can
// still serve — agents older than this are rejected at hello with an
// upgrade instruction rather than silently half-working. Kept equal to
// ProtocolVersion while v1 is the only version; lower it only when the
// server genuinely carries compatibility code for an older dialect.
const MinAgentProtocolVersion = 1

// ---------------------------------------------------------------------------
// Node identity & hardware
// ---------------------------------------------------------------------------

type NodeStatus string

const (
	NodeStatusPending  NodeStatus = "pending"  // enrolled, awaiting admin approval
	NodeStatusApproved NodeStatus = "approved" // may connect and serve workloads
	NodeStatusRevoked  NodeStatus = "revoked"  // administratively disabled
)

// GPU describes a single detected accelerator.
type GPU struct {
	Name       string `json:"name"`
	Vendor     string `json:"vendor,omitempty"`      // nvidia, amd, apple, intel
	VRAMBytes  uint64 `json:"vram_bytes,omitempty"`  // dedicated VRAM; 0 when shared/unknown
	SharedVRAM bool   `json:"shared_vram,omitempty"` // unified memory (Apple Silicon, iGPU)
	Driver     string `json:"driver,omitempty"`
}

// HardwareInfo is a point-in-time snapshot of a node's static hardware.
type HardwareInfo struct {
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`   // darwin, linux, windows
	Arch         string `json:"arch"` // arm64, amd64
	CPUModel     string `json:"cpu_model"`
	CPUCores     int    `json:"cpu_cores"`
	MemoryBytes  uint64 `json:"memory_bytes"`
	GPUs         []GPU  `json:"gpus,omitempty"`
	AgentVersion string `json:"agent_version"`
}

// Metrics is a live utilization snapshot reported with each heartbeat.
type Metrics struct {
	CPUUtilPercent   float64 `json:"cpu_util_percent"` // 0-100 across all cores
	MemoryUsedBytes  uint64  `json:"memory_used_bytes"`
	MemoryTotalBytes uint64  `json:"memory_total_bytes"`
	// MemoryAvailableBytes estimates memory available without swapping
	// (includes reclaimable caches). Falls back to total-used when 0.
	MemoryAvailableBytes uint64   `json:"memory_available_bytes,omitempty"`
	GPUUtilPercent       float64  `json:"gpu_util_percent,omitempty"` // 0-100, best-effort
	GPUVRAMUsedBytes     uint64   `json:"gpu_vram_used_bytes,omitempty"`
	ActiveRequests       int      `json:"active_requests"` // in-flight inference on this node
	LoadedModels         []string `json:"loaded_models,omitempty"`
	UptimeSeconds        int64    `json:"uptime_seconds"`
	// Disk bytes on the filesystem holding the agent state dir (where
	// update staging lands). DiskMeasured=false → fields are unknown,
	// never 0.
	DiskFreeBytes  uint64 `json:"disk_free_bytes,omitempty"`
	DiskTotalBytes uint64 `json:"disk_total_bytes,omitempty"`
	DiskMeasured   bool   `json:"disk_measured,omitempty"`
	// Measured flags: when false, the corresponding field is UNKNOWN —
	// the platform could not produce a real measurement this cycle.
	// Consumers must not interpret an unmeasured value as 0/idle/free.
	CPUUtilMeasured  bool `json:"cpu_util_measured,omitempty"`
	MemAvailMeasured bool `json:"mem_avail_measured,omitempty"`
	GPUUtilMeasured  bool `json:"gpu_util_measured,omitempty"`
	VRAMUsedMeasured bool `json:"vram_used_measured,omitempty"`
	LoadedMeasured   bool `json:"loaded_measured,omitempty"` // LoadedModels is real engine state
}

// EngineInfo describes one inference engine detected on the node.
type EngineInfo struct {
	Type    string `json:"type"`    // "ollama", future: "vllm", "llamacpp", "mlx"
	Version string `json:"version"` // engine-reported version
	Healthy bool   `json:"healthy"`
	Address string `json:"address,omitempty"` // local endpoint, e.g. http://127.0.0.1:11434
}

// Model describes one model installed on a node.
type Model struct {
	Name         string `json:"name"`                     // e.g. "llama3.1:8b"
	Engine       string `json:"engine"`                   // owning engine type
	Family       string `json:"family,omitempty"`         // e.g. "llama"
	ParameterSz  string `json:"parameter_size,omitempty"` // e.g. "8.0B"
	Quantization string `json:"quantization,omitempty"`
	SizeBytes    uint64 `json:"size_bytes"`
	Digest       string `json:"digest,omitempty"`
	// Capabilities come from engine metadata (e.g. Ollama /api/show):
	// "completion", "tools", "vision", "embedding", "insert".
	Capabilities []string   `json:"capabilities,omitempty"`
	ModifiedAt   *time.Time `json:"modified_at,omitempty"`
	Loaded       bool       `json:"loaded"` // currently resident in memory
	// LoadedBytes / LoadedVRAM are the REAL resident footprint reported
	// by the engine while the model is loaded (Ollama /api/ps `size` /
	// `size_vram`) — larger than the file size (context, KV cache,
	// runtime). Zero when not loaded or not measurable.
	LoadedBytes uint64 `json:"loaded_bytes,omitempty"`
	LoadedVRAM  uint64 `json:"loaded_vram_bytes,omitempty"`
	// Runner is the engine runner serving the model (e.g. "llamacpp").
	Runner string `json:"runner,omitempty"`
}

// NodeState is the server-side view of a node: static info plus live metrics.
type NodeState struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Status        NodeStatus        `json:"status"`
	Online        bool              `json:"online"`
	Hardware      HardwareInfo      `json:"hardware"`
	Engines       []EngineInfo      `json:"engines,omitempty"`
	Models        []Model           `json:"models,omitempty"`
	Metrics       *Metrics          `json:"metrics,omitempty"`
	MaxConcurrent int               `json:"max_concurrent"`
	Labels        map[string]string `json:"labels,omitempty"`
	LastSeenAt    time.Time         `json:"last_seen_at"`
	CreatedAt     time.Time         `json:"created_at"`
	// LatencyMS is the measured round-trip for a lightweight ping, used by
	// the scheduler as a soft signal.
	LatencyMS float64 `json:"latency_ms,omitempty"`
}

// ---------------------------------------------------------------------------
// Inference request/response (engine-agnostic, OpenAI-shaped)
// ---------------------------------------------------------------------------

// Message is a single chat message in OpenAI format.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// InferenceRequest is dispatched from server to a chosen node.
type InferenceRequest struct {
	RequestID   string    `json:"request_id"`
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream"`
	Temperature *float64  `json:"temperature,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
	Stop        []string  `json:"stop,omitempty"`
}

// Usage mirrors OpenAI usage stats.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatChunk is one streamed piece of a chat completion. The agent
// normalizes engine output into this shape so the gateway stays
// engine-agnostic.
type ChatChunk struct {
	Delta  string `json:"delta,omitempty"`
	Done   bool   `json:"done,omitempty"`
	Finish string `json:"finish_reason,omitempty"` // "stop", "length", ...
	Usage  *Usage `json:"usage,omitempty"`
	Error  string `json:"error,omitempty"`
	Model  string `json:"model,omitempty"`
}

// ---------------------------------------------------------------------------
// Scheduling
// ---------------------------------------------------------------------------

// CandidateReport explains the scheduler's view of one node for one request.
type CandidateReport struct {
	NodeID         string   `json:"node_id"`
	NodeName       string   `json:"node_name"`
	Eligible       bool     `json:"eligible"`
	Score          float64  `json:"score"`
	Selected       bool     `json:"selected"`
	Reasons        []string `json:"reasons,omitempty"`         // why it scored well/poorly
	FilteredReason string   `json:"filtered_reason,omitempty"` // why it was excluded
}

// Decision is the recorded outcome of one scheduling pass.
type Decision struct {
	Model      string            `json:"model"`
	Chosen     string            `json:"chosen_node_id"`
	Reason     string            `json:"reason"` // human-readable summary
	Candidates []CandidateReport `json:"candidates"`
	QueuedMS   int64             `json:"queued_ms,omitempty"`
	Attempt    int               `json:"attempt"`
}

// ---------------------------------------------------------------------------
// Wire protocol (WebSocket, JSON envelopes)
// ---------------------------------------------------------------------------

// Envelope wraps every message on the node<->server socket. Replies carry
// the request's ID in ReplyTo so either side can correlate streams.
type Envelope struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	ReplyTo string          `json:"reply_to,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Message types, agent -> server.
const (
	MsgHello        = "hello"             // first message after connect
	MsgCapability   = "capability"        // hardware/engines/models report
	MsgHeartbeat    = "heartbeat"         // periodic metrics
	MsgInferChunk   = "infer_chunk"       // one streamed token batch (reply)
	MsgInferEnd     = "infer_end"         // stream finished (reply)
	MsgInferError   = "infer_error"       // stream failed (reply)
	MsgOpProgress   = "model_op_progress" // pull/delete progress
	MsgOpDone       = "model_op_done"     // pull/delete finished
	MsgUpgradeState = "upgrade_state"     // agent upgrade progress/result
	MsgPong         = "pong"
)

// Message types, server -> agent.
const (
	MsgHelloAck     = "hello_ack"
	MsgInferReq     = "infer_req"
	MsgInferCancel  = "infer_cancel"
	MsgModelPull    = "model_pull"
	MsgModelDelete  = "model_delete"
	MsgModelOpAbort = "model_op_abort"
	MsgAgentUpgrade = "agent_upgrade" // install an approved signed release
	MsgUpgradeAbort = "upgrade_abort" // cancel a not-yet-installed upgrade
	MsgPing         = "ping"
	MsgError        = "error"
)

// Hello is the agent's first frame after the socket is upgraded.
// Authentication happens via the Authorization header; Hello carries
// identity confirmation and resumption info.
type Hello struct {
	NodeID          string `json:"node_id"`
	ProtocolVersion int    `json:"protocol_version"`
	AgentVersion    string `json:"agent_version"`
}

// HelloAck confirms registration on the hub.
type HelloAck struct {
	NodeID           string `json:"node_id"`
	ProtocolVersion  int    `json:"protocol_version"`
	HeartbeatSeconds int    `json:"heartbeat_seconds"`
	PingSeconds      int    `json:"ping_seconds"`
	ServerVersion    string `json:"server_version"`
}

// CapabilityReport is sent on connect and whenever local state changes.
type CapabilityReport struct {
	Hardware HardwareInfo `json:"hardware"`
	Engines  []EngineInfo `json:"engines"`
	Models   []Model      `json:"models"`
}

// Heartbeat carries live utilization.
type Heartbeat struct {
	Metrics Metrics `json:"metrics"`
}

// InferCancel asks the agent to abort an in-flight request.
type InferCancel struct {
	RequestID string `json:"request_id"`
}

// ModelPull asks the agent to download a model.
type ModelPull struct {
	OperationID string `json:"operation_id"`
	Model       string `json:"model"`
}

// ModelDelete asks the agent to remove a model.
type ModelDelete struct {
	OperationID string `json:"operation_id"`
	Model       string `json:"model"`
}

// OpProgress reports pull/delete progress.
type OpProgress struct {
	OperationID string `json:"operation_id"`
	Model       string `json:"model"`
	Status      string `json:"status"` // engine-provided status line
	Completed   uint64 `json:"completed"`
	Total       uint64 `json:"total"`
	// Phase is the engine's phase string verbatim (e.g. Ollama's
	// "pulling manifest"/"downloading"/"verifying sha256 digest").
	// RateBPS is the measured byte rate since the previous progress
	// frame — 0 when not yet measurable, never an estimate.
	// Indeterminate marks progress whose Total is unknown: Completed
	// and Total carry no meaningful fraction in that case.
	Phase         string `json:"phase,omitempty"`
	RateBPS       uint64 `json:"rate_bps,omitempty"`
	Indeterminate bool   `json:"indeterminate,omitempty"`
}

// OpDone completes an operation. On success it carries the final
// counters: Completed==Total when the total was measured, otherwise
// Indeterminate=true and Phase="complete" — a finished op never
// reports 0/0 as if it were a measurement.
type OpDone struct {
	OperationID string `json:"operation_id"`
	Model       string `json:"model"`
	Success     bool   `json:"success"`
	Cancelled   bool   `json:"cancelled,omitempty"` // aborted by admin request
	Error       string `json:"error,omitempty"`
	// Retryable marks the failure class: transient engine/transport
	// errors (dial, 5xx, mid-stream EOF, deadline) may be retried by
	// the server; permanent errors (model not found, invalid ref) end
	// the operation. Meaningful only when Success is false.
	Retryable     bool   `json:"retryable,omitempty"`
	Completed     uint64 `json:"completed,omitempty"`
	Total         uint64 `json:"total,omitempty"`
	Phase         string `json:"phase,omitempty"`
	RateBPS       uint64 `json:"rate_bps,omitempty"`
	Indeterminate bool   `json:"indeterminate,omitempty"`
}

// ModelOpAbort asks the agent to cancel a running pull/delete.
type ModelOpAbort struct {
	OperationID string `json:"operation_id"`
}

// Ping / Pong measure node<->server latency.
type Ping struct {
	SentAt time.Time `json:"sent_at"`
}
type Pong struct {
	SentAt time.Time `json:"sent_at"`
}

// AgentUpgrade dispatches one admin-approved release to one agent.
//
// TRUST MODEL: the frame carries *where the release is* and *which key
// must have signed it* — never the key itself. The agent verifies the
// detached signature against the release public key provisioned in its
// own config (join flag / update-key command / enrollment field). A
// compromised control plane can point at a malicious artifact or lie
// about key_id, but cannot supply the verification key — a
// signature that doesn't verify against the agent's pinned key is
// refused.
//
// Source is an https:// base URL the agent reads release-manifest.json,
// SHA256SUMS, signatures and the artifact from; the scheme
// "localmesh://releases/<release-id>" routes the same fetches through
// the control plane's authenticated release proxy (for agents without
// outbound access to the release host).
type AgentUpgrade struct {
	OperationID string `json:"operation_id"`
	ReleaseID   string `json:"release_id"`
	Version     string `json:"version"`
	Source      string `json:"source"`
	KeyID       string `json:"key_id"` // expected signer identity
	// Protocol bounds from the manifest — the agent double-checks these
	// itself after verifying the signature; these fields let it reject
	// an incompatible dispatch before downloading anything.
	ProtocolVersion   int  `json:"protocol_version"`
	MinAgentProtocol  int  `json:"min_agent_protocol"`
	AllowInsecureHTTP bool `json:"allow_insecure_http,omitempty"` // loopback mirror only
}

// UpgradeState is the agent's report on an upgrade operation. Terminal
// phases: succeeded, failed, rolled_back, aborted. Progress phases:
// verifying, downloading, staging, installing, restarting,
// verify_health.
type UpgradeState struct {
	OperationID string `json:"operation_id"`
	Phase       string `json:"phase"`
	Detail      string `json:"detail,omitempty"`
	Error       string `json:"error,omitempty"`
	FromVersion string `json:"from_version,omitempty"`
	ToVersion   string `json:"to_version,omitempty"`
}

// UpgradeAbort cancels a dispatched upgrade. Only honored before the
// install phase — a binary already swapped cannot be un-swapped by a
// message (that's what rollback is for).
type UpgradeAbort struct {
	OperationID string `json:"operation_id"`
}

// ErrorPayload is a generic wire error.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// MarshalPayload is a helper for building envelopes.
func MarshalPayload(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// Decode unmarshals the payload into v.
func (e Envelope) Decode(v any) error {
	return json.Unmarshal(e.Payload, v)
}
