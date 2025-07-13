// Package main is the entry point for the audio streaming server.
// It handles initialization, configuration, and startup of all components.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aposazhennikov/stream-audio-to-web/audio"
	httpServer "github.com/aposazhennikov/stream-audio-to-web/http"
	"github.com/aposazhennikov/stream-audio-to-web/playlist"
	"github.com/aposazhennikov/stream-audio-to-web/radio"
	"github.com/aposazhennikov/stream-audio-to-web/relay"
	sentryhelper "github.com/aposazhennikov/stream-audio-to-web/sentry_helper"

	sentry "github.com/getsentry/sentry-go"
	"github.com/gorilla/mux"
)

// Global Sentry helper for use in functions without config access.
var globalSentryHelper *sentryhelper.SentryHelper

// Default configuration.
const (
	defaultPort     = 8000
	defaultAudioDir = "./audio"

	defaultBitrate         = 128
	defaultMaxClients      = 500
	defaultBufferSize      = 65536               // 64KB
	defaultShuffle         = false               // Shuffle tracks is disabled by default
	defaultNormalizeVolume = true                // Volume normalization is enabled by default
	defaultRelayEnabled    = false               // Relay functionality disabled by default
	defaultRelayConfigFile = "./relay_list.json" // Default path for relay configuration
	readTimeoutSec         = 15
	idleTimeoutSec         = 60
	shutdownTimeoutSec     = 10
	defaultRoute           = "/status"
	maxSplitParts          = 2      // Maximum number of parts when splitting configuration strings
	strTrue                = "true" // Строковое значение "true" для проверки
)

// Config describes the application configuration parameters.
type Config struct {
	Port                   int
	AudioDir               string
	DirectoryRoutes        map[string]string
	Bitrate                int
	MaxClients             int
	BufferSize             int
	Shuffle                bool                       // Global shuffle tracks setting
	PerStreamShuffle       map[string]bool            // Per-stream shuffle configuration
	NormalizeVolume        bool                       // Global volume normalization setting
	NormalizeRuntime       string                     // Runtime normalization mode: "auto", "on", "off"
	NormalizeSampleWindows int                        // Number of analysis windows for normalization
	NormalizeSampleMs      int                        // Duration of each analysis window in milliseconds
	EnableRelay            bool                       // Enable relay functionality
	RelayConfigFile        string                     // Path to relay configuration file
	SentryDSN              string                     // DSN для Sentry
	SentryHelper           *sentryhelper.SentryHelper // Helper для безопасной работы с Sentry
}

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
	server, stationManager, relayManager := initializeComponents(logger, config)
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
	// First attempt with original DSN.
	err := sentry.Init(sentry.ClientOptions{
		Dsn:   sentryDSN,
		Debug: true, // Enable debug mode for more verbose logging
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
		Debug: true,
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

// initializeComponents creates and initializes all application components.
func initializeComponents(
	logger *slog.Logger,
	config *Config,
) (*httpServer.Server, *radio.StationManager, *relay.Manager) {
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

	// Create relay manager if needed.
	logger.Debug("Checking relay configuration...")
	var relayManager *relay.Manager
	if config.EnableRelay {
		logger.Debug("Relay enabled, initializing relay manager...")
		relayManager = initializeRelayManager(logger, config, server)
		logger.Debug("Relay manager initialized")
	} else {
		logger.Info("Relay functionality is disabled")
	}

	// Create minimal dummy streams for /healthz to immediately find at least one route.
	logger.Debug("Creating initial dummy streams...")
	createInitialDummyStreams(logger, server)
	logger.Debug("Initial dummy streams created")

	logger.Debug("Component initialization completed successfully")
	return server, stationManager, relayManager
}

// initializeRelayManager creates and initializes the relay manager.
func initializeRelayManager(logger *slog.Logger, config *Config, server *httpServer.Server) *relay.Manager {
	logger.Info("Initializing relay manager", "config_file", config.RelayConfigFile)
	relayManager := relay.NewRelayManager(config.RelayConfigFile, logger)

	// Configure default state (enabled by default when feature is enabled).
	relayManager.SetActive(true)

	// Set relay manager for HTTP server.
	server.SetRelayManager(relayManager)

	// CRITICAL: Setup relay routes AFTER relay manager is set
	server.SetupRelayRoutes()
	logger.Info("Relay manager initialized and set for HTTP server")

	return relayManager
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

// configureRootRedirection sets up redirection from root route.
func configureRootRedirection(logger *slog.Logger, config *Config, server *httpServer.Server) string {
	// Redirect from root route.
	redirectPath := defaultRoute // redirect to /status by default
	if _, exists := config.DirectoryRoutes["/humor"]; !exists {
		// If /humor doesn't exist, take the first route from configuration.
		for route := range config.DirectoryRoutes {
			redirectPath = route
			break
		}
	}

	// Replace temporary handler for root route with redirection.
	configureRootHandler(logger, server, redirectPath)

	logger.Info("Redirect configured", slog.String("from", "/"), slog.String("to", redirectPath))
	return redirectPath
}

// configureRootHandler configures the handler for the root route.
func configureRootHandler(logger *slog.Logger, server *httpServer.Server, redirectTo string) {
	var routeErr error
	handler := server.Handler()
	router, ok := handler.(*mux.Router)
	if !ok {
		routeErr = errors.New("failed to get mux.Router handler")
		logger.Error("Failed to get router handler", slog.String("error", routeErr.Error()))
		return
	}

	// Получаем обработчик с проверкой ошибки.
	routeHandler := router.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		logger.Info("Redirecting from / to %s (method: %s)", redirectTo, r.Method)
		// For HEAD requests return only headers without redirect.
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, redirectTo, http.StatusSeeOther)
	}).Methods("GET", "HEAD")

	// Проверяем ошибку добавления обработчика.
	if routeHandlerErr := routeHandler.GetError(); routeHandlerErr != nil {
		routeErr = routeHandlerErr
		logger.Error("Failed to register root handler", "error", routeErr)
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureError(routeErr, "main", "route_config")
		}
	}

	if routeErr != nil {
		logger.Error("Error setting up route handler", "error", routeErr)
	}
}

