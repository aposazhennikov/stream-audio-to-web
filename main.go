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
	"os/signal"
	"strings"
	"syscall"
	"time"

	sentry "github.com/getsentry/sentry-go"
	"github.com/gorilla/mux"
	"github.com/user/stream-audio-to-web/audio"
	httpServer "github.com/user/stream-audio-to-web/http"
	"github.com/user/stream-audio-to-web/playlist"
	"github.com/user/stream-audio-to-web/radio"
	"github.com/user/stream-audio-to-web/relay"
)

// Default configuration.
const (
	defaultPort            = 8000
	defaultAudioDir        = "./audio"
	defaultStreamFormat    = "mp3"
	defaultBitrate         = 128
	defaultMaxClients      = 500
	defaultLogLevel        = "info"
	defaultBufferSize      = 65536               // 64KB
	defaultShuffle         = false               // Shuffle tracks is disabled by default
	defaultNormalizeVolume = true                // Volume normalization is enabled by default
	defaultRelayEnabled    = false               // Relay functionality disabled by default
	defaultRelayConfigFile = "./relay_list.json" // Default path for relay configuration
	readTimeoutSec         = 15
	idleTimeoutSec         = 60
	shutdownTimeoutSec     = 10
	defaultRoute           = "/humor"
	maxSplitParts          = 2 // Maximum number of parts when splitting configuration strings
)

// Config describes the application configuration parameters.
type Config struct {
	Port                   int
	AudioDir               string
	DirectoryRoutes        map[string]string
	StreamFormat           string
	Bitrate                int
	MaxClients             int
	LogLevel               string
	BufferSize             int
	Shuffle                bool            // Global shuffle tracks setting
	PerStreamShuffle       map[string]bool // Per-stream shuffle configuration
	NormalizeVolume        bool            // Global volume normalization setting
	NormalizeRuntime       string          // Runtime normalization mode: "auto", "on", "off"
	NormalizeSampleWindows int             // Number of analysis windows for normalization
	NormalizeSampleMs      int             // Duration of each analysis window in milliseconds
	RelayEnabled           bool            // Enable relay functionality
	RelayConfigFile        string          // Path to relay configuration file
}

func main() {
	// Load configuration first to get log level.
	config := loadConfig()

	// Initialize logger with proper log level.
	logger := setupLogger(config.LogLevel)

	// Initialize Sentry.
	initSentry(logger)

	// Print configuration.
	logConfiguration(logger, config)

	// Create and initialize components.
	server, stationManager, relayManager := initializeComponents(logger, config)

	// Create and start HTTP server.
	httpSrv := startHTTPServer(logger, config.Port, server.Handler())

	// Configure root route redirection.
	redirectTarget := configureRootRedirection(logger, config, server)
	logger.Info("Root redirection configured", "redirectTarget", redirectTarget)

	// Asynchronously configure audio routes.
	configureAudioRoutes(logger, server, stationManager, config)

	// Check stream status.
	checkStreamStatus(logger, server)

	// Wait for shutdown signal.
	sig := waitForShutdownSignal()

	// Handle the signal.
	handleShutdownSignal(logger, sig, server, stationManager, httpSrv)

	// Проверяем и выводим состояние relayManager при наличии.
	if relayManager != nil {
		logger.Info("Relay manager status at shutdown", "active", relayManager.IsActive())
	}
}

// setupLogger creates and configures a logger with the specified log level.
func setupLogger(logLevel string) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(logLevel) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	// Create handler with the specified level.
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})

	// Create and return the logger.
	return slog.New(handler)
}

