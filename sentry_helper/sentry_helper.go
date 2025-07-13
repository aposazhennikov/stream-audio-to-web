package sentry_helper

import (
	"context"
	"log/slog"
	"time"

	"github.com/getsentry/sentry-go"
)

// SentryHelper provides safe and optional Sentry operations following best practices.
type SentryHelper struct {
	enabled bool
	logger  *slog.Logger
}

// NewSentryHelper creates a new SentryHelper instance.
func NewSentryHelper(enabled bool, logger *slog.Logger) *SentryHelper {
	return &SentryHelper{
		enabled: enabled,
		logger:  logger,
	}
}

// IsEnabled returns whether Sentry is enabled.
func (h *SentryHelper) IsEnabled() bool {
	return h.enabled
}

// CaptureException captures an exception with proper hub isolation.
func (h *SentryHelper) CaptureException(err error) {
	if !h.enabled || err == nil {
		return
	}

	// Clone hub to avoid data races in goroutines.
	hub := sentry.CurrentHub().Clone()
	hub.CaptureException(err)
}

// CaptureExceptionWithContext captures an exception with additional context.
func (h *SentryHelper) CaptureExceptionWithContext(err error, tags map[string]string, extra map[string]interface{}) {
	if !h.enabled || err == nil {
		return
	}

	// Clone hub to avoid data races in goroutines.
	hub := sentry.CurrentHub().Clone()
	hub.WithScope(func(scope *sentry.Scope) {
		// Add tags for error categorization.
		for key, value := range tags {
			scope.SetTag(key, value)
		}
		
		// Add extra context data.
		for key, value := range extra {
			scope.SetExtra(key, value)
		}
		
		hub.CaptureException(err)
	})
}

// CaptureMessage captures a message with proper hub isolation.
func (h *SentryHelper) CaptureMessage(msg string) {
	if !h.enabled || msg == "" {
		return
	}

	// Clone hub to avoid data races in goroutines.
	hub := sentry.CurrentHub().Clone()
	hub.CaptureMessage(msg)
}

// CaptureMessageWithContext captures a message with additional context.
func (h *SentryHelper) CaptureMessageWithContext(msg string, tags map[string]string, extra map[string]interface{}) {
	if !h.enabled || msg == "" {
		return
	}

	// Clone hub to avoid data races in goroutines.
	hub := sentry.CurrentHub().Clone()
	hub.WithScope(func(scope *sentry.Scope) {
		// Add tags for message categorization.
		for key, value := range tags {
			scope.SetTag(key, value)
		}
		
		// Add extra context data.
		for key, value := range extra {
			scope.SetExtra(key, value)
		}
		
		hub.CaptureMessage(msg)
	})
}

// AddBreadcrumb adds a breadcrumb to track the path to an error.
func (h *SentryHelper) AddBreadcrumb(category, message string, level sentry.Level, data map[string]interface{}) {
	if !h.enabled || message == "" {
		return
	}

	// Clone hub to avoid data races in goroutines.
	hub := sentry.CurrentHub().Clone()
	hub.AddBreadcrumb(&sentry.Breadcrumb{
		Category: category,
		Message:  message,
		Level:    level,
		Data:     data,
		Timestamp: time.Now(),
	}, nil)
}

// WithContext returns a new hub with context applied.
func (h *SentryHelper) WithContext(ctx context.Context) *sentry.Hub {
	if !h.enabled {
		return nil
	}

	hub := sentry.GetHubFromContext(ctx)
	if hub == nil {
		hub = sentry.CurrentHub().Clone()
	}
	return hub
}

// CaptureError captures an error with automatic context detection.
func (h *SentryHelper) CaptureError(err error, component string, operation string) {
	if !h.enabled || err == nil {
		return
	}

	tags := map[string]string{
		"component": component,
		"operation": operation,
	}
	
	h.CaptureExceptionWithContext(err, tags, nil)
}

// CaptureWarning captures a warning message with context.
func (h *SentryHelper) CaptureWarning(msg string, component string, operation string) {
	if !h.enabled || msg == "" {
		return
	}

	tags := map[string]string{
		"component": component,
		"operation": operation,
		"level":     "warning",
	}
	
	h.CaptureMessageWithContext(msg, tags, nil)
}

// CaptureInfo captures an info message with context.
func (h *SentryHelper) CaptureInfo(msg string, component string, operation string) {
	if !h.enabled || msg == "" {
		return
	}

	tags := map[string]string{
		"component": component,
		"operation": operation,
		"level":     "info",
	}
	
	h.CaptureMessageWithContext(msg, tags, nil)
}

// SafeFlush safely flushes Sentry events with timeout.
func (h *SentryHelper) SafeFlush(timeout time.Duration) {
	if !h.enabled {
		return
	}

	// Use regular flush with timeout.
	if !sentry.Flush(timeout) {
		h.logger.Warn("Sentry flush timeout", "timeout", timeout)
	}
} 