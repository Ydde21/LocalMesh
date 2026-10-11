// Package gateway exposes the unified OpenAI-compatible API. It
// authenticates requests, hands them to the scheduler, dispatches to the
// chosen node via nodehub, and renders responses in OpenAI wire format.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/server/auth"
	"github.com/localmesh/localmesh/internal/server/events"
	"github.com/localmesh/localmesh/internal/server/nodehub"
	"github.com/localmesh/localmesh/internal/server/scheduler"
	"github.com/localmesh/localmesh/internal/server/store"
)

type Gateway struct {
	st    *store.DB
	hub   *nodehub.Hub
	sched *scheduler.Scheduler
	ev    *events.Broker
	log   *slog.Logger

	// RequestTimeout bounds total execution once dispatched.
	RequestTimeout time.Duration
	// sem bounds concurrent chat-completions so a flood of clients
	// can't pile unbounded goroutines into the scheduling queue.
	sem chan struct{}
}

func New(st *store.DB, hub *nodehub.Hub, sched *scheduler.Scheduler, ev *events.Broker, log *slog.Logger) *Gateway {
	return &Gateway{
		st: st, hub: hub, sched: sched, ev: ev,
		log:            log.With("component", "gateway"),
		RequestTimeout: 10 * time.Minute,
		sem:            make(chan struct{}, 256),
	}
}

// RegisterRoutes mounts the /v1 endpoints on mux. authn is the API-key
// middleware for the inference role.
func (g *Gateway) RegisterRoutes(mux *http.ServeMux, authn func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/models", authn(http.HandlerFunc(g.handleModels)))
	mux.Handle("POST /v1/chat/completions", authn(http.HandlerFunc(g.handleChatCompletions)))
}

// ---------------------------------------------------------------------------
// GET /v1/models
// ---------------------------------------------------------------------------

type oaiModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// handleModels lists models an inference request could be dispatched to
// RIGHT NOW: at least one approved, online, fresh-heartbeat node not
// under maintenance reports the model installed. This is a point-in-time
// advertisement — dispatch still re-checks eligibility per request (a
// node can drop between list and schedule). The admin inventory
// (/api/v1/models) is the full installed view including offline-only
// models.
func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	views, err := g.hub.Snapshot(r.Context())
	if err != nil {
		g.log.Warn("models snapshot failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	seen := map[string]bool{}
	var out []oaiModel
	for _, v := range views {
		n := v.Node
		if n.Status != contracts.NodeStatusApproved || !v.Online || v.StaleBeat ||
			n.Maintenance != "" {
			continue
		}
		for _, m := range n.Models {
			if seen[m.Name] {
				continue
			}
			seen[m.Name] = true
			out = append(out, oaiModel{
				ID:      m.Name,
				Object:  "model",
				Created: n.CreatedAt.Unix(),
				OwnedBy: "localmesh",
			})
		}
	}
	if out == nil {
		out = []oaiModel{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

// ---------------------------------------------------------------------------
// POST /v1/chat/completions
// ---------------------------------------------------------------------------

type chatRequest struct {
	Model       string              `json:"model"`
	Messages    []contracts.Message `json:"messages"`
	Stream      bool                `json:"stream"`
	Temperature *float64            `json:"temperature"`
	TopP        *float64            `json:"top_p"`
	MaxTokens   *int                `json:"max_tokens"`
	Stop        []string            `json:"stop"`
	StreamOpts  *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	// LocalMesh extensions (ignored by stock OpenAI clients).
	PreferNodeID string            `json:"x_localmesh_node,omitempty"`
	PreferLabels map[string]string `json:"x_localmesh_labels,omitempty"`
}

func (r *chatRequest) validate() string {
	if r.Model == "" {
		return "model is required"
	}
	if len(r.Messages) == 0 {
		return "messages must not be empty"
	}
	for _, m := range r.Messages {
		if m.Role == "" {
			return "message role is required"
		}
	}
	return ""
}

func (g *Gateway) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	var req chatRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if msg := req.validate(); msg != "" {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", msg)
		return
	}

	// Backpressure: refuse when the gateway is saturated rather than
	// queueing unboundedly.
	select {
	case g.sem <- struct{}{}:
		defer func() { <-g.sem }()
	default:
		writeErr(w, http.StatusServiceUnavailable, "overloaded",
			"control plane is at its concurrent request limit; retry shortly")
		return
	}

	wl := &store.Workload{
		ID:          uuid.NewString(),
		Model:       req.Model,
		Streaming:   req.Stream,
		PromptChars: promptChars(req.Messages),
	}
	if p.ID != "" {
		wl.APIKeyID = &p.ID
	}
	if err := g.st.CreateWorkload(r.Context(), wl); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to record workload")
		return
	}
	g.ev.Publish("workload.queued", wl)

	schedReq := scheduler.Request{
		Model:        req.Model,
		Streaming:    req.Stream,
		PreferNodeID: req.PreferNodeID,
		PreferLabels: req.PreferLabels,
	}

	pick, err := g.sched.Schedule(r.Context(), schedReq, 1)
	if err != nil {
		status := "failed"
		if errors.Is(err, context.Canceled) {
			status = "cancelled"
		}
		g.finishWorkload(r.Context(), wl, status, errString(err), nil, nil, nil, nil)
		if errors.Is(err, scheduler.ErrNoCapacity) {
			writeErr(w, http.StatusServiceUnavailable, "no_capacity", err.Error())
		} else if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			writeErr(w, 499, "client_closed", "client disconnected while queued")
		} else {
			g.log.Warn("schedule failed", "err", err)
			writeErr(w, http.StatusInternalServerError, "internal_error", "internal error")
		}
		return
	}

	attempts := g.sched.Config().MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	// Dispatch misses (node went offline/busy between pick and send)
	// never reached a node, so they don't count as execution attempts —
	// but they still get their own bound to avoid spinning.
	dispatchTries := attempts * 4

	var lastErr error
	for attempt := 1; attempt <= attempts && dispatchTries > 0; {
		if pick == nil {
			pick, err = g.sched.Schedule(r.Context(), schedReq, attempt)
			if err != nil {
				lastErr = err
				break
			}
		}
		execCtx, cancel := context.WithTimeout(r.Context(), g.RequestTimeout)
		chunks, derr := g.hub.Dispatch(execCtx, pick.Node.Node.ID, contracts.InferenceRequest{
			RequestID:   wl.ID,
			Model:       req.Model,
			Messages:    req.Messages,
			Stream:      true, // always stream internally so we can observe TTFT
			Temperature: req.Temperature,
			TopP:        req.TopP,
			MaxTokens:   req.MaxTokens,
			Stop:        req.Stop,
		})
		if derr != nil {
			cancel()
			lastErr = derr
			pick = nil
			dispatchTries--
			continue // safe to retry: nothing was dispatched
		}
		g.st.SetWorkloadRunning(r.Context(), wl.ID, pick.Node.Node.ID, pick.Decision)
		wl.Status = "running"
		wl.NodeID = &pick.Node.Node.ID
		wl.NodeName = pick.Node.Node.Name
		wl.Decision = pick.Decision
		g.ev.Publish("workload.started", wl)

		// streamOrCollect returns true when the stream died before any
		// token was delivered AND nothing reached the client — the only
		// situation where replay is safe.
		if retry := g.streamOrCollect(w, r, wl, req, chunks, cancel); !retry {
			return
		}
		cancel()
		lastErr = errFirstToken
		pick = nil
		attempt++
		g.log.Warn("dispatch failed before first token; retrying",
			"workload", wl.ID, "attempt", attempt)
	}

	status := "failed"
	if errors.Is(lastErr, context.Canceled) {
		status = "cancelled"
	}
	g.finishWorkload(r.Context(), wl, status, errString(lastErr), nil, nil, nil, nil)
	writeErr(w, http.StatusServiceUnavailable, "dispatch_failed",
		"could not dispatch the request to any node")
}

var errFirstToken = errors.New("stream failed before first token")

// streamOrCollect renders the node stream to the client. Streaming
// responses become SSE; buffered responses are assembled and returned as
// a single JSON body. Returns true only when the upstream failed before
// producing any tokens and nothing was written to the client — the sole
// condition under which the caller may retry.
func (g *Gateway) streamOrCollect(
	w http.ResponseWriter, r *http.Request,
	wl *store.Workload, req chatRequest,
	chunks <-chan contracts.ChatChunk,
	cancel context.CancelFunc,
) bool {
	defer cancel()
	ctx := r.Context()
	start := time.Now()
	var (
		gotFirst  bool
		ttft      int
		content   strings.Builder
		finish    = "stop"
		usage     *contracts.Usage
		streamErr error
	)

	writeSSEHeaders := func() {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
	}

	id := "chatcmpl-" + wl.ID[:24]
	created := time.Now().Unix()
	rc := http.NewResponseController(w)
	flush := func() { rc.Flush() }

	// headersSent tracks whether we've committed to a streaming response.
	headersSent := false
	sendChunk := func(delta string, finishReason *string, usage *contracts.Usage) {
		payload := map[string]any{
			"id": id, "object": "chat.completion.chunk",
			"created": created, "model": req.Model,
			"choices": []any{chunkChoice(delta, finishReason)},
		}
		if usage != nil {
			payload["usage"] = usage
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flush()
	}

	for chunk := range chunks {
		if ctx.Err() != nil {
			streamErr = ctx.Err()
			break
		}
		if chunk.Error != "" {
			streamErr = errors.New(chunk.Error)
			break
		}
		if !gotFirst {
			gotFirst = true
			ttft = int(time.Since(start).Milliseconds())
			if req.Stream {
				writeSSEHeaders()
				headersSent = true
				roleChunk := "assistant"
				sendRole(w, id, req.Model, created, roleChunk)
				flush()
			}
		}
		if chunk.Delta != "" {
			if req.Stream {
				sendChunk(chunk.Delta, nil, nil)
			} else {
				content.WriteString(chunk.Delta)
			}
		}
		if chunk.Finish != "" {
			finish = chunk.Finish
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}

	total := int(time.Since(start).Milliseconds())

	if streamErr != nil && !gotFirst {
		// Nothing reached the client — caller may safely retry.
		if headersSent {
			// Shouldn't happen (headers only sent after first token) but
			// guard anyway: the stream is committed, emit the error.
			et, em := clientErr(streamErr)
			sendStreamError(w, et, em)
			g.finishWorkload(r.Context(), wl, statusFor(streamErr), streamErr.Error(), nil, nil, nil, &total)
			return false
		}
		return true
	}

	if req.Stream {
		if headersSent {
			if streamErr != nil {
				et, em := clientErr(streamErr)
				sendStreamError(w, et, em)
			} else {
				sendChunk("", &finish, nil)
				if req.StreamOpts != nil && req.StreamOpts.IncludeUsage && usage != nil {
					sendUsageChunk(w, id, req.Model, created, usage)
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
			}
			flush()
		}
		status := "completed"
		errStr := ""
		if streamErr != nil {
			status, errStr = statusFor(streamErr), streamErr.Error()
		}
		g.finishWorkload(r.Context(), wl, status, errStr, tokens(usage, 0), tokens(usage, 1), &ttft, &total)
		return false
	}

	// Buffered response.
	if streamErr != nil {
		g.finishWorkload(r.Context(), wl, statusFor(streamErr), streamErr.Error(), nil, nil, &ttft, &total)
		et, em := clientErr(streamErr)
		writeErr(w, http.StatusBadGateway, et, em)
		return false
	}
	resp := map[string]any{
		"id": id, "object": "chat.completion",
		"created": created, "model": req.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]string{"role": "assistant", "content": content.String()},
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	g.finishWorkload(r.Context(), wl, "completed", "", tokens(usage, 0), tokens(usage, 1), &ttft, &total)
	writeJSON(w, http.StatusOK, resp)
	return false
}

func tokens(u *contracts.Usage, which int) *int {
	if u == nil {
		return nil
	}
	var v int
	if which == 0 {
		v = u.PromptTokens
	} else {
		v = u.CompletionTokens
	}
	return &v
}

func sendRole(w http.ResponseWriter, id, model string, created int64, role string) {
	payload := map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]string{"role": role},
			"finish_reason": nil,
		}},
	}
	b, _ := json.Marshal(payload)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func sendUsageChunk(w http.ResponseWriter, id, model string, created int64, u *contracts.Usage) {
	payload := map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []any{}, "usage": u,
	}
	b, _ := json.Marshal(payload)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

// clientErr classifies an upstream node/engine failure for the
// inference client. The verbatim node error is persisted on the
// workload row (the operator surface); clients get the failure class,
// not node internals (trust-review T-5).
func clientErr(err error) (typ, msg string) {
	if errors.Is(err, context.Canceled) {
		return "request_cancelled", "request cancelled"
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "not found") || strings.Contains(s, "no such model") ||
		strings.Contains(s, "does not exist") {
		return "model_unavailable", "the requested model is not available on any node"
	}
	return "upstream_error", "the upstream node failed to complete the request"
}

func sendStreamError(w http.ResponseWriter, typ, msg string) {
	payload := map[string]any{
		"error": map[string]string{
			"message": msg,
			"type":    typ,
		},
	}
	b, _ := json.Marshal(payload)
	fmt.Fprintf(w, "data: %s\n\n", b)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func chunkChoice(delta string, finishReason *string) map[string]any {
	c := map[string]any{
		"index": 0,
		"delta": map[string]any{},
	}
	if delta != "" {
		c["delta"].(map[string]any)["content"] = delta
	}
	c["finish_reason"] = finishReason
	return c
}

func promptChars(msgs []contracts.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
	}
	return n
}

func (g *Gateway) finishWorkload(ctx context.Context, wl *store.Workload, status, errStr string, pt, ct, ttft, total *int) {
	// Finalization must outlive client disconnect — the request ctx is
	// already dead at this point when the caller went away mid-stream.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := g.st.FinishWorkload(ctx, wl.ID, status, errStr, pt, ct, ttft, total); err != nil {
		g.log.Warn("finish workload", "err", err)
	}
	wl.Status = status
	wl.Error = errStr
	wl.TTFTms = ttft
	wl.TotalMS = total
	g.ev.Publish("workload.finished", wl)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, typ, msg string) {
	writeJSON(w, code, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    typ,
		},
	})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// statusFor maps a stream failure to a terminal workload status.
// Client cancellation is a normal outcome, not a failure.
func statusFor(err error) string {
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "failed"
}
