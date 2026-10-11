package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/agent"
	"github.com/localmesh/localmesh/internal/server"
	"github.com/localmesh/localmesh/internal/testutil"
	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
)

const adminKey = "lmk_testadmin_0123456789abcdef"

// testEnv is a running control plane + fake ollama + helpers.
type testEnv struct {
	base   string // http://127.0.0.1:PORT
	ollama *fakeollama.Server
	srv    *server.Server
	cancel context.CancelFunc
	t      *testing.T
}

func startServer(t *testing.T, ollama *fakeollama.Server) *testEnv {
	t.Helper()
	return startServerWithDSN(t, testutil.PostgresDSN(t), ollama)
}

func startServerWithDSN(t *testing.T, dsn string, ollama *fakeollama.Server) *testEnv {
	t.Helper()

	// Free port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	cfg := server.Config{
		Listen:          fmt.Sprintf("127.0.0.1:%d", port),
		DatabaseURL:     dsn,
		DataDir:         t.TempDir(),
		DevInsecureHTTP: true,
		AdminKey:        adminKey,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("LM_TEST_LOGS") != "" {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	srv := server.New(cfg, log)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("server did not shut down")
		}
	})

	e := &testEnv{
		base:   fmt.Sprintf("http://127.0.0.1:%d", port),
		ollama: ollama,
		srv:    srv,
		cancel: cancel,
		t:      t,
	}
	e.waitReady(t)
	return e
}

func (e *testEnv) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(e.base + "/healthz")
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("server did not start")
}

func (e *testEnv) adminReq(t *testing.T, method, path string, body any) (int, map[string]any, json.RawMessage) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	json.Unmarshal(raw, &m)
	return resp.StatusCode, m, raw
}

func (e *testEnv) adminList(t *testing.T, path string) []map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", e.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

