// Package adminapi implements the management REST API consumed by the
// dashboard and CLI tooling. All endpoints under /api/v1 require an API
// key; mutations require the admin role.
package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/creds"
	"github.com/localmesh/localmesh/internal/server/auth"
	"github.com/localmesh/localmesh/internal/server/events"
	"github.com/localmesh/localmesh/internal/server/modelops"
	"github.com/localmesh/localmesh/internal/server/nodehub"
	"github.com/localmesh/localmesh/internal/server/scheduler"
	"github.com/localmesh/localmesh/internal/server/store"
)

type API struct {
	st    *store.DB
	hub   *nodehub.Hub
	sched *scheduler.Scheduler
	ev    *events.Broker
	log   *slog.Logger
	caPEM []byte // CA cert served to enrolling agents

	defaultMaxConcurrent int
	enrollTokenTTL       time.Duration

	// discoveryStatus reports LAN advertise state; nil = unset (server
	// wires it post-construction once the announcer exists).
	discoveryStatus func() map[string]any

	// modelOps is the persistent model-operation queue; nil falls back
	// to direct hub dispatch (tests that build a bare API).
	modelOps *modelops.Queue
	// modelOpsReload re-reads persisted modelops settings into the queue.
	modelOpsReload func(ctx context.Context)
}

// SetDiscoveryStatus wires the server's announce-state provider.
func (a *API) SetDiscoveryStatus(fn func() map[string]any) { a.discoveryStatus = fn }

// SetModelOps wires the persistent model-operation queue and its
// settings-reload hook.
func (a *API) SetModelOps(q *modelops.Queue, reload func(ctx context.Context)) {
	a.modelOps = q
	a.modelOpsReload = reload
}

func New(st *store.DB, hub *nodehub.Hub, sched *scheduler.Scheduler, ev *events.Broker, caPEM []byte, log *slog.Logger) *API {
	return &API{
		st: st, hub: hub, sched: sched, ev: ev, caPEM: caPEM,
		log:                  log.With("component", "adminapi"),
		defaultMaxConcurrent: 2,
		enrollTokenTTL:       24 * time.Hour,
	}
}

// Register mounts all endpoints. mwAdmin/mwRead are auth middlewares.
func (a *API) Register(mux *http.ServeMux, mwAdmin, mwRead func(http.Handler) http.Handler) {
	// Public (unauthenticated) endpoints — used by enrolling agents.
	mux.HandleFunc("GET /api/v1/ca.pem", a.handleCAPEM)
	mux.HandleFunc("POST /api/v1/enroll", a.handleEnroll)
	mux.HandleFunc("GET /api/v1/enroll/{id}", a.handleEnrollPoll)

	// Admin endpoints.
	mux.Handle("GET /api/v1/overview", mwRead(http.HandlerFunc(a.handleOverview)))
	mux.Handle("GET /api/v1/events", mwRead(http.HandlerFunc(a.handleEvents)))
	mux.Handle("GET /api/v1/audit", mwRead(http.HandlerFunc(a.handleAudit)))

	mux.Handle("GET /api/v1/nodes", mwRead(http.HandlerFunc(a.handleNodes)))
	mux.Handle("GET /api/v1/nodes/{id}", mwRead(http.HandlerFunc(a.handleNode)))
	mux.Handle("PATCH /api/v1/nodes/{id}", mwAdmin(http.HandlerFunc(a.handleNodePatch)))
	mux.Handle("DELETE /api/v1/nodes/{id}", mwAdmin(http.HandlerFunc(a.handleNodeDelete)))
	mux.Handle("POST /api/v1/nodes/{id}/revoke", mwAdmin(http.HandlerFunc(a.handleNodeRevoke)))
	mux.Handle("POST /api/v1/nodes/{id}/reinstate", mwAdmin(http.HandlerFunc(a.handleNodeReinstate)))

	mux.Handle("GET /api/v1/enrollment-tokens", mwRead(http.HandlerFunc(a.handleListEnrollTokens)))
	mux.Handle("POST /api/v1/enrollment-tokens", mwAdmin(http.HandlerFunc(a.handleCreateEnrollToken)))
	mux.Handle("DELETE /api/v1/enrollment-tokens/{id}", mwAdmin(http.HandlerFunc(a.handleRevokeEnrollToken)))

	mux.Handle("GET /api/v1/enrollments", mwRead(http.HandlerFunc(a.handleListEnrollments)))
	mux.Handle("POST /api/v1/enrollments/{id}/approve", mwAdmin(http.HandlerFunc(a.handleApproveEnrollment)))
	mux.Handle("POST /api/v1/enrollments/{id}/deny", mwAdmin(http.HandlerFunc(a.handleDenyEnrollment)))

	mux.Handle("GET /api/v1/models", mwRead(http.HandlerFunc(a.handleModels)))
	mux.Handle("POST /api/v1/models/pull", mwAdmin(http.HandlerFunc(a.handleModelPull)))
	mux.Handle("POST /api/v1/models/delete", mwAdmin(http.HandlerFunc(a.handleModelDelete)))
	mux.Handle("GET /api/v1/model-operations", mwRead(http.HandlerFunc(a.handleModelOps)))
	mux.Handle("POST /api/v1/model-operations/{id}/cancel", mwAdmin(http.HandlerFunc(a.handleModelOpCancel)))
	mux.Handle("GET /api/v1/models/placement", mwRead(http.HandlerFunc(a.handleModelPlacement)))
	mux.Handle("GET /api/v1/discovery/status", mwRead(http.HandlerFunc(a.handleDiscoveryStatus)))
	mux.Handle("POST /api/v1/dashboard/sessions/revoke", mwAdmin(http.HandlerFunc(a.handleSessionRevoke)))
	mux.Handle("GET /api/v1/dashboard/sessions/check", mwAdmin(http.HandlerFunc(a.handleSessionCheck)))

	mux.Handle("GET /api/v1/workloads", mwRead(http.HandlerFunc(a.handleWorkloads)))
	mux.Handle("GET /api/v1/workloads/{id}", mwRead(http.HandlerFunc(a.handleWorkload)))

	mux.Handle("GET /api/v1/api-keys", mwRead(http.HandlerFunc(a.handleListKeys)))
	mux.Handle("POST /api/v1/api-keys", mwAdmin(http.HandlerFunc(a.handleCreateKey)))
	mux.Handle("DELETE /api/v1/api-keys/{id}", mwAdmin(http.HandlerFunc(a.handleRevokeKey)))

	mux.Handle("GET /api/v1/settings", mwRead(http.HandlerFunc(a.handleGetSettings)))
	mux.Handle("PUT /api/v1/settings/{key}", mwAdmin(http.HandlerFunc(a.handlePutSetting)))
}

