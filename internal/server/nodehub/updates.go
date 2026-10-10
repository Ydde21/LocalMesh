package nodehub

import (
	"context"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/server/store"
)

// UpgradeDispatcher adapts the hub's conn registry to the update
// orchestrator's Dispatcher interface (internal/server/updateplane) so
// the orchestrator stays free of hub internals — same shape as
// OpDispatcher for model ops. The compile-time assertion is added where
// the interface is defined.
type UpgradeDispatcher struct {
	hub *Hub
	st  *store.DB
}

// NewUpgradeDispatcher wraps a hub for use as an agent-update dispatcher.
func NewUpgradeDispatcher(h *Hub, st *store.DB) *UpgradeDispatcher {
	return &UpgradeDispatcher{hub: h, st: st}
}

// Online reports whether the node currently has a live connection.
func (d *UpgradeDispatcher) Online(nodeID string) bool { return d.hub.Online(nodeID) }

// SendUpgrade delivers an agent_upgrade frame. The op lands in the
// pending index so a mid-update disconnect can attribute the drop.
func (d *UpgradeDispatcher) SendUpgrade(ctx context.Context, nodeID string, u *contracts.AgentUpgrade) error {
	d.hub.mu.RLock()
	nc, ok := d.hub.conns[nodeID]
	d.hub.mu.RUnlock()
	if !ok {
		return ErrNodeOffline
	}
	return nc.send(ctx, contracts.Envelope{
		ID:      u.OperationID,
		Type:    contracts.MsgAgentUpgrade,
		Payload: contracts.MarshalPayload(u),
	})
}

// AbortUpgrade asks the agent to cancel a pre-install upgrade.
// ErrNodeOffline means the agent is already gone — the caller finalizes
// the row locally.
func (d *UpgradeDispatcher) AbortUpgrade(ctx context.Context, nodeID, opID string) error {
	d.hub.mu.RLock()
	nc, ok := d.hub.conns[nodeID]
	d.hub.mu.RUnlock()
	if !ok {
		return ErrNodeOffline
	}
	return nc.send(ctx, contracts.Envelope{
		ID:      opID,
		Type:    contracts.MsgUpgradeAbort,
		Payload: contracts.MarshalPayload(contracts.UpgradeAbort{OperationID: opID}),
	})
}

// AgentVersion returns the hello-reported agent build stamp for the
// connected node — "" when the node is offline or predates version
// reporting.
func (d *UpgradeDispatcher) AgentVersion(nodeID string) string {
	d.hub.mu.RLock()
	nc, ok := d.hub.conns[nodeID]
	d.hub.mu.RUnlock()
	if !ok {
		return ""
	}
	return nc.agentVer
}

// NodeDrained reports whether the node has zero in-flight inference
// dispatches AND zero dispatched/running model ops — the precondition
// for safe maintenance/update action. Delegates to Hub.Drained.
func (d *UpgradeDispatcher) NodeDrained(nodeID string) bool {
	return d.hub.Drained(nodeID)
}
