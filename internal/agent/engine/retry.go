package engine

import (
	"context"
	"errors"
	"io"
)

// Retryable reports whether an engine failure is transient and worth a
// scheduled retry: dial/transport errors (ErrEngineDown), HTTP 5xx,
// mid-stream EOF, and deadlines. Permanent failures — model not found,
// invalid requests, other 4xx, cancellation — are not retried.
//
// Unknown error shapes are NOT retryable: classifying an unrecognized
// failure as transient would loop a pull that can never succeed.
func Retryable(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, context.Canceled):
		return false // an abort is deliberate, never a transient failure
	case errors.Is(err, context.DeadlineExceeded):
		return true
	case errors.Is(err, ErrModelNotFound):
		return false
	case errors.Is(err, ErrEngineDown):
		return true
	case errors.Is(err, io.ErrUnexpectedEOF):
		return true
	}
	var pe *PermanentError
	if errors.As(err, &pe) {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Code >= 500 || he.Code == 0
	}
	return false
}
