// Package http implements the HTTP server for audio streaming.
// It provides endpoints for streaming audio, health checks, and status monitoring.
package http

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	html "html"

	"github.com/getsentry/sentry-go"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/user/stream-audio-to-web/audio"
	"github.com/user/stream-audio-to-web/relay"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

var (
	// Prometheus metrics.
	listenerCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "audio_stream_listeners",
			Help: "Number of active listeners per stream",
		},
		[]string{"stream"},
	)

	bytesSent = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "audio_stream_bytes_sent_total",
			Help: "Total number of bytes sent to clients",
		},
		[]string{"stream"},
	)

	// Counter for audio playback time in seconds.
	trackSecondsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "audio_stream_track_seconds_total",
			Help: "Total seconds of audio played",
		},
		[]string{"stream"},
	)
)

// RegisterPrometheusMetrics регистрирует метрики Prometheus.
func RegisterPrometheusMetrics() {
	prometheus.MustRegister(listenerCount)
	prometheus.MustRegister(bytesSent)
	prometheus.MustRegister(trackSecondsTotal)
}

// StreamHandler interface for handling audio stream.
type StreamHandler interface {
	AddClient() (<-chan []byte, int, error)
	RemoveClient(clientID int)
	GetClientCount() int
	GetCurrentTrackChannel() <-chan string
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
}

// Server represents HTTP server for audio streaming.
type Server struct {
	router         *mux.Router
	streams        map[string]StreamHandler
	playlists      map[string]PlaylistManager
	streamFormat   string
	maxClients     int
	mutex          sync.RWMutex
	currentTracks  map[string]string
	trackMutex     sync.RWMutex
	statusPassword string                                    // Password for accessing /status page.
	stationManager interface{ RestartPlayback(string) bool } // Interface for restarting playback.
	relayManager   *relay.RelayManager                       // Manager for relay functionality.
}

const xmlHTTPRequestHeader = "XMLHttpRequest"

const (
	defaultStatusPassword     = "1234554321"
	defaultStreamsSortTimeout = 1 * time.Second
	asciiMax         = 127
	shortSleepMs     = 50
	kb               = 1024
	mb               = 1024 * 1024
	cookieExpireHours = 24
	logIntervalSec   = 5
)

// NewServer creates a new HTTP server.
func NewServer(streamFormat string, maxClients int) *Server {
	// Явная регистрация метрик Prometheus
	RegisterPrometheusMetrics()

	// Get password for status page from environment variable.
	statusPassword := getEnvOrDefault("STATUS_PASSWORD", defaultStatusPassword)

	server := &Server{
		router:         mux.NewRouter(),
		streams:        make(map[string]StreamHandler),
		playlists:      make(map[string]PlaylistManager),
		streamFormat:   streamFormat,
		maxClients:     maxClients,
		currentTracks:  make(map[string]string),
		statusPassword: statusPassword,
	}

	// Setup routes.
	server.setupRoutes()

	// Pass trackSecondsTotal metric directly to audio package.
	audio.SetTrackSecondsMetric(trackSecondsTotal)
	slog.Info("DIAGNOSTICS: trackSecondsTotal metric passed to audio package")

	slog.Info("HTTP server created, stream format: %s, max clients: %d", streamFormat, maxClients)
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
	slog.Info("DIAGNOSTICS: Starting audio stream registration", slog.String("route", route))

	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Make sure route starts with a slash.
	if route[0] != '/' {
		route = "/" + route
		slog.Info("Fixed route during registration", slog.String("route", route))
	}

	slog.Info("DIAGNOSTICS: Adding stream to streams map", slog.String("route", route))
	s.streams[route] = stream
	slog.Info("DIAGNOSTICS: Adding playlist to playlists map", slog.String("route", route))
	s.playlists[route] = playlist

	// Check if stream was added.
	if _, exists := s.streams[route]; exists {
		slog.Info("DIAGNOSTICS: Stream for route successfully added to streams map", slog.String("route", route))
	} else {
		slog.Error("ERROR: Stream for route was not added to streams map!", slog.String("route", route))
	}

	// IMPORTANT: Register route handler in router for GET and HEAD requests.
	s.router.HandleFunc(route, s.StreamAudioHandler(route)).Methods("GET", "HEAD")
	slog.Info("DIAGNOSTICS: HTTP handler registered for route", slog.String("route", route))

	// Start goroutine to track current track.
	slog.Info("DIAGNOSTICS: Starting goroutine to track current track", slog.String("route", route))
	go s.trackCurrentTrack(route, stream.GetCurrentTrackChannel())

	slog.Info("DIAGNOSTICS: Audio stream for route successfully registered", slog.String("route", route))
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
		slog.Info("Current track for %s: %s (path: %s)", route, fileName, trackPath)

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
	for i := 0; i < len(s); i++ {
		if s[i] > asciiMax {
			return true
		}
	}
	return false
}

