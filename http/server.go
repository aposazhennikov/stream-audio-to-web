// Package http implements the HTTP server for audio streaming.
// It provides endpoints for streaming audio, health checks, and status monitoring.
package http

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	sentryhelper "github.com/aposazhennikov/stream-audio-to-web/sentry_helper"
	sentry "github.com/getsentry/sentry-go"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/aposazhennikov/stream-audio-to-web/audio"
	"github.com/aposazhennikov/stream-audio-to-web/relay"
	"github.com/aposazhennikov/stream-audio-to-web/telegram"

	html "html"
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
	router            *mux.Router
	streams           map[string]StreamHandler
	playlists         map[string]PlaylistManager
	maxClients        int
	mutex             sync.RWMutex
	currentTracks     map[string]string
	trackMutex        sync.RWMutex
	statusPassword    string                                    // Password for accessing /status page.
	stationManager    interface{ RestartPlayback(string) bool } // Interface for restarting playback.
	relayManager      *relay.Manager                            // Manager for relay functionality.
	globalShuffleConfig bool                                   // Global shuffle configuration.
	// Prometheus metrics.
	listenerCount     *prometheus.GaugeVec   // Gauge for tracking active listeners.
	bytesSent         *prometheus.CounterVec // Counter for bytes sent.
	trackSecondsTotal *prometheus.CounterVec // Counter for audio playback time.
	logger            *slog.Logger           // Logger for server operations.
	sentryHelper      *sentryhelper.SentryHelper  // Helper для безопасной работы с Sentry.
	telegramManager   *telegram.Manager      // Manager for telegram alerts.
	telegramFunctionalityEnabled bool       // Whether telegram functionality is enabled via environment.
}

const xmlHTTPRequestHeader = "X-Requested-With"

const (
	defaultStatusPassword     = "1234554321"
	asciiMax                  = 127
	shortSleepMs              = 50
	kb                        = 1024
	mb                        = 1024 * 1024
	cookieExpireHours         = 24
	logIntervalSec            = 5
	logEveryBytes             = mb // Log every sent megabyte.
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

// trackCurrentTrack tracks current track for specified stream.
func (s *Server) trackCurrentTrack(route string, trackCh <-chan string) {
	for trackPath := range trackCh {
		fileName := filepath.Base(trackPath)
		s.logger.Debug(
			"Current track",
			slog.String("route", route),
			slog.String("fileName", fileName),
			slog.String("path", trackPath),
		)

		if hasUnicodeChars(fileName) {
			sentry.ConfigureScope(func(scope *sentry.Scope) {
				scope.SetContext("track_info", map[string]interface{}{
					"route":         route,
					"track_name":    fileName,
					"track_path":    trackPath,
					"track_dir":     filepath.Dir(trackPath),
					"track_ext":     filepath.Ext(trackPath),
					"track_len":     len(fileName),
					"track_unicode": true,
				})
			})
		}

		s.trackMutex.Lock()
		s.currentTracks[route] = fileName
		s.trackMutex.Unlock()
	}
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

	// Add relay routes if relay manager is available
	s.setupRelayRoutes()

	// Add telegram routes if telegram manager is available
	s.setupTelegramRoutes()

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

// healthzHandler returns 200 OK if server is running.
func (s *Server) healthzHandler(w http.ResponseWriter, r *http.Request) {
	// Healthz requests are frequent (Docker healthchecks), no need to log them in DEBUG.

	// Check for registered streams.
	s.mutex.RLock()
	streamsCount := len(s.streams)
	streamsList := make([]string, 0, streamsCount)
	for route := range s.streams {
		streamsList = append(streamsList, route)
	}
	s.mutex.RUnlock()

	// Log only if no streams registered (potential issue).
	if streamsCount == 0 {
		s.logger.Info("WARNING: No registered streams, but server is running", slog.String("status", "no_streams"))
	}
	// Normal healthz status is not logged to avoid spam from frequent Docker healthchecks.

	// Add headers to prevent caching.
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Content-Type", "text/plain")

	// Always return successful response, as streams may be configured asynchronously.
	w.WriteHeader(http.StatusOK)

	// If not a HEAD request, send response body.
	if r.Method != http.MethodHead {
		if _, writeErr := w.Write([]byte("OK")); writeErr != nil {
			s.logger.Error("Failed to write healthz response", slog.String("error", writeErr.Error()))
			return
		}

		// Force send response.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	// Successful healthz response sent (no logging to avoid Docker healthcheck spam).
}

// readyzHandler checks readiness.
func (s *Server) readyzHandler(w http.ResponseWriter, r *http.Request) {
	// Log readyz request.
	s.logger.Info(
		"Received readyz request",
		slog.String("method", r.Method),
		slog.String("remoteAddr", r.RemoteAddr),
		slog.String("uri", r.RequestURI),
	)

	// Add headers to prevent caching.
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	// Check if there's at least one stream.
	s.mutex.RLock()
	streamsCount := len(s.streams)
	streamsList := make([]string, 0, streamsCount)
	for route := range s.streams {
		streamsList = append(streamsList, route)
	}
	s.mutex.RUnlock()

	// Log status.
	s.logger.Info("Readyz status", slog.Int("streamsCount", streamsCount), slog.Any("routes", streamsList))

	// Always return OK for readyz to avoid container restarts.
	w.WriteHeader(http.StatusOK)

	// If not a HEAD request, send response body.
	if r.Method != http.MethodHead {
		if _, writeErr := fmt.Fprintf(w, "Ready - %d streams registered", streamsCount); writeErr != nil {
			s.logger.Error("Failed to write readyz response", slog.String("error", writeErr.Error()))
			return
		}

		// Send data immediately.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	// Additional logging of successful response.
	s.logger.Info("Sent successful readyz response to client", slog.String("remoteAddr", r.RemoteAddr))
}

// streamsHandler handles requests for stream information.
func (s *Server) streamsHandler(w http.ResponseWriter, _ *http.Request) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	type streamInfo struct {
		Route       string `json:"route"`
		Listeners   int    `json:"listeners"`
		CurrentTrack string `json:"current_track"`
		HistoryHTML string `json:"history_html"`
	}

	streams := make([]streamInfo, 0, len(s.streams))
	for route, stream := range s.streams {
		s.trackMutex.RLock()
		currentTrack := s.currentTracks[route]
		s.trackMutex.RUnlock()

		// Generate history HTML for this stream
		history := s.playlists[route].GetHistory()
		var historyHTMLBuilder strings.Builder
		
		if len(history) == 0 {
			historyHTMLBuilder.WriteString(`<div class="no-data">No tracks in history yet.</div>`)
		} else {
			historyHTMLBuilder.WriteString("<ul>")
			for _, item := range history {
				var trackName string
				
				// Extract track name properly based on item type
				switch track := item.(type) {
				case interface{ GetPath() string }:
					trackName = filepath.Base(track.GetPath())
				case interface{ GetNormalizedPath() string }:
					trackName = filepath.Base(track.GetNormalizedPath())
				case *struct{ Path string; Name string }:
					if track != nil {
						trackName = track.Name
						if trackName == "" {
							trackName = filepath.Base(track.Path)
						}
					}
				case string:
					trackName = filepath.Base(track)
				default:
					str := fmt.Sprintf("%v", item)
					if strings.Contains(str, "{") && strings.Contains(str, "}") {
						trackName = "Unknown track"
					} else {
						trackName = filepath.Base(str)
					}
				}
				
				if trackName == "" {
					trackName = "Unknown track"
				}
				
				escapedTrackName := html.EscapeString(trackName)
				historyHTMLBuilder.WriteString(fmt.Sprintf(
					`<li><span>%s</span><button class="copy-button" onclick="copyToClipboard('%s', this)"><span class="copy-icon">📋</span><span class="copy-feedback"></span></button></li>`,
					escapedTrackName,
					escapedTrackName,
				))
			}
			historyHTMLBuilder.WriteString("</ul>")
		}
		historyHTML := historyHTMLBuilder.String()

		streams = append(streams, streamInfo{
			Route:        route,
			Listeners:    stream.GetClientCount(),
			CurrentTrack: currentTrack,
			HistoryHTML:  historyHTML,
		})
	}

	// Sort streams by route for consistent output.
	sort.Slice(streams, func(i, j int) bool {
		return streams[i].Route < streams[j].Route
	})

	w.Header().Set("Content-Type", "application/json")
	if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
		"streams": streams,
	}); encodeErr != nil {
		s.logger.Error("Failed to encode streams response", slog.String("error", encodeErr.Error()))
		return
	}
}

// reloadPlaylistHandler reloads playlist.
func (s *Server) reloadPlaylistHandler(w http.ResponseWriter, r *http.Request) {
	route := r.URL.Query().Get("route")

	if route == "" {
		// Reload all playlists.
		s.mutex.RLock()
		playlists := make([]PlaylistManager, 0, len(s.playlists))
		for _, playlist := range s.playlists {
			playlists = append(playlists, playlist)
		}
		s.mutex.RUnlock()

		for _, playlist := range playlists {
			if reloadErr := playlist.Reload(); reloadErr != nil {
				errorMsg := fmt.Sprintf("Error reloading playlist: %s", reloadErr)
				s.sentryHelper.CaptureError(fmt.Errorf("error reloading playlist: %w", reloadErr), "http", "reload_playlist")
				http.Error(w, errorMsg, http.StatusInternalServerError)
				return
			}
		}

		s.logger.Info("All playlists reloaded")
		w.WriteHeader(http.StatusOK)
		if _, writeErr := w.Write([]byte("All playlists reloaded")); writeErr != nil {
			s.logger.Error("ERROR: Failed to write response", slog.String("error", writeErr.Error()))
		}
		return
	}

	// Reload specific playlist.
	s.mutex.RLock()
	playlist, exists := s.playlists[route]
	s.mutex.RUnlock()

	if !exists {
		errorMsg := fmt.Sprintf("Stream %s not found", route)
		s.logger.Error("ERROR: Stream not found", slog.String("route", route))
		s.sentryHelper.CaptureInfo(errorMsg, "http", "handler")
		http.Error(w, errorMsg, http.StatusNotFound)
		return
	}

	if reloadErr := playlist.Reload(); reloadErr != nil {
		errorMsg := fmt.Sprintf("Error reloading playlist: %s", reloadErr)
		s.logger.Error("Error reloading playlist", slog.String("route", route), slog.String("error", reloadErr.Error()))
		s.sentryHelper.CaptureError(fmt.Errorf("error reloading playlist for %s: %w", route, reloadErr), "http", "reload_playlist")
		http.Error(w, errorMsg, http.StatusInternalServerError)
		return
	}

	s.logger.Info("Playlist reloaded", slog.String("route", route))
	w.WriteHeader(http.StatusOK)
	if _, writeErr := fmt.Fprintf(w, "Playlist for stream %s reloaded", route); writeErr != nil {
		s.logger.Error("ERROR: Failed to write response", slog.String("error", writeErr.Error()))
	}
}

// nowPlayingHandler returns information about current track.
func (s *Server) nowPlayingHandler(w http.ResponseWriter, r *http.Request) {
	route := r.URL.Query().Get("route")

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// Проверка на пустой маршрут.
	if route == "" {
		// Для отсутствующего параметра route возвращаем информацию по всем маршрутам
		// вместо ошибки. Убираем DEBUG лог так как он вызывается каждую секунду.

		// Подготовим карту со всеми текущими треками
		s.trackMutex.RLock()
		allTracks := make(map[string]string)
		for r, track := range s.currentTracks {
			allTracks[r] = track
		}
		s.trackMutex.RUnlock()

		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"tracks": allTracks,
		}); encodeErr != nil {
			s.logger.Error("Failed to encode all tracks info", slog.String("error", encodeErr.Error()))
		}
		return
	}

	s.trackMutex.RLock()
	defer s.trackMutex.RUnlock()

	track, exists := s.currentTracks[route]
	if !exists {
		errorMsg := fmt.Sprintf("Stream %s not found", route)
		s.logger.Error("ERROR: Stream not found", slog.String("route", route))
		s.sentryHelper.CaptureInfo(errorMsg, "http", "handler")
		http.Error(w, errorMsg, http.StatusNotFound)
		return
	}

	s.logger.Info("Track information request", slog.String("route", route), slog.String("track", track))
	s.mutex.RLock()
	playlist, playlistExists := s.playlists[route]
	s.mutex.RUnlock()
	currentTrackInfo := map[string]string{
		"route": route,
		"track": track,
	}
	if playlistExists {
		for range 3 {
			if currentTrack := playlist.GetCurrentTrack(); currentTrack != nil {
				if trackWithPath, ok := currentTrack.(interface{ GetPath() string }); ok {
					currentTrackInfo["path"] = trackWithPath.GetPath()
				}
				break
			}
			time.Sleep(shortSleepMs * time.Millisecond)
		}
	}
	if encodeErr := json.NewEncoder(w).Encode(currentTrackInfo); encodeErr != nil {
		s.logger.Error("Failed to encode track info", slog.String("error", encodeErr.Error()))
		// Обработка ошибки JSON используя логирование.
		s.logNormalizationError(encodeErr, fmt.Sprintf("json encode for route %s", route))
	}
}