// configureAudioRoutes configures audio routes from the configuration.
func configureAudioRoutes(
	logger *slog.Logger,
	server *httpServer.Server,
	stationManager *radio.StationManager,
	config *Config,
) {
	logger.Info("Starting audio route configuration...")
	logger.Debug("Directory routes found", "count", len(config.DirectoryRoutes))

	// Log all routes for debugging
	for route, dir := range config.DirectoryRoutes {
		logger.Debug("Found route", "route", route, "directory", dir)
	}

	if len(config.DirectoryRoutes) == 0 {
		criticalErr := fmt.Errorf("CRITICAL: No directory routes configured")
		logger.Error("CRITICAL: No directory routes configured! Check DIRECTORY_ROUTES environment variable.")
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureError(criticalErr, "main", "critical")
		}
		return
	}

	// CRITICAL CHECK: Verify normalization configuration only if normalization is enabled
	if config.NormalizeVolume && config.NormalizeRuntime != "off" && config.NormalizeSampleWindows <= 0 {
		criticalErr := fmt.Errorf("CRITICAL: Invalid normalization sample windows: %d (normalization is enabled)", config.NormalizeSampleWindows)
		logger.Error("CRITICAL: Invalid normalization sample windows configuration",
			"windows", config.NormalizeSampleWindows,
			"normalizeVolume", config.NormalizeVolume,
			"normalizeRuntime", config.NormalizeRuntime,
			"expected", "> 0")
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureError(criticalErr, "main", "critical")
		}
		// This is a critical configuration error - don't start audio streams
		return
	}

	logger.Debug("Audio route configuration checks passed",
		"normalizeVolume", config.NormalizeVolume,
		"normalizeRuntime", config.NormalizeRuntime,
		"normalizeWindows", config.NormalizeSampleWindows)

	// Configure routes from configuration ASYNCHRONOUSLY.
	for route, dir := range config.DirectoryRoutes {
		// Route should already be normalized with leading slash in loadConfig.
		// But check just in case.
		if !strings.HasPrefix(route, "/") {
			route = "/" + route
		}

		// Copy variables for goroutine.
		routeCopy := route
		dirCopy := dir

		logger.Debug("Starting goroutine for route", "route", routeCopy, "directory", dirCopy)

		// Start configuring EACH stream in a separate goroutine.
		go func(r, d string) {
			logger.Debug("Asynchronous configuration of route started", slog.String("route", r), slog.String("directory", d))
			if success := configureSyncRoute(logger, server, stationManager, r, d, config); success {
				logger.Debug("Route successfully configured", slog.String("route", r))
			} else {
				criticalErr := fmt.Errorf("CRITICAL: Route configuration failed for %s", r)
				logger.Error("CRITICAL: Route configuration failed", slog.String("route", r))
				if globalSentryHelper != nil {
					globalSentryHelper.CaptureError(criticalErr, "main", "critical")
				}
			}
		}(routeCopy, dirCopy)
	}
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

// addHealthCheckHandler adds a health check handler.
func addHealthCheckHandler(logger *slog.Logger, server *httpServer.Server) {
	router, ok := server.Handler().(*mux.Router)
	if !ok {
		logger.Error("Failed to get mux.Router handler")
		return
	}

	err := router.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, writeErr := w.Write([]byte("OK")); writeErr != nil {
			logger.Error("Failed to write health check response", slog.String("error", writeErr.Error()))
			return
		}
	}).GetError()

	if err != nil {
		logger.Error("Failed to register healthz handler", slog.String("error", err.Error()))
	}
}