// setupRoutes configures HTTP server routes.
func (s *Server) setupRoutes() {
	// Monitoring and health endpoints
	s.router.HandleFunc("/healthz", s.healthzHandler).Methods("GET", "HEAD")
	s.router.HandleFunc("/readyz", s.readyzHandler).Methods("GET", "HEAD")
	s.router.Handle("/metrics", promhttp.Handler()).Methods("GET")

	// API for playlist management
	s.router.HandleFunc("/streams", s.streamsHandler).Methods("GET")
	s.router.HandleFunc("/reload-playlist", s.reloadPlaylistHandler).Methods("POST")
	s.router.HandleFunc("/now-playing", s.nowPlayingHandler).Methods("GET")

	// Add status page with password check
	s.router.HandleFunc("/status", s.statusLoginHandler).Methods("GET", "HEAD")
	s.router.HandleFunc("/status", s.statusLoginSubmitHandler).Methods("POST")
	s.router.HandleFunc("/status-page", s.statusPageHandler).Methods("GET")

	// Add handlers for track switching
	s.router.HandleFunc("/next-track/{route}", s.nextTrackHandler).Methods("POST")
	s.router.HandleFunc("/prev-track/{route}", s.prevTrackHandler).Methods("POST")

	// Endpoint to shuffle playlist manually
	s.router.HandleFunc("/shuffle-playlist/{route}", s.handleShufflePlaylist).Methods("POST")

	// Endpoint to set shuffle mode for specific stream
	s.router.HandleFunc("/set-shuffle/{route}/{mode}", s.SetShuffleMode).Methods("POST")

	// Add static files for web interface
	s.router.PathPrefix("/web/").Handler(http.StripPrefix("/web/", http.FileServer(http.Dir("./web"))))

	// Add favicon and image files
	s.router.PathPrefix("/image/").Handler(http.StripPrefix("/image/", http.FileServer(http.Dir("./image"))))

	// Handle favicon.ico requests in root
	s.router.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./image/favicon.ico")
	})

	// Configure 404 handler
	s.router.NotFoundHandler = http.HandlerFunc(s.notFoundHandler)

	slog.Info("HTTP routes configured", slog.String("status", "done"))
}

// healthzHandler returns 200 OK if server is running.
func (s *Server) healthzHandler(w http.ResponseWriter, r *http.Request) {
	// Log healthz request.
	slog.Info("Received healthz request", slog.String("method", r.Method), slog.String("remoteAddr", r.RemoteAddr), slog.String("uri", r.RequestURI))

	// Check for registered streams.
	s.mutex.RLock()
	streamsCount := len(s.streams)
	streamsList := make([]string, 0, streamsCount)
	for route := range s.streams {
		streamsList = append(streamsList, route)
	}
	s.mutex.RUnlock()

	// Log status.
	if streamsCount == 0 {
		slog.Info("WARNING: No registered streams, but server is running", slog.String("status", "no_streams"))
	} else {
		slog.Info("Healthz status", slog.Int("streamsCount", streamsCount), slog.Any("routes", streamsList))
	}

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
			slog.Error("Failed to write healthz response", slog.String("error", writeErr.Error()))
			return
		}

		// Force send response.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	// Additional logging of successful response.
	slog.Info("Sent successful healthz response to client", slog.String("remoteAddr", r.RemoteAddr))
}

// readyzHandler checks readiness.
func (s *Server) readyzHandler(w http.ResponseWriter, r *http.Request) {
	// Log readyz request.
	slog.Info("Received readyz request", slog.String("method", r.Method), slog.String("remoteAddr", r.RemoteAddr), slog.String("uri", r.RequestURI))

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
	slog.Info("Readyz status", slog.Int("streamsCount", streamsCount), slog.Any("routes", streamsList))

	// Always return OK for readyz to avoid container restarts.
	w.WriteHeader(http.StatusOK)

	// If not a HEAD request, send response body.
	if r.Method != http.MethodHead {
		if _, writeErr := fmt.Fprintf(w, "Ready - %d streams registered", streamsCount); writeErr != nil {
			slog.Error("Failed to write readyz response", slog.String("error", writeErr.Error()))
			return
		}

		// Send data immediately.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	// Additional logging of successful response.
	slog.Info("Sent successful readyz response to client", slog.String("remoteAddr", r.RemoteAddr))
}

// streamsHandler handles requests for stream information.
func (s *Server) streamsHandler(w http.ResponseWriter, r *http.Request) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	type streamInfo struct {
		Route        string `json:"route"`
		Listeners    int    `json:"listeners"`
		CurrentTrack string `json:"current_track"`
	}

	streams := make([]streamInfo, 0, len(s.streams))
	for route, stream := range s.streams {
		s.trackMutex.RLock()
		currentTrack := s.currentTracks[route]
		s.trackMutex.RUnlock()

		streams = append(streams, streamInfo{
			Route:        route,
			Listeners:    stream.GetClientCount(),
			CurrentTrack: currentTrack,
		})
	}

	// Sort streams by route for consistent output
	sort.Slice(streams, func(i, j int) bool {
		return streams[i].Route < streams[j].Route
	})

	w.Header().Set("Content-Type", "application/json")
	if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
		"streams": streams,
	}); encodeErr != nil {
		slog.Error("Failed to encode streams response", slog.String("error", encodeErr.Error()))
		return
	}
}