// ---------------------------------------------------------------------------
// Public enrollment endpoints
// ---------------------------------------------------------------------------

// handleDiscoveryStatus reports mDNS advertise state for the dashboard's
// onboarding flow. Everything it returns is already public on the LAN
// (the cluster name and CA fingerprint are the advertisement itself).
func (a *API) handleDiscoveryStatus(w http.ResponseWriter, r *http.Request) {
	if a.discoveryStatus == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false, "advertising": false, "cluster_name": "", "ca_fingerprint": "",
		})
		return
	}
	writeJSON(w, http.StatusOK, a.discoveryStatus())
}

func (a *API) handleCAPEM(w http.ResponseWriter, r *http.Request) {
	if len(a.caPEM) == 0 {
		http.Error(w, "TLS disabled", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Write(a.caPEM)
}

type enrollRequest struct {
	Token     string                 `json:"token"`
	Name      string                 `json:"name"`
	Hardware  contracts.HardwareInfo `json:"hardware"`
	NodeToken string                 `json:"node_token"` // agent-generated credential (hashed at rest)
	AgentVer  string                 `json:"agent_version,omitempty"`
}

func (a *API) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req enrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Token == "" || req.Name == "" || req.NodeToken == "" {
		writeErr(w, http.StatusBadRequest, "token, name and node_token are required")
		return
	}
	if !strings.HasPrefix(req.Token, creds.PrefixEnrollment+"_") {
		writeErr(w, http.StatusBadRequest, "malformed enrollment token")
		return
	}
	// Node credentials must be an agent-generated lmn_ token with real
	// entropy — anything shorter/guessable would let a client
	// impersonate the node over the agent socket.
	if !strings.HasPrefix(req.NodeToken, creds.PrefixNode+"_") || len(req.NodeToken) < 24 {
		writeErr(w, http.StatusBadRequest, "node_token too weak")
		return
	}
	if req.Name = sanitizeName(req.Name); req.Name == "" {
		writeErr(w, http.StatusBadRequest, "invalid name")
		return
	}

	tok, err := a.st.ConsumeEnrollmentToken(r.Context(), creds.Hash(req.Token))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusUnauthorized, "invalid, expired, or exhausted enrollment token")
			return
		}
		a.internalErr(w, r, err)
		return
	}

	// The poll secret is generated server-side; only its hash is stored.
	// The agent uses (request_id, poll_secret) to retrieve the decision.
	pollSecret := creds.Generate(creds.PrefixPoll, 32)
	er, err := a.st.CreateEnrollmentRequest(r.Context(), tok.ID, req.Name, req.Hardware,
		creds.Hash(pollSecret), creds.Hash(req.NodeToken))
	if err != nil {
		a.internalErr(w, r, err)
		return
	}

	a.st.Audit(r.Context(), "agent", "enroll.request", req.Name, map[string]string{"request_id": er.ID})
	a.ev.Publish("enrollment.requested", map[string]any{"id": er.ID, "name": req.Name, "hardware": req.Hardware})

	writeJSON(w, http.StatusAccepted, map[string]any{
		"request_id":  er.ID,
		"poll_secret": pollSecret,
		"status":      "pending",
		"message":     "awaiting administrator approval",
	})
}

