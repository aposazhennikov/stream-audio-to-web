// Package slog provides logging functionality
package slog

import "log/slog"

// Default returns the default logger
func Default() *slog.Logger {
    return slog.Default()
}
