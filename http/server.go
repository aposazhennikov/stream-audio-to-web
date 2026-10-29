// Package http implements the HTTP server for audio streaming.
// It provides endpoints for streaming audio, health checks, and status monitoring.
package http

import (
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/aposazhennikov/stream-audio-to-web/audio"
	"github.com/aposazhennikov/stream-audio-to-web/relay"
	sentryhelper "github.com/aposazhennikov/stream-audio-to-web/sentry_helper"
	"github.com/aposazhennikov/stream-audio-to-web/telegram"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// StreamHandler interface for handling audio stream.
type StreamHandler interface {
	AddClient() (<-chan []byte, int, error)
	RemoveClient(clientID int)
	GetClientCount() int
	GetCurrentTrackChannel() <-chan string
	GetPlaybackInfo() (string, time.Time, time.Duration, time.Duration)
}

// PlaylistManager interface for playlist management.
type PlaylistManager interface {
	Reload() error
	GetCurrentTrack() interface{}
	NextTrack() interface{}
	GetHistory() []interface{}  // Get track history.
	GetStartTime() time.Time    // Get start time.
	PreviousTrack() interface{} // Switch to previous track.
	Shuffle()                   // Shuffle the playlist.
	GetShuffleEnabled() bool    // Get current shuffle status.
	SetShuffleEnabled(bool)     // Set shuffle status.
}

// Server represents HTTP server for audio streaming.
type Server struct {
	router              *mux.Router
	streams             map[string]StreamHandler
	playlists           map[string]PlaylistManager
	maxClients          int
	mutex               sync.RWMutex
	currentTracks       map[string]string
	trackMutex          sync.RWMutex
	statusPassword      string                                    // Password for accessing /status page.
	stationManager      interface{ RestartPlayback(string) bool } // Interface for restarting playback.
	relayManager        *relay.Manager                            // Manager for relay functionality.
	globalShuffleConfig bool                                      // Global shuffle configuration.
	// Prometheus metrics.
	listenerCount                *prometheus.GaugeVec       // Gauge for tracking active listeners.
	bytesSent                    *prometheus.CounterVec     // Counter for bytes sent.
	trackSecondsTotal            *prometheus.CounterVec     // Counter for audio playback time.
	logger                       *slog.Logger               // Logger for server operations.
	sentryHelper                 *sentryhelper.SentryHelper // Helper для безопасной работы с Sentry.
	telegramManager              *telegram.Manager          // Manager for telegram alerts.
	telegramFunctionalityEnabled bool                       // Whether telegram functionality is enabled via environment.
	relayFunctionalityEnabled    bool                       // Whether relay functionality is enabled via environment.
}

const xmlHTTPRequestHeader = "X-Requested-With"

const (
	defaultStatusPassword  = "1234554321"
	asciiMax               = 127
	shortSleepMs           = 50
	kb                     = 1024
	mb                     = 1024 * 1024
	cookieExpireHours      = 24
	logIntervalSec         = 5
	logEveryBytes          = mb               // Log every sent megabyte.
	streamSilenceThreshold = 30 * time.Second // Stream without audio data for longer is considered down.
	clientWriteTimeout     = 30 * time.Second // A client that accepts no data for longer is disconnected.
)

// NewServer creates a new HTTP server.
func NewServer(maxClients int, logger *slog.Logger, sentryHelper *sentryhelper.SentryHelper) *Server {
	// Get password for status page from environment variable.
	statusPassword := getEnvOrDefault("STATUS_PASSWORD", defaultStatusPassword)

	// Create Prometheus metrics.
	listenerCount := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "audio_stream_listeners",
			Help: "Number of active listeners per stream",
		},
		[]string{"stream"},
	)

	bytesSent := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "audio_stream_bytes_sent_total",
			Help: "Total number of bytes sent to clients",
		},
		[]string{"stream"},
	)

	trackSecondsTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "audio_stream_track_seconds_total",
			Help: "Total seconds of audio played",
		},
		[]string{"stream"},
	)

	// Register metrics.
	prometheus.MustRegister(listenerCount)
	prometheus.MustRegister(bytesSent)
	prometheus.MustRegister(trackSecondsTotal)

	// Use provided logger or default.
	if logger == nil {
		logger = slog.Default()
	}

	server := &Server{
		router:            mux.NewRouter(),
		streams:           make(map[string]StreamHandler),
		playlists:         make(map[string]PlaylistManager),
		maxClients:        maxClients,
		currentTracks:     make(map[string]string),
		statusPassword:    statusPassword,
		listenerCount:     listenerCount,
		bytesSent:         bytesSent,
		trackSecondsTotal: trackSecondsTotal,
		logger:            logger,
		sentryHelper:      sentryHelper,
	}

	// Setup routes.
	server.setupRoutes()

	server.logger.Info(
		"HTTP server created",
		slog.Int("maxClients", maxClients),
	)
	return server
}

