// Package main is the entry point for the audio streaming server.
// It handles initialization, configuration, and startup of all components.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aposazhennikov/stream-audio-to-web/audio"
	httpServer "github.com/aposazhennikov/stream-audio-to-web/http"
	"github.com/aposazhennikov/stream-audio-to-web/radio"
	"github.com/aposazhennikov/stream-audio-to-web/relay"
	sentryhelper "github.com/aposazhennikov/stream-audio-to-web/sentry_helper"
	"github.com/aposazhennikov/stream-audio-to-web/telegram"
	"github.com/aposazhennikov/stream-audio-to-web/version"

	sentry "github.com/getsentry/sentry-go"
	"github.com/gorilla/mux"
)

// Global Sentry helper for use in functions without config access.
var globalSentryHelper *sentryhelper.SentryHelper

func main() {
	// Настраиваем recovery от panic
	defer func() {
		if r := recover(); r != nil {
			// Логируем panic в Sentry
			if globalSentryHelper != nil {
				globalSentryHelper.CaptureError(fmt.Errorf("PANIC: %v", r), "main", "panic")
			}
			sentry.Flush(time.Second * 5)
			panic(r) // Re-panic после логирования
		}
	}()

	// Настраиваем логгер перед началом работы
	logger := setupLogger()
	logLevelEnv := strings.ToUpper(os.Getenv("LOG_LEVEL"))
	if logLevelEnv == "" {
		logLevelEnv = "WARNING (default)"
	}
	logger.Info("APPLICATION STARTUP: Logger initialized",
		slog.String("log_level_env", logLevelEnv))
	buildInfo := version.Get()
	logger.Info("APPLICATION STARTUP: Version",
		slog.String("version", buildInfo.Version),
		slog.String("commit", buildInfo.Commit),
		slog.String("buildTime", buildInfo.BuildTime))

	// Загружаем конфигурацию
	logger.Info("APPLICATION STARTUP: Loading configuration...")
	config := loadConfig()
	logger.Info("APPLICATION STARTUP: Configuration loaded")

	// Log all environment variables from docker-compose.yml for debugging.
	logger.Info("ENVIRONMENT VARIABLES",
		// Application Configuration
		slog.String("LOG_LEVEL", logLevelEnv),
		slog.String("BITRATE", os.Getenv("BITRATE")),
		slog.String("MAX_CLIENTS", os.Getenv("MAX_CLIENTS")),
		slog.String("PORT", os.Getenv("PORT")),

		// Audio Processing
		slog.String("NORMALIZE_VOLUME", os.Getenv("NORMALIZE_VOLUME")),
		slog.String("NORMALIZE_RUNTIME", os.Getenv("NORMALIZE_RUNTIME")),
		slog.String("NORMALIZE_SAMPLE_WINDOWS", os.Getenv("NORMALIZE_SAMPLE_WINDOWS")),
		slog.String("NORMALIZE_SAMPLE_MS", os.Getenv("NORMALIZE_SAMPLE_MS")),

		// Routing Configuration
		slog.String("DIRECTORY_ROUTES", os.Getenv("DIRECTORY_ROUTES")),
		slog.String("SHUFFLE", os.Getenv("SHUFFLE")),
		slog.String("ROUTES_SHUFFLE", os.Getenv("ROUTES_SHUFFLE")),

		// Security
		slog.Bool("STATUS_PASSWORD_SET", os.Getenv("STATUS_PASSWORD") != ""),

		// External Services
		slog.Bool("SENTRY_ENABLED", os.Getenv("SENTRY_DSN") != ""),
		slog.Int("SENTRY_DSN_LENGTH", len(os.Getenv("SENTRY_DSN"))),

		// Relay Configuration
		slog.String("RELAY", os.Getenv("RELAY")),
		slog.String("RELAY_CONFIG_FILE", os.Getenv("RELAY_CONFIG_FILE")))

	// Initialize Sentry.
	logger.Info("APPLICATION STARTUP: Initializing Sentry...")
	config.SentryHelper = initSentry(logger)
	globalSentryHelper = config.SentryHelper // Записываем в глобальную переменную для использования в других функциях
	logger.Info("APPLICATION STARTUP: Sentry initialized", "enabled", config.SentryHelper.IsEnabled())

	// Log application configuration.
	logConfiguration(logger, config)

	// Create and initialize components.
	logger.Info("STEP 1: Starting component initialization...")
	server, stationManager, relayManager, _ := initializeComponents(logger, config)
	logger.Info("STEP 2: Component initialization completed")

	// Create and start HTTP server.
	logger.Info("STEP 3: Starting HTTP server...")
	httpSrv := startHTTPServer(logger, config.Port, server.Handler())
	logger.Info("STEP 4: HTTP server started")

	// Configure root route redirection.
	logger.Info("STEP 5: Configuring root redirection...")
	redirectTarget := configureRootRedirection(logger, config, server)
	logger.Info("STEP 6: Root redirection configured", "redirectTarget", redirectTarget)

	// Asynchronously configure audio routes.
	logger.Info("STEP 7: Starting audio route configuration...")
	configureAudioRoutes(logger, server, stationManager, config)
	logger.Info("STEP 8: Audio route configuration initiated")

	// Check stream status.
	logger.Info("STEP 9: Checking stream status...")
	checkStreamStatus(logger, server)
	logger.Info("STEP 10: Stream status checked")

	// Start automatic history cleanup routine
	logger.Info("STEP 10.5: Starting automatic history cleanup routine...")
	go startHistoryCleanupRoutine(logger, server)
	logger.Info("STEP 10.5: History cleanup routine started")

	// Wait for shutdown signal.
	logger.Info("STEP 11: Waiting for shutdown signal...")
	sig := waitForShutdownSignal()

	// Handle the signal.
	handleShutdownSignal(logger, sig, server, stationManager, httpSrv)

	// Проверяем и выводим состояние relayManager при наличии.
	if relayManager != nil {
		logger.Info("Relay manager status at shutdown", "active", relayManager.IsActive())
	}
}

