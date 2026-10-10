package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/contracts"
)

// ollamaStub serves the subset of the Ollama HTTP API the adapter uses.
type ollamaStub struct {
	*httptest.Server
	chatFn func(w http.ResponseWriter, r *http.Request)
	pullFn func(w http.ResponseWriter, r *http.Request)
	psFn   func(w http.ResponseWriter, r *http.Request)
}

func newStub(t *testing.T) *ollamaStub {
	t.Helper()
	s := &ollamaStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"version": "0.40.2"})
	})
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{
			{
				"name": "qwen2.5:0.5b", "model": "qwen2.5:0.5b",
				"size": 397000000, "digest": "abc123",
				"modified_at":  "2026-01-01T00:00:00Z",
				"capabilities": []string{"completion"},
				"details": map[string]string{
					"family": "qwen2", "parameter_size": "0.5B",
					"quantization_level": "Q4_K_M",
				},
			},
		}})
	})
	mux.HandleFunc("GET /api/ps", func(w http.ResponseWriter, r *http.Request) {
		if s.psFn != nil {
			s.psFn(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{
			{"name": "qwen2.5:0.5b", "model": "qwen2.5:0.5b",
				"size": 628000000, "size_vram": 400000000,
				"runner": "llamacpp", "context_length": 4096},
		}})
	})
	mux.HandleFunc("POST /api/chat", func(w http.ResponseWriter, r *http.Request) {
		if s.chatFn != nil {
			s.chatFn(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"model":"qwen2.5:0.5b","message":{"role":"assistant","content":"Hi"},"done":false}`)
		fmt.Fprintln(w, `{"model":"qwen2.5:0.5b","message":{"role":"assistant","content":"."},"done":true,"done_reason":"stop","prompt_eval_count":10,"eval_count":2}`)
	})
	mux.HandleFunc("POST /api/pull", func(w http.ResponseWriter, r *http.Request) {
		if s.pullFn != nil {
			s.pullFn(w, r)
			return
		}
		fmt.Fprintln(w, `{"status":"pulling manifest","completed":0,"total":0}`)
		fmt.Fprintln(w, `{"status":"success","completed":100,"total":100}`)
	})
	mux.HandleFunc("DELETE /api/delete", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestDetect(t *testing.T) {
	s := newStub(t)
	info, err := NewOllama(s.URL).Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "0.40.2" || !info.Healthy {
		t.Fatalf("info = %+v", info)
	}
}

func TestDetectEngineDown(t *testing.T) {
	_, err := NewOllama("http://127.0.0.1:1").Detect(context.Background())
	if !errors.Is(err, ErrEngineDown) {
		t.Fatalf("err = %v, want ErrEngineDown", err)
	}
}

func TestModelsLoadedAndMetadata(t *testing.T) {
	s := newStub(t)
	models, err := NewOllama(s.URL).Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 {
		t.Fatalf("got %d models", len(models))
	}
	m := models[0]
	if !m.Loaded {
		t.Fatal("model should be marked loaded via /api/ps")
	}
	if m.LoadedBytes != 628000000 || m.LoadedVRAM != 400000000 {
		t.Fatalf("loaded footprint mismatch: %+v", m)
	}
	if m.Runner != "llamacpp" {
		t.Fatalf("runner mismatch: %+v", m)
	}
	if m.Family != "qwen2" || m.ParameterSz != "0.5B" || m.Quantization != "Q4_K_M" {
		t.Fatalf("metadata mismatch: %+v", m)
	}
	if m.Digest != "abc123" || len(m.Capabilities) != 1 {
		t.Fatalf("digest/capabilities missing: %+v", m)
	}
}

func TestModelsZeroVRAM(t *testing.T) {
	s := newStub(t)
	s.psFn = func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{
			{"name": "qwen2.5:0.5b", "size": 628000000, "size_vram": 0,
				"runner": "llamacpp"},
		}})
	}
	models, err := NewOllama(s.URL).Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := models[0]
	if !m.Loaded || m.LoadedBytes != 628000000 {
		t.Fatalf("loaded state mismatch: %+v", m)
	}
	if m.LoadedVRAM != 0 {
		t.Fatalf("size_vram=0 must map to LoadedVRAM=0, got %d", m.LoadedVRAM)
	}
}

// A failing /api/ps must not break the model listing — models simply
// report not-loaded.
func TestModelsPSDown(t *testing.T) {
	s := newStub(t)
	s.psFn = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"boom"}`)
	}
	models, err := NewOllama(s.URL).Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 {
		t.Fatalf("got %d models", len(models))
	}
	if models[0].Loaded || models[0].LoadedBytes != 0 || models[0].Runner != "" {
		t.Fatalf("ps failure must degrade to not-loaded: %+v", models[0])
	}
}