// configureSyncRoute configures one audio stream route synchronously.
func configureSyncRoute(
	logger *slog.Logger,
	server *httpServer.Server,
	stationManager *radio.StationManager,
	route, dir string,
	config *Config,
) bool {
	// Добавляем recovery от panic внутри goroutine
	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("PANIC in configureSyncRoute for route %s: %v", route, r)
			logger.Error("PANIC in route configuration", "route", route, "panic", r)
			if globalSentryHelper != nil {
				globalSentryHelper.CaptureError(err, "main", "operation")
			}
		}
	}()

	logger.Info("Starting synchronous configuration of route", slog.String("route", route), slog.String("directory", dir))

	logger.Debug("ROUTE CONFIG STEP 1: Checking directory exists", "route", route, "directory", dir)
	if !ensureDirectoryExists(logger, dir, route) {
		err := fmt.Errorf("directory check failed for route %s: %s", route, dir)
		logger.Error("Directory check failed", "route", route, "directory", dir)
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureError(err, "main", "operation")
		}
		return false
	}

	logger.Debug("ROUTE CONFIG STEP 2: Checking audio files", "route", route, "directory", dir)
	if !checkAudioFiles(logger, dir, route) {
		err := fmt.Errorf("audio files check failed for route %s: %s", route, dir)
		logger.Error("Audio files check failed", "route", route, "directory", dir)
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureError(err, "main", "operation")
		}
		return false
	}

	logger.Debug("ROUTE CONFIG STEP 2.5: Checking and converting audio bitrate", "route", route, "directory", dir)
	if !checkAndConvertBitrate(logger, dir, route, config.Bitrate) {
		err := fmt.Errorf("bitrate conversion failed for route %s: %s", route, dir)
		logger.Error("Bitrate conversion failed", "route", route, "directory", dir)
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureError(err, "main", "operation")
		}
		return false
	}

	logger.Debug("ROUTE CONFIG STEP 3: Creating playlist", "route", route, "directory", dir)
	pl := createPlaylistOrNil(logger, dir, route, config)
	if pl == nil {
		err := fmt.Errorf("playlist creation failed for route %s: %s", route, dir)
		logger.Error("Playlist creation failed", "route", route, "directory", dir)
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureError(err, "main", "operation")
		}
		return false
	}

	logger.Debug("ROUTE CONFIG STEP 4: Creating streamer", "route", route)
	streamer := createStreamer(logger, config, route)
	if streamer == nil {
		err := fmt.Errorf("streamer creation failed for route %s", route)
		logger.Error("Streamer creation failed", "route", route)
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureError(err, "main", "operation")
		}
		return false
	}

	logger.Debug("ROUTE CONFIG STEP 5: Adding station to manager", "route", route)
	stationManager.AddStation(route, streamer, pl)
	logger.Debug("Radio station successfully added to manager", slog.String("route", route))

	logger.Debug("ROUTE CONFIG STEP 6: Registering stream on HTTP server", "route", route)
	server.RegisterStream(route, streamer, pl)
	logger.Debug("Audio stream successfully registered on HTTP server", slog.String("route", route))

	if !server.IsStreamRegistered(route) {
		logger.Error("CRITICAL ERROR: Stream not registered after all operations", slog.String("route", route))
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureInfo(fmt.Sprintf("Stream %s not registered after all operations", route), "main", "stream_registration")
		}
		return false
	}

	// Check if normalization should be used.
	switch config.NormalizeRuntime {
	case "on":
		streamer.SetVolumeNormalization(true)
		logger.Info(
			"DIAGNOSTICS: Runtime normalization mode 'on' overrides default setting for route",
			slog.String("route", route),
		)
	case "off":
		streamer.SetVolumeNormalization(false)
		logger.Debug("Runtime normalization mode 'off' overrides default setting for route",
			slog.String("route", route))
	case "auto", "":
		logger.Debug("Using default normalization setting for route", slog.String("route", route))
	}

	logger.Debug("RESULT: Route configuration SUCCESSFULLY COMPLETED", slog.String("route", route))
	return true
}

func ensureDirectoryExists(logger *slog.Logger, dir, route string) bool {
	if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
		logger.Debug("Creating directory for route", slog.String("route", route))
		if mkdirErr := os.MkdirAll(dir, 0750); mkdirErr != nil {
			logger.Error("ERROR: When creating directory",
				"directory", dir,
				"error", mkdirErr,
			)
			if globalSentryHelper != nil {
				globalSentryHelper.CaptureError(fmt.Errorf("error creating directory %s: %w", dir, mkdirErr), "main", "directory_creation")
			}
			return false
		}
	}
	return true
}