func (a *API) handleEnrollPoll(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	secret := r.URL.Query().Get("secret")
	if secret == "" {
		secret = r.Header.Get("X-Poll-Secret") // kept out of URL logs
	}
	if id == "" || secret == "" {
		writeErr(w, http.StatusBadRequest, "id and secret are required")
		return
	}
	er, err := a.st.GetEnrollmentRequestForPoll(r.Context(), id, creds.Hash(secret))
	if err != nil {
		writeErr(w, http.StatusNotFound, "unknown enrollment request")
		return
	}
	resp := map[string]any{"status": er.Status}
	switch er.Status {
	case "approved":
		resp["node_id"] = er.NodeID
		resp["ca_pem"] = string(a.caPEM)
	case "denied":
		resp["message"] = "enrollment was denied by an administrator"
	default:
		resp["message"] = "awaiting administrator approval"
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Enrollment admin
// ---------------------------------------------------------------------------

func (a *API) handleListEnrollTokens(w http.ResponseWriter, r *http.Request) {
	toks, err := a.st.ListEnrollmentTokens(r.Context())
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	if toks == nil {
		toks = []*store.EnrollmentToken{}
	}
	writeJSON(w, http.StatusOK, toks)
}

type createEnrollTokenReq struct {
	Label   string `json:"label"`
	MaxUses int    `json:"max_uses"`
	TTLH    int    `json:"ttl_hours"`
}

func (a *API) handleCreateEnrollToken(w http.ResponseWriter, r *http.Request) {
	var req createEnrollTokenReq
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
	if req.MaxUses <= 0 {
		req.MaxUses = 1
	}
	if req.MaxUses > 100 {
		req.MaxUses = 100
	}
	ttl := time.Duration(req.TTLH) * time.Hour
	if ttl <= 0 || ttl > 30*24*time.Hour {
		ttl = a.enrollTokenTTL
	}
	plaintext := creds.Generate(creds.PrefixEnrollment, 24)
	tok, err := a.st.CreateEnrollmentToken(r.Context(), req.Label, plaintext[:12], creds.Hash(plaintext), req.MaxUses, ttl)
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	principal, _ := auth.PrincipalFrom(r.Context())
	a.st.Audit(r.Context(), principal.Name, "enroll_token.create", tok.ID, nil)
	a.ev.Publish("enrollment_token.created", tok)
	// Plaintext is returned exactly once.
	writeJSON(w, http.StatusCreated, map[string]any{"token": plaintext, "record": tok})
}

func (a *API) handleRevokeEnrollToken(w http.ResponseWriter, r *http.Request) {
	if err := a.st.RevokeEnrollmentToken(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, "token not found or already revoked")
		return
	}
	principal, _ := auth.PrincipalFrom(r.Context())
	a.st.Audit(r.Context(), principal.Name, "enroll_token.revoke", r.PathValue("id"), nil)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleListEnrollments(w http.ResponseWriter, r *http.Request) {
	reqs, err := a.st.ListEnrollmentRequests(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	if reqs == nil {
		reqs = []*store.EnrollmentRequest{}
	}
	writeJSON(w, http.StatusOK, reqs)
}

func (a *API) handleApproveEnrollment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	hash, err := a.st.EnrollmentRequestNodeTokenHash(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "enrollment request not found")
		return
	}
	// Fetch the request directly — the pending list is capped, so a
	// request beyond the page limit must still be approvable.
	target, err := a.st.GetEnrollmentRequest(r.Context(), id)
	if err != nil || target == nil {
		writeErr(w, http.StatusNotFound, "enrollment request not found")
		return
	}
	if target.Status != "pending" {
		writeErr(w, http.StatusConflict, "request already decided")
		return
	}
	node, err := a.st.ApproveEnrollmentRequest(r.Context(), id, target.Name, hash, target.Hardware, a.defaultMaxConcurrent)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	principal, _ := auth.PrincipalFrom(r.Context())
	a.st.Audit(r.Context(), principal.Name, "enroll.approve", node.Name, map[string]string{"node_id": node.ID})
	a.ev.Publish("node.approved", map[string]string{"id": node.ID, "name": node.Name})
	writeJSON(w, http.StatusOK, node)
}

func (a *API) handleDenyEnrollment(w http.ResponseWriter, r *http.Request) {
	if err := a.st.DenyEnrollmentRequest(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, "enrollment request not found or already decided")
		return
	}
	principal, _ := auth.PrincipalFrom(r.Context())
	a.st.Audit(r.Context(), principal.Name, "enroll.deny", r.PathValue("id"), nil)
	a.ev.Publish("enrollment.denied", map[string]string{"id": r.PathValue("id")})
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Nodes
// ---------------------------------------------------------------------------

func (a *API) nodeViews(r *http.Request) ([]nodehub.View, error) {
	return a.hub.Snapshot(r.Context())
}

func (a *API) handleNodes(w http.ResponseWriter, r *http.Request) {
	views, err := a.nodeViews(r)
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	out := []map[string]any{}
	for _, v := range views {
		out = append(out, nodeJSON(v))
	}
	writeJSON(w, http.StatusOK, out)
}

func nodeJSON(v nodehub.View) map[string]any {
	n := v.Node
	return map[string]any{
		"id": n.ID, "name": n.Name, "status": n.Status,
		"online": v.Online, "stale_heartbeat": v.StaleBeat,
		"hardware": n.Hardware, "engines": n.Engines, "models": n.Models,
		"metrics": n.Metrics, "labels": n.Labels,
		"max_concurrent": n.MaxConcurrent, "in_flight": v.InFlight,
		"latency_ms":   v.LatencyMS,
		"last_seen_at": n.LastSeenAt, "created_at": n.CreatedAt,
	}
}

func (a *API) handleNode(w http.ResponseWriter, r *http.Request) {
	n, err := a.st.GetNode(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "node not found")
		return
	}
	v := nodehub.View{Node: n, Online: a.hub.Online(n.ID), InFlight: a.hub.InFlight(n.ID)}
	writeJSON(w, http.StatusOK, nodeJSON(v))
}

type nodePatch struct {
	Name          *string            `json:"name"`
	MaxConcurrent *int               `json:"max_concurrent"`
	Labels        *map[string]string `json:"labels"`
}

func (a *API) handleNodePatch(w http.ResponseWriter, r *http.Request) {
	var p nodePatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if p.Name != nil {
		n := sanitizeName(*p.Name)
		if n == "" {
			writeErr(w, http.StatusBadRequest, "invalid name")
			return
		}
		p.Name = &n
	}
	id := r.PathValue("id")
	if p.MaxConcurrent != nil && (*p.MaxConcurrent < 1 || *p.MaxConcurrent > 64) {
		writeErr(w, http.StatusBadRequest, "max_concurrent must be 1-64")
		return
	}
	var labels map[string]string
	if p.Labels != nil {
		labels = *p.Labels
	}
	if err := a.st.UpdateNodeConfig(r.Context(), id, p.Name, p.MaxConcurrent, labels); err != nil {
		a.internalErr(w, r, err)
		return
	}
	n, err := a.st.GetNode(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "node not found")
		return
	}
	// Keep any live conn's config in sync — the capacity gate reads it.
	a.hub.UpdateNodeConfig(id, n.MaxConcurrent, n.Labels)
	a.ev.Publish("node.updated", map[string]string{"id": id})
	writeJSON(w, http.StatusOK, nodeJSON(nodehub.View{Node: n, Online: a.hub.Online(id)}))
}