// enrollAndApprove runs the full join handshake: the agent posts its
// enrollment, we find+approve it via the admin API, agent gets creds.
func enrollAndApprove(t *testing.T, e *testEnv, name, ollamaURL string) *agent.Config {
	t.Helper()
	// Create an enrollment token.
	code, tokBody, _ := e.adminReq(t, "POST", "/api/v1/enrollment-tokens",
		map[string]any{"label": "test", "max_uses": 5})
	if code != 201 {
		t.Fatalf("create enrollment token: %d", code)
	}
	token, _ := tokBody["token"].(string)
	if token == "" {
		t.Fatal("no token in response")
	}

	resCh := make(chan *agent.Config, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := agent.Enroll(context.Background(), agent.EnrollOptions{
			Server: e.base, Token: token, Name: name,
			PollEvery: 200 * time.Millisecond, Wait: 30 * time.Second,
		})
		if err != nil {
			errCh <- err
			return
		}
		res.Config.OllamaURL = ollamaURL
		resCh <- res.Config
	}()

	// Poll pending enrollments and approve.
	deadline := time.Now().Add(15 * time.Second)
	approved := false
	for time.Now().Before(deadline) && !approved {
		reqs := e.adminList(t, "/api/v1/enrollments?status=pending")
		for _, r := range reqs {
			code, _, _ := e.adminReq(t, "POST",
				"/api/v1/enrollments/"+r["id"].(string)+"/approve", nil)
			if code == 200 {
				approved = true
			}
		}
		if !approved {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if !approved {
		t.Fatal("no pending enrollment found to approve")
	}

	select {
	case cfg := <-resCh:
		return cfg
	case err := <-errCh:
		t.Fatalf("enroll: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("enroll timed out")
	}
	return nil
}

func (e *testEnv) waitNodeOnline(t *testing.T, want int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		nodes := e.adminList(t, "/api/v1/nodes")
		online := 0
		for _, n := range nodes {
			if n["online"] == true {
				online++
			}
		}
		if online >= want {
			return nodes
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("expected %d online node(s)", want)
	return nil
}

// nodeHasModel reports whether a /api/v1/nodes entry advertises a model.
func nodeHasModel(n map[string]any, name string) bool {
	models, _ := n["models"].([]any)
	for _, m := range models {
		if mm, _ := m.(map[string]any); mm["name"] == name {
			return true
		}
	}
	return false
}

// waitNodeModel polls until some online node advertises `model` and
// returns that node's view. This is the RIGHT readiness gate for tests
// that assert on reported inventory or schedule a model: "online" flips
// at WS registration (hub.register), while engines/models only enter
// the store when the agent's capability report is processed — under
// load that gap (engine probe → WS frame → UpdateNodeReport commit)
// stretches to seconds, so waiting on bare `online` and reading
// `models` races the first report.
func (e *testEnv) waitNodeModel(t *testing.T, model string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last []map[string]any
	for time.Now().Before(deadline) {
		last = e.adminList(t, "/api/v1/nodes")
		for _, n := range last {
			if n["online"] == true && nodeHasModel(n, model) {
				return n
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("no online node ever reported model %q; nodes=%v", model, last)
	return nil
}

// chat sends a /v1/chat/completions request (streaming or not) and
// returns the assembled assistant text + finish + usage.
func (e *testEnv) chat(t *testing.T, model, prompt string, stream bool) (string, int) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model": model, "stream": stream,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	})
	req, _ := http.NewRequest("POST", e.base+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+adminKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if !stream {
		var out struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			return "HTTP " + resp.Status + ": " + string(b), resp.StatusCode
		}
		json.Unmarshal(b, &out)
		if len(out.Choices) == 0 {
			t.Fatal("no choices in response")
		}
		return out.Choices[0].Message.Content, resp.StatusCode
	}

	// SSE stream.
	var text strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	done := false
	for sc.Scan() && !done {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			done = true
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) == nil && len(chunk.Choices) > 0 {
			text.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	return text.String(), resp.StatusCode
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestE2EJoinAndInference(t *testing.T) {
	ol := fakeollama.New(
		fakeollama.Model{Name: "llama3.1:8b", Family: "llama", Parameter: "8B", Size: 4 << 30},
	)
	defer ol.Close()

	e := startServer(t, ol)

	// Agent enrolls and connects.
	cfg := enrollAndApprove(t, e, "test-node-1", ol.URL())
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	agCtx, agCancel := context.WithCancel(context.Background())
	defer agCancel()
	go ag.Run(agCtx)

	// Gate on the capability report's model inventory, not bare
	// "online" — registration precedes the first report reaching the
	// node store, and under load the gap outlives a zero-wait poll.
	e.waitNodeModel(t, "llama3.1:8b")

	// /v1/models lists it.
	code, ml, _ := e.adminReq(t, "GET", "/v1/models", nil)
	if code != 200 {
		t.Fatalf("/v1/models: %d", code)
	}
	data, _ := ml["data"].([]any)
	if len(data) == 0 {
		t.Fatal("/v1/models empty")
	}

	// Non-streaming chat.
	text, code := e.chat(t, "llama3.1:8b", "hello cluster", false)
	if code != 200 {
		t.Fatalf("chat non-stream: %d %s", code, text)
	}
	if !strings.Contains(text, "localmesh-echo") {
		t.Fatalf("unexpected completion: %q", text)
	}

	// Streaming chat.
	text, code = e.chat(t, "llama3.1:8b", "stream me", true)
	if code != 200 {
		t.Fatalf("chat stream: %d", code)
	}
	if !strings.Contains(text, "localmesh-echo") {
		t.Fatalf("streamed content wrong: %q", text)
	}

	// Workload recorded with routing decision.
	wls := e.adminList(t, "/api/v1/workloads")
	if len(wls) < 2 {
		t.Fatalf("expected workloads, got %d", len(wls))
	}
	last := wls[0]
	if last["status"] != "completed" {
		t.Fatalf("workload status=%v", last["status"])
	}
	dec, _ := last["decision"].(map[string]any)
	if dec["chosen_node_id"] == "" && dec["chosen"] == "" {
		t.Fatal("workload missing scheduling decision")
	}
}

func TestE2EUnauthenticatedRejected(t *testing.T) {
	ol := fakeollama.New()
	defer ol.Close()
	e := startServer(t, ol)

	resp, err := http.Post(e.base+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestE2ENoNodeReturns503(t *testing.T) {
	ol := fakeollama.New(
		fakeollama.Model{Name: "m:1", Family: "f", Size: 1 << 30},
	)
	defer ol.Close()
	e := startServer(t, ol)

	// Shrink the queue timeout so the test is fast.
	// (settings API would also work; direct is fine here.)
	s := e.srv.Scheduler().Config()
	s.QueueTimeout = 300 * time.Millisecond
	e.srv.Scheduler().UpdateConfig(s)

	code, body, _ := e.adminReq(t, "POST", "/v1/chat/completions",
		map[string]any{"model": "m:1", "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	if code != 503 {
		t.Fatalf("expected 503 with no nodes, got %d: %v", code, body)
	}
}

func TestE2EModelPullAndDelete(t *testing.T) {
	ol := fakeollama.New(
		fakeollama.Model{Name: "m:1", Family: "f", Size: 1 << 30},
	)
	defer ol.Close()
	e := startServer(t, ol)

	cfg := enrollAndApprove(t, e, "pull-node", ol.URL())
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	e.waitNodeOnline(t, 1)

	// Pull a new model.
	code, res, _ := e.adminReq(t, "POST", "/api/v1/models/pull",
		map[string]any{"model": "newmodel:7b"})
	if code != 202 {
		t.Fatalf("pull: %d %v", code, res)
	}
	// Wait for the fake ollama to have it and node to re-report.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if ol.HasModel("newmodel:7b") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ol.HasModel("newmodel:7b") {
		t.Fatal("model never appeared in ollama")
	}

	// Cluster model list should include it after re-report.
	deadline = time.Now().Add(15 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		models := e.adminList(t, "/api/v1/models")
		for _, m := range models {
			if m["name"] == "newmodel:7b" {
				found = true
			}
		}
		if !found {
			time.Sleep(300 * time.Millisecond)
		}
	}
	if !found {
		t.Fatal("cluster models did not include pulled model")
	}
}
