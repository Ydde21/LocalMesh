// Package nodehub tracks live node-agent WebSocket connections and
// routes inference/model operations to them. It is the only place that
// speaks the node wire protocol on the server side.
package nodehub

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/server/events"
	"github.com/localmesh/localmesh/internal/server/modelops"
	"github.com/localmesh/localmesh/internal/server/store"
	"github.com/localmesh/localmesh/internal/wsjson"
)

var (
	ErrNodeOffline      = errors.New("node is not connected")
	ErrNodeNotFound     = errors.New("node not found")
	ErrNodeNotApproved  = errors.New("node is not approved")
	ErrRequestCancelled = errors.New("request cancelled")
	ErrNodeUnavailable  = errors.New("node became unavailable")
	// ErrNodeBusy means the node's configured concurrency limit is full.
	// The gateway treats it as a scheduling miss and re-picks.
	ErrNodeBusy = errors.New("node has no free execution slots")
)

// Config controls hub timings.
type Config struct {
	HeartbeatInterval  time.Duration // suggested to agents in hello_ack
	PingInterval       time.Duration // ws-level keepalive
	HeartbeatTimeout   time.Duration // staleness threshold for scheduling
	MaxInFlightPerConn int           // sanity bound on dispatches
}

func DefaultConfig() Config {
	return Config{
		HeartbeatInterval:  10 * time.Second,
		PingInterval:       25 * time.Second,
		HeartbeatTimeout:   45 * time.Second,
		MaxInFlightPerConn: 64,
	}
}

// View is the scheduler-facing snapshot of one node.
type View struct {
	Node      *store.Node
	Online    bool    // currently connected
	InFlight  int     // dispatches currently executing
	LatencyMS float64 // measured ws ping RTT
	StaleBeat bool    // heartbeat overdue
	AgentVer  string  // hello-reported agent build stamp ("" if unset)
}

// Hub owns all live agent connections.
type Hub struct {
	st  *store.DB
	ev  *events.Broker
	cfg Config
	log *slog.Logger

	mu    sync.RWMutex
	conns map[string]*conn

	beatTimeout atomic.Int64 // staleness threshold, ns; live-tunable

	notifyCh chan struct{} // closed/replaced to wake the scheduler

	pendingOpsMu sync.Mutex
	pendingOps   map[string]string // operationID -> nodeID

	// opQueue receives lifecycle + op-frame callbacks when the modelops
	// queue is installed (server.go). Nil in tests that build a bare hub.
	opQueue *modelops.Queue

	// updates receives agent-upgrade state frames + connect lifecycle
	// callbacks when the update orchestrator is installed (server.go).
	updates updateReporter
}

// updateReporter is the hub→updateplane callback surface, defined here
// (not as updateplane import) to keep a one-way dep edge.
type updateReporter interface {
	OnUpgradeState(ctx context.Context, nodeID string, s contracts.UpgradeState)
	OnNodeConnected(nodeID, agentVersion string)
	OnNodeDisconnected(nodeID string)
}

// SetModelOpsQueue wires the persistent model-op queue into the hub's
// conn lifecycle and op-frame handling.
func (h *Hub) SetModelOpsQueue(q *modelops.Queue) { h.opQueue = q }

// SetUpdateReporter wires the update orchestrator into conn lifecycle +
// upgrade_state frames.
func (h *Hub) SetUpdateReporter(r updateReporter) { h.updates = r }

func New(st *store.DB, ev *events.Broker, cfg Config, log *slog.Logger) *Hub {
	h := &Hub{
		st:         st,
		ev:         ev,
		cfg:        cfg,
		log:        log.With("component", "nodehub"),
		conns:      map[string]*conn{},
		notifyCh:   make(chan struct{}),
		pendingOps: map[string]string{},
	}
	h.beatTimeout.Store(int64(cfg.HeartbeatTimeout))
	return h
}