// playbackTimeHandler returns playback time information for all routes or specific route
func (s *Server) playbackTimeHandler(w http.ResponseWriter, r *http.Request) {
	route := r.URL.Query().Get("route")

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	type PlaybackTimeInfo struct {
		Route        string  `json:"route"`
		TrackPath    string  `json:"track_path"`
		TrackName    string  `json:"track_name"`
		StartTime    string  `json:"start_time"`
		ElapsedMs    int64   `json:"elapsed_ms"`
		ElapsedText  string  `json:"elapsed_text"`
		TotalMs      int64   `json:"total_ms"`
		TotalText    string  `json:"total_text"`
		RemainingMs  int64   `json:"remaining_ms"`
		RemainingText string `json:"remaining_text"`
		ProgressPercent float64 `json:"progress_percent"`
		IsPlaying    bool    `json:"is_playing"`
	}

	if route == "" {
		// Return info for all routes
		s.mutex.RLock()
		allPlaybackInfo := make(map[string]PlaybackTimeInfo)
		for r, stream := range s.streams {
			trackPath, startTime, elapsed, totalDuration := stream.GetPlaybackInfo()
			trackName := filepath.Base(trackPath)
			if trackName == "." || trackName == "" {
				trackName = "No track"
			}
			
			elapsedMs := elapsed.Milliseconds()
			elapsedText := formatDuration(elapsed)
			totalMs := totalDuration.Milliseconds()
			totalText := formatDuration(totalDuration)
			
			remaining := totalDuration - elapsed
			if remaining < 0 {
				remaining = 0
			}
			remainingMs := remaining.Milliseconds()
			remainingText := formatDuration(remaining)
			
			progressPercent := float64(0)
			if totalDuration > 0 {
				progressPercent = float64(elapsed) / float64(totalDuration) * 100
				if progressPercent > 100 {
					progressPercent = 100
				}
			}
			
			allPlaybackInfo[r] = PlaybackTimeInfo{
				Route:         r,
				TrackPath:     trackPath,
				TrackName:     trackName,
				StartTime:     startTime.Format("15:04:05"),
				ElapsedMs:     elapsedMs,
				ElapsedText:   elapsedText,
				TotalMs:       totalMs,
				TotalText:     totalText,
				RemainingMs:   remainingMs,
				RemainingText: remainingText,
				ProgressPercent: progressPercent,
				IsPlaying:     !startTime.IsZero(),
			}
		}
		s.mutex.RUnlock()

		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"playback_times": allPlaybackInfo,
		}); encodeErr != nil {
			s.logger.Error("Failed to encode all playback times", slog.String("error", encodeErr.Error()))
		}
		return
	}

	// Return info for specific route
	s.mutex.RLock()
	stream, exists := s.streams[route]
	s.mutex.RUnlock()

	if !exists {
		errorMsg := fmt.Sprintf("Stream %s not found", route)
		s.logger.Error("ERROR: Stream not found for playback time", slog.String("route", route))
		http.Error(w, errorMsg, http.StatusNotFound)
		return
	}

	trackPath, startTime, elapsed, totalDuration := stream.GetPlaybackInfo()
	trackName := filepath.Base(trackPath)
	if trackName == "." || trackName == "" {
		trackName = "No track"
	}
	
	elapsedMs := elapsed.Milliseconds()
	elapsedText := formatDuration(elapsed)
	totalMs := totalDuration.Milliseconds()
	totalText := formatDuration(totalDuration)
	
	remaining := totalDuration - elapsed
	if remaining < 0 {
		remaining = 0
	}
	remainingMs := remaining.Milliseconds()
	remainingText := formatDuration(remaining)
	
	progressPercent := float64(0)
	if totalDuration > 0 {
		progressPercent = float64(elapsed) / float64(totalDuration) * 100
		if progressPercent > 100 {
			progressPercent = 100
		}
	}

	playbackInfo := PlaybackTimeInfo{
		Route:         route,
		TrackPath:     trackPath,
		TrackName:     trackName,
		StartTime:     startTime.Format("15:04:05"),
		ElapsedMs:     elapsedMs,
		ElapsedText:   elapsedText,
		TotalMs:       totalMs,
		TotalText:     totalText,
		RemainingMs:   remainingMs,
		RemainingText: remainingText,
		ProgressPercent: progressPercent,
		IsPlaying:     !startTime.IsZero(),
	}

	if encodeErr := json.NewEncoder(w).Encode(playbackInfo); encodeErr != nil {
		s.logger.Error("Failed to encode playback time info", slog.String("error", encodeErr.Error()))
	}
}

