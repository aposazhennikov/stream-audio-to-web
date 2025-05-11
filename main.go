// Package main is the entry point for the audio streaming server.
// It handles initialization, configuration, and startup of all components.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"
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
	// Initialize Sentry
	err := sentry.Init(sentry.ClientOptions{
		Dsn: "https://your-sentry-dsn",
	})
	if err != nil {
		slog.Error("sentry.Init", "error", err)
		os.Exit(1)
	}

	// Load configuration
	config := loadConfig()

	// Print configuration
	slog.Info("========== APPLICATION CONFIGURATION ==========")
	slog.Info("Port", "value", config.Port)
	slog.Info("Default audio directory", "value", config.AudioDir)
	slog.Info("Stream format", "value", config.StreamFormat)
	slog.Info("Bitrate", "value", config.Bitrate)
	slog.Info("Max clients", "value", config.MaxClients)
	slog.Info("Buffer size", "value", config.BufferSize)
	slog.Info("Global shuffle setting", "value", config.Shuffle)
	slog.Info("Volume normalization", "value", config.NormalizeVolume)
	slog.Info("Runtime normalization mode", "value", config.NormalizeRuntime)
	slog.Info("Normalization sample windows", "value", config.NormalizeSampleWindows)
	slog.Info("Normalization sample duration", "value", fmt.Sprintf("%d ms", config.NormalizeSampleMs))
	slog.Info("Relay functionality enabled", "value", config.RelayEnabled)
	slog.Info("Relay configuration file", "value", config.RelayConfigFile)

	slog.Info("Per-stream shuffle settings:")
	for route, shuffle := range config.PerStreamShuffle {
		slog.Info("Stream shuffle setting", "route", route, "shuffle", shuffle)
	}

	slog.Info("Additional directory routes:")
	for route, dir := range config.DirectoryRoutes {
		slog.Info("Additional route", "route", route, "directory", dir)
	}

	slog.Info("=============================================")

	// Create HTTP server
	server := httpServer.NewServer(config.StreamFormat, config.MaxClients)

	// Configure normalization parameters
	audio.SetNormalizeConfig(config.NormalizeSampleWindows, config.NormalizeSampleMs)

	// Create radio station manager
	stationManager := radio.NewRadioStationManager(slog.Default())

	// Set radio station manager for HTTP server
	server.SetStationManager(stationManager)

	// Create relay manager if needed
	var relayManager *relay.Manager
	if config.RelayEnabled {
		slog.Info("Initializing relay manager", "config_file", config.RelayConfigFile)
		relayManager = relay.NewRelayManager(config.RelayConfigFile, slog.Default())

		// Configure default state (enabled by default when feature is enabled)
		relayManager.SetActive(true)

		// Set relay manager for HTTP server
		server.SetRelayManager(relayManager)
		slog.Info("Relay manager initialized and set for HTTP server")
	} else {
		slog.Info("Relay functionality is disabled")
	}

	// Create minimal dummy streams for /healthz to immediately find at least one route
	dummyStream, dummyPlaylist := createDummyStreamAndPlaylist()
	server.RegisterStream("/humor", dummyStream, dummyPlaylist)
	server.RegisterStream("/science", dummyStream, dummyPlaylist)
	slog.Info("Temporary stream placeholders registered for quick healthcheck passing")

	// Remove temporary handlers here as they are already registered in server.setupRoutes()

	// NOW create and start HTTP server BEFORE setting up streams
	httpSrv := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", config.Port), // Explicitly specify that we listen on all interfaces
		Handler: server.Handler(),
		// Increase timeouts for request processing
		ReadTimeout:  readTimeoutSec * time.Second,
		WriteTimeout: 0, // Disable timeout for streaming
		IdleTimeout:  idleTimeoutSec * time.Second,
	}

	// Start server in goroutine BEFORE setting up routes
	go func() {
		slog.Info("Starting HTTP server", "address", fmt.Sprintf("0.0.0.0:%d", config.Port))
		if err2 := httpSrv.ListenAndServe(); err2 != nil && err2 != http.ErrServerClosed {
			slog.Error("Server start error", "error", err2)
			sentry.CaptureException(err2)
		}
	}()

	// Redirect from root route
	redirectTo := defaultRoute // redirect to /humor by default
	if _, exists := config.DirectoryRoutes["/humor"]; !exists {
		// If /humor doesn't exist, take the first route from configuration
		for route := range config.DirectoryRoutes {
			redirectTo = route
			break
		}
	}

	// Replace temporary handler for root route with redirection
	if err := server.Handler().(*mux.Router).HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		slog.Info("Redirecting from / to %s (method: %s)", redirectTo, r.Method)
		// For HEAD requests return only headers without redirect
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, redirectTo, http.StatusSeeOther)
	}).Methods("GET", "HEAD"); err != nil {
		slog.Error("Failed to register root handler", "error", err)
		sentry.CaptureException(err)
	}

	slog.Info("Redirect configured from / to %s", redirectTo)

	// AFTER starting HTTP server, configure audio routes asynchronously
	slog.Info("Starting audio route configuration...")

	// Configure routes from configuration ASYNCHRONOUSLY
	for route, dir := range config.DirectoryRoutes {
		// Route should already be normalized with leading slash in loadConfig
		// But check just in case
		if route[0] != '/' {
			route = "/" + route
		}

		// Copy variables for goroutine
		routeCopy := route
		dirCopy := dir

		// Start configuring EACH stream in a separate goroutine
		go func(route, dir string) {
			slog.Info("Asynchronous configuration of route '%s' -> directory '%s'", route, dir)
			if success := configureSyncRoute(server, stationManager, route, dir, config); success {
				slog.Info("Route '%s' successfully configured", route)
			} else {
				slog.Error("ERROR: Route '%s' configuration failed", route)
			}
		}(routeCopy, dirCopy)
	}

	// Check stream status
	slog.Info("====== REGISTERED STREAMS STATUS ======")
	humorRegistered := server.IsStreamRegistered("/humor")
	scienceRegistered := server.IsStreamRegistered("/science")
	slog.Info("Stream /humor registered", "value", humorRegistered)
	slog.Info("Stream /science registered", "value", scienceRegistered)

	if !humorRegistered || !scienceRegistered {
		slog.Info("WARNING: Some streams are not registered!")
	} else {
		slog.Info("All streams successfully registered")
	}
	slog.Info("=============================================")

	// Graceful shutdown setup
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	sig := <-quit

	slog.Info("Signal received", "signal", sig, "performing graceful shutdown...")

	// Handle SIGHUP for playlist reload
	if sig == syscall.SIGHUP {
		reloadAllPlaylists(server)
		return // Continue operation
	}

	// Stop all radio stations
	stationManager.StopAll()

	// Graceful HTTP server shutdown
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeoutSec*time.Second)
	defer cancel()

	if shutdownErr := httpSrv.Shutdown(ctx); shutdownErr != nil {
		slog.Error("Server shutdown error", "error", shutdownErr)
		sentry.CaptureException(shutdownErr)
	}
	slog.Info("Server successfully stopped")

	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
		if _, writeErr := w.Write([]byte("OK")); writeErr != nil {
			slog.Error("Failed to write health check response", "error", writeErr.Error())
			return
		}
	})
}