// initSentry initializes Sentry for error tracking.
func initSentry(logger *slog.Logger) {
	// Get DSN from environment.
	sentryDSN := strings.TrimSpace(os.Getenv("SENTRY_DSN"))
	if sentryDSN == "" {
		logger.Info("Sentry monitoring disabled (SENTRY_DSN not set or empty)")
		return
	}

	logger.Info("Initializing Sentry with DSN", "dsn_length", len(sentryDSN))

	// Initialize Sentry with DSN from environment.
	err := initSentryWithDSN(logger, sentryDSN)
	if err == nil {
		logger.Info("Sentry initialization succeeded")
	}
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
	logger.Info("========== APPLICATION CONFIGURATION ==========")
	logger.Info("Port", slog.Int("value", config.Port))
	logger.Info("Default audio directory", slog.String("value", config.AudioDir))
	logger.Info("Stream format", slog.String("value", config.StreamFormat))
	logger.Info("Bitrate", slog.Int("value", config.Bitrate))
	logger.Info("Max clients", slog.Int("value", config.MaxClients))
	logger.Info("Buffer size", slog.Int("value", config.BufferSize))
	logger.Info("Global shuffle setting", slog.Bool("value", config.Shuffle))
	logger.Info("Volume normalization", slog.Bool("value", config.NormalizeVolume))
	logger.Info("Runtime normalization mode", slog.String("value", config.NormalizeRuntime))
	logger.Info("Normalization sample windows", slog.Int("value", config.NormalizeSampleWindows))
	logger.Info("Normalization sample duration", slog.String("value", fmt.Sprintf("%d ms", config.NormalizeSampleMs)))
	logger.Info("Relay functionality enabled", slog.Bool("value", config.RelayEnabled))
	logger.Info("Relay configuration file", slog.String("value", config.RelayConfigFile))

	// Log per-stream shuffle settings.
	logger.Info("Per-stream shuffle settings:")
	for route, shuffle := range config.PerStreamShuffle {
		logger.Info("Per-stream shuffle", slog.String("route", route), slog.Bool("value", shuffle))
	}

	// Log additional directory routes.
	logger.Info("Additional directory routes:")
	for route, dir := range config.DirectoryRoutes {
		logger.Info("Additional route", slog.String("route", route), slog.String("directory", dir))
	}

	logger.Info("=============================================")
}

// initializeComponents creates and initializes all application components.
func initializeComponents(
	logger *slog.Logger,
	config *Config,
) (*httpServer.Server, *radio.StationManager, *relay.Manager) {
	// Create HTTP server.
	server := httpServer.NewServer(config.StreamFormat, config.MaxClients)

	// Configure normalization parameters.
	audio.SetNormalizeConfig(config.NormalizeSampleWindows, config.NormalizeSampleMs)

	// Create radio station manager.
	stationManager := radio.NewRadioStationManager(logger)

	// Set radio station manager for HTTP server.
	server.SetStationManager(stationManager)

	// Create relay manager if needed.
	var relayManager *relay.Manager
	if config.RelayEnabled {
		relayManager = initializeRelayManager(logger, config, server)
	} else {
		logger.Info("Relay functionality is disabled")
	}

	// Create minimal dummy streams for /healthz to immediately find at least one route.
	createInitialDummyStreams(logger, server)

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
			sentry.CaptureException(err)
		}
	}()

	return httpSrv
}