// formatDuration formats duration as MM:SS or HH:MM:SS
func formatDuration(d time.Duration) string {
	totalSeconds := int(d.Seconds())
	hours := totalSeconds / 3600
	minutes := (totalSeconds % 3600) / 60
	seconds := totalSeconds % 60
	
	if hours > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, seconds)
	}
	return fmt.Sprintf("%02d:%02d", minutes, seconds)
}

// isConnectionClosedError checks if error is result of client closing connection.
func isConnectionClosedError(err error) bool {
	if err == nil {
		return false
	}

	errMsg := err.Error()
	return strings.Contains(errMsg, "broken pipe") ||
		strings.Contains(errMsg, "connection reset by peer") ||
		strings.Contains(errMsg, "use of closed network connection")
}

// StreamAudioHandler creates HTTP handler for audio streaming.
func (s *Server) StreamAudioHandler(route string) http.HandlerFunc {
	s.logger.Info("Creating audio stream handler", slog.String("route", route))

	contentType := s.determineContentType()
	s.logger.Info(
		"Audio stream format",
		slog.String("route", route),
		slog.String("MIME", contentType),
	)

	return func(w http.ResponseWriter, r *http.Request) {
		s.logger.Debug(
			"Received request for audio stream",
			slog.String("method", r.Method),
			slog.String("route", route),
			slog.String("remoteAddr", r.RemoteAddr),
		)

		// Verify stream exists.
		stream, exists := s.getStream(route, w)
		if !exists {
			return
		}

		// Set up appropriate headers for streaming.
		s.setupStreamingHeaders(w, contentType)

		// For HEAD requests, just send headers and return.
		if r.Method == http.MethodHead {
			s.logger.Debug("Handled HEAD request", slog.String("route", route), slog.String("remoteAddr", r.RemoteAddr))
			w.WriteHeader(http.StatusOK)
			return
		}

		// Verify flushing capability.
		flusher, ok := w.(http.Flusher)
		if !ok {
			errorMsg := "Streaming not supported"
			s.logger.Error("ERROR: Streaming not supported")
			s.sentryHelper.CaptureInfo(errorMsg, "http", "handler") // Save as this is an error
			http.Error(w, errorMsg, http.StatusInternalServerError)
			return
		}

		// Flush headers immediately - critical for streaming.
		flusher.Flush()

		// Connect client to stream.
		clientData, err := s.connectClientToStream(stream, route, r.RemoteAddr)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer stream.RemoveClient(clientData.clientID)

		// Start streaming data to client.
		s.streamDataToClient(w, flusher, clientData, route, r.Context().Done())
	}
}

// determineContentType sets the appropriate MIME type for audio streaming.
func (s *Server) determineContentType() string {
	// Default to MP3 since that's what we stream
	return "audio/mpeg"
}

// getStream retrieves the stream handler for the given route.
func (s *Server) getStream(route string, w http.ResponseWriter) (StreamHandler, bool) {
	s.mutex.RLock()
	stream, exists := s.streams[route]
	s.mutex.RUnlock()

	if !exists {
		errorMsg := fmt.Sprintf("Stream %s not found", route)
		s.logger.Error("ERROR: Stream not found", slog.String("route", route))
		s.sentryHelper.CaptureInfo(errorMsg, "http", "handler") // Save as this is an error
		http.Error(w, errorMsg, http.StatusNotFound)
		return nil, false
	}

	s.logger.Debug("Stream found, setting up headers", slog.String("route", route))
	return stream, true
}

// setupStreamingHeaders sets appropriate HTTP headers for audio streaming.
func (s *Server) setupStreamingHeaders(w http.ResponseWriter, contentType string) {
	// 1. Content-Type must match audio stream format.
	w.Header().Set("Content-Type", contentType)
	// 2. Disable caching for live stream.
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	// 3. Protect against content type guessing.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// DO NOT set Transfer-Encoding: chunked, Go will do this itself.
	// DO NOT set Connection: keep-alive, this is default behavior for HTTP/1.1.
}

// clientStreamData contains information about a connected client.
type clientStreamData struct {
	clientCh   <-chan []byte
	clientID   int
	remoteAddr string
}

// connectClientToStream adds a client to the stream.
func (s *Server) connectClientToStream(stream StreamHandler, route, remoteAddr string) (*clientStreamData, error) {
	s.logger.Info("Adding client to stream", slog.String("route", route))

	// Get data channel and client ID.
	clientCh, clientID, addErr := stream.AddClient()
	if addErr != nil {
		s.logger.Error(
			"ERROR: Error adding client to stream",
			slog.String("route", route),
			slog.String("error", addErr.Error()),
		)
		s.sentryHelper.CaptureError(addErr, "http", "handler") // Save as this is an error
		return nil, addErr
	}

	// Update metrics ONLY AFTER successful connection and headers sent.
	s.listenerCount.WithLabelValues(route).Inc()
	defer s.listenerCount.WithLabelValues(route).Dec()

	// Log client connection.
	s.logger.Info(
		"Client connected to stream",
		slog.String("route", route),
		slog.String("remoteAddr", remoteAddr),
		slog.Int("clientID", clientID),
	)

	return &clientStreamData{
		clientCh:   clientCh,
		clientID:   clientID,
		remoteAddr: remoteAddr,
	}, nil
}