// HeartbeatTimeout reports the current staleness threshold.
func (h *Hub) HeartbeatTimeout() time.Duration {
	return time.Duration(h.beatTimeout.Load())
}

// SetHeartbeatTimeout updates the staleness threshold used by Snapshot.
func (h *Hub) SetHeartbeatTimeout(d time.Duration) {
	h.beatTimeout.Store(int64(d))
}

// NotifyCh returns a channel closed whenever cluster state changes in a
// way that may make new schedulable capacity available.
func (h *Hub) NotifyCh() <-chan struct{} {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.notifyCh
}

func (h *Hub) notify() {
	h.mu.Lock()
	close(h.notifyCh)
	h.notifyCh = make(chan struct{})
	h.mu.Unlock()
}

// Online reports whether a node currently has a live connection.
func (h *Hub) Online(nodeID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.conns[nodeID]
	return ok
}

// Drained reports whether a node holds no active work: zero in-flight
// inference dispatches AND zero dispatched/running model operations.
// The op count comes from the store (a claimed op that hasn't reached
// pendingOps still occupies the node — the durable source). An offline
// node is trivially drained: nothing executes on a dead connection.
// This is the maintenance/update precondition everywhere it's needed —
// admin drain gate, server drain watcher, update orchestrator.
func (h *Hub) Drained(nodeID string) bool {
	h.mu.RLock()
	nc, ok := h.conns[nodeID]
	h.mu.RUnlock()
	if ok && nc.inFlight() > 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ops, err := h.st.ModelOpsActiveByNode(ctx)
	if err != nil {
		return false // unsure → not drained; callers retry
	}
	return ops[nodeID] == 0
}

// InFlight returns the number of in-flight dispatches on a node.
func (h *Hub) InFlight(nodeID string) int {
	h.mu.RLock()
	c, ok := h.conns[nodeID]
	h.mu.RUnlock()
	if !ok {
		return 0
	}
	return c.inFlight()
}

// Snapshot returns live views of every node (connected or not).
func (h *Hub) Snapshot(ctx context.Context) ([]View, error) {
	nodes, err := h.st.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]View, 0, len(nodes))
	for _, n := range nodes {
		v := View{Node: n}
		if c, ok := h.conns[n.ID]; ok {
			v.Online = true
			v.InFlight = c.inFlight()
			v.LatencyMS = c.latencyMS()
			v.StaleBeat = time.Since(c.lastBeat()) > time.Duration(h.beatTimeout.Load())
			v.AgentVer = c.agentVer
		}
		out = append(out, v)
	}
	return out, nil
}