// reloadPlaylistHandler reloads playlist.
func (s *Server) reloadPlaylistHandler(w http.ResponseWriter, r *http.Request) {
	route := r.URL.Query().Get("route")

	if route == "" {
		// Reload all playlists
		s.mutex.RLock()
		playlists := make([]PlaylistManager, 0, len(s.playlists))
		for _, playlist := range s.playlists {
			playlists = append(playlists, playlist)
		}
		s.mutex.RUnlock()

		for _, playlist := range playlists {
			if reloadErr := playlist.Reload(); reloadErr != nil {
				errorMsg := fmt.Sprintf("Error reloading playlist: %s", reloadErr)
				sentry.CaptureException(fmt.Errorf("error reloading playlist: %w", reloadErr))
				http.Error(w, errorMsg, http.StatusInternalServerError)
				return
			}
		}

		slog.Info("All playlists reloaded")
		w.WriteHeader(http.StatusOK)
		if _, writeErr := w.Write([]byte("All playlists reloaded")); writeErr != nil {
			slog.Error("ERROR: Failed to write response", slog.String("error", writeErr.Error()))
		}
		return
	}

	// Reload specific playlist
	s.mutex.RLock()
	playlist, exists := s.playlists[route]
	s.mutex.RUnlock()

	if !exists {
		errorMsg := fmt.Sprintf("Stream %s not found", route)
		slog.Error("ERROR: %s", errorMsg)
		sentry.CaptureMessage(errorMsg)
		http.Error(w, errorMsg, http.StatusNotFound)
		return
	}

	if reloadErr := playlist.Reload(); reloadErr != nil {
		errorMsg := fmt.Sprintf("Error reloading playlist: %s", reloadErr)
		sentry.CaptureException(fmt.Errorf("error reloading playlist for %s: %w", route, reloadErr))
		http.Error(w, errorMsg, http.StatusInternalServerError)
		return
	}

	slog.Info("Playlist for stream %s reloaded", route)
	w.WriteHeader(http.StatusOK)
	if _, writeErr := fmt.Fprintf(w, "Playlist for stream %s reloaded", route); writeErr != nil {
		slog.Error("ERROR: Failed to write response", slog.String("error", writeErr.Error()))
	}
}