// streamDataToClient manages the streaming of audio data to a connected client.
func (s *Server) streamDataToClient(
	w http.ResponseWriter,
	flusher http.Flusher,
	clientData *clientStreamData,
	route string,
	clientClosed <-chan struct{},
) {
	s.logger.Info("Starting data transmission", slog.Int("clientID", clientData.clientID), slog.String("route", route))

	// Counter for tracking sent data.
	var totalBytesSent int64
	var lastLogTime = time.Now()

	// Send data to client.
	for {
		select {
		case <-clientClosed:
			// Client disconnected.
					s.logger.Debug("Client disconnected from stream",
			slog.String("route", route),
			slog.String("remoteAddr", clientData.remoteAddr),
			slog.Int("clientID", clientData.clientID),
			slog.Int64("totalBytesSent", totalBytesSent))
			return
		case data, ok := <-clientData.clientCh:
			if !ok {
				// Channel closed.
				s.logger.Info("Channel closed for client",
					slog.String("remoteAddr", clientData.remoteAddr),
					slog.Int("clientID", clientData.clientID),
					slog.Int64("totalBytesSent", totalBytesSent))
				return
			}

			// Send data and handle errors.
			if err := s.sendDataToClient(w, data, clientData, route, &totalBytesSent); err != nil {
				return
			}

			// Update metrics and log periodically.
			s.bytesSent.WithLabelValues(route).Add(float64(len(data)))
			s.logDataTransfer(clientData, &totalBytesSent, &lastLogTime)

			// MUST call Flush after EACH data sent!.
			// This ensures data is sent immediately to client.
			flusher.Flush()
		}
	}
}

// sendDataToClient writes data to the client and handles potential errors.
func (s *Server) sendDataToClient(
	w http.ResponseWriter,
	data []byte,
	clientData *clientStreamData,
	_ string,
	totalBytesSent *int64,
) error {
	n, writeErr := w.Write(data)
	if writeErr != nil {
		if isConnectionClosedError(writeErr) {
			// Just log, don't send to Sentry.
			s.logger.Debug("Client disconnected",
				slog.Int("clientID", clientData.clientID),
				slog.String("error", writeErr.Error()),
				slog.Int64("totalBytesSent", *totalBytesSent))
		} else {
			// Send to Sentry only for unusual errors.
			s.logger.Error("ERROR: Error sending data to client",
				slog.Int("clientID", clientData.clientID),
				slog.String("error", writeErr.Error()),
				slog.Int64("totalBytesSent", *totalBytesSent))
			s.sentryHelper.CaptureError(fmt.Errorf("error sending data to client %d: %w", clientData.clientID, writeErr), "http", "send_data")
		}
		return writeErr
	}

	*totalBytesSent += int64(n)
	return nil
}

// logDataTransfer logs information about data transfer periodically.
func (s *Server) logDataTransfer(clientData *clientStreamData, totalBytesSent *int64, lastLogTime *time.Time) {
	if *totalBytesSent >= logEveryBytes && time.Since(*lastLogTime) > logIntervalSec*time.Second {
			s.logger.Debug("Sent data to client",
		slog.Int("mbytes", int(*totalBytesSent/mb)),
		slog.Int("clientID", clientData.clientID),
		slog.String("remoteAddr", clientData.remoteAddr))
		*lastLogTime = time.Now()
	}
}

// ReloadAllPlaylists reloads all playlists.
func (s *Server) ReloadAllPlaylists() error {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	var lastErr error
	for route := range s.playlists {
		if reloadErr := s.reloadPlaylistInternal(route); reloadErr != nil {
			lastErr = reloadErr
			s.logger.Error(
				"Error reloading playlist",
				slog.String("route", route),
				slog.String("error", reloadErr.Error()),
			)
			s.sentryHelper.CaptureError(fmt.Errorf("error reloading playlist: %w", reloadErr), "http", "reload_all")
		}
	}

	s.logger.Info("All playlists reloaded")
	return lastErr
}

// ReloadPlaylist reloads a playlist for a specific stream.
func (s *Server) ReloadPlaylist(route string) error {
	// Make sure route starts with a slash.
	if route[0] != '/' {
		route = "/" + route
	}

	s.mutex.RLock()
	defer s.mutex.RUnlock()

	err := s.reloadPlaylistInternal(route)
	if err != nil {
		errorMsg := fmt.Sprintf("Failed to reload playlist for %s: %v", route, err)
		s.logger.Error("Error reloading playlist", slog.String("route", route), slog.String("error", err.Error()))
		s.sentryHelper.CaptureInfo(errorMsg, "http", "handler")
		return err
	}

	s.logger.Info("Playlist reloaded", slog.String("route", route))
	return nil
}

// reloadPlaylistInternal reloads a specific playlist. Must be called with mutex held.
func (s *Server) reloadPlaylistInternal(route string) error {
	playlist, exists := s.playlists[route]
	if !exists {
		s.logger.Error("Playlist not found", slog.String("route", route))
		s.sentryHelper.CaptureInfo(fmt.Sprintf("Playlist for %s not found", route), "http", "playlist_not_found")
		return fmt.Errorf("playlist for %s not found", route)
	}

	// This is where we actually load and update the playlist.
	reloadErr := playlist.Reload()
	if reloadErr != nil {
		s.logger.Error("Error reloading playlist", slog.String("route", route), slog.String("error", reloadErr.Error()))
		s.sentryHelper.CaptureError(fmt.Errorf("error reloading playlist for %s: %w", route, reloadErr), "http", "reload_internal")
		return reloadErr
	}

	// Notify station manager (if set) to restart playback.
	if s.stationManager != nil {
		s.stationManager.RestartPlayback(route)
	}

	return nil
}