// HandleConnection runs one agent connection to completion. The node
// credential was already validated by the HTTP handler; the first frame
// must be a hello.
func (h *Hub) HandleConnection(ctx context.Context, c *wsjson.Conn, node *store.Node) {
	// 4MiB: capability reports carry the full model inventory — a node
	// with hundreds of models can exceed the old 1MiB and get dropped.
	c.SetReadLimit(4 << 20)

	// Read hello — bounded: a peer that accepts the upgrade but never
	// sends hello would otherwise park this handler forever.
	hctx, hcancel := context.WithTimeout(ctx, 15*time.Second)
	env, err := c.Read(hctx)
	hcancel()
	if err != nil {
		h.log.Debug("node hello read failed", "node", node.Name, "err", err)
		return
	}
	if env.Type != contracts.MsgHello {
		h.log.Warn("first frame was not hello", "node", node.Name, "type", env.Type)
		c.Close(websocket.StatusProtocolError, "expected hello")
		return
	}
	var hello contracts.Hello
	if err := env.Decode(&hello); err != nil {
		c.Close(websocket.StatusProtocolError, "bad hello")
		return
	}
	if hello.ProtocolVersion > contracts.ProtocolVersion {
		h.sendError(ctx, c, "protocol_version", "agent protocol is newer than server; upgrade the server")
		c.Close(websocket.StatusUnsupportedData, "protocol version")
		return
	}
	if hello.ProtocolVersion < contracts.MinAgentProtocolVersion {
		h.sendError(ctx, c, "protocol_version", "agent protocol is too old; upgrade the agent")
		c.Close(websocket.StatusUnsupportedData, "protocol version too old")
		return
	}
	if hello.NodeID != "" && hello.NodeID != node.ID {
		h.log.Warn("hello node_id does not match credential", "claimed", hello.NodeID, "node", node.ID)
		c.Close(websocket.StatusPolicyViolation, "node identity mismatch")
		return
	}

	nc := newConn(node, c, h)
	nc.agentVer = hello.AgentVersion
	ack := contracts.HelloAck{
		NodeID:           node.ID,
		ProtocolVersion:  contracts.ProtocolVersion,
		HeartbeatSeconds: int(h.cfg.HeartbeatInterval / time.Second),
		PingSeconds:      int(h.cfg.PingInterval / time.Second),
		ServerVersion:    version(),
	}
	if err := nc.send(ctx, contracts.Envelope{
		ID:      newID(),
		Type:    contracts.MsgHelloAck,
		ReplyTo: env.ID,
		Payload: contracts.MarshalPayload(ack),
	}); err != nil {
		return
	}

	h.register(node.ID, nc)
	defer h.unregister(node.ID, nc)

	pingDone := make(chan struct{})
	go nc.pingLoop(ctx, h.cfg.PingInterval, pingDone)
	defer close(pingDone)

	h.log.Info("node connected", "node", node.Name, "id", node.ID)
	h.st.TouchNode(ctx, node.ID)

	// Read loop: route inbound frames.
	for {
		env, err := c.Read(ctx)
		if err != nil {
			return
		}
		h.handleFrame(ctx, nc, env)
	}
}

func (h *Hub) register(id string, nc *conn) {
	h.mu.Lock()
	if old, ok := h.conns[id]; ok {
		// Duplicate connection for the same node: kick the stale one —
		// immediately, no 5s close-handshake stall on a dead peer.
		go old.closeNow(websocket.StatusNormalClosure, "replaced by new connection")
	}
	h.conns[id] = nc
	h.mu.Unlock()
	if err := h.st.AddNodeEvent(context.Background(), id, "connected", "", nc.agentVer); err != nil {
		h.log.Debug("record connect event", "node", id, "err", err)
	}
	h.ev.Publish("node.connected", map[string]string{"id": id, "name": nc.node.Name})
	if h.opQueue != nil {
		h.opQueue.OnNodeConnected(id)
	}
	if h.updates != nil {
		h.updates.OnNodeConnected(id, nc.agentVer)
	}
	h.notify()
}

// Disconnect severs a node's live connection, if any — used by revoke
// and delete so a compromised or removed credential stops working
// immediately rather than at the next reconnect.
func (h *Hub) Disconnect(nodeID, reason string) {
	h.mu.RLock()
	c, ok := h.conns[nodeID]
	h.mu.RUnlock()
	if ok {
		c.closeNow(websocket.StatusPolicyViolation, reason)
	}
}

// UpdateNodeConfig refreshes a live conn's view of mutable node config
// (capacity limit, labels) so dispatch-time enforcement reflects admin
// changes without waiting for a reconnect.
func (h *Hub) UpdateNodeConfig(nodeID string, maxConcurrent int, labels map[string]string) {
	h.mu.RLock()
	c, ok := h.conns[nodeID]
	h.mu.RUnlock()
	if !ok {
		return
	}
	c.mu.Lock()
	if maxConcurrent > 0 {
		c.node.MaxConcurrent = maxConcurrent
	}
	if labels != nil {
		c.node.Labels = labels
	}
	c.mu.Unlock()
	h.notify()
}