// nowPlayingHandler returns information about current track.
func (s *Server) nowPlayingHandler(w http.ResponseWriter, r *http.Request) {
	route := r.URL.Query().Get("route")

	s.trackMutex.RLock()
	defer s.trackMutex.RUnlock()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	track, exists := s.currentTracks[route]
	if !exists {
		errorMsg := fmt.Sprintf("Stream %s not found", route)
		slog.Error("ERROR: %s", errorMsg)
		sentry.CaptureMessage(errorMsg)
		http.Error(w, errorMsg, http.StatusNotFound)
		return
	}
	slog.Info("Request for track information for %s: %s", route, track)
	s.mutex.RLock()
	playlist, playlistExists := s.playlists[route]
	s.mutex.RUnlock()
	currentTrackInfo := map[string]string{
		"route": route,
		"track": track,
	}
	if playlistExists {
		for attempt := 0; attempt < 3; attempt++ {
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
		slog.Error("ERROR: Failed to encode track info: %v", encodeErr)
	}
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
	slog.Info("Creating audio stream handler for route %s", route)

	contentType := ""
	switch s.streamFormat {
	case "mp3":
		contentType = "audio/mpeg"
	case "aac":
		contentType = "audio/aac"
	case "ogg":
		contentType = "audio/ogg"
	default:
		contentType = "audio/mpeg"
	}

	slog.Info("Audio stream format for route %s: %s (MIME: %s)", route, s.streamFormat, contentType)

	return func(w http.ResponseWriter, r *http.Request) {
		slog.Info("Received %s request for audio stream %s from %s", r.Method, route, r.RemoteAddr)

		s.mutex.RLock()
		stream, exists := s.streams[route]
		s.mutex.RUnlock()

		if !exists {
			errorMsg := fmt.Sprintf("Stream %s not found", route)
			slog.Error("ERROR: %s", errorMsg)
			sentry.CaptureMessage(errorMsg) // Save as this is an error
			http.Error(w, errorMsg, http.StatusNotFound)
			return
		}

		slog.Info("Stream %s found, setting up headers for streaming", route)

		// Set up streaming headers - IMPORTANT!
		// 1. Content-Type must match audio stream format
		w.Header().Set("Content-Type", contentType)
		// 2. Disable caching for live stream
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		// 3. Protect against content type guessing
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// DO NOT set Transfer-Encoding: chunked, Go will do this itself
		// DO NOT set Connection: keep-alive, this is default behavior for HTTP/1.1

		// For HEAD requests, just send headers and return
		if r.Method == http.MethodHead {
			slog.Info("Handled HEAD request for stream %s from %s", route, r.RemoteAddr)
			w.WriteHeader(http.StatusOK)
			return
		}

		// IMPORTANT: DO NOT set Content-Length for streams, otherwise browser will wait for exact byte count

		// Check for Flusher support
		flusher, ok2 := w.(http.Flusher)
		if !ok2 {
			errorMsg := "Streaming not supported"
			slog.Error("ERROR: %s", errorMsg)
			sentry.CaptureMessage(errorMsg) // Save as this is an error
			http.Error(w, errorMsg, http.StatusInternalServerError)
			return
		}

		// Immediately send headers - CRITICAL IMPORTANT!
		// This signals to the browser that streaming is starting
		flusher.Flush()

		slog.Info("Adding client to stream %s...", route)

		// Get data channel and client ID
		clientCh, clientID, addErr := stream.AddClient()
		if addErr != nil {
			slog.Error("ERROR: Error adding client to stream %s: %s", route, addErr)
			sentry.CaptureException(addErr) // Save as this is an error
			http.Error(w, addErr.Error(), http.StatusServiceUnavailable)
			return
		}
		defer stream.RemoveClient(clientID)

		// Update metrics ONLY AFTER successful connection and headers sent
		listenerCount.WithLabelValues(route).Inc()
		defer listenerCount.WithLabelValues(route).Dec()

		// Log client connection
		remoteAddr := r.RemoteAddr
		slog.Info("Client connected to stream %s: %s (ID: %d)", route, remoteAddr, clientID)

		// Check for connection closure
		clientClosed := r.Context().Done()

		slog.Info("Starting data transmission to client %d for stream %s", clientID, route)

		// Counter for tracking sent data
		var totalBytesSent int64
		var lastLogTime = time.Now()
		const logEveryBytes = mb // Log every sent megabyte

		// Send data to client
		for {
			select {
			case <-clientClosed:
				// Client disconnected
				slog.Info("Client disconnected from stream %s: %s (ID: %d). Total sent: %d bytes",
					route, remoteAddr, clientID, totalBytesSent)
				return
			case data, ok := <-clientCh:
				if !ok {
					// Channel closed
					slog.Info("Channel closed for client %s (ID: %d). Total sent: %d bytes",
						remoteAddr, clientID, totalBytesSent)
					return
				}

				// Send data to client
				n, writeErr := w.Write(data)
				if writeErr != nil {
					if isConnectionClosedError(writeErr) {
						// Just log, don't send to Sentry
						slog.Info("Client %d disconnected: %s. Total sent: %d bytes",
							clientID, writeErr, totalBytesSent)
					} else {
						// Send to Sentry only for unusual errors
						slog.Error("ERROR: Error sending data to client %d: %s. Total sent: %d bytes",
							clientID, writeErr, totalBytesSent)
						sentry.CaptureException(fmt.Errorf("error sending data to client %d: %w", clientID, writeErr))
					}
					return
				}

				// Update counter and metrics
				totalBytesSent += int64(n)
				bytesSent.WithLabelValues(route).Add(float64(n))

				// Periodically log sent data amount
				if totalBytesSent >= logEveryBytes && time.Since(lastLogTime) > logIntervalSec*time.Second {
					slog.Info("Sent %d Mbytes of data to client %d (IP: %s)",
						totalBytesSent/1024/1024, clientID, remoteAddr)
					lastLogTime = time.Now()
				}

				// MUST call Flush after EACH data sent!
				// This ensures data is sent immediately to client
				flusher.Flush()
			}
		}
	}
}

// ReloadPlaylist reloads playlist for specified route.
func (s *Server) ReloadPlaylist(route string) error {
	s.mutex.RLock()
	playlist, exists := s.playlists[route]
	s.mutex.RUnlock()

	if !exists {
		return fmt.Errorf("playlist for route %s not found", route)
	}

	return playlist.Reload()
}

// statusLoginHandler displays login form page
func (s *Server) statusLoginHandler(w http.ResponseWriter, r *http.Request) {
	// If already authenticated, redirect to status page
	if s.checkAuth(r) {
		http.Redirect(w, r, "/status-page", http.StatusFound)
		return
	}

	// Load and render login template page
	tmpl, tmplErr := template.ParseFiles(
		"templates/login.html",
		"templates/partials/head.html",
	)
	if tmplErr != nil {
		slog.Error("ERROR: Unable to load login.html template: %v", tmplErr)
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	data := map[string]interface{}{
		"Title": "Login - Audio Stream Status",
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if executeErr := tmpl.Execute(w, data); executeErr != nil {
		slog.Error("Failed to execute template", slog.String("error", executeErr.Error()))
		return
	}
}

// statusLoginSubmitHandler handles login form submission
func (s *Server) statusLoginSubmitHandler(w http.ResponseWriter, r *http.Request) {
	parseErr := r.ParseForm()
	if parseErr != nil {
		slog.Error("ERROR: Unable to parse login form: %v", parseErr)
		http.Error(w, "Form processing error", http.StatusInternalServerError)
		return
	}

	// Get password from form
	password := r.FormValue("password")

	// Check password
	if password == s.statusPassword {
		// Create cookies for authentication
		http.SetCookie(w, &http.Cookie{
			Name:     "status_auth",
			Value:    s.statusPassword,
			Path:     "/",
			Expires:  time.Now().Add(cookieExpireHours * time.Hour),
			HttpOnly: true,
		})

		// Redirect to status page
		http.Redirect(w, r, "/status-page", http.StatusFound)
		return
	}

	// If password is incorrect, show error message
	tmpl, tmplErr := template.ParseFiles(
		"templates/login.html",
		"templates/partials/head.html",
	)
	if tmplErr != nil {
		slog.Error("ERROR: Unable to load login.html template: %v", tmplErr)
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	data := map[string]interface{}{
		"Title":        "Login - Audio Stream Status",
		"ErrorMessage": "Invalid password",
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if executeErr := tmpl.Execute(w, data); executeErr != nil {
		slog.Error("Failed to execute template", slog.String("error", executeErr.Error()))
		return
	}
}

// checkAuth checks authentication for accessing status page
func (s *Server) checkAuth(r *http.Request) bool {
	cookie, err := r.Cookie("status_auth")
	return err == nil && cookie.Value == s.statusPassword
}

// statusPageHandler handles requests for the status page
func (s *Server) statusPageHandler(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(r) {
		s.redirectToLogin(w, r)
		return
	}

	s.mutex.RLock()
	defer s.mutex.RUnlock()

	type StreamInfo struct {
		Route        string
		RouteID      string
		DisplayName  string
		StartTime    string
		CurrentTrack string
		Listeners    int
		HistoryHTML  string
	}

	streams := make([]StreamInfo, 0, len(s.streams))
	for route, stream := range s.streams {
		s.trackMutex.RLock()
		currentTrack := s.currentTracks[route]
		s.trackMutex.RUnlock()

		// Get playlist history
		history := s.playlists[route].GetHistory()
		historyHTML := ""
		for i := range make([]struct{}, len(history)) {
			if i > 0 {
				historyHTML += "<br>"
			}
			historyHTML += html.EscapeString(fmt.Sprintf("%v", history[i]))
		}

		streams = append(streams, StreamInfo{
			Route:        route,
			RouteID:      strings.TrimPrefix(route, "/"),
			DisplayName:  strings.TrimPrefix(route, "/"),
			StartTime:    s.playlists[route].GetStartTime().Format("15:04:05"),
			CurrentTrack: currentTrack,
			Listeners:    stream.GetClientCount(),
			HistoryHTML:  historyHTML,
		})
	}

	// Sort streams by route for consistent output
	sort.Slice(streams, func(i, j int) bool {
		return streams[i].Route < streams[j].Route
	})

	tmpl := template.Must(template.ParseFiles("templates/status.html"))
	if executeErr := tmpl.Execute(w, map[string]interface{}{
		"Streams": streams,
	}); executeErr != nil {
		slog.Error("Failed to execute template", slog.String("error", executeErr.Error()))
		return
	}
}

// handleTrackSwitchHandler handles track switching
func (s *Server) handleTrackSwitchHandler(w http.ResponseWriter, r *http.Request, direction string) {
	if !s.checkAuth(r) {
		isAjax := r.Header.Get(xmlHTTPRequestHeader) == "1" || r.URL.Query().Get("ajax") == "1"
		if isAjax {
			w.Header().Set("Content-Type", "application/json")
			if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"route":   "",
				"error":   "Authentication required",
			}); encodeErr != nil {
				slog.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
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
		isAjax := r.Header.Get(xmlHTTPRequestHeader) == "1" || r.URL.Query().Get("ajax") == "1"
		if isAjax {
			w.Header().Set("Content-Type", "application/json")
			if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"route":   route,
				"error":   "Route not found",
			}); encodeErr != nil {
				slog.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
				return
			}
			return
		}
		http.Error(w, "Route not found", http.StatusNotFound)
		return
	}

	var track interface{}
	if direction == "next" {
		track = playlist.NextTrack()
	} else {
		track = playlist.PreviousTrack()
	}

	if track == nil {
		c := cases.Title(language.Und)
		slog.Warn("WARNING: %sTrack returned nil for route %s, likely timeout", c.String(direction), route)
		isAjax := r.Header.Get(xmlHTTPRequestHeader) == "1" || r.URL.Query().Get("ajax") == "1"
		if isAjax {
			w.Header().Set("Content-Type", "application/json")
			if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"route":   route,
				"error":   "Failed to switch track, operation timed out",
			}); encodeErr != nil {
				slog.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
				return
			}
		} else {
			http.Error(w, "Failed to switch track, operation timed out", http.StatusInternalServerError)
		}
		return
	}

	slog.Info("DIAGNOSTICS: Manual track switch to %s track for route %s", direction, route)

	newTrackName := "Unknown"
	if s.stationManager == nil {
		slog.Warn("WARNING: Station manager not set, restart playback not possible")
		isAjax := r.Header.Get(xmlHTTPRequestHeader) == "1" || r.URL.Query().Get("ajax") == "1"
		if isAjax {
			w.Header().Set("Content-Type", "application/json")
			if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"route":   route,
				"track":   newTrackName,
			}); encodeErr != nil {
				slog.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
				return
			}
		} else {
			http.Redirect(w, r, "/status-page", http.StatusFound)
		}
		return
	}

	success := s.stationManager.RestartPlayback(route)
	if success {
		slog.Info("DIAGNOSTICS: Restart playback for route %s completed successfully", route)
		if trackWithPath, ok := track.(interface{ GetPath() string }); ok {
			newTrackName = filepath.Base(trackWithPath.GetPath())
			s.trackMutex.Lock()
			s.currentTracks[route] = newTrackName
			s.trackMutex.Unlock()
			slog.Info("DIAGNOSTICS: Immediately updated current track information for %s: %s", route, newTrackName)
		}
	} else {
		slog.Error("ERROR: Unable to restart playback for route %s", route)
	}
	isAjax := r.Header.Get(xmlHTTPRequestHeader) == "1" || r.URL.Query().Get("ajax") == "1"
	if isAjax {
		w.Header().Set("Content-Type", "application/json")
		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"success": success,
			"route":   route,
			"track":   newTrackName,
		}); encodeErr != nil {
			slog.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
			return
		}
	} else {
		http.Redirect(w, r, "/status-page", http.StatusFound)
	}
}