// configureRootRedirection sets up redirection from root route.
func configureRootRedirection(logger *slog.Logger, config *Config, server *httpServer.Server) string {
	// Redirect from root route.
	redirectPath := defaultRoute // redirect to /humor by default
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
		sentry.CaptureException(routeErr)
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

	// Configure routes from configuration ASYNCHRONOUSLY.
	for route, dir := range config.DirectoryRoutes {
		// Route should already be normalized with leading slash in loadConfig.
		// But check just in case.
		if route[0] != '/' {
			route = "/" + route
		}

		// Copy variables for goroutine.
		routeCopy := route
		dirCopy := dir

		// Start configuring EACH stream in a separate goroutine.
		go func(route, dir string) {
			logger.Info("Asynchronous configuration of route", slog.String("route", route))
			if success := configureSyncRoute(logger, server, stationManager, route, dir, config); success {
				logger.Info("Route successfully configured", slog.String("route", route))
			} else {
				logger.Error("ERROR: Route configuration failed", slog.String("route", route))
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
		sentry.CaptureException(shutdownErr)
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
	logger.Info("Starting synchronous configuration of route", slog.String("route", route))

	if !ensureDirectoryExists(logger, dir, route) {
		return false
	}

	if !checkAudioFiles(logger, dir, route) {
		return false
	}

	pl := createPlaylistOrNil(logger, dir, route, config)
	if pl == nil {
		return false
	}

	streamer := createStreamer(logger, config, route)
	if streamer == nil {
		return false
	}

	stationManager.AddStation(route, streamer, pl)
	logger.Info("Radio station successfully added to manager", slog.String("route", route))

	server.RegisterStream(route, streamer, pl)
	logger.Info("Audio stream successfully registered on HTTP server", slog.String("route", route))

	if !server.IsStreamRegistered(route) {
		logger.Error("CRITICAL ERROR: Stream not registered after all operations", slog.String("route", route))
		sentry.CaptureMessage(fmt.Sprintf("Stream %s not registered after all operations", route))
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
		logger.Info("DIAGNOSTICS: Runtime normalization mode 'off' overrides default setting for route",
			slog.String("route", route))
	case "auto", "":
		logger.Info("DIAGNOSTICS: Using default normalization setting for route", slog.String("route", route))
	}

	logger.Info("RESULT: Route configuration SUCCESSFULLY COMPLETED", slog.String("route", route))
	return true
}

func ensureDirectoryExists(logger *slog.Logger, dir, route string) bool {
	if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
		logger.Info("Creating directory for route", slog.String("route", route))
		if mkdirErr := os.MkdirAll(dir, 0750); mkdirErr != nil {
			logger.Error("ERROR: When creating directory",
				"directory", dir,
				"error", mkdirErr,
			)
			sentry.CaptureException(fmt.Errorf("error creating directory %s: %w", dir, mkdirErr))
			return false
		}
	}
	return true
}

func checkAudioFiles(logger *slog.Logger, dir, _ string) bool {
	files, readErr := os.ReadDir(dir)
	if readErr != nil {
		logger.Error("ERROR: When reading directory",
			"directory", dir,
			"error", readErr,
		)
		sentry.CaptureException(fmt.Errorf("error reading directory %s: %w", dir, readErr))
		return false
	}
	audioFiles := 0
	for _, file := range files {
		if !file.IsDir() && (strings.HasSuffix(strings.ToLower(file.Name()), ".mp3") ||
			strings.HasSuffix(strings.ToLower(file.Name()), ".ogg") ||
			strings.HasSuffix(strings.ToLower(file.Name()), ".aac")) {
			audioFiles++
		}
	}
	logger.Info("Directory contains", slog.Int("audio_files", audioFiles), slog.String("directory", dir))
	if audioFiles == 0 {
		logger.Error("CRITICAL ERROR: No audio files in directory", slog.String("directory", dir))
		sentry.CaptureMessage(fmt.Sprintf("No audio files in directory %s", dir))
		return false
	}
	return true
}

func createPlaylistOrNil(logger *slog.Logger, dir, route string, config *Config) httpServer.PlaylistManager {
	shuffleSetting := config.Shuffle
	if specificShuffle, exists := config.PerStreamShuffle[route]; exists {
		shuffleSetting = specificShuffle
		logger.Info("Using specific shuffle setting for route", slog.String("route", route))
	} else {
		logger.Info("Using global shuffle setting for route", slog.String("route", route))
	}
	pl, playlistErr := playlist.NewPlaylist(dir, nil, shuffleSetting, logger)
	if playlistErr != nil {
		logger.Error("ERROR creating playlist", "error", playlistErr)
		sentry.CaptureException(fmt.Errorf("error creating playlist: %w", playlistErr))
		return nil
	}
	logger.Info("Playlist for route successfully created", slog.String("route", route))
	return pl
}

func createStreamer(logger *slog.Logger, config *Config, route string) *audio.Streamer {
	logger.Info("Creating audio streamer for route", slog.String("route", route))
	streamer := audio.NewStreamer(config.BufferSize, config.MaxClients, config.StreamFormat, config.Bitrate)
	streamer.SetVolumeNormalization(config.NormalizeVolume)
	logger.Info("Audio streamer for route successfully created", slog.String("route", route))
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
	streamFormat := flag.String("format", defaultStreamFormat, "Stream format (mp3, ogg, aac)")
	bitrate := flag.Int("bitrate", defaultBitrate, "Stream bitrate")
	maxClients := flag.Int("max-clients", defaultMaxClients, "Maximum number of clients")
	logLevel := flag.String("log-level", defaultLogLevel, "Log level")
	bufferSize := flag.Int("buffer-size", defaultBufferSize, "Buffer size for audio streaming")
	shuffle := flag.Bool("shuffle", defaultShuffle, "Enable shuffle mode for all streams")
	normalizeVolume := flag.Bool("normalize", defaultNormalizeVolume, "Enable volume normalization")
	normalizeRuntime := flag.String("normalize-runtime", "auto", "Runtime normalization mode (auto, on, off)")
	normalizeSampleWindows := flag.Int("normalize-windows", 10, "Number of analysis windows for normalization")
	normalizeSampleMs := flag.Int("normalize-ms", 1000, "Duration of each analysis window in milliseconds")
	relayEnabled := flag.Bool("relay", defaultRelayEnabled, "Enable relay functionality")
	relayConfigFile := flag.String("relay-config", defaultRelayConfigFile, "Path to relay configuration file")

	flag.Parse()

	// Create configuration.
	return &Config{
		Port:                   *port,
		AudioDir:               *audioDir,
		DirectoryRoutes:        make(map[string]string),
		StreamFormat:           *streamFormat,
		Bitrate:                *bitrate,
		MaxClients:             *maxClients,
		LogLevel:               *logLevel,
		BufferSize:             *bufferSize,
		Shuffle:                *shuffle,
		PerStreamShuffle:       make(map[string]bool),
		NormalizeVolume:        *normalizeVolume,
		NormalizeRuntime:       *normalizeRuntime,
		NormalizeSampleWindows: *normalizeSampleWindows,
		NormalizeSampleMs:      *normalizeSampleMs,
		RelayEnabled:           *relayEnabled,
		RelayConfigFile:        *relayConfigFile,
	}
}

// loadConfigFromEnv загружает конфигурацию из переменных окружения.
func loadConfigFromEnv(config *Config) {
	// Load directory routes from environment.
	loadDirectoryRoutesFromEnv(config)

	// Load per-stream shuffle settings from environment.
	loadShuffleSettingsFromEnv(config)
}

// loadDirectoryRoutesFromEnv загружает маршруты директорий из переменных окружения.
func loadDirectoryRoutesFromEnv(config *Config) {
	logger := setupLogger(config.LogLevel)
	dirRoutes := getEnvOrDefault("DIRECTORY_ROUTES", "")
	if dirRoutes == "" {
		return
	}

	// Try to parse as JSON first.
	var directoryRoutes map[string]string
	err := json.Unmarshal([]byte(dirRoutes), &directoryRoutes)
	if err == nil {
		// JSON parsing succeeded.
		logger.Debug("Successfully parsed DIRECTORY_ROUTES as JSON", "directoryRoutes", directoryRoutes)
		addRoutesToConfig(config, directoryRoutes)
		return
	}

	// Log the error for debugging.
	logger.Debug("Error parsing DIRECTORY_ROUTES as JSON", "error", err, "fallback", "Using comma-separated format")

	// Fallback to old comma-separated format.
	parseCommaSeparatedRoutes(config, dirRoutes)
}

// addRoutesToConfig добавляет маршруты в конфигурацию, обеспечивая правильный формат.
func addRoutesToConfig(config *Config, routes map[string]string) {
	for route, dir := range routes {
		// Ensure route starts with slash.
		if len(route) > 0 && route[0] != '/' {
			route = "/" + route
		}
		config.DirectoryRoutes[route] = dir
	}
}

// parseCommaSeparatedRoutes парсит маршруты в формате через запятую.
func parseCommaSeparatedRoutes(config *Config, dirRoutes string) {
	routes := strings.Split(dirRoutes, ",")
	for _, route := range routes {
		parts := strings.SplitN(route, ":", maxSplitParts)
		if len(parts) == maxSplitParts {
			routePath := strings.TrimSpace(parts[0])
			dirPath := strings.TrimSpace(parts[1])

			// Ensure route starts with slash.
			if routePath[0] != '/' {
				routePath = "/" + routePath
			}

			config.DirectoryRoutes[routePath] = dirPath
		}
	}
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
			case "true":
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
				sentry.CaptureException(reloadErr)
			} else {
				logger.Info("Playlist successfully reloaded", slog.String("route", path))
			}
		}

		return nil
	}); walkErr != nil {
		logger.Error("Error walking routes for playlist reload",
			slog.String("error", walkErr.Error()))
		sentry.CaptureException(walkErr)
	}

	logger.Info("Playlist reload complete")
}