// initializeComponents creates and initializes all application components.
func initializeComponents(
	logger *slog.Logger,
	config *Config,
) (*httpServer.Server, *radio.StationManager, *relay.Manager, *telegram.Manager) {
	// Create HTTP server.
	logger.Debug("Creating HTTP server...")
	server := httpServer.NewServer(config.MaxClients, logger, config.SentryHelper)
	logger.Debug("HTTP server created")

	// Set global shuffle configuration.
	server.SetGlobalShuffleConfig(config.Shuffle)
	logger.Info("Global shuffle configuration set for HTTP server", slog.Bool("enabled", config.Shuffle))

	// Configure normalization parameters with safe defaults.
	normalizeWindows := config.NormalizeSampleWindows
	normalizeMs := config.NormalizeSampleMs

	// CRITICAL: If normalization is disabled, use zero values
	if !config.NormalizeVolume || config.NormalizeRuntime == "off" || normalizeWindows <= 0 || normalizeMs <= 0 {
		normalizeWindows = 0
		normalizeMs = 0
		logger.Info("NORMALIZATION DISABLED - Using raw audio streaming only",
			"normalizeVolume", config.NormalizeVolume,
			"normalizeRuntime", config.NormalizeRuntime,
			"configWindows", config.NormalizeSampleWindows,
			"configMs", config.NormalizeSampleMs)
	} else {
		logger.Info("NORMALIZATION ENABLED - Using configured parameters",
			"windows", normalizeWindows,
			"durationMs", normalizeMs)
	}

	audio.SetNormalizeConfig(normalizeWindows, normalizeMs)
	logger.Info("Audio configuration completed",
		"normalizationEnabled", normalizeWindows > 0 && normalizeMs > 0,
		"windows", normalizeWindows,
		"durationMs", normalizeMs)

	// Create radio station manager.
	logger.Debug("Creating radio station manager...")
	stationManager := radio.NewRadioStationManager(logger, config.SentryHelper)
	logger.Debug("Radio station manager created")

	// Set radio station manager for HTTP server.
	logger.Debug("Setting station manager for HTTP server...")
	server.SetStationManager(stationManager)
	logger.Debug("Station manager set for HTTP server")

	// Create relay manager (always create, but may be inactive).
	logger.Debug("Initializing relay manager...")
	relayManager := initializeRelayManager(logger, config, server)
	if config.EnableRelay {
		logger.Debug("Relay functionality enabled and active")
	} else {
		logger.Info("Relay functionality is disabled (inactive)")
	}

	// Create telegram manager (always create, but may be inactive).
	logger.Debug("Initializing telegram manager...")
	telegramManager := initializeTelegramManager(logger, config, server, relayManager)
	if config.EnableTelegramAlerts {
		logger.Debug("Telegram alerts enabled and active")
	} else {
		logger.Info("Telegram alerts functionality is disabled (inactive)")
	}

	// Create minimal dummy streams for /healthz to immediately find at least one route.
	logger.Debug("Creating initial dummy streams...")
	createInitialDummyStreams(logger, server)
	logger.Debug("Initial dummy streams created")

	logger.Debug("Component initialization completed successfully")
	return server, stationManager, relayManager, telegramManager
}