func (s *Server) nextTrackHandler(w http.ResponseWriter, r *http.Request) {
	s.handleTrackSwitchHandler(w, r, "next")
}

func (s *Server) prevTrackHandler(w http.ResponseWriter, r *http.Request) {
	s.handleTrackSwitchHandler(w, r, "prev")
}

// notFoundHandler handles requests to non-existent routes
func (s *Server) notFoundHandler(w http.ResponseWriter, r *http.Request) {
	slog.Info("404 - route not found: %s", r.URL.Path)

	// Load and render 404 template page
	tmpl, err := template.ParseFiles(
		"templates/404.html",
		"templates/partials/head.html",
	)
	if err != nil {
		slog.Error("ERROR: Unable to load 404.html template: %v", err)
		http.Error(w, "Page not found", http.StatusNotFound)
		return
	}

	data := map[string]interface{}{
		"Title": "404 - Page not found",
		"Path":  r.URL.Path,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	if executeErr := tmpl.Execute(w, data); executeErr != nil {
		slog.Error("Failed to execute template", slog.String("error", executeErr.Error()))
		return
	}
}

// SetStationManager sets station manager for restarting playback.
func (s *Server) SetStationManager(manager interface{ RestartPlayback(string) bool }) {
	s.stationManager = manager
}

// redirectToLogin redirects user to login page
func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/status", http.StatusSeeOther)
}