// CloseAll severs every agent connection (graceful shutdown). Pending
// dispatches receive an error and unwind via failAllPending.
func (h *Hub) CloseAll(reason string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.conns {
		// CloseNow: no per-conn handshake stall — N dead peers can't
		// pile 5s waits inside this lock. The reason is recorded on the
		// conn for node_events; agents see EOF and reconnect.
		c.closeNow(websocket.StatusGoingAway, reason)
	}
}

func (h *Hub) unregister(id string, nc *conn) {
	h.mu.Lock()
	if h.conns[id] == nc {
		delete(h.conns, id)
	}
	h.mu.Unlock()
	nc.failAllPending("node disconnected")

	// Any model ops dispatched to this node can no longer report — mark
	// them interrupted so they don't linger 'running' forever, and drop
	// them from the index.
	h.pendingOpsMu.Lock()
	var lost []string
	for opID, nodeID := range h.pendingOps {
		if nodeID == id {
			lost = append(lost, opID)
			delete(h.pendingOps, opID)
		}
	}
	h.pendingOpsMu.Unlock()
	if len(lost) > 0 {
		if err := h.st.InterruptNodeModelOps(context.Background(), id,
			"node disconnected mid-operation"); err != nil {
			h.log.Warn("interrupt model ops on disconnect", "err", err)
		}
		for _, opID := range lost {
			h.ev.Publish("model.operation", map[string]any{
				"operation_id": opID, "node_id": id, "status": "interrupted",
			})
		}
	}

	reason, _ := nc.closeReason.Load().(string)
	if reason == "" {
		reason = "connection closed by agent"
	}
	if err := h.st.AddNodeEvent(context.Background(), id, "disconnected", reason, nc.agentVer); err != nil {
		h.log.Debug("record disconnect event", "node", id, "err", err)
	}
	h.log.Info("node disconnected", "node", nc.node.Name, "id", id, "reason", reason)
	h.ev.Publish("node.disconnected", map[string]string{"id": id, "name": nc.node.Name})
	if h.opQueue != nil {
		// Queue owns interruption + retry-resume bookkeeping; its
		// InterruptNodeModelOps is status-guarded and idempotent vs the
		// direct call above.
		h.opQueue.OnNodeDisconnected(id)
	}
	if h.updates != nil {
		h.updates.OnNodeDisconnected(id)
	}
	h.notify()
}

