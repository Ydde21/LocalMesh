//go:build !darwin && !linux && !windows

package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"runtime"
)

func serviceAction(string, string, io.Writer) error {
	return fmt.Errorf("service management is not supported on %s", runtime.GOOS)
}

func runUnderService(context.Context, *Config, *slog.Logger, string) (bool, error) {
	return false, nil
}