// Helper function to get environment variable value.
func getEnvOrDefault(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

// Handler returns HTTP request handler.
func (s *Server) Handler() http.Handler {
	return s.router
}

// RegisterStream registers a new audio stream.
func (s *Server) RegisterStream(route string, stream StreamHandler, playlist PlaylistManager) {
	s.logger.Debug("Starting audio stream registration", slog.String("route", route))

	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Make sure route starts with a slash.
	if route[0] != '/' {
		route = "/" + route
		s.logger.Info("Fixed route during registration", slog.String("route", route))
	}

	s.logger.Debug("Adding stream to streams map", slog.String("route", route))
	s.streams[route] = stream
	s.logger.Debug("Adding playlist to playlists map", slog.String("route", route))
	s.playlists[route] = playlist

	// Set trackSecondsTotal metric directly to stream.
	if streamer, ok := stream.(*audio.Streamer); ok {
		streamer.SetTrackSecondsMetric(s.trackSecondsTotal)
		s.logger.Debug("trackSecondsTotal metric set for streamer", slog.String("route", route))
	}

	// Check if stream was added.
	if _, exists := s.streams[route]; exists {
		s.logger.Debug("Stream for route successfully added to streams map", slog.String("route", route))
	} else {
		s.logger.Error("ERROR: Stream for route was not added to streams map!", slog.String("route", route))
	}

	// IMPORTANT: Register route handler in router for GET and HEAD requests.
	s.router.HandleFunc(route, s.StreamAudioHandler(route)).Methods("GET", "HEAD")
	s.logger.Debug("HTTP handler registered for route", slog.String("route", route))

	// Start goroutine to track current track.
	s.logger.Debug("Starting goroutine to track current track", slog.String("route", route))
	go s.trackCurrentTrack(route, stream.GetCurrentTrackChannel())

	s.logger.Debug("Audio stream for route successfully registered", slog.String("route", route))
}

// IsStreamRegistered checks if stream with specified route is registered.
func (s *Server) IsStreamRegistered(route string) bool {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	// Check if stream exists for the specified route.
	_, exists := s.streams[route]
	return exists
}

// hasUnicodeChars checks for non-ASCII characters in string.
func hasUnicodeChars(s string) bool {
	for _, char := range s {
		if char > asciiMax {
			return true
		}
	}
	return false
}

// isAjaxRequest checks if request is an AJAX request.
func isAjaxRequest(r *http.Request) bool {
	return r.Header.Get(xmlHTTPRequestHeader) == "XMLHttpRequest"
}

// setupRoutes configures HTTP server routes.
func (s *Server) setupRoutes() {
	// Monitoring and health endpoints.
	s.router.HandleFunc("/healthz", s.healthzHandler).Methods("GET", "HEAD")
	s.router.HandleFunc("/readyz", s.readyzHandler).Methods("GET", "HEAD")
	s.router.Handle("/metrics", promhttp.Handler()).Methods("GET")
	s.router.HandleFunc("/version", s.versionHandler).Methods("GET")

	// API for playlist management.
	s.router.HandleFunc("/streams", s.streamsHandler).Methods("GET")
	s.router.HandleFunc("/stream/status", s.streamStatusHandler).Methods("GET")
	s.router.HandleFunc("/reload-playlist", s.reloadPlaylistHandler).Methods("POST")
	s.router.HandleFunc("/now-playing", s.nowPlayingHandler).Methods("GET")
	s.router.HandleFunc("/playback-time", s.playbackTimeHandler).Methods("GET")

	// Add status page with password check.
	s.router.HandleFunc("/status", s.statusLoginHandler).Methods("GET", "HEAD")
	s.router.HandleFunc("/status", s.statusLoginSubmitHandler).Methods("POST")
	s.router.HandleFunc("/status-page", s.statusPageHandler).Methods("GET")
	s.router.HandleFunc("/status-page/", s.statusPageHandler).Methods("GET")

	// Add handlers for track switching.
	s.router.HandleFunc("/next-track/{route}", s.nextTrackHandler).Methods("POST")
	s.router.HandleFunc("/prev-track/{route}", s.prevTrackHandler).Methods("POST")
	s.router.HandleFunc("/track/{route}", s.trackHandler).Methods("GET")

	// Endpoint to shuffle playlist manually.
	s.router.HandleFunc("/shuffle-playlist/{route}", s.handleShufflePlaylist).Methods("POST")

	// Endpoint to set shuffle mode for specific stream.
	s.router.HandleFunc("/set-shuffle/{route}/{mode}", s.handleSetShuffleMode).Methods("POST")

	// Endpoint to clear track history for specific stream.
	s.router.HandleFunc("/clear-history/{route}", s.handleClearHistory).Methods("POST")

	// NOTE: Relay and Telegram routes will be set up later after managers are created.
	// Do NOT call setupRelayRoutes() and setupTelegramRoutes() here - they need managers to be set first.

	// Add static files for web interface.
	s.router.PathPrefix("/web/").Handler(http.StripPrefix("/web/", http.FileServer(http.Dir("./web"))))

	// Add favicon and image files.
	s.router.PathPrefix("/image/").Handler(http.StripPrefix("/image/", http.FileServer(http.Dir("./image"))))

	// Handle favicon.ico requests in root.
	s.router.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./image/favicon.ico")
	})

	// Configure 404 handler.
	s.router.NotFoundHandler = http.HandlerFunc(s.notFoundHandler)

	s.logger.Info("HTTP routes configured", slog.String("status", "done"))
}