func checkAudioFiles(logger *slog.Logger, dir, route string) bool {
	logger.Debug("Checking audio files in directory",
		slog.String("directory", dir), slog.String("route", route))

	files, readErr := os.ReadDir(dir)
	if readErr != nil {
		logger.Error("ERROR: When reading directory",
			"directory", dir,
			"error", readErr,
		)
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureError(fmt.Errorf("error reading directory %s: %w", dir, readErr), "main", "directory_read")
		}
		return false
	}

	// Выводим список всех файлов для диагностики
	logger.Debug("List of files in directory", slog.String("directory", dir))
	for i, file := range files {
		logger.Debug("File found",
			slog.Int("index", i),
			slog.String("name", file.Name()),
			slog.Bool("isDir", file.IsDir()),
			slog.String("directory", dir))
	}

	audioFiles := 0
	for _, file := range files {
		fileName := file.Name()
		lowerFileName := strings.ToLower(fileName)

		// Проверяем расширения файлов, учитывая возможные варианты регистра (.MP3, .Mp3 и т.д.)
		isMP3 := strings.HasSuffix(lowerFileName, ".mp3")
		isOGG := strings.HasSuffix(lowerFileName, ".ogg")
		isAAC := strings.HasSuffix(lowerFileName, ".aac")
		isWAV := strings.HasSuffix(lowerFileName, ".wav")   // Добавим поддержку WAV
		isFLAC := strings.HasSuffix(lowerFileName, ".flac") // Добавим поддержку FLAC

		// Проверка на наличие точки в начале имени файла (скрытый файл в Unix)
		isHidden := strings.HasPrefix(fileName, ".")

		// Детальное логирование проверки файла
		logger.Debug("Checking file",
			slog.String("fileName", fileName),
			slog.String("lowerFileName", lowerFileName),
			slog.Bool("isDir", file.IsDir()),
			slog.Bool("isHidden", isHidden),
			slog.Bool("isMP3", isMP3),
			slog.Bool("isOGG", isOGG),
			slog.Bool("isAAC", isAAC),
			slog.Bool("isWAV", isWAV),
			slog.Bool("isFLAC", isFLAC))

		// Считаем аудиофайлы (не директории, не скрытые файлы, с поддержкой аудиоформатов)
		if !file.IsDir() && !isHidden && (isMP3 || isOGG || isAAC || isWAV || isFLAC) {
			audioFiles++
			logger.Debug("Audio file counted",
				slog.String("fileName", fileName),
				slog.Int("totalSoFar", audioFiles))
		}
	}

	logger.Debug("Directory contains", slog.Int("audio_files", audioFiles), slog.String("directory", dir))

	// Вместо ошибки только выводим предупреждение, если нет аудиофайлов, но разрешаем продолжить работу
	if audioFiles == 0 {
		logger.Warn("WARNING: No audio files in directory, stream will be empty until files are added",
			slog.String("directory", dir),
			slog.String("route", route))

		// Только записываем сообщение в Sentry, но не считаем это ошибкой
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureInfo(fmt.Sprintf("No audio files in directory %s, but stream will be configured anyway", dir), "main", "empty_directory")
		}

		// Возвращаем true, чтобы разрешить конфигурацию маршрута даже без файлов
		return true
	}
	return true
}

func createPlaylistOrNil(logger *slog.Logger, dir, route string, config *Config) httpServer.PlaylistManager {
	shuffleSetting := config.Shuffle
	if specificShuffle, exists := config.PerStreamShuffle[route]; exists {
		shuffleSetting = specificShuffle
		logger.Debug("Using specific shuffle setting for route", slog.String("route", route))
	} else {
		logger.Debug("Using global shuffle setting for route", slog.String("route", route))
	}
	pl, playlistErr := playlist.NewPlaylist(dir, nil, shuffleSetting, logger, config.SentryHelper)
	if playlistErr != nil {
		logger.Error("ERROR creating playlist", "error", playlistErr)
		config.SentryHelper.CaptureError(fmt.Errorf("error creating playlist: %w", playlistErr), "main", "playlist_creation")
		return nil
	}
	logger.Debug("Playlist for route successfully created", slog.String("route", route))
	return pl
}

func createStreamer(logger *slog.Logger, config *Config, route string) *audio.Streamer {
	logger.Debug("Creating audio streamer for route", slog.String("route", route))
	streamer := audio.NewStreamer(config.BufferSize, config.MaxClients, config.Bitrate, logger, config.SentryHelper)
	streamer.SetVolumeNormalization(config.NormalizeVolume)
	logger.Debug("Audio streamer for route successfully created", slog.String("route", route))
	return streamer
}