func (a *API) handleNodeRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.st.SetNodeStatus(r.Context(), id, contracts.NodeStatusRevoked); err != nil {
		writeErr(w, http.StatusNotFound, "node not found")
		return
	}
	// Revocation is effective immediately: drop the live connection so
	// the node stops receiving work now, not whenever it happens to
	// reconnect.
	a.hub.Disconnect(id, "node revoked")
	principal, _ := auth.PrincipalFrom(r.Context())
	a.st.Audit(r.Context(), principal.Name, "node.revoke", id, nil)
	a.ev.Publish("node.revoked", map[string]string{"id": id})
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleNodeReinstate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.st.SetNodeStatus(r.Context(), id, contracts.NodeStatusApproved); err != nil {
		writeErr(w, http.StatusNotFound, "node not found")
		return
	}
	principal, _ := auth.PrincipalFrom(r.Context())
	a.st.Audit(r.Context(), principal.Name, "node.reinstate", id, nil)
	a.ev.Publish("node.reinstated", map[string]string{"id": id})
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleNodeDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Drop the live conn BEFORE deleting the row so a reconnect racing
	// the delete can't slip in under the old credential.
	a.hub.Disconnect(id, "node deleted")
	if err := a.st.DeleteNode(r.Context(), id); err != nil {
		writeErr(w, http.StatusNotFound, "node not found")
		return
	}
	principal, _ := auth.PrincipalFrom(r.Context())
	a.st.Audit(r.Context(), principal.Name, "node.delete", id, nil)
	a.ev.Publish("node.deleted", map[string]string{"id": id})
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

type clusterModel struct {
	Name        string   `json:"name"`
	Engine      string   `json:"engine"`
	Family      string   `json:"family,omitempty"`
	ParameterSz string   `json:"parameter_size,omitempty"`
	SizeBytes   uint64   `json:"size_bytes"`
	Digest      string   `json:"digest,omitempty"`
	NodeIDs     []string `json:"node_ids"`
	NodeNames   []string `json:"node_names"`
	LoadedOn    []string `json:"loaded_on"`
	Nodes       int      `json:"nodes"`
}

func (a *API) handleModels(w http.ResponseWriter, r *http.Request) {
	nodes, err := a.st.ListNodes(r.Context())
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	idx := map[string]*clusterModel{}
	for _, n := range nodes {
		if n.Status != contracts.NodeStatusApproved {
			continue
		}
		for _, m := range n.Models {
			cm, ok := idx[m.Name]
			if !ok {
				cm = &clusterModel{Name: m.Name, Engine: m.Engine, Family: m.Family,
					ParameterSz: m.ParameterSz, SizeBytes: m.SizeBytes, Digest: m.Digest}
				idx[m.Name] = cm
			}
			cm.NodeIDs = append(cm.NodeIDs, n.ID)
			cm.NodeNames = append(cm.NodeNames, n.Name)
			cm.Nodes++
			if m.Loaded {
				cm.LoadedOn = append(cm.LoadedOn, n.ID)
			}
		}
	}
	out := []clusterModel{}
	for _, cm := range idx {
		out = append(out, *cm)
	}
	writeJSON(w, http.StatusOK, out)
}

type modelOpRequest struct {
	Model    string   `json:"model"`
	NodeIDs  []string `json:"node_ids"` // empty = all online nodes that have (pull) / all nodes
	Force    bool     `json:"force"`    // delete: allow removing the cluster's last replica
	Priority int      `json:"priority"` // queue: higher dispatches first; 0 = normal
}

func (a *API) handleModelPull(w http.ResponseWriter, r *http.Request)   { a.modelOp(w, r, "pull") }
func (a *API) handleModelDelete(w http.ResponseWriter, r *http.Request) { a.modelOp(w, r, "delete") }