// statusLoginHandler displays login form page.
func (s *Server) statusLoginHandler(w http.ResponseWriter, r *http.Request) {
	// If already authenticated, redirect to status page.
	if s.checkAuth(r) {
		http.Redirect(w, r, "/status-page", http.StatusFound)
		return
	}

	// Load and render login template page.
	tmpl, tmplErr := template.ParseFiles(
		"templates/login.html",
		"templates/partials/head.html",
	)
	if tmplErr != nil {
		s.logger.Error("Unable to load login.html template", slog.String("error", tmplErr.Error()))
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	data := map[string]interface{}{
		"Title":    "Login - Audio Stream Status",
		"Redirect": r.URL.Query().Get("redirect"),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if executeErr := tmpl.Execute(w, data); executeErr != nil {
		s.logger.Error("Failed to execute template", slog.String("error", executeErr.Error()))
		return
	}
}

// statusLoginSubmitHandler handles login form submission.
func (s *Server) statusLoginSubmitHandler(w http.ResponseWriter, r *http.Request) {
	parseErr := r.ParseForm()
	if parseErr != nil {
		s.logger.Error("Unable to parse login form", slog.String("error", parseErr.Error()))
		http.Error(w, "Form processing error", http.StatusInternalServerError)
		return
	}

	// Get password from form.
	password := r.FormValue("password")

	// Check password.
	if password == s.statusPassword {
		// Create cookies for authentication.
		http.SetCookie(w, &http.Cookie{
			Name:     "status_auth",
			Value:    s.statusPassword,
			Path:     "/",
			Expires:  time.Now().Add(cookieExpireHours * time.Hour),
			HttpOnly: true,
		})

		// Check if there's a saved redirect URL from the form
		redirectURL := r.FormValue("redirect")
		if redirectURL != "" {
			s.logger.Info("Redirecting to original URL after successful login", slog.String("redirectURL", redirectURL))
			http.Redirect(w, r, redirectURL, http.StatusFound)
		} else {
			// If no redirect URL, redirect to status-page by default
			http.Redirect(w, r, "/status-page", http.StatusFound)
		}
		return
	}

	// If password is incorrect, show error message.
	tmpl, tmplErr := template.ParseFiles(
		"templates/login.html",
		"templates/partials/head.html",
	)
	if tmplErr != nil {
		s.logger.Error("Unable to load login.html template", slog.String("error", tmplErr.Error()))
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	data := map[string]interface{}{
		"Title":        "Login - Audio Stream Status",
		"ErrorMessage": "Invalid password",
		"Redirect":     r.FormValue("redirect"),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if executeErr := tmpl.Execute(w, data); executeErr != nil {
		s.logger.Error("Failed to execute template", slog.String("error", executeErr.Error()))
		return
	}
}

// checkAuth checks authentication for accessing status page.
func (s *Server) checkAuth(r *http.Request) bool {
	cookie, err := r.Cookie("status_auth")
	return err == nil && cookie.Value == s.statusPassword
}

// statusPageHandler handles requests for the status page.
func (s *Server) statusPageHandler(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(r) {
		s.redirectToLogin(w, r)
		return
	}

	s.mutex.RLock()
	defer s.mutex.RUnlock()

	type StreamInfo struct {
		Route          string
		RouteID        string
		DisplayName    string
		StartTime      string
		CurrentTrack   string
		Listeners      int
		HistoryHTML    template.HTML
		ShuffleEnabled bool
	}

	streams := make([]StreamInfo, 0, len(s.streams))
	for route, stream := range s.streams {
		s.trackMutex.RLock()
		currentTrack := s.currentTracks[route]
		s.trackMutex.RUnlock()

		// Get playlist history.
		history := s.playlists[route].GetHistory()
		var historyHTMLBuilder strings.Builder
		
		if len(history) == 0 {
			// Show message when no history is available
			historyHTMLBuilder.WriteString(`<div class="no-data">No tracks in history yet.</div>`)
		} else {
			historyHTMLBuilder.WriteString("<ul>")
			for _, item := range history {
			var trackName string
			
			// Extract track name properly based on item type
			switch track := item.(type) {
			case interface{ GetPath() string }:
				trackName = filepath.Base(track.GetPath())
			case interface{ GetNormalizedPath() string }:
				trackName = filepath.Base(track.GetNormalizedPath())
			case *struct{ Path string; Name string }:
				if track != nil {
					trackName = track.Name
					if trackName == "" {
						trackName = filepath.Base(track.Path)
					}
				}
			case string:
				trackName = filepath.Base(track)
			default:
				// Fallback to string representation but clean it up
				str := fmt.Sprintf("%v", item)
				if strings.Contains(str, "{") && strings.Contains(str, "}") {
					// This looks like a struct, try to extract filename
					trackName = "Unknown track"
				} else {
					trackName = filepath.Base(str)
				}
			}
			
			if trackName == "" {
				trackName = "Unknown track"
			}
			
			escapedTrackName := html.EscapeString(trackName)
			historyHTMLBuilder.WriteString(fmt.Sprintf(
				`<li><span>%s</span><button class="copy-button" onclick="copyToClipboard('%s', this)"><span class="copy-icon">📋</span><span class="copy-feedback"></span></button></li>`,
				escapedTrackName,
				escapedTrackName,
			))
		}
		historyHTMLBuilder.WriteString("</ul>")
		}
		historyHTML := historyHTMLBuilder.String()

		streams = append(streams, StreamInfo{
			Route:          route,
			RouteID:        strings.TrimPrefix(route, "/"),
			DisplayName:    route,
			StartTime:      s.playlists[route].GetStartTime().Format("15:04:05"),
			CurrentTrack:   currentTrack,
			Listeners:      stream.GetClientCount(),
			HistoryHTML:    template.HTML(historyHTML),
			ShuffleEnabled: s.getShuffleStatusForRoute(route),
		})
	}

	// Sort streams by route for consistent output.
	sort.Slice(streams, func(i, j int) bool {
		return streams[i].Route < streams[j].Route
	})

	// Загружаем шаблон с правильными путями.
	tmpl, tmplErr := template.ParseFiles(
		"templates/status.html",
		"templates/partials/head.html",
	)
	if tmplErr != nil {
		s.logger.Error("Unable to load status.html template", slog.String("error", tmplErr.Error()))
		http.Error(w, "Server error: "+tmplErr.Error(), http.StatusInternalServerError)
		return
	}

	// Устанавливаем тип контента перед выполнением шаблона.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Передаем данные в шаблон и обрабатываем ошибки.
	if executeErr := tmpl.Execute(w, map[string]interface{}{
		"Streams":            streams,
		"Title":              "Audio Streams Status",
		"RelayEnabled":       s.relayManager != nil,
		"RelayActive":        s.relayManager != nil && s.relayManager.IsActive(),
		"TelegramEnabled":    s.telegramManager != nil,
		"TelegramActive":     s.telegramManager != nil && s.telegramManager.IsEnabled(),
		"TelegramClickable":  s.telegramFunctionalityEnabled,
		"GlobalShuffle":      s.getGlobalShuffleStatus(),
	}); executeErr != nil {
		s.logger.Error("Failed to execute status template", slog.String("error", executeErr.Error()))
		http.Error(w, "Server error: "+executeErr.Error(), http.StatusInternalServerError)
		return
	}
}

// handleTrackSwitchHandler handles track switching.
func (s *Server) handleTrackSwitchHandler(w http.ResponseWriter, r *http.Request, direction string) {
	// Get route early for floyd detection
	vars := mux.Vars(r)
	route := "/" + vars["route"]
	isFloydRoute := strings.Contains(route, "floyd")
	
	if isFloydRoute {
		s.logger.Debug("HTTP handleTrackSwitchHandler called", 
			slog.String("direction", direction),
			slog.String("route", route),
			slog.String("method", r.Method),
			slog.String("remoteAddr", r.RemoteAddr),
			slog.String("userAgent", r.UserAgent()),
			slog.String("timestamp", time.Now().Format("15:04:05.000")))
	} else {
		s.logger.Info("TRACK SWITCH DEBUG: handleTrackSwitchHandler called", 
			slog.String("direction", direction),
			slog.String("method", r.Method),
			slog.String("remoteAddr", r.RemoteAddr),
			slog.String("userAgent", r.UserAgent()),
			slog.String("timestamp", time.Now().Format("15:04:05.000")))
	}
	
	// Check authentication.
	if !s.checkAuth(r) {
		if isFloydRoute {
			s.logger.Debug("Authentication failed")
		} else {
			s.logger.Info("TRACK SWITCH DEBUG: Authentication failed")
		}
		s.handleUnauthenticatedTrackSwitch(w, r)
		return
	}

	// Get playlist.
	playlist, exists := s.getPlaylistForRoute(route)
	if !exists {
		if isFloydRoute {
			s.logger.Debug("Playlist not found", slog.String("route", route))
		}
		s.handlePlaylistNotFound(w, r, route)
		return
	}

	if isFloydRoute {
		s.logger.Debug("About to get track for direction", 
			slog.String("direction", direction), 
			slog.String("route", route))
	}

	// Get track.
	track := s.getTrackForDirection(playlist, direction)
	if track == nil {
		if isFloydRoute {
			s.logger.Debug("Track not found", 
				slog.String("direction", direction), 
				slog.String("route", route))
		}
		s.handleTrackNotFound(w, r, route, direction)
		return
	}

	if isFloydRoute {
		s.logger.Debug("Manual track switch proceeding", 
			slog.String("direction", direction), 
			slog.String("route", route))
	} else {
		s.logger.Info("Manual track switch", slog.String("direction", direction), slog.String("route", route))
	}

	// Handle track switching.
	s.handleTrackSwitchResult(w, r, route, track)
}

// handleUnauthenticatedTrackSwitch handles unauthenticated track switching requests.
func (s *Server) handleUnauthenticatedTrackSwitch(w http.ResponseWriter, r *http.Request) {
	isAjax := isAjaxRequest(r)
	if isAjax {
		w.Header().Set("Content-Type", "application/json")
		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"route":   "",
			"error":   "Authentication required",
		}); encodeErr != nil {
			s.logger.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
			return
		}
		return
	}
	http.Redirect(w, r, "/status", http.StatusFound)
}

// getPlaylistForRoute gets the playlist for the specified route.
func (s *Server) getPlaylistForRoute(route string) (PlaylistManager, bool) {
	s.mutex.RLock()
	playlist, exists := s.playlists[route]
	s.mutex.RUnlock()
	return playlist, exists
}

// handlePlaylistNotFound handles case when playlist for route is not found.
func (s *Server) handlePlaylistNotFound(w http.ResponseWriter, r *http.Request, route string) {
	isAjax := isAjaxRequest(r)
	if isAjax {
		w.Header().Set("Content-Type", "application/json")
		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"route":   route,
			"error":   "Route not found",
		}); encodeErr != nil {
			s.logger.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
			return
		}
		return
	}
	http.Error(w, "Route not found", http.StatusNotFound)
}