// Load configuration from command line flags and environment variables.
func loadConfig() *Config {
	// Parse command line flags.
	config := parseCommandLineFlags()

	// Load configuration from environment variables.
	loadConfigFromEnv(config)

	return config
}

// parseCommandLineFlags парсит флаги командной строки и возвращает начальную конфигурацию.
func parseCommandLineFlags() *Config {
	port := flag.Int("port", defaultPort, "HTTP server port")
	audioDir := flag.String("audio-dir", defaultAudioDir, "Directory with audio files")
	directoryRoutes := flag.String("directory-routes", "", "JSON mapping of route prefixes to directories")
	bitrate := flag.Int("bitrate", defaultBitrate, "Stream bitrate")
	maxClients := flag.Int("max-clients", defaultMaxClients, "Maximum number of clients")
	bufferSize := flag.Int("buffer-size", defaultBufferSize, "Buffer size for audio streaming")
	shuffle := flag.Bool("shuffle", defaultShuffle, "Enable shuffle mode for all streams")
	normalizeVolume := flag.Bool("normalize", defaultNormalizeVolume, "Enable volume normalization")
	normalizeRuntime := flag.String("normalize-runtime", "auto", "Runtime normalization mode (auto, on, off)")
	normalizeSampleWindows := flag.Int("normalize-windows", 10, "Number of analysis windows for normalization")
	normalizeSampleMs := flag.Int("normalize-ms", 1000, "Duration of each analysis window in milliseconds")
	relayEnabled := flag.Bool("relay", defaultRelayEnabled, "Enable relay functionality")
	relayConfigFile := flag.String("relay-config", defaultRelayConfigFile, "Path to relay configuration file")
	sentryDSN := flag.String("sentry-dsn", "", "DSN for Sentry error tracking")

	flag.Parse()

	// Parse directory routes from command line flag.
	directoryRoutesMap := make(map[string]string)
	if *directoryRoutes != "" {
		if err := json.Unmarshal([]byte(*directoryRoutes), &directoryRoutesMap); err != nil {
			slog.Default().Error("Invalid directory-routes JSON", slog.String("error", err.Error()))
		}
	}

	// Create configuration.
	return &Config{
		Port:                   *port,
		AudioDir:               *audioDir,
		DirectoryRoutes:        directoryRoutesMap,
		Bitrate:                *bitrate,
		MaxClients:             *maxClients,
		BufferSize:             *bufferSize,
		Shuffle:                *shuffle,
		PerStreamShuffle:       make(map[string]bool),
		NormalizeVolume:        *normalizeVolume,
		NormalizeRuntime:       *normalizeRuntime,
		NormalizeSampleWindows: *normalizeSampleWindows,
		NormalizeSampleMs:      *normalizeSampleMs,
		EnableRelay:            *relayEnabled,
		RelayConfigFile:        *relayConfigFile,
		SentryDSN:              *sentryDSN,
	}
}

// loadConfigFromEnv загружает конфигурацию из переменных окружения.
func loadConfigFromEnv(config *Config) {
	// Разделим функцию на более простые части
	loadGenericConfig(config)
	loadNormalizationConfig(config)
	loadRelayConfig(config)
	loadStreamConfig(config)

	// Load directory routes from environment.
	loadDirectoryRoutesFromEnv(config, slog.Default())

	// Load per-stream shuffle settings from environment.
	loadShuffleSettingsFromEnv(config)
}

// loadGenericConfig загружает общие параметры конфигурации из переменных окружения.
func loadGenericConfig(config *Config) {
	// Override config with environment variables if provided
	if portStr := os.Getenv("PORT"); portStr != "" {
		if port, err := strconv.Atoi(portStr); err == nil {
			config.Port = port
		}
	}

	if bufferSizeStr := os.Getenv("BUFFER_SIZE"); bufferSizeStr != "" {
		if bufferSize, err := strconv.Atoi(bufferSizeStr); err == nil {
			config.BufferSize = bufferSize
		}
	}

	if shuffleStr := os.Getenv("SHUFFLE"); shuffleStr != "" {
		config.Shuffle = strings.ToLower(shuffleStr) == strTrue
	}

	if sentryDSN := os.Getenv("SENTRY_DSN"); sentryDSN != "" {
		config.SentryDSN = sentryDSN
	}
}