func TestChatStreamsAndUsage(t *testing.T) {
	s := newStub(t)
	var chunks []contracts.ChatChunk
	err := NewOllama(s.URL).Chat(context.Background(),
		contracts.InferenceRequest{Model: "qwen2.5:0.5b",
			Messages: []contracts.Message{{Role: "user", Content: "hi"}}},
		func(c contracts.ChatChunk) { chunks = append(chunks, c) })
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks", len(chunks))
	}
	if chunks[0].Delta != "Hi" || chunks[0].Done {
		t.Fatalf("chunk0 = %+v", chunks[0])
	}
	last := chunks[1]
	if !last.Done || last.Finish != "stop" {
		t.Fatalf("final chunk = %+v", last)
	}
	if last.Usage == nil || last.Usage.PromptTokens != 10 || last.Usage.CompletionTokens != 2 || last.Usage.TotalTokens != 12 {
		t.Fatalf("usage = %+v", last.Usage)
	}
}

func TestChatModelNotFound(t *testing.T) {
	s := newStub(t)
	s.chatFn = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"model 'nope' not found"}`)
	}
	err := NewOllama(s.URL).Chat(context.Background(),
		contracts.InferenceRequest{Model: "nope", Messages: []contracts.Message{{Role: "user", Content: "x"}}},
		func(contracts.ChatChunk) {})
	if !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("err = %v, want ErrModelNotFound", err)
	}
}

func TestChatCancelPropagates(t *testing.T) {
	s := newStub(t)
	s.chatFn = func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":"tok"},"done":false}`)
		fl.Flush()
		<-r.Context().Done() // hold stream open until client aborts
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- NewOllama(s.URL).Chat(ctx,
			contracts.InferenceRequest{Model: "m", Messages: []contracts.Message{{Role: "user", Content: "x"}}},
			func(contracts.ChatChunk) {})
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if errors.Is(err, ErrEngineDown) {
			t.Fatal("cancel must not be classified as engine-down")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Chat did not return after cancel")
	}
}

func TestPullProgress(t *testing.T) {
	s := newStub(t)
	var events []string
	err := NewOllama(s.URL).Pull(context.Background(), "new:latest",
		func(st string, done, total uint64) {
			events = append(events, fmt.Sprintf("%s %d/%d", st, done, total))
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || !strings.Contains(events[1], "success") {
		t.Fatalf("events = %v", events)
	}
}

func TestPullCancel(t *testing.T) {
	s := newStub(t)
	s.pullFn = func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		fmt.Fprintln(w, `{"status":"downloading","completed":10,"total":100}`)
		fl.Flush()
		<-r.Context().Done()
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- NewOllama(s.URL).Pull(ctx, "big:latest", nil) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected err %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Pull did not return after cancel")
	}
}

func TestPullStreamError(t *testing.T) {
	s := newStub(t)
	s.pullFn = func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"status":"downloading","completed":5,"total":100}`)
		fmt.Fprintln(w, `{"error":"digest mismatch"}`)
	}
	err := NewOllama(s.URL).Pull(context.Background(), "x:latest", nil)
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("err = %v", err)
	}
}

func TestDelete(t *testing.T) {
	s := newStub(t)
	if err := NewOllama(s.URL).Delete(context.Background(), "m:latest"); err != nil {
		t.Fatal(err)
	}
}
