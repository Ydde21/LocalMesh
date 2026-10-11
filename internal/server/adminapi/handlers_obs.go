package adminapi

// Observability endpoints added in v0.10 — split out of handlers.go
// for the parallel-workstream ownership model.
//
// INTEGRATOR: inside Register (handlers.go), mount these under mwRead —
// next to the existing control-plane/nodes lines:
//
//	mux.Handle("GET /api/v1/control-plane/peers", mwRead(http.HandlerFunc(a.handleControlPlanePeers)))
//	mux.Handle("GET /api/v1/nodes/{id}/disk",     mwRead(http.HandlerFunc(a.handleNodeDisk)))
//
// Read-scoped deliberately: both expose operational state the dashboard
// and operator tooling legitimately read; nothing here mutates.

import (
	"net/http"
	"time"

	"github.com/localmesh/localmesh/internal/server/store"
)

// handleControlPlanePeers lists control_plane_peers rows — the live
// view behind the /control-plane "standby" presence block. Each row is
// annotated with computed freshness (heartbeat age + fresh flag) so
// consumers don't have to reproduce the staleness window. Rows are
// newest-heartbeat first; stale rows (crashed peers that never deleted
// themselves) are included — presence semantics live in the `fresh`
// field, matching store.PeerStaleAfter.
func (a *API) handleControlPlanePeers(w http.ResponseWriter, r *http.Request) {
	peers, err := a.st.ListPeers(r.Context(), "")
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(peers))
	for _, p := range peers {
		out = append(out, map[string]any{
			"instance_id":           p.InstanceID,
			"role":                  p.Role,
			"hostname":              p.Hostname,
			"started_at":            p.StartedAt,
			"heartbeat_at":          p.HeartbeatAt,
			"heartbeat_age_seconds": int64(time.Since(p.HeartbeatAt).Seconds()),
			"fresh":                 p.Fresh(store.PeerStaleAfter),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleNodeDisk returns one node's disk-telemetry triple — the fields
// the node rows gain (disk_free_bytes / disk_total_bytes /
// disk_measured_at) without waiting on the shared nodeJSON diff. All
// three are null until the agent reports a real measurement;
// disk_measured_at is stamped server-side on receipt (see
// store.UpdateNodeDisk) so it also marks "how stale is this reading".
func (a *API) handleNodeDisk(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := a.st.GetNode(r.Context(), id); err != nil {
		writeErr(w, http.StatusNotFound, "node not found")
		return
	}
	disk, err := a.st.GetNodeDisk(r.Context(), id)
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"node_id":          id,
		"disk_free_bytes":  disk.FreeBytes,
		"disk_total_bytes": disk.TotalBytes,
		"disk_measured_at": disk.MeasuredAt,
	})
}
