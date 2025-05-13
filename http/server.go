// Package http implements the HTTP server for audio streaming.
// It provides endpoints for streaming audio, health checks, and status monitoring.
package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	sentry "github.com/getsentry/sentry-go"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/user/stream-audio-to-web/audio"
	"github.com/user/stream-audio-to-web/relay"

	html "html"
)

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
	relayManager   *relay.Manager                            // Manager for relay functionality.
	// Prometheus metrics.
	listenerCount     *prometheus.GaugeVec   // Gauge for tracking active listeners.
	bytesSent         *prometheus.CounterVec // Counter for bytes sent.
	trackSecondsTotal *prometheus.CounterVec // Counter for audio playback time.
	logger            *slog.Logger           // Logger for server operations.
}

const xmlHTTPRequestHeader = "Xmlhttprequest"

const (
	defaultStatusPassword     = "1234554321"
	defaultStreamsSortTimeout = 1 * time.Second
	asciiMax                  = 127
	shortSleepMs              = 50
	kb                        = 1024
	mb                        = 1024 * 1024
	cookieExpireHours         = 24
	logIntervalSec            = 5
	logEveryBytes             = mb // Log every sent megabyte.
)

// NewServer creates a new HTTP server.
func NewServer(streamFormat string, maxClients int) *Server {
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

	// Create logger.
	logger := slog.Default()

	server := &Server{
		router:            mux.NewRouter(),
		streams:           make(map[string]StreamHandler),
		playlists:         make(map[string]PlaylistManager),
		streamFormat:      streamFormat,
		maxClients:        maxClients,
		currentTracks:     make(map[string]string),
		statusPassword:    statusPassword,
		listenerCount:     listenerCount,
		bytesSent:         bytesSent,
		trackSecondsTotal: trackSecondsTotal,
		logger:            logger,
	}

	// Setup routes.
	server.setupRoutes()

	server.logger.Info(
		"HTTP server created",
		slog.String("streamFormat", streamFormat),
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
	s.logger.Info("DIAGNOSTICS: Starting audio stream registration", slog.String("route", route))

	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Make sure route starts with a slash.
	if route[0] != '/' {
		route = "/" + route
		s.logger.Info("Fixed route during registration", slog.String("route", route))
	}

	s.logger.Info("DIAGNOSTICS: Adding stream to streams map", slog.String("route", route))
	s.streams[route] = stream
	s.logger.Info("DIAGNOSTICS: Adding playlist to playlists map", slog.String("route", route))
	s.playlists[route] = playlist

	// Set trackSecondsTotal metric directly to stream.
	if streamer, ok := stream.(*audio.Streamer); ok {
		streamer.SetTrackSecondsMetric(s.trackSecondsTotal)
		s.logger.Info("DIAGNOSTICS: trackSecondsTotal metric set for streamer", slog.String("route", route))
	}

	// Check if stream was added.
	if _, exists := s.streams[route]; exists {
		s.logger.Info("DIAGNOSTICS: Stream for route successfully added to streams map", slog.String("route", route))
	} else {
		s.logger.Error("ERROR: Stream for route was not added to streams map!", slog.String("route", route))
	}

	// IMPORTANT: Register route handler in router for GET and HEAD requests.
	s.router.HandleFunc(route, s.StreamAudioHandler(route)).Methods("GET", "HEAD")
	s.logger.Info("DIAGNOSTICS: HTTP handler registered for route", slog.String("route", route))

	// Start goroutine to track current track.
	s.logger.Info("DIAGNOSTICS: Starting goroutine to track current track", slog.String("route", route))
	go s.trackCurrentTrack(route, stream.GetCurrentTrackChannel())

	s.logger.Info("DIAGNOSTICS: Audio stream for route successfully registered", slog.String("route", route))
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
		s.logger.Info(
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

// setupRoutes configures HTTP server routes.
func (s *Server) setupRoutes() {
	// Monitoring and health endpoints.
	s.router.HandleFunc("/healthz", s.healthzHandler).Methods("GET", "HEAD")
	s.router.HandleFunc("/readyz", s.readyzHandler).Methods("GET", "HEAD")
	s.router.Handle("/metrics", promhttp.Handler()).Methods("GET")

	// API for playlist management.
	s.router.HandleFunc("/streams", s.streamsHandler).Methods("GET")
	s.router.HandleFunc("/reload-playlist", s.reloadPlaylistHandler).Methods("POST")
	s.router.HandleFunc("/now-playing", s.nowPlayingHandler).Methods("GET")

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
	// Log healthz request.
	s.logger.Info(
		"Received healthz request",
		slog.String("method", r.Method),
		slog.String("remote_addr", r.RemoteAddr),
	)

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
		s.logger.Info("WARNING: No registered streams, but server is running", slog.String("status", "no_streams"))
	} else {
		s.logger.Info("Healthz status", slog.Int("streamsCount", streamsCount), slog.Any("routes", streamsList))
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
			s.logger.Error("Failed to write healthz response", slog.String("error", writeErr.Error()))
			return
		}

		// Force send response.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	// Additional logging of successful response.
	s.logger.Info("Sent successful healthz response to client", slog.String("remoteAddr", r.RemoteAddr))
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
				sentry.CaptureException(fmt.Errorf("error reloading playlist: %w", reloadErr))
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
		sentry.CaptureMessage(errorMsg)
		http.Error(w, errorMsg, http.StatusNotFound)
		return
	}

	if reloadErr := playlist.Reload(); reloadErr != nil {
		errorMsg := fmt.Sprintf("Error reloading playlist: %s", reloadErr)
		s.logger.Error("Error reloading playlist", slog.String("route", route), slog.String("error", reloadErr.Error()))
		sentry.CaptureException(fmt.Errorf("error reloading playlist for %s: %w", route, reloadErr))
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

	s.trackMutex.RLock()
	defer s.trackMutex.RUnlock()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// Проверка на пустой маршрут.
	if route == "" {
		errorMsg := "Route parameter is required"
		s.logger.Error("ERROR: Empty route parameter")
		// Обработка ошибки используя логирование при отсутствии маршрута
		s.logNormalizationError(errors.New("empty route parameter"), "nowPlayingHandler")
		http.Error(w, errorMsg, http.StatusBadRequest)
		return
	}

	track, exists := s.currentTracks[route]
	if !exists {
		errorMsg := fmt.Sprintf("Stream %s not found", route)
		s.logger.Error("ERROR: Stream not found", slog.String("route", route))
		sentry.CaptureMessage(errorMsg)
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
		// Обработка ошибки JSON используя логирование
		s.logNormalizationError(encodeErr, fmt.Sprintf("json encode for route %s", route))
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
	s.logger.Info("Creating audio stream handler", slog.String("route", route))

	contentType := s.determineContentType()
	s.logger.Info(
		"Audio stream format",
		slog.String("route", route),
		slog.String("format", s.streamFormat),
		slog.String("MIME", contentType),
	)

	return func(w http.ResponseWriter, r *http.Request) {
		s.logger.Info(
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
			s.logger.Info("Handled HEAD request", slog.String("route", route), slog.String("remoteAddr", r.RemoteAddr))
			w.WriteHeader(http.StatusOK)
			return
		}

		// Verify flushing capability.
		flusher, ok := w.(http.Flusher)
		if !ok {
			errorMsg := "Streaming not supported"
			s.logger.Error("ERROR: Streaming not supported")
			sentry.CaptureMessage(errorMsg) // Save as this is an error
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

// determineContentType sets the appropriate MIME type based on stream format.
func (s *Server) determineContentType() string {
	var contentType string
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
	return contentType
}

// getStream retrieves the stream handler for the given route.
func (s *Server) getStream(route string, w http.ResponseWriter) (StreamHandler, bool) {
	s.mutex.RLock()
	stream, exists := s.streams[route]
	s.mutex.RUnlock()

	if !exists {
		errorMsg := fmt.Sprintf("Stream %s not found", route)
		s.logger.Error("ERROR: Stream not found", slog.String("route", route))
		sentry.CaptureMessage(errorMsg) // Save as this is an error
		http.Error(w, errorMsg, http.StatusNotFound)
		return nil, false
	}

	s.logger.Info("Stream found, setting up headers", slog.String("route", route))
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
		sentry.CaptureException(addErr) // Save as this is an error
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
			s.logger.Info("Client disconnected from stream",
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
			s.logger.Info("Client disconnected",
				slog.Int("clientID", clientData.clientID),
				slog.String("error", writeErr.Error()),
				slog.Int64("totalBytesSent", *totalBytesSent))
		} else {
			// Send to Sentry only for unusual errors.
			s.logger.Error("ERROR: Error sending data to client",
				slog.Int("clientID", clientData.clientID),
				slog.String("error", writeErr.Error()),
				slog.Int64("totalBytesSent", *totalBytesSent))
			sentry.CaptureException(fmt.Errorf("error sending data to client %d: %w", clientData.clientID, writeErr))
		}
		return writeErr
	}

	*totalBytesSent += int64(n)
	return nil
}

// logDataTransfer logs information about data transfer periodically.
func (s *Server) logDataTransfer(clientData *clientStreamData, totalBytesSent *int64, lastLogTime *time.Time) {
	if *totalBytesSent >= logEveryBytes && time.Since(*lastLogTime) > logIntervalSec*time.Second {
		s.logger.Info("Sent data to client",
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
			sentry.CaptureException(fmt.Errorf("error reloading playlist: %w", reloadErr))
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
		sentry.CaptureMessage(errorMsg)
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
		sentry.CaptureMessage(fmt.Sprintf("Playlist for %s not found", route))
		return fmt.Errorf("playlist for %s not found", route)
	}

	// This is where we actually load and update the playlist.
	reloadErr := playlist.Reload()
	if reloadErr != nil {
		s.logger.Error("Error reloading playlist", slog.String("route", route), slog.String("error", reloadErr.Error()))
		sentry.CaptureException(fmt.Errorf("error reloading playlist for %s: %w", route, reloadErr))
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
		"Title": "Login - Audio Stream Status",
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

		// Redirect to status page.
		http.Redirect(w, r, "/status-page", http.StatusFound)
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

		// Get playlist history.
		history := s.playlists[route].GetHistory()
		historyHTML := ""
		for i, item := range history {
			if i > 0 {
				historyHTML += "<br>"
			}
			historyHTML += html.EscapeString(fmt.Sprintf("%v", item))
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
		"Streams":      streams,
		"Title":        "Audio Streams Status",
		"RelayEnabled": s.relayManager != nil,
		"RelayActive":  s.relayManager != nil && s.relayManager.IsActive(),
	}); executeErr != nil {
		s.logger.Error("Failed to execute status template", slog.String("error", executeErr.Error()))
		http.Error(w, "Server error: "+executeErr.Error(), http.StatusInternalServerError)
		return
	}
}

// handleTrackSwitchHandler handles track switching.
func (s *Server) handleTrackSwitchHandler(w http.ResponseWriter, r *http.Request, direction string) {
	// Check authentication.
	if !s.checkAuth(r) {
		s.handleUnauthenticatedTrackSwitch(w, r)
		return
	}

	// Get route.
	vars := mux.Vars(r)
	route := "/" + vars["route"]

	// Get playlist.
	playlist, exists := s.getPlaylistForRoute(route)
	if !exists {
		s.handlePlaylistNotFound(w, r, route)
		return
	}

	// Get track.
	track := s.getTrackForDirection(playlist, direction)
	if track == nil {
		s.handleTrackNotFound(w, r, route, direction)
		return
	}

	s.logger.Info("Manual track switch", slog.String("direction", direction), slog.String("route", route))

	// Handle track switching.
	s.handleTrackSwitchResult(w, r, route, track)
}

// handleUnauthenticatedTrackSwitch handles unauthenticated track switching requests.
func (s *Server) handleUnauthenticatedTrackSwitch(w http.ResponseWriter, r *http.Request) {
	isAjax := r.Header.Get(xmlHTTPRequestHeader) == "1" || r.URL.Query().Get("ajax") == "1"
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
	isAjax := r.Header.Get(xmlHTTPRequestHeader) == "1" || r.URL.Query().Get("ajax") == "1"
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
	if direction == "next" {
		return playlist.NextTrack()
	}
	return playlist.PreviousTrack()
}

// handleTrackNotFound handles case when track for direction is not found.
func (s *Server) handleTrackNotFound(w http.ResponseWriter, r *http.Request, route, direction string) {
	s.logger.Error("Track not found for direction", slog.String("direction", direction))
	isAjax := r.Header.Get(xmlHTTPRequestHeader) == "1" || r.URL.Query().Get("ajax") == "1"
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
	isAjax := r.Header.Get(xmlHTTPRequestHeader) == "1" || r.URL.Query().Get("ajax") == "1"
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
	isAjax := r.Header.Get(xmlHTTPRequestHeader) == "1" || r.URL.Query().Get("ajax") == "1"
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

	http.Redirect(w, r, "/status", http.StatusFound)
}

// handleShufflePlaylist shuffles the playlist for a specific stream.
func (s *Server) handleShufflePlaylist(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(r) {
		isAjax := r.Header.Get(xmlHTTPRequestHeader) == "1" || r.URL.Query().Get("ajax") == "1"
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
		isAjax := r.Header.Get(xmlHTTPRequestHeader) == "1" || r.URL.Query().Get("ajax") == "1"
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

	isAjax := r.Header.Get(xmlHTTPRequestHeader) == "1" || r.URL.Query().Get("ajax") == "1"
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
		isAjax := r.Header.Get(xmlHTTPRequestHeader) == "1" || r.URL.Query().Get("ajax") == "1"
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
		isAjax := r.Header.Get(xmlHTTPRequestHeader) == "1" || r.URL.Query().Get("ajax") == "1"
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
		isAjax := r.Header.Get(xmlHTTPRequestHeader) == "1" || r.URL.Query().Get("ajax") == "1"
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
	// TODO: Implement setting shuffle mode for the playlist.
	// This would require extending the PlaylistManager interface.
	// For now, just acknowledge the request.

	isAjax := r.Header.Get(xmlHTTPRequestHeader) == "1" || r.URL.Query().Get("ajax") == "1"
	if isAjax {
		w.Header().Set("Content-Type", "application/json")
		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"route":   route,
			"mode":    mode,
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

// SetStatusPassword sets the password for accessing the status page.
func (s *Server) SetStatusPassword(password string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.statusPassword = password
	s.logger.Info("Status password set for HTTP server")
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

// ERROR during audio normalization error="error streaming to buffer: EOF".
func (s *Server) logNormalizationError(err error, filePath string) {
	// Обрабатываем только критические ошибки для Sentry.
	if err != nil {
		// Проверяем, содержит ли ошибка строку "EOF", которая обычно не является критической.
		if strings.Contains(err.Error(), "EOF") {
			// Для EOF-ошибок только логируем без отправки в Sentry.
			s.logger.Info("DIAGNOSTICS: Non-critical normalization error",
				slog.String("error", err.Error()),
				slog.String("filePath", filePath))
		} else {
			// Для более серьезных ошибок и логируем, и отправляем в Sentry.
			errMsg := fmt.Sprintf("ERROR during audio normalization error=%q", err.Error())
			s.logger.Error(errMsg)
			sentry.CaptureMessage(errMsg)
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
