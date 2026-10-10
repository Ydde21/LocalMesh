// Package fakeollama is a lightweight Ollama API stub for tests and
// local development. It implements /api/version, /api/tags, /api/ps,
// /api/chat (streaming), /api/pull, and /api/delete against an
// in-memory model set. It is only wired into tests — never shipped
// behavior presented as a real engine.
package fakeollama

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Model struct {
	Name      string
	Family    string
	Parameter string
	Size      uint64
	Loaded    bool
	// Resident-footprint fields reported via /api/ps while Loaded.
	// LoadedSize defaults to Size and Runner to "llamacpp" when zero.
	LoadedSize uint64
	VRAMSize   uint64
	Runner     string
}

// Server is a fake Ollama HTTP server.
type Server struct {
	srv *http.Server
	ln  net.Listener

	mu     sync.Mutex
	models map[string]*Model

	// ChatDelay simulates per-token latency.
	ChatDelay time.Duration
	// PullStepDelay stretches each pull-progress step (default 5ms) —
	// set higher to make pulls long enough to cancel mid-flight.
	PullStepDelay time.Duration
	// ChatErr, when non-empty, fails chat with this message.
	ChatErr atomic.Value
	// FailChatBeforeTokens, if >0, closes chat streams after N tokens.
	FailAfterTokens atomic.Int64

	// ChatCalls counts received chat requests (for tests).
	ChatCalls atomic.Int64
	// CurChat / MaxChat track observed /api/chat concurrency so tests can
	// prove the control plane never oversubscribes a node.
	CurChat atomic.Int64
	MaxChat atomic.Int64

	// PullCalls counts received /api/pull requests.
	PullCalls atomic.Int64
	// PullFailures fails the next N pull requests using PullFailMode:
	// "500" → HTTP 500 before streaming (transient), "notfound" → 404
	// model-not-found (permanent), "drop" → connection closed mid-stream
	// (transient).
	PullFailures atomic.Int64
	PullFailMode atomic.Value // string; default "500"
	// PullNoTotal emits download progress with total=0 — exercises the
	// indeterminate-progress path.
	PullNoTotal atomic.Bool
	// PullSize is the reported download total (default 400 MiB).
	PullSize uint64
}

// New starts a fake Ollama on an ephemeral loopback port. models seeds
// the catalog.
func New(models ...Model) *Server {
	s := newServer(models)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	s.ln = ln
	s.srv = &http.Server{Handler: s.mux()}
	go s.srv.Serve(ln)
	return s
}

// ServeOn runs a fake Ollama on an existing listener (dev tool use).
func ServeOn(ln net.Listener, models ...Model) *Server {
	s := newServer(models)
	s.ln = ln
	s.srv = &http.Server{Handler: s.mux()}
	go s.srv.Serve(ln)
	return s
}

func newServer(models []Model) *Server {
	s := &Server{models: map[string]*Model{}, ChatDelay: 2 * time.Millisecond}
	for _, m := range models {
		mm := m
		s.models[m.Name] = &mm
	}
	s.ChatErr.Store("")
	s.PullFailMode.Store("500")
	return s
}

func (s *Server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/version", s.version)
	mux.HandleFunc("GET /api/tags", s.tags)
	mux.HandleFunc("GET /api/ps", s.ps)
	mux.HandleFunc("POST /api/chat", s.chat)
	mux.HandleFunc("POST /api/pull", s.pull)
	mux.HandleFunc("DELETE /api/delete", s.del)
	return mux
}

func (s *Server) URL() string { return "http://" + s.ln.Addr().String() }

func (s *Server) Close() { s.srv.Close(); s.ln.Close() }

func (s *Server) AddModel(m Model) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mm := m
	s.models[m.Name] = &mm
}

func (s *Server) HasModel(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.models[name]
	return ok
}

func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"version": "0.9.9-fake"})
}

func (s *Server) tags(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []map[string]any{}
	for _, m := range s.models {
		out = append(out, map[string]any{
			"name": m.Name, "model": m.Name, "size": m.Size,
			"digest": "sha256:fake-" + m.Name,
			"details": map[string]string{
				"family": m.Family, "parameter_size": m.Parameter,
				"quantization_level": "Q4_0",
			},
		})
	}
	json.NewEncoder(w).Encode(map[string]any{"models": out})
}

