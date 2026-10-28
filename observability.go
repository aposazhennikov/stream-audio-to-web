package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	sentryhelper "github.com/aposazhennikov/stream-audio-to-web/sentry_helper"

	sentry "github.com/getsentry/sentry-go"
)

// setupLogger creates and configures a logger with the specified log level and sampling.
func setupLogger() *slog.Logger {
	var level slog.Level

	// Get log level from environment variable.
	logLevelEnv := strings.ToUpper(os.Getenv("LOG_LEVEL"))

	switch logLevelEnv {
	case "DEBUG":
		level = slog.LevelDebug
	case "INFO":
		level = slog.LevelInfo
	case "WARN", "WARNING":
		level = slog.LevelWarn
	case "ERROR":
		level = slog.LevelError
	default:
		// Default to WARNING to prevent excessive log spam.
		level = slog.LevelWarn
	}

	// Create JSON handler with specified log level.
	opts := &slog.HandlerOptions{
		Level: level,
	}
	handler := slog.NewJSONHandler(os.Stdout, opts)

	// Create and set logger.
	logger := slog.New(handler)
	slog.SetDefault(logger)

	return logger
}

// initSentry initializes Sentry for error tracking and returns SentryHelper.
func initSentry(logger *slog.Logger) *sentryhelper.SentryHelper {
	// Get DSN from environment.
	sentryDSN := strings.TrimSpace(os.Getenv("SENTRY_DSN"))
	if sentryDSN == "" {
		logger.Info("Sentry monitoring disabled (SENTRY_DSN not set or empty)")
		return sentryhelper.NewSentryHelper(false, logger)
	}

	logger.Info("Initializing Sentry with DSN", "dsn_length", len(sentryDSN))

	// Initialize Sentry with DSN from environment.
	err := initSentryWithDSN(logger, sentryDSN)
	if err == nil {
		logger.Info("Sentry initialization succeeded")
		return sentryhelper.NewSentryHelper(true, logger)
	}

	// If initialization failed, return disabled helper.
	return sentryhelper.NewSentryHelper(false, logger)
}

// initSentryWithDSN attempts to initialize Sentry with the given DSN.
// It handles common errors and tries alternative approaches if needed.
func initSentryWithDSN(logger *slog.Logger, sentryDSN string) error {
	// Enable Sentry debug mode only for debug log level.
	logLevel := strings.ToLower(getEnvOrDefault("LOG_LEVEL", "warning"))
	sentryDebug := logLevel == "debug"

	// First attempt with original DSN.
	err := sentry.Init(sentry.ClientOptions{
		Dsn:   sentryDSN,
		Debug: sentryDebug,
	})

	if err == nil {
		return nil
	}

	logger.Error("sentry.Init", "error", err)

	// If we have the specific "empty username" error, try with URL encoded @ symbol.
	if !strings.Contains(err.Error(), "empty username") {
		return err
	}

	// Try alternative solution.
	logger.Info("Attempting alternative Sentry initialization method")
	altDSN := strings.ReplaceAll(sentryDSN, "@", "%40")
	altErr := sentry.Init(sentry.ClientOptions{
		Dsn:   altDSN,
		Debug: sentryDebug,
	})

	if altErr != nil {
		logger.Error("Alternative sentry.Init also failed", "error", altErr)
		return altErr
	}

	logger.Info("Alternative Sentry initialization succeeded")
	return nil
}

// logConfiguration logs the application configuration.
func logConfiguration(logger *slog.Logger, config *Config) {
	// Use the passed logger respecting LOG_LEVEL setting.
	logger.Debug("========== APPLICATION CONFIGURATION ==========")
	logger.Debug("Port", slog.Int("value", config.Port))
	logger.Debug("Default audio directory", slog.String("value", config.AudioDir))

	logger.Debug("Bitrate", slog.Int("value", config.Bitrate))
	logger.Debug("Max clients", slog.Int("value", config.MaxClients))
	logger.Debug("Buffer size", slog.Int("value", config.BufferSize))
	logger.Debug("Global shuffle setting", slog.Bool("value", config.Shuffle))
	logger.Debug("Volume normalization", slog.Bool("value", config.NormalizeVolume))
	logger.Debug("Runtime normalization mode", slog.String("value", config.NormalizeRuntime))
	logger.Debug("Normalization sample windows", slog.Int("value", config.NormalizeSampleWindows))
	logger.Debug("Normalization sample duration",
		slog.String("value", fmt.Sprintf("%d ms", config.NormalizeSampleMs)))
	logger.Debug("Relay functionality enabled", slog.Bool("value", config.EnableRelay))
	logger.Debug("Relay configuration file", slog.String("value", config.RelayConfigFile))

	// Log per-stream shuffle settings.
	logger.Debug("Per-stream shuffle settings:")
	for route, shuffle := range config.PerStreamShuffle {
		logger.Debug("Per-stream shuffle", slog.String("route", route), slog.Bool("value", shuffle))
	}

	// Log additional directory routes.
	logger.Debug("Additional directory routes:")
	for path, route := range config.DirectoryRoutes {
		logger.Debug("Additional route", slog.String("path", path), slog.String("route", route))
	}

	logger.Debug("=============================================")
}