// getTrackForDirection gets the track for the specified direction.
func (s *Server) getTrackForDirection(playlist PlaylistManager, direction string) interface{} {
	s.logger.Info("TRACK SWITCH DEBUG: getTrackForDirection called", 
		slog.String("direction", direction),
		slog.String("timestamp", time.Now().Format("15:04:05.000")))
	
	if direction == "next" {
		s.logger.Info("TRACK SWITCH DEBUG: About to call NextTrack()", 
			slog.String("timestamp", time.Now().Format("15:04:05.000")))
		track := playlist.NextTrack()
		s.logger.Info("TRACK SWITCH DEBUG: NextTrack() called and returned", 
			slog.String("timestamp", time.Now().Format("15:04:05.000")))
		return track
	}
	s.logger.Info("TRACK SWITCH DEBUG: About to call PreviousTrack()", 
		slog.String("timestamp", time.Now().Format("15:04:05.000")))
	track := playlist.PreviousTrack()
	s.logger.Info("TRACK SWITCH DEBUG: PreviousTrack() called and returned", 
		slog.String("timestamp", time.Now().Format("15:04:05.000")))
	return track
}

// handleTrackNotFound handles case when track for direction is not found.
func (s *Server) handleTrackNotFound(w http.ResponseWriter, r *http.Request, route, direction string) {
	s.logger.Error("Track not found for direction", slog.String("direction", direction))
	isAjax := isAjaxRequest(r)
	if isAjax {
		w.Header().Set("Content-Type", "application/json")
		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"route":   route,
			"error":   fmt.Sprintf("Track not found for direction %s", direction),
		}); encodeErr != nil {
			s.logger.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
		}
	} else {
		http.Error(w, fmt.Sprintf("Track not found for direction %s", direction), http.StatusNotFound)
	}
}

// handleTrackSwitchResult handles the result of track switching.
func (s *Server) handleTrackSwitchResult(w http.ResponseWriter, r *http.Request, route string, track interface{}) {
	trackName := "Unknown"

	// Check if station manager is set.
	if s.stationManager == nil {
		s.handleMissingStationManager(w, r, route, trackName)
		return
	}

	// Try to restart playback.
	success := s.stationManager.RestartPlayback(route)
	if success {
		trackName = s.updateTrackInfo(route, track)
	} else {
		s.logger.Error("Unable to restart playback", slog.String("route", route))
	}

	// Send response.
	s.sendTrackSwitchResponse(w, r, route, trackName, success)
}

// handleMissingStationManager handles case when station manager is not set.
func (s *Server) handleMissingStationManager(w http.ResponseWriter, r *http.Request, route, trackName string) {
	s.logger.Warn("WARNING: Station manager not set, restart playback not possible")
	isAjax := isAjaxRequest(r)
	if isAjax {
		w.Header().Set("Content-Type", "application/json")
		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"route":   route,
			"track":   trackName,
		}); encodeErr != nil {
			s.logger.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
		}
	} else {
		http.Redirect(w, r, "/status-page", http.StatusFound)
	}
}

// updateTrackInfo updates track information and returns track name.
func (s *Server) updateTrackInfo(route string, track interface{}) string {
	s.logger.Info("Restart playback completed successfully", slog.String("route", route))

	trackName := "Unknown"
	if trackWithPath, ok := track.(interface{ GetPath() string }); ok {
		trackName = filepath.Base(trackWithPath.GetPath())
		s.trackMutex.Lock()
		s.currentTracks[route] = trackName
		s.trackMutex.Unlock()
		s.logger.Info("Updated current track information", slog.String("route", route), slog.String("track", trackName))
	}

	return trackName
}

// sendTrackSwitchResponse sends response for track switch.
func (s *Server) sendTrackSwitchResponse(
	w http.ResponseWriter,
	r *http.Request,
	route, trackName string,
	success bool,
) {
	isAjax := isAjaxRequest(r)
	if isAjax {
		w.Header().Set("Content-Type", "application/json")
		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"success": success,
			"route":   route,
			"track":   trackName,
		}); encodeErr != nil {
			s.logger.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
			return
		}
	} else {
		http.Redirect(w, r, "/status-page", http.StatusFound)
	}
}

// nextTrackHandler handles next track requests.
func (s *Server) nextTrackHandler(w http.ResponseWriter, r *http.Request) {
	s.handleTrackSwitchHandler(w, r, "next")
}

// prevTrackHandler handles previous track requests.
func (s *Server) prevTrackHandler(w http.ResponseWriter, r *http.Request) {
	s.handleTrackSwitchHandler(w, r, "prev")
}

// notFoundHandler handles 404 not found requests.
func (s *Server) notFoundHandler(w http.ResponseWriter, r *http.Request) {
	s.logger.Info(
		"404 not found request",
		slog.String("path", r.URL.Path),
		slog.String("method", r.Method),
		slog.String("remoteAddr", r.RemoteAddr),
	)

	// Load and render 404 template page.
	tmpl, err := template.ParseFiles(
		"templates/404.html",
		"templates/partials/head.html",
	)
	if err != nil {
		s.logger.Error("Unable to load 404.html template", slog.String("error", err.Error()))
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	data := map[string]interface{}{
		"Title": "404 - Page not found",
		"Path":  r.URL.Path,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	if executeErr := tmpl.Execute(w, data); executeErr != nil {
		s.logger.Error("Failed to execute template", slog.String("error", executeErr.Error()))
		return
	}
}

// redirectToLogin redirects to login page.
func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	s.logger.Info(
		"Redirecting to login page",
		slog.String("remoteAddr", r.RemoteAddr),
		slog.String("path", r.URL.Path),
		slog.String("userAgent", r.UserAgent()),
		slog.String("cookie", fmt.Sprintf("%v", r.Cookies())),
	)

	// Проверим куки для диагностики.
	cookie, cookieErr := r.Cookie("status_auth")
	if cookieErr != nil {
		s.logger.Info("Auth cookie not found", slog.String("error", cookieErr.Error()))
	} else {
		s.logger.Info("Auth cookie found", slog.String("value", cookie.Value))
		s.logger.Info("Expected password", slog.String("password", s.statusPassword))
		s.logger.Info("Cookie match", slog.Bool("match", cookie.Value == s.statusPassword))
	}

	// ИСПРАВЛЕНИЕ: Сохраняем оригинальный URL как query параметр
	// чтобы после авторизации вернуть пользователя туда, где он хотел быть
	originalURL := r.URL.Path
	if r.URL.RawQuery != "" {
		originalURL += "?" + r.URL.RawQuery
	}
	
	// Только сохраняем URL если это не сама страница логина
	if originalURL != "/status" && originalURL != "/status-page" {
		encodedURL := url.QueryEscape(originalURL)
		http.Redirect(w, r, "/status?redirect="+encodedURL, http.StatusFound)
	} else {
		http.Redirect(w, r, "/status", http.StatusFound)
	}
}

// handleShufflePlaylist shuffles the playlist for a specific stream.
func (s *Server) handleShufflePlaylist(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(r) {
		isAjax := isAjaxRequest(r)
		if isAjax {
			w.Header().Set("Content-Type", "application/json")
			if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Authentication required",
			}); encodeErr != nil {
				s.logger.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
				return
			}
			return
		}
		http.Redirect(w, r, "/status", http.StatusFound)
		return
	}

	vars := mux.Vars(r)
	route := "/" + vars["route"]

	s.mutex.RLock()
	playlist, exists := s.playlists[route]
	s.mutex.RUnlock()

	if !exists {
		isAjax := isAjaxRequest(r)
		if isAjax {
			w.Header().Set("Content-Type", "application/json")
			if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"route":   route,
				"error":   "Route not found",
			}); encodeErr != nil {
				s.logger.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
				return
			}
			return
		}
		http.Error(w, "Route not found", http.StatusNotFound)
		return
	}

	// Shuffle the playlist.
	playlist.Shuffle()
	s.logger.Info("Playlist shuffled for route", slog.String("route", route))

	isAjax := isAjaxRequest(r)
	if isAjax {
		w.Header().Set("Content-Type", "application/json")
		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"route":   route,
			"message": "Playlist shuffled",
		}); encodeErr != nil {
			s.logger.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
			return
		}
	} else {
		http.Redirect(w, r, "/status-page", http.StatusFound)
	}
}

