package unit_test

import (
	"log/slog"

	sentryhelper "github.com/aposazhennikov/stream-audio-to-web/sentry_helper"
)

// Helper function to create SentryHelper for tests.
func createTestSentryHelper() *sentryhelper.SentryHelper {
	return sentryhelper.NewSentryHelper(false, slog.Default())
} 