// SetStationManager sets the station manager for the server.
func (s *Server) SetStationManager(manager interface{ RestartPlayback(string) bool }) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.stationManager = manager
	s.logger.Info("Station manager set for HTTP server")
}

// SetRelayManager sets the relay manager for the server.
func (s *Server) SetRelayManager(manager *relay.Manager) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.relayManager = manager
	s.logger.Info("Relay manager set for HTTP server")
}

// SetTelegramManager sets the telegram manager for the server.
func (s *Server) SetTelegramManager(manager *telegram.Manager) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.telegramManager = manager
	s.logger.Info("Telegram manager set for HTTP server")
}

// SetTelegramFunctionalityEnabled sets whether telegram functionality is enabled via environment.
func (s *Server) SetTelegramFunctionalityEnabled(enabled bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.telegramFunctionalityEnabled = enabled
	s.logger.Info("Telegram functionality enabled flag set", "enabled", enabled)
}

// SetRelayFunctionalityEnabled sets whether relay functionality is enabled via environment.
func (s *Server) SetRelayFunctionalityEnabled(enabled bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.relayFunctionalityEnabled = enabled
	s.logger.Info("Relay functionality enabled flag set", "enabled", enabled)
}

// SetStatusPassword sets the password for accessing the status page.
func (s *Server) SetStatusPassword(password string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.statusPassword = password
	s.logger.Info("Status password set for HTTP server")
}

// SetupTelegramRoutes is a public method to configure telegram routes after telegram manager is set
func (s *Server) SetupTelegramRoutes() {
	s.setupTelegramRoutes()
}

// setupTelegramRoutes configures all telegram-related routes in the router.
func (s *Server) setupTelegramRoutes() {
	// Only setup telegram routes if a telegram manager is available
	if s.telegramManager == nil {
		s.logger.Info("Telegram manager not available, skipping telegram routes setup")
		return
	}

	s.logger.Info("Setting up telegram routes")

	// Telegram alerts management page
	s.router.HandleFunc("/telegram-alerts", s.telegramAlertsHandler).Methods("GET")

	// Update telegram alerts configuration
	s.router.HandleFunc("/telegram-alerts/update", s.telegramAlertsUpdateHandler).Methods("POST")

	// Test telegram alerts
	s.router.HandleFunc("/telegram-alerts/test", s.telegramAlertsTestHandler).Methods("POST")

	s.logger.Info("Telegram routes setup complete")
}
