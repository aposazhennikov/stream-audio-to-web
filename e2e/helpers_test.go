package e2e_test

import (
	"log/slog"
	"os"
	"testing"

	sentryhelper "github.com/aposazhennikov/stream-audio-to-web/sentry_helper"
)

// liveServerURL returns the base URL of a running server taken from TEST_SERVER_URL.
// Tests that talk to a live server call it first. When the variable is empty the
// test is skipped, so the suite passes without a server.
func liveServerURL(t *testing.T) string {
	t.Helper()

	baseURL := os.Getenv("TEST_SERVER_URL")
	if baseURL == "" {
		t.Skip("TEST_SERVER_URL is not set, skipping test that needs a live server")
	}

	return baseURL
}

// getEnvOrDefault returns the value of the environment variable or the default value.
func getEnvOrDefault(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

// newTestSentryHelper returns a disabled Sentry helper for in-process tests.
func newTestSentryHelper() *sentryhelper.SentryHelper {
	return sentryhelper.NewSentryHelper(false, slog.Default())
}