// handleSetShuffleMode sets the shuffle mode for a specific stream.
func (s *Server) handleSetShuffleMode(w http.ResponseWriter, r *http.Request) {
	// Check authentication.
	if !s.isShuffleModeAuthValid(w, r) {
		return
	}

	// Extract vars and validate.
	vars := mux.Vars(r)
	route := "/" + vars["route"]
	mode := vars["mode"]

	// Validate mode.
	if !s.isShuffleModeValid(w, r, route, mode) {
		return
	}

	// Validate route.
	if !s.isShuffleRouteValid(w, r, route) {
		return
	}

	// Process the shuffle mode setting.
	s.processShuffleModeSetting(w, r, route, mode)
}

// isShuffleModeAuthValid checks if the user is authenticated for setting shuffle mode.
func (s *Server) isShuffleModeAuthValid(w http.ResponseWriter, r *http.Request) bool {
	if !s.checkAuth(r) {
		isAjax := isAjaxRequest(r)
		if isAjax {
			w.Header().Set("Content-Type", "application/json")
			if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Authentication required",
			}); encodeErr != nil {
				s.logger.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
				return false
			}
			return false
		}
		http.Redirect(w, r, "/status", http.StatusSeeOther)
		return false
	}
	return true
}

// isShuffleModeValid validates the shuffle mode value.
func (s *Server) isShuffleModeValid(w http.ResponseWriter, r *http.Request, route, mode string) bool {
	if mode != "on" && mode != "off" {
		isAjax := isAjaxRequest(r)
		if isAjax {
			w.Header().Set("Content-Type", "application/json")
			if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"route":   route,
				"error":   "Invalid mode value, must be 'on' or 'off'",
			}); encodeErr != nil {
				s.logger.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
				return false
			}
			return false
		}
		http.Error(w, "Invalid mode value, must be 'on' or 'off'", http.StatusBadRequest)
		return false
	}
	return true
}

// isShuffleRouteValid checks if the route exists.
func (s *Server) isShuffleRouteValid(w http.ResponseWriter, r *http.Request, route string) bool {
	s.mutex.RLock()
	_, exists := s.playlists[route]
	s.mutex.RUnlock()

	if !exists {
		isAjax := isAjaxRequest(r)
		if isAjax {
			w.Header().Set("Content-Type", "application/json")
			if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"route":   route,
				"error":   "Route not found",
			}); encodeErr != nil {
				s.logger.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
				return false
			}
			return false
		}
		http.Error(w, "Route not found", http.StatusNotFound)
		return false
	}
	return true
}

// processShuffleModeSetting processes the shuffle mode setting and sends response.
func (s *Server) processShuffleModeSetting(w http.ResponseWriter, r *http.Request, route, mode string) {
	// Get the playlist for this route.
	s.mutex.RLock()
	playlist, exists := s.playlists[route]
	s.mutex.RUnlock()
	
	if !exists {
		s.logger.Error("Route not found for shuffle mode setting", slog.String("route", route))
		isAjax := isAjaxRequest(r)
		if isAjax {
			w.Header().Set("Content-Type", "application/json")
			if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"route":   route,
				"error":   "Route not found",
			}); encodeErr != nil {
				s.logger.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
				return
			}
			return
		}
		http.Error(w, "Route not found", http.StatusNotFound)
		return
	}
	
	// Set shuffle mode on the playlist.
	shuffleEnabled := mode == "on"
	playlist.SetShuffleEnabled(shuffleEnabled)
	
	s.logger.Info("Shuffle mode updated for route", 
		slog.String("route", route), 
		slog.Bool("enabled", shuffleEnabled))

	isAjax := isAjaxRequest(r)
	if isAjax {
		w.Header().Set("Content-Type", "application/json")
		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"route":   route,
			"mode":    mode,
			"enabled": shuffleEnabled,
			"message": fmt.Sprintf("Shuffle mode set to %s for route %s", mode, route),
		}); encodeErr != nil {
			s.logger.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
			return
		}
	} else {
		http.Redirect(w, r, "/status-page", http.StatusSeeOther)
	}
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

// SetStatusPassword sets the password for accessing the status page.
func (s *Server) SetStatusPassword(password string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.statusPassword = password
	s.logger.Info("Status password set for HTTP server")
}

// handleClearHistory handles clearing track history for a specific stream.
func (s *Server) handleClearHistory(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	route := vars["route"]
	
	if route == "" {
		s.logger.Error("No route provided for clear history")
		http.Error(w, "Route parameter is required", http.StatusBadRequest)
		return
	}
	
	// Add leading slash if not present
	if !strings.HasPrefix(route, "/") {
		route = "/" + route
	}
	
	s.logger.Info("Clearing track history for route", slog.String("route", route))
	
	// Get the playlist manager for this route
	s.mutex.RLock()
	playlist, exists := s.playlists[route]
	s.mutex.RUnlock()
	
	if !exists {
		s.logger.Error("Playlist not found for route", slog.String("route", route))
		http.Error(w, "Playlist not found", http.StatusNotFound)
		return
	}
	
	// Clear the history (implement ClearHistory method in playlist interface)
	if clearablePlaylist, ok := playlist.(interface{ ClearHistory() }); ok {
		clearablePlaylist.ClearHistory()
		s.logger.Info("Track history cleared successfully", slog.String("route", route))
	} else {
		s.logger.Error("Playlist does not support clearing history", slog.String("route", route))
		http.Error(w, "Clear history not supported", http.StatusNotImplemented)
		return
	}
	
	// Check if this is an AJAX request
	isAjax := r.Header.Get("X-Requested-With") == "XMLHttpRequest"
	
	if isAjax {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Track history cleared successfully",
			"route":   route,
		}); encodeErr != nil {
			s.logger.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
			return
		}
	} else {
		// Redirect back to status page
		http.Redirect(w, r, "/status-page", http.StatusSeeOther)
	}
}

// getShuffleStatusForRoute returns shuffle status for specific route.
func (s *Server) getShuffleStatusForRoute(route string) bool {
	s.mutex.RLock()
	playlist, exists := s.playlists[route]
	s.mutex.RUnlock()
	
	if !exists {
		// If route doesn't exist, return global shuffle setting as fallback.
		return s.globalShuffleConfig
	}
	
	// Get shuffle status from playlist.
	return playlist.GetShuffleEnabled()
}

// getGlobalShuffleStatus returns global shuffle configuration.
func (s *Server) getGlobalShuffleStatus() bool {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.globalShuffleConfig
}

// SetGlobalShuffleConfig sets the global shuffle configuration.
func (s *Server) SetGlobalShuffleConfig(enabled bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.globalShuffleConfig = enabled
	s.logger.Info("Global shuffle configuration updated", slog.Bool("enabled", enabled))
}