// loadNormalizationConfig загружает параметры нормализации из переменных окружения.
func loadNormalizationConfig(config *Config) {
	if normalizeVolumeStr := os.Getenv("NORMALIZE_VOLUME"); normalizeVolumeStr != "" {
		config.NormalizeVolume = strings.ToLower(normalizeVolumeStr) == strTrue
	}

	if normalizeRuntime := os.Getenv("NORMALIZE_RUNTIME"); normalizeRuntime != "" {
		config.NormalizeRuntime = normalizeRuntime
	}

	if normalizeSampleWindowsStr := os.Getenv("NORMALIZE_SAMPLE_WINDOWS"); normalizeSampleWindowsStr != "" {
		if normalizeSampleWindows, err := strconv.Atoi(normalizeSampleWindowsStr); err == nil {
			config.NormalizeSampleWindows = normalizeSampleWindows
		}
	}

	if normalizeSampleMsStr := os.Getenv("NORMALIZE_SAMPLE_MS"); normalizeSampleMsStr != "" {
		if normalizeSampleMs, err := strconv.Atoi(normalizeSampleMsStr); err == nil {
			config.NormalizeSampleMs = normalizeSampleMs
		}
	}
}

// loadRelayConfig загружает параметры реле из переменных окружения.
func loadRelayConfig(config *Config) {
	// Проверяем переменную RELAY для включения/выключения relay функциональности
	if relayEnabledStr := os.Getenv("RELAY"); relayEnabledStr != "" {
		config.EnableRelay = strings.ToLower(relayEnabledStr) == strTrue
	}

	// Для обратной совместимости также проверяем старое имя переменной RELAY_ENABLED
	if relayEnabledStr := os.Getenv("RELAY_ENABLED"); relayEnabledStr != "" && os.Getenv("RELAY") == "" {
		config.EnableRelay = strings.ToLower(relayEnabledStr) == strTrue
	}

	if relayConfigFile := os.Getenv("RELAY_CONFIG_FILE"); relayConfigFile != "" {
		config.RelayConfigFile = relayConfigFile
	}
}

// loadStreamConfig загружает параметры потока из переменных окружения.
func loadStreamConfig(config *Config) {
	if bitrateStr := os.Getenv("BITRATE"); bitrateStr != "" {
		if bitrate, err := strconv.Atoi(bitrateStr); err == nil {
			config.Bitrate = bitrate
		}
	}

	if maxClientsStr := os.Getenv("MAX_CLIENTS"); maxClientsStr != "" {
		if maxClients, err := strconv.Atoi(maxClientsStr); err == nil {
			config.MaxClients = maxClients
		}
	}
}

// loadDirectoryRoutesFromEnv загружает маршруты директорий из переменных окружения.
func loadDirectoryRoutesFromEnv(config *Config, logger *slog.Logger) {
	dirRoutes := getEnvOrDefault("DIRECTORY_ROUTES", "")
	if dirRoutes == "" {
		// No directory routes specified.
		return
	}

	// Попробуем сначала обработать формат JSON
	var jsonRoutes map[string]string
	if err := json.Unmarshal([]byte(dirRoutes), &jsonRoutes); err == nil {
		logger.Info("Обнаружен JSON формат в DIRECTORY_ROUTES")

		// Перебираем маршруты из JSON
		for route, path := range jsonRoutes {
			// Нормализуем маршрут (добавляем слэш в начало, если его нет)
			if !strings.HasPrefix(route, "/") {
				logger.Debug("Normalized route", slog.String("from", route), slog.String("to", "/"+route))
				route = "/" + route
			}

			// Проверяем существование директории
			if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
				logger.Warn("Directory does not exist", slog.String("path", path), slog.String("route", route))
				continue
			}

			// Ключ - маршрут, значение - путь к директории
			config.DirectoryRoutes[route] = path
			logger.Debug("Added directory route from JSON", slog.String("route", route), slog.String("path", path))
		}

		logger.Debug("Directory routes configured from JSON", slog.Int("count", len(config.DirectoryRoutes)))
		return
	}

	// Если JSON не сработал, пробуем старый формат
	logger.Info("Пробуем старый формат DIRECTORY_ROUTES с разделителями")

	// Process each directory route.
	routes := strings.Split(dirRoutes, ";")
	for _, route := range routes {
		parts := strings.Split(route, ":")
		if len(parts) != maxSplitParts {
			logger.Warn("Invalid directory route format", slog.String("route", route))
			continue
		}

		url := parts[0]  // Маршрут
		path := parts[1] // Путь к директории

		if !strings.HasPrefix(url, "/") {
			url = "/" + url
		}

		// Check if directory exists.
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			logger.Warn("Directory does not exist", slog.String("path", path), slog.String("route", url))
			continue
		}

		// Add to configuration.
		config.DirectoryRoutes[url] = path

		logger.Debug("Added directory route", slog.String("url", url), slog.String("path", path))
	}

	logger.Debug("Directory routes configured", slog.Int("count", len(config.DirectoryRoutes)))
}

// loadShuffleSettingsFromEnv загружает настройки перемешивания из переменных окружения.
func loadShuffleSettingsFromEnv(config *Config) {
	shuffleSettings := getEnvOrDefault("SHUFFLE_SETTINGS", "")
	parseShuffleSettings(config, shuffleSettings)
}

