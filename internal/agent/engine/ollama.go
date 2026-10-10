package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/localmesh/localmesh/internal/contracts"
)

// Ollama adapts a locally-running Ollama instance.
type Ollama struct {
	base string
	hc   *http.Client // streaming calls — ctx governs lifetime
	ctrl *http.Client // control calls — bounded
}

// Error classes so callers can distinguish "the model isn't there"
// (scheduling/retryable elsewhere) from "the engine is gone" (node-level
// failure) and generic engine errors.
var (
	ErrModelNotFound = errors.New("model not found")
	ErrEngineDown    = errors.New("engine unreachable")
)

func NewOllama(base string) *Ollama {
	if base == "" {
		base = "http://127.0.0.1:11434"
	}
	return &Ollama{
		base: strings.TrimRight(base, "/"),
		hc:   &http.Client{Timeout: 0},
		ctrl: &http.Client{Timeout: 15 * time.Second},
	}
}

// classify turns an Ollama error body into a typed error when possible.
func classify(status string, body string) error {
	msg := strings.TrimSpace(body)
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(body), &e) == nil && e.Error != "" {
		msg = e.Error
	}
	if strings.Contains(strings.ToLower(msg), "not found") {
		return fmt.Errorf("%w: %s", ErrModelNotFound, msg)
	}
	return fmt.Errorf("ollama: %s: %s", status, msg)
}

// netErr wraps transport failures so callers can detect engine-down.
// Context cancellation/deadline pass through unwrapped so callers can
// still distinguish an abort from an engine failure.
func netErr(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrEngineDown, err)
}

func (o *Ollama) Type() string { return "ollama" }

func (o *Ollama) Detect(ctx context.Context) (*contracts.EngineInfo, error) {
	info := &contracts.EngineInfo{Type: "ollama", Address: o.base}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, o.base+"/api/version", nil)
	resp, err := o.ctrl.Do(req)
	if err != nil {
		return nil, netErr(err)
	}
	defer resp.Body.Close()
	var v struct {
		Version string `json:"version"`
	}
	json.NewDecoder(resp.Body).Decode(&v)
	info.Version = v.Version
	info.Healthy = resp.StatusCode == http.StatusOK
	return info, nil
}

type ollamaTagModel struct {
	Name         string   `json:"name"`
	Model        string   `json:"model"`
	Size         uint64   `json:"size"`
	Digest       string   `json:"digest"`
	ModifiedAt   string   `json:"modified_at"`
	Capabilities []string `json:"capabilities"`
	Details      struct {
		Family          string `json:"family"`
		ParameterSize   string `json:"parameter_size"`
		QuantizationLvl string `json:"quantization_level"`
	} `json:"details"`
}