// handleShufflePlaylist handles the request to manually shuffle a playlist
func (s *Server) handleShufflePlaylist(w http.ResponseWriter, r *http.Request) {
	// Check authentication (same as for status page)
	if !s.checkAuth(r) {
		// Check if this is an AJAX request
		ajax := r.URL.Query().Get("ajax")
		isAjax := r.Header.Get(xmlHTTPRequestHeader) == "1" || ajax == "1"

		if isAjax {
			// For AJAX requests, send JSON error response
			w.Header().Set("Content-Type", "application/json")
			if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "Authentication required",
				"route":   "",
			}); encodeErr != nil {
				slog.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
				return
			}
			return
		}

		// For regular requests, redirect to login page
		s.redirectToLogin(w, r)
		return
	}

	vars := mux.Vars(r)
	route := "/" + vars["route"]

	// Get playlist by route
	s.mutex.RLock()
	playlist, ok := s.playlists[route]
	s.mutex.RUnlock()

	if !ok {
		http.Error(w, "Playlist not found", http.StatusNotFound)
		return
	}

	// Call Shuffle method on the playlist with error handling
	slog.Info("Manual shuffle requested for route %s", route)

	// Execute shuffle in goroutine with timeout protection
	shuffleDone := make(chan bool, 1)

	go func() {
		defer func() {
			// Catch any panics from Shuffle operation
			if r := recover(); r != nil {
				slog.Error("ERROR: Shuffle operation panicked: %v", r)
				shuffleDone <- false
			}
		}()

		// Call shuffle
		playlist.Shuffle()
		shuffleDone <- true
	}()

	// Wait for shuffle to complete with timeout
	select {
	case <-shuffleDone:
		slog.Info("Manual shuffle completed for route %s", route)
	case <-time.After(1 * time.Second):
		slog.Warn("WARNING: Shuffle operation timed out for route %s", route)
	}

	// Check if need to return JSON or perform redirect
	ajax := r.URL.Query().Get("ajax")
	if ajax == "1" {
		w.Header().Set("Content-Type", "application/json")
		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Playlist shuffled successfully",
			"route":   route,
		}); encodeErr != nil {
			slog.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
			return
		}
	} else {
		// Redirect back to status page
		http.Redirect(w, r, "/status-page", http.StatusSeeOther)
	}
}

