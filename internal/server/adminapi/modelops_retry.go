package adminapi

// Model-operation retry: the operator resolution path for ops parked
// 'interrupted' (uncertain dispatch outcome — deletes are never
// auto-resumed, and a pull can park past its resume sweep). Kept in its
// own file per parallel-workstream ownership; the integrator registers
// it inside Register() next to the cancel route:
//
//	mux.Handle("POST /api/v1/model-operations/{id}/retry", mwAdmin(http.HandlerFunc(a.handleModelOpRetry)))

import (
	"errors"
	"net/http"

	"github.com/localmesh/localmesh/internal/server/auth"
	"github.com/localmesh/localmesh/internal/server/modelops"
	"github.com/localmesh/localmesh/internal/server/store"
)

// handleModelOpRetry re-queues a single 'interrupted' op with its
// identity and attempt bookkeeping intact (unlike a resubmit, which
// dedupe now rejects while the parked row covers its node+model pair).
// 404 unknown op; 409 when the op isn't interrupted, when the queue is
// absent, or when a conflicting live op appeared; 200 + updated op on
// success.
func (a *API) handleModelOpRetry(w http.ResponseWriter, r *http.Request) {
	op, err := a.st.GetModelOperation(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "operation not found")
		return
	}
	if a.modelOps == nil {
		writeErr(w, http.StatusConflict, "model operation queue is not enabled")
		return
	}
	if op.Status != "interrupted" {
		writeErr(w, http.StatusConflict,
			"operation is "+op.Status+" — only interrupted operations can be retried")
		return
	}
	if err := a.modelOps.Retry(r.Context(), op.ID); err != nil {
		var de *modelops.DuplicateError
		switch {
		case errors.As(err, &de):
			writeErr(w, http.StatusConflict, de.Error())
		case errors.Is(err, modelops.ErrNotActive) || errors.Is(err, store.ErrNotFound):
			writeErr(w, http.StatusConflict, "operation is no longer interrupted")
		default:
			a.internalErr(w, r, err)
		}
		return
	}
	principal, _ := auth.PrincipalFrom(r.Context())
	a.st.Audit(r.Context(), principal.Name, "model_op.retry", op.Model,
		map[string]string{"operation_id": op.ID})
	fresh, err := a.st.GetModelOpState(r.Context(), op.ID)
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, fresh)
}
