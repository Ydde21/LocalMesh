package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
)

func TestRetryableClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"cancelled", context.Canceled, false},
		{"abort wrapped", fmt.Errorf("op aborted: %w", context.Canceled), false},
		{"deadline", context.DeadlineExceeded, true},
		{"dial/net error", netErr(&net.DNSError{IsTimeout: true}), true},
		{"engine down", ErrEngineDown, true},
		{"mid-stream EOF", io.ErrUnexpectedEOF, true},
		{"model not found", fmt.Errorf("%w: no such model", ErrModelNotFound), false},
		{"http 500", &HTTPError{Code: 500, Status: "500 Internal Server Error", Message: "boom"}, true},
		{"http 503", &HTTPError{Code: 503, Status: "503"}, true},
		{"http 400", &HTTPError{Code: 400, Status: "400", Message: "bad request"}, false},
		{"http 404", &HTTPError{Code: 404, Status: "404"}, false},
		{"permanent stream err", &PermanentError{Message: "invalid model"}, false},
		{"unknown error", errors.New("mystery"), false}, // never guess — don't loop forever
	}
	for _, c := range cases {
		if got := Retryable(c.err); got != c.want {
			t.Errorf("%s: Retryable = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPullStreamErrClassification(t *testing.T) {
	transient := []string{
		"connection reset by peer", "read tcp: eof", "request timed out",
		"temporarily unavailable",
	}
	permanent := []string{
		"pull model manifest: file does not exist",
		"invalid model name",
		"unauthorized",
		"no space left on device",
		"something completely unrecognized",
	}
	for _, m := range transient {
		if err := pullStreamErr(m); !Retryable(err) {
			t.Errorf("stream error %q classified permanent, want retryable", m)
		}
	}
	for _, m := range permanent {
		if err := pullStreamErr(m); Retryable(err) {
			t.Errorf("stream error %q classified retryable, want permanent", m)
		}
	}
}