// SetShuffleMode toggles the shuffle mode for a specific stream
func (s *Server) SetShuffleMode(w http.ResponseWriter, r *http.Request) {
	// Check authentication (same as for status page)
	if !s.checkAuth(r) {
		// Check if this is an AJAX request
		ajax := r.URL.Query().Get("ajax")
		isAjax := r.Header.Get(xmlHTTPRequestHeader) == "1" || ajax == "1"

		if isAjax {
			// For AJAX requests, send JSON error response
			w.Header().Set("Content-Type", "application/json")
			if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "Authentication required",
				"route":   "",
			}); encodeErr != nil {
				slog.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
				return
			}
			return
		}

		// For regular requests, redirect to login page
		s.redirectToLogin(w, r)
		return
	}

	// Get route and mode from URL
	vars := mux.Vars(r)
	route := "/" + vars["route"]
	mode := vars["mode"]

	if mode != "on" && mode != "off" {
		http.Error(w, "Invalid mode. Use 'on' or 'off'", http.StatusBadRequest)
		return
	}

	shuffleEnabled := mode == "on"

	// Get playlist by route
	s.mutex.RLock()
	playlist, ok := s.playlists[route]
	s.mutex.RUnlock()

	if !ok {
		http.Error(w, "Playlist not found", http.StatusNotFound)
		return
	}

	// Get the playlist type to check if we can set shuffle mode
	slog.Info("Setting shuffle mode %s for route %s", mode, route)

	// If shuffle mode is enabled, call Shuffle method, otherwise reload the playlist
	// This will result in a sequential playlist
	var success bool

	if !shuffleEnabled {
		// To effectively disable shuffle, we need to reload the playlist
		// which will reset it to sequential order
		err := playlist.Reload()
		if err != nil {
			slog.Error("ERROR: Failed to disable shuffle mode for route %s: %v", route, err)
			success = false
		} else {
			slog.Info("Shuffle mode disabled for route %s", route)
			success = true
		}
	} else {
		// Execute shuffle in goroutine with timeout protection
		shuffleDone := make(chan bool, 1)

		go func() {
			defer func() {
				// Catch any panics from Shuffle operation
				if r := recover(); r != nil {
					slog.Error("ERROR: Shuffle operation panicked: %v", r)
					shuffleDone <- false
				}
			}()

			// Call shuffle
			playlist.Shuffle()
			shuffleDone <- true
		}()

		// Wait for shuffle to complete with timeout
		select {
		case success = <-shuffleDone:
			if success {
				slog.Info("Shuffle mode enabled for route %s", route)
			} else {
				slog.Error("Failed to enable shuffle mode for route %s", route)
			}
		case <-time.After(1 * time.Second):
			slog.Warn("WARNING: Shuffle operation timed out for route %s", route)
			success = false
		}
	}

	// Check if need to return JSON or perform redirect
	ajax := r.URL.Query().Get("ajax")
	if ajax == "1" {
		w.Header().Set("Content-Type", "application/json")
		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"success": success,
			"message": fmt.Sprintf("Shuffle mode %s for route %s", mode, route),
			"route":   route,
			"mode":    mode,
		}); encodeErr != nil {
			slog.Error("Failed to encode JSON response", slog.String("error", encodeErr.Error()))
			return
		}
	} else {
		// Redirect back to status page
		http.Redirect(w, r, "/status-page", http.StatusSeeOther)
	}
}

// SetStatusPassword sets the password for status page (used for testing).
func (s *Server) SetStatusPassword(password string) {
	s.statusPassword = password
}

// SetRelayManager sets the relay manager for the server.
func (s *Server) SetRelayManager(manager *relay.RelayManager) {
	s.relayManager = manager
	slog.Info("Relay manager set for HTTP server")

	// Configure relay routes only when relay manager is set
	s.setupRelayRoutes()
}

// setupRelayRoutes adds relay-related routes to the router
func (s *Server) setupRelayRoutes() {
	// Add relay management page
	s.router.HandleFunc("/relay-management", s.relayManagementHandler).Methods("GET")

	// API endpoints for relay management
	s.router.HandleFunc("/relay/toggle", s.relayToggleHandler).Methods("POST")
	s.router.HandleFunc("/relay/add", s.relayAddHandler).Methods("POST")
	s.router.HandleFunc("/relay/remove", s.relayRemoveHandler).Methods("POST")
	s.router.HandleFunc("/relay/stream/{index:[0-9]+}", s.relayStreamHandler).Methods("GET")

	slog.Info("Relay routes configured")
}