func (h *Hub) handleFrame(ctx context.Context, nc *conn, env contracts.Envelope) {
	switch env.Type {
	case contracts.MsgCapability:
		var rep contracts.CapabilityReport
		if err := env.Decode(&rep); err != nil {
			return
		}
		nc.node.Hardware = rep.Hardware
		nc.node.Engines = rep.Engines
		nc.node.Models = rep.Models
		if err := h.st.UpdateNodeReport(ctx, nc.node.ID, rep.Hardware, rep.Engines, rep.Models); err != nil {
			h.log.Warn("persist capability report", "err", err)
		}
		h.ev.Publish("node.capability", map[string]string{"id": nc.node.ID})
		h.notify() // newly available models may unblock queued work

	case contracts.MsgHeartbeat:
		var hb contracts.Heartbeat
		if err := env.Decode(&hb); err != nil {
			return
		}
		nc.beat()
		m := hb.Metrics
		if nc.node.Metrics != nil && m.ActiveRequests < 0 {
			m.ActiveRequests = nc.node.Metrics.ActiveRequests
		}
		nc.node.Metrics = &m
		if err := h.st.UpdateNodeMetrics(ctx, nc.node.ID, m); err != nil {
			h.log.Debug("persist metrics", "err", err)
		}
		if m.DiskMeasured {
			if err := h.st.UpdateNodeDisk(ctx, nc.node.ID,
				int64(m.DiskFreeBytes), int64(m.DiskTotalBytes)); err != nil {
				h.log.Debug("persist disk metrics", "err", err)
			}
		}
		// Keep models[].loaded fresh between capability reports — the
		// heartbeat's loaded_models is the engine's current truth.
		if m.LoadedMeasured {
			loaded := make(map[string]bool, len(m.LoadedModels))
			for _, name := range m.LoadedModels {
				loaded[name] = true
			}
			changed := false
			for i := range nc.node.Models {
				want := loaded[nc.node.Models[i].Name]
				if nc.node.Models[i].Loaded != want {
					nc.node.Models[i].Loaded = want
					changed = true
				}
			}
			if changed {
				if err := h.st.UpdateNodeReport(ctx, nc.node.ID, nc.node.Hardware, nc.node.Engines, nc.node.Models); err != nil {
					h.log.Debug("persist loaded-state refresh", "err", err)
				}
				h.notify() // warm-model preference may now apply
			}
		}
		h.ev.Publish("node.metrics", map[string]any{"id": nc.node.ID, "metrics": m})

	case contracts.MsgInferChunk:
		var chunk contracts.ChatChunk
		if err := env.Decode(&chunk); err != nil {
			return
		}
		nc.deliverChunk(env.ReplyTo, chunk)

	case contracts.MsgInferEnd:
		nc.completePending(env.ReplyTo, nil)

	case contracts.MsgInferError:
		var e contracts.ErrorPayload
		env.Decode(&e)
		nc.completePending(env.ReplyTo, errors.New(e.Message))

	case contracts.MsgPong:
		var p contracts.Pong
		if env.Decode(&p) == nil {
			nc.setLatency(float64(time.Since(p.SentAt).Microseconds()) / 1000.0)
		}

	case contracts.MsgOpProgress:
		var p contracts.OpProgress
		if env.Decode(&p) != nil {
			return
		}
		if h.opQueue != nil {
			h.opQueue.OnProgress(ctx, p.OperationID, modelops.Progress{
				Phase:         p.Phase,
				Completed:     int64(p.Completed),
				Total:         int64(p.Total),
				RateBPS:       int64(p.RateBPS),
				Indeterminate: p.Indeterminate,
			})
		} else {
			h.st.UpdateModelOperation(ctx, p.OperationID, "running", p.Status, int64(p.Completed), int64(p.Total))
		}
		h.ev.Publish("model.operation", map[string]any{
			"operation_id": p.OperationID, "node_id": nc.node.ID,
			"model": p.Model, "status": "running", "detail": p.Status,
			"completed": p.Completed, "total": p.Total,
			"phase": p.Phase, "rate_bps": p.RateBPS, "indeterminate": p.Indeterminate,
		})

	case contracts.MsgOpDone:
		var d contracts.OpDone
		if env.Decode(&d) != nil {
			return
		}
		h.pendingOpsMu.Lock()
		delete(h.pendingOps, d.OperationID)
		h.pendingOpsMu.Unlock()
		status := statusOf(d.Success)
		if d.Cancelled {
			status = "cancelled"
		}
		if h.opQueue != nil {
			h.opQueue.OnDone(ctx, d.OperationID, modelops.Done{
				Success:       d.Success,
				Cancelled:     d.Cancelled,
				Retryable:     d.Retryable,
				Error:         d.Error,
				Completed:     int64(d.Completed),
				Total:         int64(d.Total),
				Phase:         d.Phase,
				RateBPS:       int64(d.RateBPS),
				Indeterminate: d.Indeterminate,
			})
		} else {
			h.st.FinishModelOperationStatus(ctx, d.OperationID, status, d.Error)
		}
		h.ev.Publish("model.operation", map[string]any{
			"operation_id": d.OperationID, "node_id": nc.node.ID,
			"model": d.Model, "status": status, "error": d.Error,
		})
		// Model inventory likely changed; ask the agent to re-report.
		h.notify()

	case contracts.MsgUpgradeState:
		// Agent update progress/terminal report — forwarded to the
		// update orchestrator. Unknown ops are ignored there.
		var u contracts.UpgradeState
		if env.Decode(&u) != nil {
			return
		}
		if h.updates != nil {
			h.updates.OnUpgradeState(ctx, nc.node.ID, u)
		}
	}
}