func (o *Ollama) Models(ctx context.Context) ([]contracts.Model, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, o.base+"/api/tags", nil)
	resp, err := o.ctrl.Do(req)
	if err != nil {
		return nil, netErr(err)
	}
	defer resp.Body.Close()
	var tags struct {
		Models []ollamaTagModel `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, err
	}
	loaded := o.loadedSet(ctx)
	out := make([]contracts.Model, 0, len(tags.Models))
	for _, t := range tags.Models {
		name := t.Name
		if name == "" {
			name = t.Model
		}
		var mod *time.Time
		if ts, err := time.Parse(time.RFC3339Nano, t.ModifiedAt); err == nil {
			mod = &ts
		}
		m := contracts.Model{
			Name:         name,
			Engine:       "ollama",
			Family:       t.Details.Family,
			ParameterSz:  t.Details.ParameterSize,
			Quantization: t.Details.QuantizationLvl,
			SizeBytes:    t.Size,
			Digest:       t.Digest,
			Capabilities: t.Capabilities,
			ModifiedAt:   mod,
		}
		if pm, ok := loaded[name]; ok {
			m.Loaded = true
			m.LoadedBytes = pm.Size
			m.LoadedVRAM = pm.SizeVRAM
			m.Runner = pm.Runner
		}
		out = append(out, m)
	}
	return out, nil
}

// ollamaPSModel is one row of /api/ps: a model currently resident in
// memory. `size` is the TOTAL resident footprint (weights + KV cache +
// runtime) — larger than the file size in /api/tags. `size_vram` is the
// portion in GPU VRAM (0 on CPU-only hosts or unified-memory runners).
type ollamaPSModel struct {
	Name     string `json:"name"`
	Model    string `json:"model"`
	Size     uint64 `json:"size"`
	SizeVRAM uint64 `json:"size_vram"`
	Runner   string `json:"runner"`
}

// loadedSet maps model name -> ps row for every model Ollama reports as
// resident. A failing /api/ps degrades to an empty map (nothing loaded);
// the tags listing still succeeds.
func (o *Ollama) loadedSet(ctx context.Context) map[string]ollamaPSModel {
	loaded := map[string]ollamaPSModel{}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, o.base+"/api/ps", nil)
	resp, err := o.ctrl.Do(req)
	if err != nil {
		return loaded
	}
	defer resp.Body.Close()
	var ps struct {
		Models []ollamaPSModel `json:"models"`
	}
	if json.NewDecoder(resp.Body).Decode(&ps) == nil {
		for _, m := range ps.Models {
			if m.Name != "" {
				loaded[m.Name] = m
			}
			if m.Model != "" {
				loaded[m.Model] = m
			}
		}
	}
	return loaded
}

// ---- chat ----

type ollamaChatReq struct {
	Model    string              `json:"model"`
	Messages []contracts.Message `json:"messages"`
	Stream   bool                `json:"stream"`
	Options  map[string]any      `json:"options,omitempty"`
}

type ollamaChatResp struct {
	Model     string `json:"model"`
	CreatedAt string `json:"created_at"`
	Message   struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
}

func (o *Ollama) Chat(ctx context.Context, req contracts.InferenceRequest, emit func(contracts.ChatChunk)) error {
	opts := map[string]any{}
	if req.Temperature != nil {
		opts["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		opts["top_p"] = *req.TopP
	}
	if req.MaxTokens != nil {
		opts["num_predict"] = *req.MaxTokens
	}
	if len(req.Stop) > 0 {
		opts["stop"] = req.Stop
	}
	body, _ := json.Marshal(ollamaChatReq{
		Model: req.Model, Messages: req.Messages, Stream: true, Options: opts,
	})
	hreq, _ := http.NewRequestWithContext(ctx, http.MethodPost, o.base+"/api/chat", bytes.NewReader(body))
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := o.hc.Do(hreq)
	if err != nil {
		return netErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return classify(resp.Status, string(b))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m ollamaChatResp
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		chunk := contracts.ChatChunk{
			Delta: m.Message.Content,
			Done:  m.Done,
			Model: m.Model,
		}
		if m.Done {
			chunk.Finish = m.DoneReason
			if chunk.Finish == "" {
				chunk.Finish = "stop"
			}
			if m.PromptEvalCount > 0 || m.EvalCount > 0 {
				chunk.Usage = &contracts.Usage{
					PromptTokens:     m.PromptEvalCount,
					CompletionTokens: m.EvalCount,
					TotalTokens:      m.PromptEvalCount + m.EvalCount,
				}
			}
		}
		emit(chunk)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return ctx.Err()
}

// ---- pull / delete ----

type ollamaPullResp struct {
	Status    string `json:"status"`
	Completed uint64 `json:"completed"`
	Total     uint64 `json:"total"`
	Error     string `json:"error,omitempty"`
}

func (o *Ollama) Pull(ctx context.Context, model string, prog func(status string, completed, total uint64)) error {
	body, _ := json.Marshal(map[string]any{"model": model, "stream": true})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, o.base+"/api/pull", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.hc.Do(req)
	if err != nil {
		return netErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return classify(resp.Status, string(b))
	}
	var lastErr string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var p ollamaPullResp
		if err := json.Unmarshal(sc.Bytes(), &p); err != nil {
			continue
		}
		if p.Error != "" {
			lastErr = p.Error
		}
		if prog != nil {
			prog(p.Status, p.Completed, p.Total)
		}
	}
	if lastErr != "" {
		return errors.New(lastErr)
	}
	return sc.Err()
}

func (o *Ollama) Delete(ctx context.Context, model string) error {
	body, _ := json.Marshal(map[string]string{"model": model})
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, o.base+"/api/delete", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.ctrl.Do(req)
	if err != nil {
		return netErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return classify(resp.Status, string(b))
	}
	return nil
}