// Relay handler functions
// relayManagementHandler serves the relay management UI
func (s *Server) relayManagementHandler(w http.ResponseWriter, r *http.Request) {
	// Check authentication
	if !s.checkAuth(r) {
		s.redirectToLogin(w, r)
		return
	}

	// Check if relay manager is set
	if s.relayManager == nil {
		http.Error(w, "Relay functionality is not available", http.StatusServiceUnavailable)
		return
	}

	// Load and render template
	tmpl, err := template.ParseFiles(
		"templates/relay.html",
		"templates/partials/head.html",
	)
	if err != nil {
		slog.Error("ERROR: Unable to load relay.html template: %v", err)
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	// Get relay links from manager
	links := s.relayManager.GetLinks()

	// Get status messages from URL query parameters
	errorMessage := r.URL.Query().Get("error")
	successMessage := r.URL.Query().Get("success")

	data := map[string]interface{}{
		"Title":          "Relay Stream Management",
		"RelayLinks":     links,
		"RelayActive":    s.relayManager.IsActive(),
		"ErrorMessage":   errorMessage,
		"SuccessMessage": successMessage,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if executeErr := tmpl.Execute(w, data); executeErr != nil {
		slog.Error("Failed to execute template", slog.String("error", executeErr.Error()))
		return
	}
}

// relayToggleHandler toggles relay functionality on/off
func (s *Server) relayToggleHandler(w http.ResponseWriter, r *http.Request) {
	// Check authentication
	if !s.checkAuth(r) {
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return
	}

	// Check if relay manager is set
	if s.relayManager == nil {
		http.Error(w, "Relay functionality is not available", http.StatusServiceUnavailable)
		return
	}

	// Parse active state from form
	activeStr := r.FormValue("active")
	active, err := strconv.ParseBool(activeStr)
	if err != nil {
		slog.Error("ERROR: Invalid active value: %s", activeStr)
		http.Redirect(w, r, "/relay-management?error=Invalid active value", http.StatusSeeOther)
		return
	}

	// Set active state
	s.relayManager.SetActive(active)

	// Redirect back to relay management page
	status := "enabled"
	if !active {
		status = "disabled"
	}

	http.Redirect(w, r, "/relay-management?success=Relay "+status, http.StatusSeeOther)
}

// relayAddHandler adds a new relay URL
func (s *Server) relayAddHandler(w http.ResponseWriter, r *http.Request) {
	// Check authentication
	if !s.checkAuth(r) {
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return
	}

	// Check if relay manager is set
	if s.relayManager == nil {
		http.Error(w, "Relay functionality is not available", http.StatusServiceUnavailable)
		return
	}

	// Parse URL from form
	url := r.FormValue("url")
	if url == "" {
		http.Redirect(w, r, "/relay-management?error=URL cannot be empty", http.StatusSeeOther)
		return
	}

	// Add URL to relay manager
	if err := s.relayManager.AddLink(url); err != nil {
		slog.Error("ERROR: Failed to add relay URL: %v", err)
		http.Redirect(w, r, "/relay-management?error="+err.Error(), http.StatusSeeOther)
		return
	}

	// Redirect back to relay management page
	http.Redirect(w, r, "/relay-management?success=Relay URL added successfully", http.StatusSeeOther)
}

// relayRemoveHandler removes a relay URL by index
func (s *Server) relayRemoveHandler(w http.ResponseWriter, r *http.Request) {
	// Check authentication
	if !s.checkAuth(r) {
		http.Error(w, "Authentication required", http.StatusUnauthorized)
		return
	}

	// Check if relay manager is set
	if s.relayManager == nil {
		http.Error(w, "Relay functionality is not available", http.StatusServiceUnavailable)
		return
	}

	// Parse index from form
	indexStr := r.FormValue("index")
	index, err := strconv.Atoi(indexStr)
	if err != nil {
		slog.Error("ERROR: Invalid index: %s", indexStr)
		http.Redirect(w, r, "/relay-management?error=Invalid index", http.StatusSeeOther)
		return
	}

	// Remove URL from relay manager
	if err := s.relayManager.RemoveLink(index); err != nil {
		slog.Error("ERROR: Failed to remove relay URL: %v", err)
		http.Redirect(w, r, "/relay-management?error="+err.Error(), http.StatusSeeOther)
		return
	}

	// Redirect back to relay management page
	http.Redirect(w, r, "/relay-management?success=Relay URL removed successfully", http.StatusSeeOther)
}

// relayStreamHandler streams the audio from a relay source
func (s *Server) relayStreamHandler(w http.ResponseWriter, r *http.Request) {
	// Check if relay manager is set
	if s.relayManager == nil {
		http.Error(w, "Relay functionality is not available", http.StatusServiceUnavailable)
		return
	}

	// Parse index from URL
	vars := mux.Vars(r)
	indexStr := vars["index"]
	index, err := strconv.Atoi(indexStr)
	if err != nil {
		slog.Error("ERROR: Invalid index: %s", indexStr)
		http.Error(w, "Invalid index", http.StatusBadRequest)
		return
	}

	// Stream the relay audio
	if err := s.relayManager.RelayAudioStream(w, r, index); err != nil {
		slog.Error("ERROR: Failed to relay audio stream: %v", err)
		http.Error(w, "Failed to relay audio stream: "+err.Error(), http.StatusInternalServerError)
		return
	}
}