// initializeRelayManager creates and initializes the relay manager.
func initializeRelayManager(logger *slog.Logger, config *Config, server *httpServer.Server) *relay.Manager {
	logger.Info("Initializing relay manager", "config_file", config.RelayConfigFile)
	relayManager := relay.NewRelayManager(config.RelayConfigFile, logger)

	// Configure active state based on configuration.
	relayManager.SetActive(config.EnableRelay)

	// Set relay manager for HTTP server.
	server.SetRelayManager(relayManager)
	server.SetRelayFunctionalityEnabled(config.EnableRelay)

	// CRITICAL: Setup relay routes AFTER relay manager is set
	server.SetupRelayRoutes()
	logger.Info("Relay manager initialized and set for HTTP server")

	return relayManager
}

// initializeTelegramManager creates and initializes the telegram manager.
func initializeTelegramManager(logger *slog.Logger, config *Config, server *httpServer.Server, relayManager *relay.Manager) *telegram.Manager {
	logger.Info("Initializing telegram manager", "config_file", config.TelegramAlertsConfig, "timezone", config.TelegramAlertTimezone)
	telegramManager := telegram.NewManager(config.TelegramAlertsConfig, logger)
	telegramManager.SetTimezone(config.TelegramAlertTimezone)

	// Load configuration from file.
	if err := telegramManager.LoadConfig(); err != nil {
		logger.Error("Failed to load telegram config", "error", err)
		// Continue with default config
	}

	// Set telegram manager for HTTP server.
	server.SetTelegramManager(telegramManager)
	server.SetTelegramFunctionalityEnabled(config.EnableTelegramAlerts)

	// Setup route check function for main streams.
	telegramManager.SetRouteCheckFunc(func(route string) bool {
		return server.IsStreamAvailable(route)
	})

	// Setup relay check function if relay manager is available.
	if relayManager != nil {
		telegramManager.SetRelayCheckFunc(func(relayIndex string) bool {
			return relayManager.IsStreamAvailable(relayIndex)
		})
	}

	// CRITICAL: Setup telegram routes AFTER telegram manager is set
	server.SetupTelegramRoutes()

	// CRITICAL FIX: Always start monitoring if telegram alerts functionality is enabled
	// This ensures that stream status is tracked even if alerts are disabled in config
	// Admin can enable alerts through web interface later and monitoring will already be running
	if config.EnableTelegramAlerts {
		telegramManager.Start()
		logger.Info("Telegram monitoring started (will track stream status regardless of alert config)")
	} else {
		logger.Info("Telegram alerts functionality disabled - monitoring not started")
	}

	logger.Info("Telegram manager initialized and set for HTTP server")
	return telegramManager
}

// createInitialDummyStreams creates placeholder streams for fast health checks.
func createInitialDummyStreams(logger *slog.Logger, server *httpServer.Server) {
	dummyStream, dummyPlaylist := createDummyStreamAndPlaylist()
	server.RegisterStream("/humor", dummyStream, dummyPlaylist)
	server.RegisterStream("/science", dummyStream, dummyPlaylist)
	logger.Info("Temporary stream placeholders registered for quick healthcheck passing")
}

// startHTTPServer starts the HTTP server.
func startHTTPServer(logger *slog.Logger, port int, handler http.Handler) *http.Server {
	httpSrv := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", port), // Explicitly specify that we listen on all interfaces
		Handler: handler,
		// Increase timeouts for request processing.
		ReadTimeout:  readTimeoutSec * time.Second,
		WriteTimeout: 0, // Disable timeout for streaming
		IdleTimeout:  idleTimeoutSec * time.Second,
	}

	// Start server in goroutine.
	go func() {
		logger.Info("Starting HTTP server", "address", fmt.Sprintf("0.0.0.0:%d", port))
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("Server start error", "error", err)
			if globalSentryHelper != nil {
				globalSentryHelper.CaptureError(err, "main", "operation")
			}
		}
	}()

	return httpSrv
}

