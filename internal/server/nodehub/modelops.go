package nodehub

import (
	"context"

	"github.com/localmesh/localmesh/internal/server/modelops"
	"github.com/localmesh/localmesh/internal/server/store"
)

// OpDispatcher adapts the hub's conn registry to the modelops.Dispatcher
// interface so the scheduling queue stays free of hub internals.
type OpDispatcher struct{ hub *Hub }

var _ modelops.Dispatcher = (*OpDispatcher)(nil)

// NewOpDispatcher wraps a hub for use as a modelops queue dispatcher.
func NewOpDispatcher(h *Hub) *OpDispatcher { return &OpDispatcher{hub: h} }

// Online reports whether the node currently has a live connection.
func (d *OpDispatcher) Online(nodeID string) bool { return d.hub.Online(nodeID) }

// Send delivers the op's pull/delete frame and registers it in the hub's
// pending-ops index (which also drives the legacy interrupt path).
func (d *OpDispatcher) Send(ctx context.Context, nodeID string, op *store.ModelOperation) error {
	return d.hub.RequestModelOp(ctx, nodeID, op.ID, op.Model, op.Op)
}

// Abort asks the agent to cancel an in-flight op. ErrNodeOffline /
// store.ErrNotFound mean the op is already dead — the caller may finalize
// it locally.
func (d *OpDispatcher) Abort(ctx context.Context, nodeID, opID string) error {
	return d.hub.AbortModelOp(ctx, opID)
}