func parseShuffleSettings(config *Config, shuffleSettings string) {
	if shuffleSettings == "" {
		return
	}
	settings := strings.Split(shuffleSettings, ",")
	for _, setting := range settings {
		parts := strings.SplitN(setting, ":", maxSplitParts)
		if len(parts) == maxSplitParts {
			routePath := strings.TrimSpace(parts[0])
			shuffleValue := strings.TrimSpace(parts[1])
			// Ensure route starts with slash.
			if routePath[0] != '/' {
				routePath = "/" + routePath
			}
			// Parse shuffle value.
			switch shuffleValue {
			case strTrue:
				config.PerStreamShuffle[routePath] = true
			case "false":
				config.PerStreamShuffle[routePath] = false
			}
		}
	}
}

// getEnvOrDefault returns environment variable value or default value.
func getEnvOrDefault(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

// This is necessary for quickly passing healthcheck before loading real streams.
func createDummyStreamAndPlaylist() (httpServer.StreamHandler, httpServer.PlaylistManager) {
	// Placeholder for StreamHandler.
	dummyStream := &dummyStreamHandler{
		clientCounter: 0,
		clientCh:      make(chan string, 1),
	}

	// Placeholder for PlaylistManager.
	dummyPlaylist := &dummyPlaylistManager{}

	return dummyStream, dummyPlaylist
}

// Minimal implementation of StreamHandler for placeholder.
type dummyStreamHandler struct {
	clientCounter int32
	clientCh      chan string
}

func (d *dummyStreamHandler) AddClient() (<-chan []byte, int, error) {
	// Empty channel that will be replaced with real stream later.
	ch := make(chan []byte, 1)
	return ch, 0, nil
}

func (d *dummyStreamHandler) RemoveClient(_ int) {
	// Do nothing.
}

func (d *dummyStreamHandler) GetClientCount() int {
	return 0
}

func (d *dummyStreamHandler) GetCurrentTrackChannel() <-chan string {
	return d.clientCh
}

func (d *dummyStreamHandler) GetPlaybackInfo() (string, time.Time, time.Duration, time.Duration) {
	return "", time.Time{}, time.Duration(0), time.Duration(0)
}

// getDummyMP3 returns the name of a dummy MP3 file.
func getDummyMP3() string {
	return "dummy.mp3"
}

// Minimal implementation of PlaylistManager for placeholder.
type dummyPlaylistManager struct{}

func (d *dummyPlaylistManager) Reload() error {
	return nil
}

func (d *dummyPlaylistManager) GetCurrentTrack() interface{} {
	return getDummyMP3()
}

func (d *dummyPlaylistManager) NextTrack() interface{} {
	return getDummyMP3()
}

func (d *dummyPlaylistManager) GetHistory() []interface{} {
	return []interface{}{}
}

func (d *dummyPlaylistManager) GetStartTime() time.Time {
	return time.Now()
}

func (d *dummyPlaylistManager) PreviousTrack() interface{} {
	return getDummyMP3()
}

// Shuffle implements PlaylistManager.Shuffle method.
func (d *dummyPlaylistManager) Shuffle() {
	// Empty implementation for dummy placeholder.
}

// GetShuffleEnabled implements PlaylistManager.GetShuffleEnabled method.
func (d *dummyPlaylistManager) GetShuffleEnabled() bool {
	// Dummy implementation - always return false.
	return false
}

// SetShuffleEnabled implements PlaylistManager.SetShuffleEnabled method.
func (d *dummyPlaylistManager) SetShuffleEnabled(enabled bool) {
	// Empty implementation for dummy placeholder.
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

// checkAndConvertBitrate checks all audio files in directory and converts them to target bitrate if needed.
func checkAndConvertBitrate(logger *slog.Logger, dir, route string, targetBitrate int) bool {
	logger.Debug("BITRATE CONVERSION: Starting bitrate check for directory",
		slog.String("directory", dir),
		slog.String("route", route),
		slog.Int("targetBitrate", targetBitrate))

	files, readErr := os.ReadDir(dir)
	if readErr != nil {
		logger.Error("ERROR: When reading directory for bitrate conversion",
			"directory", dir,
			"error", readErr,
		)
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureError(fmt.Errorf("error reading directory %s for bitrate conversion: %w", dir, readErr), "main", "bitrate_conversion")
		}
		return false
	}

	convertedFiles := 0
	skippedFiles := 0
	totalAudioFiles := 0

	for _, file := range files {
		fileName := file.Name()
		lowerFileName := strings.ToLower(fileName)

		// Check if it's an audio file.
		isMP3 := strings.HasSuffix(lowerFileName, ".mp3")
		isOGG := strings.HasSuffix(lowerFileName, ".ogg")
		isAAC := strings.HasSuffix(lowerFileName, ".aac")
		isWAV := strings.HasSuffix(lowerFileName, ".wav")
		isFLAC := strings.HasSuffix(lowerFileName, ".flac")
		isHidden := strings.HasPrefix(fileName, ".")

		// Skip non-audio files, directories, and hidden files.
		if file.IsDir() || isHidden || !(isMP3 || isOGG || isAAC || isWAV || isFLAC) {
			continue
		}

		totalAudioFiles++
		filePath := filepath.Join(dir, fileName)

		// Check current bitrate of the file.
		currentBitrate, err := getAudioBitrate(logger, filePath)
		if err != nil {
			logger.Error("BITRATE CONVERSION: Failed to get bitrate for file",
				slog.String("file", filePath),
				slog.String("error", err.Error()))
			continue
		}

		logger.Debug("BITRATE CONVERSION: File bitrate detected",
			slog.String("file", fileName),
			slog.Int("currentBitrate", currentBitrate),
			slog.Int("targetBitrate", targetBitrate))

		// Check if conversion is needed.
		if currentBitrate == targetBitrate {
			logger.Debug("BITRATE CONVERSION: File already has target bitrate, skipping",
				slog.String("file", fileName),
				slog.Int("bitrate", currentBitrate))
			skippedFiles++
			continue
		}

		// Convert file to target bitrate.
		if !convertAudioBitrate(logger, filePath, targetBitrate) {
			logger.Error("BITRATE CONVERSION: Failed to convert file",
				slog.String("file", filePath),
				slog.Int("fromBitrate", currentBitrate),
				slog.Int("toBitrate", targetBitrate))
			continue
		}

		convertedFiles++
		logger.Debug("BITRATE CONVERSION: File successfully converted",
			slog.String("file", fileName),
			slog.Int("fromBitrate", currentBitrate),
			slog.Int("toBitrate", targetBitrate))
	}

	logger.Debug("BITRATE CONVERSION: Completed for directory",
		slog.String("directory", dir),
		slog.String("route", route),
		slog.Int("totalAudioFiles", totalAudioFiles),
		slog.Int("convertedFiles", convertedFiles),
		slog.Int("skippedFiles", skippedFiles),
		slog.Int("targetBitrate", targetBitrate))

	return true
}

// getAudioBitrate returns the bitrate of an audio file using ffprobe.
func getAudioBitrate(logger *slog.Logger, filePath string) (int, error) {
	cmd := exec.Command("ffprobe", "-v", "quiet", "-show_entries", "stream=bit_rate", "-of", "csv=p=0", filePath)
	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe failed: %w", err)
	}

	bitrateStr := strings.TrimSpace(string(output))
	if bitrateStr == "" || bitrateStr == "N/A" {
		return 0, fmt.Errorf("could not determine bitrate")
	}

	bitrateFloat, err := strconv.ParseFloat(bitrateStr, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid bitrate value: %s", bitrateStr)
	}

	// Convert from bits per second to kilobits per second.
	bitrateKbps := int(bitrateFloat / 1000)

	return bitrateKbps, nil
}

