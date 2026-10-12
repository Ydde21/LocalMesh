package gateway

import (
	"context"
	"errors"
	"testing"
)

// clientErr maps internal failures to client-facing error classes. The
// verbatim node error stays on the workload row (operator surface) —
// the client gets a stable, actionable class + message instead.
func TestClientErrClassification(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantType string
	}{
		{"cancelled", context.Canceled, "request_cancelled"},
		{"model missing", errors.New("model 'x' not found"), "model_unavailable"},
		{"model missing alt", errors.New("no such model"), "model_unavailable"},
		{"node dropped mid-stream", errors.New("node disconnected"), "node_unavailable"},
		{"engine down", errors.New("engine unreachable: connection refused"), "node_unavailable"},
		{"engine missing", errors.New("no ollama engine detected"), "node_unavailable"},
		{"generic upstream", errors.New("unexpected tensor state"), "upstream_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			typ, msg := clientErr(tc.err)
			if typ != tc.wantType {
				t.Fatalf("clientErr(%v) type=%q, want %q", tc.err, typ, tc.wantType)
			}
			if msg == "" {
				t.Fatal("clientErr returned an empty message")
			}
		})
	}
}