func statusOf(ok bool) string {
	if ok {
		return "succeeded"
	}
	return "failed"
}

// Dispatch sends an inference request to a node and returns a channel of
// normalized chunks. The channel is closed when the stream ends; a
// terminal chunk may carry Error. ctx cancellation aborts the request on
// the agent side.
func (h *Hub) Dispatch(ctx context.Context, nodeID string, req contracts.InferenceRequest) (<-chan contracts.ChatChunk, error) {
	h.mu.RLock()
	nc, ok := h.conns[nodeID]
	h.mu.RUnlock()
	if !ok {
		return nil, ErrNodeOffline
	}
	if err := nc.startPending(req.RequestID); err != nil {
		return nil, err
	}
	ch := nc.pendingChan(req.RequestID)

	if err := nc.send(ctx, contracts.Envelope{
		ID:      req.RequestID,
		Type:    contracts.MsgInferReq,
		Payload: contracts.MarshalPayload(req),
	}); err != nil {
		nc.completePending(req.RequestID, err)
		return nil, err
	}

	go func() {
		select {
		case <-ctx.Done():
			h.CancelInference(nodeID, req.RequestID)
			nc.completePending(req.RequestID, ErrRequestCancelled)
		case <-nc.pendingDone(req.RequestID):
		}
	}()
	return ch, nil
}

// CancelInference asks the agent to abort a request (best effort).
func (h *Hub) CancelInference(nodeID, requestID string) {
	h.mu.RLock()
	nc, ok := h.conns[nodeID]
	h.mu.RUnlock()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	nc.send(ctx, contracts.Envelope{
		ID:      newID(),
		Type:    contracts.MsgInferCancel,
		Payload: contracts.MarshalPayload(contracts.InferCancel{RequestID: requestID}),
	})
}

// RequestModelOp sends a pull/delete command and records it in pendingOps.
func (h *Hub) RequestModelOp(ctx context.Context, nodeID, opID, model, op string) error {
	h.mu.RLock()
	nc, ok := h.conns[nodeID]
	h.mu.RUnlock()
	if !ok {
		return ErrNodeOffline
	}
	typ := contracts.MsgModelPull
	payload := contracts.MarshalPayload(contracts.ModelPull{OperationID: opID, Model: model})
	if op == "delete" {
		typ = contracts.MsgModelDelete
		payload = contracts.MarshalPayload(contracts.ModelDelete{OperationID: opID, Model: model})
	}
	h.pendingOpsMu.Lock()
	h.pendingOps[opID] = nodeID
	h.pendingOpsMu.Unlock()
	return nc.send(ctx, contracts.Envelope{ID: opID, Type: typ, Payload: payload})
}

// AbortModelOp sends an abort command for an in-flight operation.
// Returns ErrNodeOffline when the owning node has no live conn — the op
// is already dead in that case and the caller may finalize it directly.
func (h *Hub) AbortModelOp(ctx context.Context, opID string) error {
	h.pendingOpsMu.Lock()
	nodeID, ok := h.pendingOps[opID]
	h.pendingOpsMu.Unlock()
	if !ok {
		return store.ErrNotFound
	}
	h.mu.RLock()
	nc, ok := h.conns[nodeID]
	h.mu.RUnlock()
	if !ok {
		return ErrNodeOffline
	}
	return nc.send(ctx, contracts.Envelope{
		ID:      opID,
		Type:    contracts.MsgModelOpAbort,
		Payload: contracts.MarshalPayload(contracts.ModelOpAbort{OperationID: opID}),
	})
}

// PingAll nudges every connection (used by a background keepalive if needed).
func (h *Hub) sendError(ctx context.Context, c *wsjson.Conn, code, msg string) {
	c.Write(ctx, contracts.Envelope{
		ID:      newID(),
		Type:    contracts.MsgError,
		Payload: contracts.MarshalPayload(contracts.ErrorPayload{Code: code, Message: msg}),
	})
}