// convertAudioBitrate converts an audio file to the target bitrate using ffmpeg.
func convertAudioBitrate(logger *slog.Logger, filePath string, targetBitrate int) bool {
	// Create temporary file for conversion.
	tempFile := filePath + ".temp"

	// Remove temp file if it exists.
	if _, err := os.Stat(tempFile); err == nil {
		os.Remove(tempFile)
	}

	// Build ffmpeg command with explicit format specification.
	cmd := exec.Command("ffmpeg", "-y", "-i", filePath, "-b:a", fmt.Sprintf("%dk", targetBitrate), "-codec:a", "libmp3lame", "-f", "mp3", tempFile)

	// Execute conversion.
	output, err := cmd.CombinedOutput()
	if err != nil {
		logger.Error("BITRATE CONVERSION: ffmpeg conversion failed",
			slog.String("file", filePath),
			slog.Int("targetBitrate", targetBitrate),
			slog.String("error", err.Error()),
			slog.String("output", string(output)))

		// Clean up temp file.
		os.Remove(tempFile)
		return false
	}

	// Replace original file with converted file.
	if err := os.Rename(tempFile, filePath); err != nil {
		logger.Error("BITRATE CONVERSION: Failed to replace original file",
			slog.String("file", filePath),
			slog.String("error", err.Error()))

		// Clean up temp file.
		os.Remove(tempFile)
		return false
	}

	logger.Debug("BITRATE CONVERSION: Successfully converted file",
		slog.String("file", filePath),
		slog.Int("targetBitrate", targetBitrate))

	return true
}
