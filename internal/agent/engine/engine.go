// Package engine defines the pluggable inference-engine adapter and
// ships the Ollama implementation. Additional runtimes (vLLM, llama.cpp,
// MLX) can be added by implementing Engine against their local API.
package engine

import (
	"context"

	"github.com/localmesh/localmesh/internal/contracts"
)

// Engine abstracts one local inference runtime.
type Engine interface {
	// Type returns the engine identifier, e.g. "ollama".
	Type() string
	// Detect probes the engine; returns info + nil if reachable.
	Detect(ctx context.Context) (*contracts.EngineInfo, error)
	// Models lists installed models (with Loaded set from /ps-equivalent).
	Models(ctx context.Context) ([]contracts.Model, error)
	// Chat streams a completion; emit is invoked per normalized chunk.
	Chat(ctx context.Context, req contracts.InferenceRequest, emit func(contracts.ChatChunk)) error
	// Pull downloads a model, reporting progress via prog.
	Pull(ctx context.Context, model string, prog func(status string, completed, total uint64)) error
	// Delete removes a model.
	Delete(ctx context.Context, model string) error
}