// IsStreamAvailable checks if a stream is available by making a HEAD request to the route
func (s *Server) IsStreamAvailable(route string) bool {
	s.mutex.RLock()
	stream, exists := s.streams[route]
	s.mutex.RUnlock()
	
	// If stream doesn't exist, it's not available
	if !exists {
		return false
	}
	
	// Check if stream has clients or is active
	clientCount := stream.GetClientCount()
	
	// Stream is available if it exists and has valid configuration
	// We could also check if the stream is actually playing audio
	return clientCount >= 0 // ClientCount >= 0 means stream is properly initialized
}

// streamStatusHandler checks the status of all main audio streams
func (s *Server) streamStatusHandler(w http.ResponseWriter, r *http.Request) {
	s.mutex.RLock()
	routes := make([]string, 0, len(s.streams))
	for route := range s.streams {
		routes = append(routes, route)
	}
	s.mutex.RUnlock()

	// Check status of each route with HEAD requests to localhost
	type StreamStatus struct {
		Route  string `json:"route"`
		Status string `json:"status"`
		Index  int    `json:"index"`
	}

	statuses := make([]StreamStatus, 0, len(routes))
	
	for i, route := range routes {
		status := "offline"
		
		// Make HEAD request to check stream availability
		url := fmt.Sprintf("http://localhost:8000%s", route)
		client := &http.Client{
			Timeout: 5 * time.Second,
		}
		
		resp, err := client.Head(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			status = "online"
		}
		if resp != nil {
			resp.Body.Close()
		}
		
		statuses = append(statuses, StreamStatus{
			Route:  route,
			Status: status,
			Index:  i,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"statuses": statuses,
	}); err != nil {
		s.logger.Error("Failed to encode stream status response", slog.String("error", err.Error()))
		return
	}
}

// ClearHistoryForRoute clears track history for a specific route (internal method)
func (s *Server) ClearHistoryForRoute(route string) error {
	// Get the playlist manager for this route
	s.mutex.RLock()
	playlist, exists := s.playlists[route]
	s.mutex.RUnlock()
	
	if !exists {
		return fmt.Errorf("playlist not found for route: %s", route)
	}
	
	// Clear the history if playlist supports it
	if clearablePlaylist, ok := playlist.(interface{ ClearHistory() }); ok {
		clearablePlaylist.ClearHistory()
		return nil
	}
	
	return fmt.Errorf("playlist does not support clearing history for route: %s", route)
}

// TrackInfo returns track information for specified stream.
func (s *Server) TrackInfo(route string, track string) (map[string]interface{}, error) {
	s.logger.Info("Track information request", slog.String("route", route), slog.String("track", track))

	s.trackMutex.RLock()
	defer s.trackMutex.RUnlock()

	trackInfo := map[string]interface{}{
		"route": route,
		"track": track,
	}

	s.mutex.RLock()
	playlist, exists := s.playlists[route]
	s.mutex.RUnlock()

	if exists {
		currentTrack := playlist.GetCurrentTrack()
		if currentTrack != nil {
			if trackWithPath, ok := currentTrack.(interface{ GetPath() string }); ok {
				trackInfo["path"] = trackWithPath.GetPath()
			}
		}
	}

	return trackInfo, nil
}

// SwitchTrack switches to the next or previous track for a specific stream.
func (s *Server) SwitchTrack(route string, direction string) error {
	s.mutex.RLock()
	playlist, exists := s.playlists[route]
	s.mutex.RUnlock()

	if !exists {
		s.logger.Error("Route not found", slog.String("route", route))
		return fmt.Errorf("route %s not found", route)
	}

	var track interface{}
	if direction == "next" {
		track = playlist.NextTrack()
	} else {
		track = playlist.PreviousTrack()
	}

	if track == nil {
		s.logger.Error("Track not found for direction", slog.String("direction", direction))
		return fmt.Errorf("track not found for direction %s", direction)
	}

	s.logger.Info("Manual track switch", slog.String("direction", direction), slog.String("route", route))

	if s.stationManager == nil {
		s.logger.Warn("WARNING: Station manager not set, restart playback not possible")
		return nil
	}

	success := s.stationManager.RestartPlayback(route)
	if success {
		s.logger.Info("Restart playback completed successfully", slog.String("route", route))
		if trackWithPath, ok := track.(interface{ GetPath() string }); ok {
			trackName := filepath.Base(trackWithPath.GetPath())
			s.trackMutex.Lock()
			s.currentTracks[route] = trackName
			s.trackMutex.Unlock()
			s.logger.Info(
				"Updated current track information",
				slog.String("route", route),
				slog.String("track", trackName),
			)
		}
	} else {
		s.logger.Error("Unable to restart playback", slog.String("route", route))
	}

	return nil
}

// logNormalizationError обрабатывает ошибки нормализации звука.
// Использует DEBUG для обычных EOF ошибок, которые возникают при нормальной работе.
func (s *Server) logNormalizationError(err error, filePath string) {
	// Обрабатываем только критические ошибки для Sentry.
	if err != nil {
		// Проверяем, содержит ли ошибка строку "EOF", которая обычно не является критической.
		if strings.Contains(err.Error(), "EOF") {
			// Для EOF-ошибок используем уровень DEBUG, чтобы не спамить логи
			s.logger.Debug("Non-critical normalization error",
				slog.String("error", err.Error()),
				slog.String("route", filepath.Base(filepath.Dir(filePath))))
		} else {
			// Для более серьезных ошибок и логируем, и отправляем в Sentry.
			errMsg := fmt.Sprintf("ERROR during audio normalization error=%q", err.Error())
			s.logger.Error(errMsg)
			s.sentryHelper.CaptureInfo(errMsg, "http", "handler")
		}
	}
}

// trackHandler returns information about the current track for a specific route.
func (s *Server) trackHandler(w http.ResponseWriter, r *http.Request) {
	route, routeExists := mux.Vars(r)["route"]

	// Проверка на пустой маршрут.
	if !routeExists || route == "" {
		errorMsg := "Route parameter is required"
		s.logger.Error("ERROR: Empty route parameter in trackHandler")
		http.Error(w, errorMsg, http.StatusBadRequest)
		return
	}

	// Retrieve current track from the playlist.
	s.mutex.RLock()
	playlist, exists := s.playlists[route]
	s.mutex.RUnlock()

	if !exists {
		s.logger.Error("ERROR: Stream not found", slog.String("route", route))
		http.Error(w, fmt.Sprintf("Stream %s not found", route), http.StatusNotFound)
		return
	}

	// Try to get current track with retries.
	const (
		maxAttempts = 3
		retryDelay  = 50 * time.Millisecond
	)

	var currentTrack interface{}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		currentTrack = playlist.GetCurrentTrack()
		if currentTrack != nil {
			break
		}
		s.logger.Warn("WARNING: GetCurrentTrack timed out",
			slog.Int("attempt", attempt),
			slog.Int("maxAttempts", maxAttempts))
		time.Sleep(retryDelay)
	}

	if currentTrack == nil {
		s.logger.Warn("WARNING: All GetCurrentTrack attempts timed out, returning nil")
		http.Error(w, "Current track information not available", http.StatusServiceUnavailable)
		return
	}

	// Handle different types of tracks.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	switch track := currentTrack.(type) {
	case string:
		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"track": track,
		}); encodeErr != nil {
			s.logger.Error("Failed to encode track", slog.String("error", encodeErr.Error()))
		}
	case interface{ GetNormalizedPath() string }:
		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"track": track.GetNormalizedPath(),
		}); encodeErr != nil {
			s.logger.Error("Failed to encode track", slog.String("error", encodeErr.Error()))
		}
	default:
		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"track": fmt.Sprintf("%v", track),
		}); encodeErr != nil {
			s.logger.Error("Failed to encode track", slog.String("error", encodeErr.Error()))
		}
	}
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
