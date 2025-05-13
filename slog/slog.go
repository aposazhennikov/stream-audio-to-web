// Package slog provides a simple wrapper around log/slog.
package slog

import "log/slog"

// Default returns the default logger.
func Default() *slog.Logger {
	return slog.Default()
}
