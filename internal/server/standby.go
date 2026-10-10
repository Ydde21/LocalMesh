package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/server/nodehub"
	"github.com/localmesh/localmesh/internal/server/store"
)

// roleMux is the standby-mode root handler: one stable handler object
// whose inner handler swaps atomically at promotion. The listener never
// rebinds and in-flight standby responses finish — clients simply see
// 503-until-active on the same address the cluster always serves. The
// box keeps atomic.Value's concrete-type consistency across swaps.
type roleMux struct{ inner atomic.Value } // stores *handlerBox

type handlerBox struct{ h http.Handler }

func (m *roleMux) swap(h http.Handler) { m.inner.Store(&handlerBox{h: h}) }

func (m *roleMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if box, ok := m.inner.Load().(*handlerBox); ok && box != nil {
		box.h.ServeHTTP(w, r)
		return
	}
	writeStandbyUnavailable(w)
}

func writeStandbyUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"type":    "standby",
			"message": "this control plane is in standby — the active instance serves requests",
		},
	})
}

// standbyHandler is the whole standby surface. Deliberately tiny:
// healthz/readyz report NOT healthy (a router probing them excludes the
// standby from the stable endpoint automatically), /api/v1/standby
// gives operators the incumbent owner + wait duration, and
// /api/v1/version keeps agent compatibility checks working. Everything
// else — especially anything that mutates or dispatches — is 503.
func standbyHandler(st *store.DB, waitingSince time.Time) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "standby", http.StatusServiceUnavailable)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "standby", http.StatusServiceUnavailable)
	})
	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"server_version": nodehub.Version(), "protocol": contracts.ProtocolVersion,
			"min_agent_protocol": contracts.MinAgentProtocolVersion, "role": "standby",
		})
	})
	mux.HandleFunc("GET /api/v1/standby", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]any{
			"role":            "standby",
			"waiting_since":   waitingSince.Format(time.RFC3339),
			"waiting_seconds": int64(time.Since(waitingSince).Seconds()),
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if owner, err := st.ReadControlPlane(ctx); err == nil && owner != nil {
			out["owner"] = map[string]any{
				"instance_id":           owner.OwnerID,
				"epoch":                 owner.Epoch,
				"started_at":            owner.StartedAt,
				"heartbeat_age_seconds": int64(time.Since(owner.HeartbeatAt).Seconds()),
			}
		} else {
			out["owner"] = nil
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeStandbyUnavailable(w)
	})
	return mux
}