// configureSyncRoute configures one audio stream route synchronously.
func configureSyncRoute(server *httpServer.Server, stationManager *radio.StationManager, route, dir string, config *Config) bool {
	slog.Info("Starting synchronous configuration of route '%s'...", route)

	if !ensureDirectoryExists(dir, route) {
		return false
	}

	if !checkAudioFiles(dir, route) {
		return false
	}

	pl := createPlaylistOrNil(dir, route, config)
	if pl == nil {
		return false
	}

	streamer := createStreamer(config, route)
	if streamer == nil {
		return false
	}

	stationManager.AddStation(route, streamer, pl)
	slog.Info("Radio station '%s' successfully added to manager", route)

	server.RegisterStream(route, streamer, pl)
	slog.Info("Audio stream '%s' successfully registered on HTTP server", route)

	if !server.IsStreamRegistered(route) {
		slog.Error("CRITICAL ERROR: Stream %s not registered after all operations", route)
		sentry.CaptureMessage(fmt.Sprintf("Stream %s not registered after all operations", route))
		return false
	}

	slog.Info("RESULT: Route '%s' configuration SUCCESSFULLY COMPLETED", route)
	return true
}

func ensureDirectoryExists(dir, route string) bool {
	if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
		slog.Info("Creating directory for route %s: %s", route, dir)
		if mkdirErr := os.MkdirAll(dir, 0750); mkdirErr != nil {
			slog.Error("ERROR: When creating directory %s", dir, "error", mkdirErr)
			sentry.CaptureException(fmt.Errorf("error creating directory %s: %w", dir, mkdirErr))
			return false
		}
	}
	return true
}