// modelRef validates an Ollama-style model reference before it is sent
// to an agent — the name is embedded in engine API calls, so it must be
// a plain identifier, never a URL, path, or control string.
var modelRef = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}(/[a-zA-Z0-9][a-zA-Z0-9._-]{0,127})?(:[a-zA-Z0-9._-]{1,64})?$`)

func (a *API) modelOp(w http.ResponseWriter, r *http.Request, op string) {
	var req modelOpRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || req.Model == "" {
		writeErr(w, http.StatusBadRequest, "model is required")
		return
	}
	if !modelRef.MatchString(req.Model) {
		writeErr(w, http.StatusBadRequest, "invalid model identifier")
		return
	}
	if op == "delete" && !req.Force {
		// Guard the destructive case: removing the model's only
		// remaining replica makes it unschedulable cluster-wide.
		replicas := 0
		if all, lerr := a.st.ListNodes(r.Context()); lerr == nil {
			// Empty node_ids dispatches to every eligible host — a copy
			// survives only on nodes outside the dispatch set.
			allTargeted := len(req.NodeIDs) == 0
			targets := map[string]bool{}
			for _, id := range req.NodeIDs {
				targets[id] = true
			}
			for _, n := range all {
				for _, m := range n.Models {
					if m.Name == req.Model && !allTargeted && !targets[n.ID] {
						replicas++ // replica survives this delete
					}
				}
			}
		}
		if replicas == 0 {
			writeErr(w, http.StatusConflict,
				"this delete removes the model's last available replica — retry with force to confirm")
			return
		}
	}
	views, err := a.nodeViews(r)
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	targets := map[string]bool{}
	for _, id := range req.NodeIDs {
		targets[id] = true
	}
	var ops []*store.ModelOperation
	var skipped []string
	for _, v := range views {
		if !v.Online || v.Node.Status != contracts.NodeStatusApproved {
			continue
		}
		if len(targets) > 0 && !targets[v.Node.ID] {
			continue
		}
		if a.modelOps != nil {
			// Queue path: the op row lands 'queued' first; the dispatch
			// loop sends the wire frame when capacity allows.
			opRec, err := a.modelOps.Submit(r.Context(), modelops.SubmitRequest{
				NodeID: v.Node.ID, Model: req.Model, Op: op, Priority: req.Priority,
			})
			if err != nil {
				var de *modelops.DuplicateError
				if errors.As(err, &de) {
					skipped = append(skipped, v.Node.Name+" (op "+de.OpID+" already "+de.Status+")")
					continue
				}
				if errors.Is(err, modelops.ErrQueueFull) {
					writeErr(w, http.StatusTooManyRequests, "model operation queue is full — retry shortly")
					return
				}
				a.internalErr(w, r, err)
				return
			}
			ops = append(ops, opRec)
			continue
		}
		opRec, err := a.st.CreateModelOperation(r.Context(), v.Node.ID, req.Model, op)
		if err != nil {
			if errors.Is(err, store.ErrActiveModelOp) {
				skipped = append(skipped, v.Node.Name+" (busy with an op on this model)")
				continue
			}
			a.internalErr(w, r, err)
			return
		}
		if err := a.hub.RequestModelOp(r.Context(), v.Node.ID, opRec.ID, req.Model, op); err != nil {
			a.st.FinishModelOperation(r.Context(), opRec.ID, false, err.Error())
			skipped = append(skipped, v.Node.Name)
			continue
		}
		ops = append(ops, opRec)
	}
	if len(ops) == 0 {
		writeErr(w, http.StatusConflict, "no online eligible nodes for this operation"+skippedSuffix(skipped))
		return
	}
	principal, _ := auth.PrincipalFrom(r.Context())
	a.st.Audit(r.Context(), principal.Name, "model."+op, req.Model, map[string]string{"nodes": strconv.Itoa(len(ops))})
	writeJSON(w, http.StatusAccepted, map[string]any{"operations": ops})
}

// handleModelOpCancel aborts an in-flight pull/delete. The agent marks
// the op 'cancelled' via OpDone; if the node is unreachable the op is
// already dead and we finalize it here.
func (a *API) handleModelOpCancel(w http.ResponseWriter, r *http.Request) {
	op, err := a.st.GetModelOperation(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "operation not found")
		return
	}
	if a.modelOps != nil {
		// Queue owns the full cancel matrix: queued → terminal in place,
		// dispatched/running → abort frame + local finalize.
		if err := a.modelOps.Cancel(r.Context(), op.ID); err != nil {
			if errors.Is(err, store.ErrNotFound) || errors.Is(err, modelops.ErrNotActive) {
				writeErr(w, http.StatusConflict, "operation already finished")
				return
			}
			a.internalErr(w, r, err)
			return
		}
		principal, _ := auth.PrincipalFrom(r.Context())
		a.st.Audit(r.Context(), principal.Name, "model_op.cancel", op.Model, map[string]string{"operation_id": op.ID})
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if op.Status != "dispatched" && op.Status != "running" {
		writeErr(w, http.StatusConflict, "operation already finished")
		return
	}
	err = a.hub.AbortModelOp(r.Context(), op.ID)
	if err != nil {
		if err == nodehub.ErrNodeOffline || errors.Is(err, store.ErrNotFound) {
			a.st.FinishModelOperationStatus(r.Context(), op.ID, "cancelled", "cancelled by administrator")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		a.internalErr(w, r, err)
		return
	}
	principal, _ := auth.PrincipalFrom(r.Context())
	a.st.Audit(r.Context(), principal.Name, "model_op.cancel", op.Model, map[string]string{"operation_id": op.ID})
	w.WriteHeader(http.StatusNoContent)
}

// handleModelPlacement builds the cluster-wide placement view: for each
// model, which nodes host it (with per-node digest/loaded), estimated
// memory need, which online nodes could host it, and active ops. Purely
// derived from stored node/model/metrics state — no fake data.
func (a *API) handleModelPlacement(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	nodes, err := a.st.ListNodes(ctx)
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	ops, err := a.st.ListModelOperations(ctx, 50)
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	activeOps := map[string][]*store.ModelOperation{}
	for _, op := range ops {
		if op.Status == "dispatched" || op.Status == "running" {
			activeOps[op.Model] = append(activeOps[op.Model], op)
		}
	}
	online := map[string]bool{}
	if views, verr := a.hub.Snapshot(ctx); verr == nil {
		for _, v := range views {
			if v.Online {
				online[v.Node.ID] = true
			}
		}
	}

	type placementNode struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Online   bool   `json:"online"`
		Digest   string `json:"digest,omitempty"`
		Loaded   bool   `json:"loaded"`
		LoadedMB uint64 `json:"loaded_mb,omitempty"`
	}
	type candidate struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Reason   string `json:"reason"`
		FreeMB   uint64 `json:"free_mb,omitempty"`
		Eligible bool   `json:"eligible"`
	}
	type entry struct {
		Name        string          `json:"name"`
		Engine      string          `json:"engine"`
		Family      string          `json:"family,omitempty"`
		SizeMB      uint64          `json:"size_mb"`
		EstMemMB    uint64          `json:"est_mem_mb"`
		Replicas    int             `json:"replicas"`
		LoadedOn    int             `json:"loaded_on"`
		Nodes       []placementNode `json:"nodes"`
		Candidates  []candidate     `json:"candidates"`
		ActiveOps   []string        `json:"active_ops,omitempty"`
		LastReplica bool            `json:"last_replica"`
	}

	byName := map[string]*entry{}
	order := []string{}
	for _, n := range nodes {
		for _, m := range n.Models {
			e, ok := byName[m.Name]
			if !ok {
				e = &entry{Name: m.Name, Engine: m.Engine, Family: m.Family,
					SizeMB: m.SizeBytes >> 20, Nodes: []placementNode{}}
				byName[m.Name] = e
				order = append(order, m.Name)
			}
			e.Nodes = append(e.Nodes, placementNode{
				ID: n.ID, Name: n.Name, Online: online[n.ID],
				Digest: m.Digest, Loaded: m.Loaded, LoadedMB: m.LoadedBytes >> 20,
			})
			if m.LoadedBytes>>20 > e.EstMemMB {
				e.EstMemMB = m.LoadedBytes >> 20 // real residency beats estimates
			}
		}
	}
	for _, name := range order {
		e := byName[name]
		if e.EstMemMB == 0 {
			e.EstMemMB = uint64(float64(e.SizeMB) * 1.5) // documented estimate
		}
		seen := map[string]bool{}
		for _, pn := range e.Nodes {
			if !seen[pn.ID] {
				seen[pn.ID] = true
				e.Replicas++
				if pn.Loaded {
					e.LoadedOn++
				}
			}
		}
		e.LastReplica = e.Replicas == 1
		if ops, ok := activeOps[name]; ok {
			for _, op := range ops {
				e.ActiveOps = append(e.ActiveOps, op.Op+":"+op.Status)
			}
		}
		// Candidate nodes: online, not already hosting, plausibly able.
		for _, n := range nodes {
			if !online[n.ID] || seen[n.ID] {
				continue
			}
			c := candidate{ID: n.ID, Name: n.Name}
			var freeMB uint64
			if n.Metrics != nil && n.Metrics.MemoryTotalBytes > 0 {
				freeMB = (n.Metrics.MemoryTotalBytes - n.Metrics.MemoryUsedBytes) >> 20
				c.FreeMB = freeMB
			}
			switch {
			case len(n.Engines) == 0:
				c.Reason = "no inference engine detected"
			case freeMB > 0 && freeMB < e.EstMemMB:
				c.Reason = "insufficient free memory for estimated footprint"
			case freeMB > 0:
				c.Reason = "sufficient estimated headroom"
				c.Eligible = true
			default:
				c.Reason = "memory availability unmeasured — unknown"
				c.Eligible = true // unknown ≠ ineligible
			}
			e.Candidates = append(e.Candidates, c)
		}
	}
	out := make([]*entry, 0, len(order))
	for _, n := range order {
		out = append(out, byName[n])
	}
	writeJSON(w, http.StatusOK, out)
}

// Dashboard session revocation — called by the web BFF with the admin
// key on logout so a revoked cookie stays dead across web restarts.
func (a *API) handleSessionRevoke(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Nonce     string `json:"nonce"`
		ExpiresAt int64  `json:"expires_at"` // epoch ms from the cookie
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Nonce == "" {
		writeErr(w, http.StatusBadRequest, "nonce required")
		return
	}
	if len(req.Nonce) > 128 || len(req.Nonce) < 16 {
		writeErr(w, http.StatusBadRequest, "invalid nonce")
		return
	}
	exp := time.UnixMilli(req.ExpiresAt)
	if req.ExpiresAt <= 0 {
		exp = time.Now().Add(12 * time.Hour)
	}
	if err := a.st.RevokeSessionNonce(r.Context(), req.Nonce, exp); err != nil {
		a.internalErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSessionCheck lets the BFF verify a nonce isn't revoked. Cheap
// indexed lookup — called per authenticated dashboard request.
func (a *API) handleSessionCheck(w http.ResponseWriter, r *http.Request) {
	nonce := r.URL.Query().Get("nonce")
	if nonce == "" {
		writeErr(w, http.StatusBadRequest, "nonce required")
		return
	}
	revoked, err := a.st.IsSessionRevoked(r.Context(), nonce)
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"revoked": revoked})
}

func skippedSuffix(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return " (dispatch failed on: " + strings.Join(s, ", ") + ")"
}

func (a *API) handleModelOps(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	// OpState carries the queue fields (attempt/next_attempt/phase/rate/
	// indeterminate) flat on the op record — the dashboard needs them.
	ops, err := a.st.ListModelOpStates(r.Context(), limit)
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	if ops == nil {
		ops = []*store.OpState{}
	}
	writeJSON(w, http.StatusOK, ops)
}

// ---------------------------------------------------------------------------
// Workloads
// ---------------------------------------------------------------------------

func (a *API) handleWorkloads(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	f := store.WorkloadFilter{
		Status: r.URL.Query().Get("status"),
		NodeID: r.URL.Query().Get("node_id"),
		Limit:  limit,
	}
	ws, err := a.st.ListWorkloads(r.Context(), f)
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	if ws == nil {
		ws = []*store.Workload{}
	}
	writeJSON(w, http.StatusOK, ws)
}

func (a *API) handleWorkload(w http.ResponseWriter, r *http.Request) {
	wl, err := a.st.GetWorkload(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "workload not found")
		return
	}
	writeJSON(w, http.StatusOK, wl)
}

// ---------------------------------------------------------------------------
// API keys
// ---------------------------------------------------------------------------

func (a *API) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := a.st.ListAPIKeys(r.Context())
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	if keys == nil {
		keys = []*store.APIKey{}
	}
	writeJSON(w, http.StatusOK, keys)
}

type createKeyReq struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

func (a *API) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var req createKeyReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	switch req.Role {
	case auth.RoleAdmin, auth.RoleInference, auth.RoleReadonly:
	default:
		writeErr(w, http.StatusBadRequest, "role must be admin, inference, or readonly")
		return
	}
	plaintext := creds.Generate(creds.PrefixAPIKey, 32)
	key, err := a.st.CreateAPIKey(r.Context(), req.Name, plaintext[:12], creds.Hash(plaintext), req.Role)
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	principal, _ := auth.PrincipalFrom(r.Context())
	a.st.Audit(r.Context(), principal.Name, "api_key.create", req.Name, map[string]string{"role": req.Role})
	a.ev.Publish("api_key.created", key)
	writeJSON(w, http.StatusCreated, map[string]any{"key": plaintext, "record": key})
}

func (a *API) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	principal, _ := auth.PrincipalFrom(r.Context())
	if principal.ID == id {
		writeErr(w, http.StatusConflict, "cannot revoke the key you are authenticated with")
		return
	}
	if err := a.st.RevokeAPIKey(r.Context(), id); err != nil {
		writeErr(w, http.StatusNotFound, "key not found or already revoked")
		return
	}
	a.st.Audit(r.Context(), principal.Name, "api_key.revoke", id, nil)
	a.ev.Publish("api_key.revoked", map[string]string{"id": id})
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Overview, events, audit, settings
// ---------------------------------------------------------------------------

func (a *API) handleOverview(w http.ResponseWriter, r *http.Request) {
	views, err := a.nodeViews(r)
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	var (
		online, totalCPU  int
		totalMem, usedMem uint64
		totalVRAM         uint64
		gpuCount          int
		activeReq         int
		modelSet          = map[string]bool{}
	)
	for _, v := range views {
		if v.Online {
			online++
		}
		totalCPU += v.Node.Hardware.CPUCores
		totalMem += v.Node.Hardware.MemoryBytes
		gpuCount += len(v.Node.Hardware.GPUs)
		for _, g := range v.Node.Hardware.GPUs {
			totalVRAM += g.VRAMBytes
		}
		if v.Node.Metrics != nil {
			usedMem += v.Node.Metrics.MemoryUsedBytes
		}
		activeReq += v.InFlight
		for _, m := range v.Node.Models {
			modelSet[m.Name] = true
		}
	}
	active, _ := a.st.CountActiveWorkloads(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"nodes_total":      len(views),
		"nodes_online":     online,
		"cpu_cores_total":  totalCPU,
		"memory_bytes":     totalMem,
		"memory_used":      usedMem,
		"gpu_count":        gpuCount,
		"vram_bytes":       totalVRAM,
		"active_requests":  activeReq,
		"queued_or_active": active,
		"models_distinct":  len(modelSet),
	})
}

func (a *API) handleEvents(w http.ResponseWriter, r *http.Request) {
	principal, hasPrincipal := auth.PrincipalFrom(r.Context())
	owner := ""
	if hasPrincipal {
		owner = principal.ID
	}
	ch, unsub, ok := a.ev.SubscribeFor(owner, 256, 4)
	if !ok {
		writeErr(w, http.StatusTooManyRequests, "too many event subscriptions")
		return
	}
	defer unsub()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	flush := func() { rc.Flush() }
	// Commit the 200 + SSE headers to the wire now — without a flush the
	// client can't tell a live stream from a hung connection until the
	// first event or keepalive (up to 20s).
	flush()

	ka := time.NewTicker(20 * time.Second)
	defer ka.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			w.Write([]byte("event: " + ev.Type + "\n"))
			w.Write([]byte("data: "))
			w.Write(ev.Marshal())
			w.Write([]byte("\n\n"))
			flush()
		case <-ka.C:
			w.Write([]byte(": keepalive\n\n"))
			flush()
		}
	}
}

func (a *API) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := a.st.ListAudit(r.Context(), limit)
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	if entries == nil {
		entries = []store.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

func (a *API) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	all, err := a.st.AllSettings(r.Context())
	if err != nil {
		a.internalErr(w, r, err)
		return
	}
	if all == nil {
		all = map[string]json.RawMessage{}
	}
	if _, ok := all["modelops"]; !ok && a.modelOps != nil {
		// Surface the effective queue bounds so the dashboard can render
		// the tuning card before the operator first saves them.
		c := a.modelOps.Config()
		if raw, err := json.Marshal(map[string]any{
			"global_concurrent":   c.GlobalConcurrent,
			"per_node_concurrent": c.PerNodeConcurrent,
			"queue_max":           c.QueueMax,
			"max_attempts":        c.MaxAttempts,
		}); err == nil {
			all["modelops"] = raw
		}
	}
	writeJSON(w, http.StatusOK, all)
}

func (a *API) handlePutSetting(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	allowed := map[string]bool{"scheduling": true, "privacy": true, "cluster": true, "modelops": true}
	if !allowed[key] {
		writeErr(w, http.StatusBadRequest, "unknown settings key")
		return
	}
	var v json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&v); err != nil || len(v) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if msg := validateSetting(key, v); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if err := a.st.SetSetting(r.Context(), key, v); err != nil {
		a.internalErr(w, r, err)
		return
	}
	if key == "scheduling" || key == "privacy" {
		a.reloadSchedulingConfig(r.Context())
	}
	if key == "modelops" && a.modelOpsReload != nil {
		a.modelOpsReload(r.Context())
	}
	principal, _ := auth.PrincipalFrom(r.Context())
	a.st.Audit(r.Context(), principal.Name, "settings.update", key, nil)
	a.ev.Publish("settings.updated", map[string]string{"key": key})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// reloadSchedulingConfig merges persisted scheduling + privacy settings
// into the live scheduler config.
func (a *API) reloadSchedulingConfig(ctx context.Context) {
	cfg := scheduler.DefaultConfig()
	var sched map[string]any
	if a.st.GetSetting(ctx, "scheduling", &sched) == nil {
		if v, ok := sched["queue_timeout_ms"].(float64); ok {
			cfg.QueueTimeout = time.Duration(v) * time.Millisecond
		}
		if v, ok := sched["max_attempts"].(float64); ok {
			cfg.MaxAttempts = int(v)
		}
		if v, ok := sched["heartbeat_timeout_seconds"].(float64); ok && v >= 10 && v <= 600 {
			a.hub.SetHeartbeatTimeout(time.Duration(v) * time.Second)
		}
		if w, ok := sched["weights"].(map[string]any); ok {
			cfg.Weights = scheduler.Weights{
				ModelLoaded:    num(w["model_loaded"], cfg.Weights.ModelLoaded),
				MemoryHeadroom: num(w["memory_headroom"], cfg.Weights.MemoryHeadroom),
				ModelFit:       num(w["model_fit"], cfg.Weights.ModelFit),
				GPUUtil:        num(w["gpu_util"], cfg.Weights.GPUUtil),
				CPUUtil:        num(w["cpu_util"], cfg.Weights.CPUUtil),
				ActiveRequests: num(w["active_requests"], cfg.Weights.ActiveRequests),
				Latency:        num(w["latency"], cfg.Weights.Latency),
				Preferred:      num(w["preferred"], cfg.Weights.Preferred),
			}
		}
	}
	var priv map[string]any
	if a.st.GetSetting(ctx, "privacy", &priv) == nil {
		if v, ok := priv["allow_remote_nodes"].(bool); ok {
			cfg.AllowRemote = v
		}
		if rl, ok := priv["required_labels"].(map[string]any); ok {
			for k, v := range rl {
				if s, ok := v.(string); ok {
					cfg.RequiredLabels[k] = s
				}
			}
		}
		if pl, ok := priv["prefer_labels"].(map[string]any); ok {
			for k, v := range pl {
				if s, ok := v.(string); ok {
					cfg.PreferLabels[k] = s
				}
			}
		}
	}
	a.sched.UpdateConfig(cfg)
}

// internalErr reports a 500 without leaking internals to the client;
// the detail goes to the log with the request path attached.
func (a *API) internalErr(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("internal error", "path", r.Method+" "+r.URL.Path, "err", err)
	writeErr(w, http.StatusInternalServerError, "internal server error")
}

// validateSetting enforces sane bounds on writable settings so a bad
// PUT can't wedge scheduling or heartbeat behavior.
func validateSetting(key string, v json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(v, &m); err != nil {
		return "settings value must be a JSON object"
	}
	numAt := func(k string) (float64, bool) {
		f, ok := m[k].(float64)
		return f, ok
	}
	switch key {
	case "scheduling":
		if f, ok := numAt("queue_timeout_ms"); ok && (f < 1000 || f > 300000) {
			return "queue_timeout_ms must be 1000-300000"
		}
		if f, ok := numAt("max_attempts"); ok && (f < 1 || f > 10 || f != math.Trunc(f)) {
			return "max_attempts must be an integer 1-10"
		}
		if f, ok := numAt("heartbeat_timeout_seconds"); ok && (f < 10 || f > 600) {
			return "heartbeat_timeout_seconds must be 10-600"
		}
		if w, ok := m["weights"].(map[string]any); ok {
			for name, val := range w {
				f, isNum := val.(float64)
				if !isNum || f < 0 || f > 100 {
					return "weight " + name + " must be a number 0-100"
				}
			}
		}
	case "privacy":
		if v, ok := m["allow_remote_nodes"]; ok {
			if _, isBool := v.(bool); !isBool {
				return "allow_remote_nodes must be a boolean"
			}
		}
		for _, lk := range []string{"required_labels", "prefer_labels"} {
			if lm, ok := m[lk].(map[string]any); ok {
				for k, v := range lm {
					if _, isStr := v.(string); !isStr || k == "" {
						return lk + " must map non-empty names to strings"
					}
				}
			} else if _, present := m[lk]; present {
				return lk + " must be an object"
			}
		}
	case "modelops":
		bounds := map[string][2]float64{
			"global_concurrent":   {1, 16},
			"per_node_concurrent": {1, 4},
			"queue_max":           {8, 256},
			"max_attempts":        {0, 8},
		}
		for k, b := range bounds {
			if f, ok := numAt(k); ok {
				if f < b[0] || f > b[1] || f != math.Trunc(f) {
					return k + " must be an integer in [" +
						strconv.FormatFloat(b[0], 'f', -1, 64) + "," +
						strconv.FormatFloat(b[1], 'f', -1, 64) + "]"
				}
			}
		}
	}
	return ""
}

func num(v any, def float64) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return def
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{
		"error": map[string]any{"message": msg, "type": http.StatusText(code)},
	})
}

func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 64 {
		s = s[:64]
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.' || r == ' ') {
			return ""
		}
	}
	return s
}