func (s *Server) ps(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []map[string]any{}
	for _, m := range s.models {
		if !m.Loaded {
			continue
		}
		size := m.LoadedSize
		if size == 0 {
			size = m.Size
		}
		runner := m.Runner
		if runner == "" {
			runner = "llamacpp"
		}
		out = append(out, map[string]any{
			"name": m.Name, "model": m.Name,
			"size": size, "size_vram": m.VRAMSize,
			"runner": runner, "context_length": 4096,
			"expires_at": time.Now().Add(5 * time.Minute).Format(time.RFC3339Nano),
		})
	}
	json.NewEncoder(w).Encode(map[string]any{"models": out})
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	s.ChatCalls.Add(1)
	cur := s.CurChat.Add(1)
	for {
		m := s.MaxChat.Load()
		if cur <= m || s.MaxChat.CompareAndSwap(m, cur) {
			break
		}
	}
	defer s.CurChat.Add(-1)
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Stream bool `json:"stream"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	s.mu.Lock()
	_, ok := s.models[req.Model]
	if ok {
		s.models[req.Model].Loaded = true
	}
	s.mu.Unlock()
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "model not found"})
		return
	}
	if e, _ := s.ChatErr.Load().(string); e != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": e})
		return
	}

	// Deterministic reply: tokenized echo of the last user message.
	prompt := ""
	for _, m := range req.Messages {
		if m.Role == "user" {
			prompt = m.Content
		}
	}
	reply := "localmesh-echo: " + prompt
	tokens := strings.Fields(reply)
	limit := int(s.FailAfterTokens.Load())
	if limit > 0 && len(tokens) > limit {
		tokens = tokens[:limit]
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)

	send := func(v map[string]any) {
		b, _ := json.Marshal(v)
		w.Write(b)
		w.Write([]byte("\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	for _, tok := range tokens {
		send(map[string]any{
			"model":      req.Model,
			"message":    map[string]string{"role": "assistant", "content": tok + " "},
			"done":       false,
			"created_at": time.Now().Format(time.RFC3339Nano),
		})
		if s.ChatDelay > 0 {
			time.Sleep(s.ChatDelay)
		}
	}
	if limit > 0 {
		// Simulate a dropped upstream: close connection mid-stream.
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
	}
	promptTokens := len(strings.Fields(prompt))
	send(map[string]any{
		"model":             req.Model,
		"message":           map[string]string{"role": "assistant", "content": ""},
		"done":              true,
		"done_reason":       "stop",
		"prompt_eval_count": promptTokens,
		"eval_count":        len(tokens),
		"created_at":        time.Now().Format(time.RFC3339Nano),
	})
}

func (s *Server) pull(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	s.PullCalls.Add(1)

	if s.PullFailures.Load() > 0 {
		s.PullFailures.Add(-1)
		mode, _ := s.PullFailMode.Load().(string)
		switch mode {
		case "notfound":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "model not found"})
		case "drop":
			// Stream one chunk then kill the connection — a mid-pull
			// transport failure, like an engine restart.
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			b, _ := json.Marshal(map[string]any{"status": "downloading", "completed": 1, "total": 100})
			w.Write(b)
			w.Write([]byte("\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, _ := hj.Hijack()
				conn.Close()
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "simulated engine failure"})
		}
		return
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)

	total := s.PullSize
	if total == 0 {
		total = 400 << 20
	}
	reported := total
	if s.PullNoTotal.Load() {
		reported = 0
	}
	// Realistic Ollama pull sequence: multi-chunk download with byte
	// counters, status-only phases without counters (indeterminate),
	// and a final "success" carrying the full totals.
	steps := []map[string]any{
		{"status": "pulling manifest"},
		{"status": "downloading", "completed": reported / 4, "total": reported},
		{"status": "downloading", "completed": reported / 2, "total": reported},
		{"status": "downloading", "completed": reported * 3 / 4, "total": reported},
		{"status": "downloading", "completed": reported, "total": reported},
		{"status": "verifying sha256 digest"},
		{"status": "writing manifest"},
		{"status": "removing any unused layers"},
		{"status": "success", "completed": reported, "total": reported},
	}
	delay := s.PullStepDelay
	if delay <= 0 {
		delay = 5 * time.Millisecond
	}
	for _, st := range steps {
		b, _ := json.Marshal(st)
		w.Write(b)
		w.Write([]byte("\n"))
		if flusher != nil {
			flusher.Flush()
		}
		// Honour request cancellation — real Ollama aborts a pull when
		// the client connection drops.
		select {
		case <-r.Context().Done():
			return
		case <-time.After(delay):
		}
	}
	s.mu.Lock()
	s.models[req.Model] = &Model{Name: req.Model, Family: "pulled", Size: total}
	s.mu.Unlock()
}

func (s *Server) del(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	delete(s.models, req.Model)
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{}`)
}