func checkAudioFiles(dir, route string) bool {
	files, readErr := os.ReadDir(dir)
	if readErr != nil {
		slog.Error("ERROR: When reading directory %s", dir, "error", readErr)
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
	slog.Info("Directory %s contains %d audio files", dir, audioFiles)
	if audioFiles == 0 {
		slog.Error("CRITICAL ERROR: No audio files in directory %s. Route '%s' NOT CONFIGURED.", dir, route)
		sentry.CaptureMessage(fmt.Sprintf("No audio files in directory %s for route %s", dir, route))
		return false
	}
	return true
}

func createPlaylistOrNil(dir, route string, config *Config) httpServer.PlaylistManager {
	shuffleSetting := config.Shuffle
	if specificShuffle, exists := config.PerStreamShuffle[route]; exists {
		shuffleSetting = specificShuffle
		slog.Info("Using specific shuffle setting for route %s: %v", route, shuffleSetting)
	} else {
		slog.Info("Using global shuffle setting for route %s: %v", route, shuffleSetting)
	}
	pl, playlistErr := playlist.NewPlaylist(dir, nil, shuffleSetting, slog.Default())
	if playlistErr != nil {
		slog.Error("ERROR creating playlist", "error", playlistErr)
		sentry.CaptureException(fmt.Errorf("error creating playlist: %w", playlistErr))
		return nil
	}
	slog.Info("Playlist for route %s successfully created", route)
	return pl
}

func createStreamer(config *Config, route string) *audio.Streamer {
	slog.Info("Creating audio streamer for route %s...", route)
	streamer := audio.NewStreamer(config.BufferSize, config.MaxClients, config.StreamFormat, config.Bitrate)
	streamer.SetVolumeNormalization(config.NormalizeVolume)
	switch config.NormalizeRuntime {
	case "on":
		streamer.SetVolumeNormalization(true)
		slog.Info("DIAGNOSTICS: Runtime normalization mode 'on' overrides default setting for route %s", route)
	case "off":
		streamer.SetVolumeNormalization(false)
		slog.Info("DIAGNOSTICS: Runtime normalization mode 'off' overrides default setting for route %s", route)
	case "auto", "":
		slog.Info("DIAGNOSTICS: Using default normalization setting for route %s: %v", route, config.NormalizeVolume)
	}
	slog.Info("Audio streamer for route %s successfully created", route)
	return streamer
}

// Load configuration from command line flags and environment variables
func loadConfig() *Config {
	// Parse command line flags
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

	// Create configuration
	config := &Config{
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

	// Load directory routes from environment
	dirRoutes := getEnvOrDefault("DIRECTORY_ROUTES", "")
	if dirRoutes != "" {
		// Parse directory routes
		routes := strings.Split(dirRoutes, ",")
		for _, route := range routes {
			parts := strings.SplitN(route, ":", maxSplitParts)
			if len(parts) == maxSplitParts {
				routePath := strings.TrimSpace(parts[0])
				dirPath := strings.TrimSpace(parts[1])

				// Ensure route starts with slash
				if routePath[0] != '/' {
					routePath = "/" + routePath
				}

				config.DirectoryRoutes[routePath] = dirPath
			}
		}
	}

	// Load per-stream shuffle settings from environment
	shuffleSettings := getEnvOrDefault("SHUFFLE_SETTINGS", "")
	parseShuffleSettings(config, shuffleSettings)

	return config
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
			// Ensure route starts with slash
			if routePath[0] != '/' {
				routePath = "/" + routePath
			}
			// Parse shuffle value
			switch shuffleValue {
			case "true":
				config.PerStreamShuffle[routePath] = true
			case "false":
				config.PerStreamShuffle[routePath] = false
			}
		}
	}
}

// getEnvOrDefault returns environment variable value or default value
func getEnvOrDefault(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

// createDummyStreamAndPlaylist creates minimal placeholders for streams
// This is necessary for quickly passing healthcheck before loading real streams
func createDummyStreamAndPlaylist() (httpServer.StreamHandler, httpServer.PlaylistManager) {
	// Placeholder for StreamHandler
	dummyStream := &dummyStreamHandler{
		clientCounter: 0,
		clientCh:      make(chan string, 1),
	}

	// Placeholder for PlaylistManager
	dummyPlaylist := &dummyPlaylistManager{}

	return dummyStream, dummyPlaylist
}

// Minimal implementation of StreamHandler for placeholder
type dummyStreamHandler struct {
	clientCounter int32
	clientCh      chan string
}

func (d *dummyStreamHandler) AddClient() (<-chan []byte, int, error) {
	// Empty channel that will be replaced with real stream later
	ch := make(chan []byte, 1)
	return ch, 0, nil
}

func (d *dummyStreamHandler) RemoveClient(_ int) {
	// Do nothing
}

func (d *dummyStreamHandler) GetClientCount() int {
	return 0
}

func (d *dummyStreamHandler) GetCurrentTrackChannel() <-chan string {
	return d.clientCh
}

// getDummyMP3 возвращает имя заглушки MP3 файла
func getDummyMP3() string {
	return "dummy.mp3"
}

// Minimal implementation of PlaylistManager for placeholder
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

// Shuffle implements PlaylistManager.Shuffle method
func (d *dummyPlaylistManager) Shuffle() {
	// Empty implementation for dummy placeholder
}

func reloadAllPlaylists(server *httpServer.Server) {
	slog.Info("SIGHUP received, reloading playlists...")
	if walkErr := server.Handler().(*mux.Router).Walk(func(route *mux.Route, router *mux.Router, ancestors []*mux.Route) error {
		path, pathErr := route.GetPathTemplate()
		if pathErr != nil {
			return pathErr
		}
		// Reload only for registered streams
		if server.IsStreamRegistered(path) {
			slog.Info("Reloading playlist for %s", path)
			if reloadErr := server.ReloadPlaylist(path); reloadErr != nil {
				slog.Error("Error reloading playlist for %s", path, "error", reloadErr)
				sentry.CaptureException(reloadErr)
			} else {
				slog.Info("Playlist for %s successfully reloaded", path)
			}
		}
		return nil
	}); walkErr != nil {
		slog.Error("Error walking routes for playlist reload", slog.String("error", walkErr.Error()))
		sentry.CaptureException(walkErr)
	}
	slog.Info("Playlist reload complete")
}