// checkStreamStatus checks the status of registered streams.
func checkStreamStatus(logger *slog.Logger, server *httpServer.Server) {
	logger.Info("====== REGISTERED STREAMS STATUS ======")
	humorRegistered := server.IsStreamRegistered("/humor")
	scienceRegistered := server.IsStreamRegistered("/science")
	logger.Info("Stream /humor registered", "value", humorRegistered)
	logger.Info("Stream /science registered", "value", scienceRegistered)

	if !humorRegistered || !scienceRegistered {
		logger.Info("WARNING: Some streams are not registered!")
	} else {
		logger.Info("All streams successfully registered")
	}
	logger.Info("=============================================")
}

// waitForShutdownSignal waits for shutdown signal.
func waitForShutdownSignal() os.Signal {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	return <-quit
}

// handleShutdownSignal handles shutdown signal.
func handleShutdownSignal(
	logger *slog.Logger,
	sig os.Signal,
	server *httpServer.Server,
	stationManager *radio.StationManager,
	httpSrv *http.Server,
) {
	logger.Info(
		"Signal received",
		slog.String("signal", sig.String()),
		slog.String("action", "performing graceful shutdown"),
	)

	// Handle SIGHUP for playlist reload.
	if sig == syscall.SIGHUP {
		reloadAllPlaylists(logger, server)
		return // Continue operation
	}

	// Stop all radio stations.
	stationManager.StopAll()

	// Graceful HTTP server shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeoutSec*time.Second)
	defer cancel()

	if shutdownErr := httpSrv.Shutdown(ctx); shutdownErr != nil {
		logger.Error("Server shutdown error", "error", shutdownErr)
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureError(shutdownErr, "main", "shutdown")
		}
	}
	logger.Info("Server successfully stopped")

	addHealthCheckHandler(logger, server)
}

func reloadAllPlaylists(logger *slog.Logger, server *httpServer.Server) {
	logger.Info("SIGHUP received, reloading playlists...")

	router, ok := server.Handler().(*mux.Router)
	if !ok {
		logger.Error("Failed to get mux.Router handler")
		return
	}

	if walkErr := router.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		path, pathErr := route.GetPathTemplate()
		if pathErr != nil {
			return pathErr
		}

		// Reload only for registered streams.
		if server.IsStreamRegistered(path) {
			logger.Info("Reloading playlist", slog.String("route", path))

			if reloadErr := server.ReloadPlaylist(path); reloadErr != nil {
				logger.Error("Error reloading playlist",
					slog.String("route", path),
					slog.String("error", reloadErr.Error()))
				if globalSentryHelper != nil {
					globalSentryHelper.CaptureError(reloadErr, "main", "reload")
				}
			} else {
				logger.Info("Playlist successfully reloaded", slog.String("route", path))
			}
		}

		return nil
	}); walkErr != nil {
		logger.Error("Error walking routes for playlist reload",
			slog.String("error", walkErr.Error()))
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureError(walkErr, "main", "file_walk")
		}
	}

	logger.Info("Playlist reload complete")
}

// startHistoryCleanupRoutine starts a routine that cleans track history every 12 hours
func startHistoryCleanupRoutine(logger *slog.Logger, server *httpServer.Server) {
	logger.Info("History cleanup routine started - will clean every 12 hours")

	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			cleanAllTrackHistories(logger, server)
		}
	}
}

// cleanAllTrackHistories clears track history for all streams
func cleanAllTrackHistories(logger *slog.Logger, server *httpServer.Server) {
	logger.Info("Starting automatic cleanup of all track histories")

	// Get all registered streams by checking common routes
	commonRoutes := []string{"/humor", "/science", "/politics", "/nature", "/shaov", "/troshin", "/test_audio"}

	clearedCount := 0
	for _, route := range commonRoutes {
		if server.IsStreamRegistered(route) {
			// Try to clear history via HTTP endpoint internally
			err := server.ClearHistoryForRoute(route)
			if err == nil {
				logger.Info("Cleared history for route", slog.String("route", route))
				clearedCount++
			} else {
				logger.Error("Failed to clear history for route",
					slog.String("route", route),
					slog.String("error", err.Error()))
			}
		}
	}

	logger.Info("Automatic history cleanup completed",
		slog.Int("streams_cleaned", clearedCount),
		slog.String("next_cleanup", time.Now().Add(12*time.Hour).Format("2006-01-02 15:04:05")))
}
