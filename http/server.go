package http

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/user/stream-audio-to-web/audio"
)

var (
	// Prometheus metrics
	listenerCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "audio_stream_listener_count",
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
	
	// Counter for audio playback time in seconds
	trackSecondsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "audio_stream_track_seconds_total",
			Help: "Total seconds of audio played",
		},
		[]string{"stream"},
	)
)

func init() {
	// Register Prometheus metrics
	prometheus.MustRegister(listenerCount)
	prometheus.MustRegister(bytesSent)
	prometheus.MustRegister(trackSecondsTotal)
}

// StreamHandler interface for handling audio stream
type StreamHandler interface {
	AddClient() (<-chan []byte, int, error)
	RemoveClient(clientID int)
	GetClientCount() int
	GetCurrentTrackChannel() <-chan string
}

// PlaylistManager interface for playlist management
type PlaylistManager interface {
	Reload() error
	GetCurrentTrack() interface{}
	NextTrack() interface{}
	GetHistory() []interface{} // Get track history
	GetStartTime() time.Time   // Get start time
	PreviousTrack() interface{} // Switch to previous track
	Shuffle() // Shuffle the playlist
}

// Server represents HTTP server for audio streaming
type Server struct {
	router          *mux.Router
	streams         map[string]StreamHandler
	playlists       map[string]PlaylistManager
	streamFormat    string
	maxClients      int
	mutex           sync.RWMutex
	currentTracks   map[string]string
	trackMutex      sync.RWMutex
	statusPassword  string // Password for accessing /status page
	stationManager  interface { RestartPlayback(string) bool } // Interface for restarting playback
}

// NewServer creates a new HTTP server
func NewServer(streamFormat string, maxClients int) *Server {
	// Get password for status page from environment variable
	statusPassword := getEnvOrDefault("STATUS_PASSWORD", "1234554321")
	
	server := &Server{
		router:         mux.NewRouter(),
		streams:        make(map[string]StreamHandler),
		playlists:      make(map[string]PlaylistManager),
		streamFormat:   streamFormat,
		maxClients:     maxClients,
		currentTracks:  make(map[string]string),
		statusPassword: statusPassword,
	}

	// Setup routes
	server.setupRoutes()
	
	// Pass trackSecondsTotal metric directly to audio package
	audio.SetTrackSecondsMetric(trackSecondsTotal)
	log.Printf("DIAGNOSTICS: trackSecondsTotal metric passed to audio package")

	log.Printf("HTTP server created, stream format: %s, max clients: %d", streamFormat, maxClients)
	return server
}

// Helper function to get environment variable value
func getEnvOrDefault(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

// Handler returns HTTP request handler
func (s *Server) Handler() http.Handler {
	return s.router
}

// RegisterStream registers a new audio stream
func (s *Server) RegisterStream(route string, stream StreamHandler, playlist PlaylistManager) {
	log.Printf("DIAGNOSTICS: Starting audio stream registration for route '%s'", route)
	
	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Make sure route starts with a slash
	if route[0] != '/' {
		route = "/" + route
		log.Printf("Fixed route during registration: '%s'", route)
	}

	log.Printf("DIAGNOSTICS: Adding stream to streams map for route '%s'", route)
	s.streams[route] = stream
	log.Printf("DIAGNOSTICS: Adding playlist to playlists map for route '%s'", route)
	s.playlists[route] = playlist

	// Check if stream was added
	if _, exists := s.streams[route]; exists {
		log.Printf("DIAGNOSTICS: Stream for route '%s' successfully added to streams map", route)
	} else {
		log.Printf("ERROR: Stream for route '%s' was not added to streams map!", route)
	}
	
	// IMPORTANT: Register route handler in router for GET and HEAD requests
	s.router.HandleFunc(route, s.StreamAudioHandler(route)).Methods("GET", "HEAD")
	log.Printf("DIAGNOSTICS: HTTP handler registered for route '%s'", route)

	// Start goroutine to track current track
	log.Printf("DIAGNOSTICS: Starting goroutine to track current track for route '%s'", route)
	go s.trackCurrentTrack(route, stream.GetCurrentTrackChannel())

	log.Printf("DIAGNOSTICS: Audio stream for route '%s' successfully registered", route)
}

// IsStreamRegistered checks if stream with specified route is registered
func (s *Server) IsStreamRegistered(route string) bool {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	
	// Check if stream exists for the specified route
	_, exists := s.streams[route]
	return exists
}

// trackCurrentTrack tracks current track for specified stream
func (s *Server) trackCurrentTrack(route string, trackCh <-chan string) {
	for trackPath := range trackCh {
		// Extract only filename without path
		fileName := filepath.Base(trackPath)
		
		// Handle specific characters in filename (logging)
		log.Printf("Current track for %s: %s (path: %s)", route, fileName, trackPath)
		
		// Save extended information to Sentry only for problematic filenames with unicode
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

// hasUnicodeChars checks for non-ASCII characters in string
func hasUnicodeChars(s string) bool {
	for _, r := range s {
		if r > 127 {
			return true
		}
	}
	return false
}

// setupRoutes configures HTTP server routes
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

	log.Printf("HTTP routes configured")
}

// healthzHandler returns 200 OK if server is running
func (s *Server) healthzHandler(w http.ResponseWriter, r *http.Request) {
	// Log healthz request
	log.Printf("Received %s healthz request from %s (URI: %s)", r.Method, r.RemoteAddr, r.RequestURI)
	
	// Check for registered streams
	s.mutex.RLock()
	streamsCount := len(s.streams)
	streamsList := make([]string, 0, streamsCount)
	for route := range s.streams {
		streamsList = append(streamsList, route)
	}
	s.mutex.RUnlock()
	
	// Log status
	if streamsCount == 0 {
		log.Printf("WARNING: No registered streams, but server is running")
	} else {
		log.Printf("Healthz status: %d streams registered. Routes: %v", streamsCount, streamsList)
	}
	
	// Add headers to prevent caching
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Content-Type", "text/plain")
	
	// Always return successful response, as streams may be configured asynchronously
	w.WriteHeader(http.StatusOK)
	
	// If not a HEAD request, send response body
	if r.Method != "HEAD" {
		w.Write([]byte("OK"))
		
		// Force send response
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	
	// Additional logging of successful response
	log.Printf("Sent successful healthz response to client %s", r.RemoteAddr)
}

// readyzHandler checks readiness
func (s *Server) readyzHandler(w http.ResponseWriter, r *http.Request) {
	// Log readyz request
	log.Printf("Received %s readyz request from %s (URI: %s)", r.Method, r.RemoteAddr, r.RequestURI)
	
	// Add headers to prevent caching
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	
	// Check if there's at least one stream
	s.mutex.RLock()
	streamsCount := len(s.streams)
	streamsList := make([]string, 0, streamsCount)
	for route := range s.streams {
		streamsList = append(streamsList, route)
	}
	s.mutex.RUnlock()
	
	// Log status 
	log.Printf("Readyz status: %d streams. Routes: %v", streamsCount, streamsList)

	// Always return OK for readyz to avoid container restarts
	w.WriteHeader(http.StatusOK)
	
	// If not a HEAD request, send response body
	if r.Method != "HEAD" {
		w.Write([]byte(fmt.Sprintf("Ready - %d streams registered", streamsCount)))
		
		// Send data immediately
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	
	// Additional logging of successful response
	log.Printf("Sent successful readyz response to client %s", r.RemoteAddr)
}

// streamsHandler returns information about all available streams
func (s *Server) streamsHandler(w http.ResponseWriter, r *http.Request) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	s.trackMutex.RLock()
	defer s.trackMutex.RUnlock()

	type streamInfo struct {
		Route        string `json:"route"`
		Listeners    int    `json:"listeners"`
		CurrentTrack string `json:"current_track"`
	}

	streams := make([]streamInfo, 0, len(s.streams))
	for route, stream := range s.streams {
		info := streamInfo{
			Route:        route,
			Listeners:    stream.GetClientCount(),
			CurrentTrack: s.currentTracks[route],
		}
		streams = append(streams, info)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"streams": streams,
	})
}

// reloadPlaylistHandler reloads playlist
func (s *Server) reloadPlaylistHandler(w http.ResponseWriter, r *http.Request) {
	route := r.URL.Query().Get("route")

	if route != "" {
		// Reload specific playlist
		s.mutex.RLock()
		playlist, exists := s.playlists[route]
		s.mutex.RUnlock()

		if !exists {
			errorMsg := fmt.Sprintf("Stream %s not found", route)
			log.Printf("ERROR: %s", errorMsg)
			sentry.CaptureMessage(errorMsg) // Save as this is an error
			http.Error(w, errorMsg, http.StatusNotFound)
			return
		}

		if err := playlist.Reload(); err != nil {
			errorMsg := fmt.Sprintf("Error reloading playlist: %s", err)
			sentry.CaptureException(fmt.Errorf("error reloading playlist for %s: %w", route, err))
			http.Error(w, errorMsg, http.StatusInternalServerError)
			return
		}

		log.Printf("Playlist for stream %s reloaded", route)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(fmt.Sprintf("Playlist for stream %s reloaded", route)))
	} else {
		// Reload all playlists
		s.mutex.RLock()
		playlists := make([]PlaylistManager, 0, len(s.playlists))
		for _, playlist := range s.playlists {
			playlists = append(playlists, playlist)
		}
		s.mutex.RUnlock()

		for _, playlist := range playlists {
			if err := playlist.Reload(); err != nil {
				errorMsg := fmt.Sprintf("Error reloading playlist: %s", err)
				sentry.CaptureException(fmt.Errorf("error reloading playlist: %w", err))
				http.Error(w, errorMsg, http.StatusInternalServerError)
				return
			}
		}

		log.Printf("All playlists reloaded")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("All playlists reloaded"))
	}
}

// nowPlayingHandler returns information about current track
func (s *Server) nowPlayingHandler(w http.ResponseWriter, r *http.Request) {
	route := r.URL.Query().Get("route")

	s.trackMutex.RLock()
	defer s.trackMutex.RUnlock()

	// Set correct headers for Unicode
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if route != "" {
		// Information about specific stream
		if track, exists := s.currentTracks[route]; exists {
			// Just logging, don't send to Sentry
			log.Printf("Request for track information for %s: %s", route, track)
			
			// Try to get current track with timeout protection in case playlists are busy
			s.mutex.RLock()
			playlist, playlistExists := s.playlists[route]
			s.mutex.RUnlock()
			
			currentTrackInfo := map[string]string{
				"route": route,
				"track": track,
			}
			
			// If playlist exists, try to get additional info about current track
			if playlistExists {
				// Get current track with protection against nil/timeout
				for attempt := 0; attempt < 3; attempt++ {
					if currentTrack := playlist.GetCurrentTrack(); currentTrack != nil {
						// If we have more detailed track info, add it
						if trackWithPath, ok := currentTrack.(interface{ GetPath() string }); ok {
							currentTrackInfo["path"] = trackWithPath.GetPath()
						}
						break
					}
					// Small delay before retry
					time.Sleep(50 * time.Millisecond)
				}
			}
			
			// Send JSON response
			json.NewEncoder(w).Encode(currentTrackInfo)
		} else {
			errorMsg := fmt.Sprintf("Stream %s not found", route)
			log.Printf("ERROR: %s", errorMsg)
			sentry.CaptureMessage(errorMsg) // Save as this is an error
			http.Error(w, errorMsg, http.StatusNotFound)
		}
	} else {
		// Information about all streams
		// Just logging, don't send to Sentry
		log.Printf("Request for information about all current tracks")
		
		// Send JSON response
		json.NewEncoder(w).Encode(s.currentTracks)
	}
}

// isConnectionClosedError checks if error is result of client closing connection
func isConnectionClosedError(err error) bool {
	if err == nil {
		return false
	}
	
	errMsg := err.Error()
	return strings.Contains(errMsg, "broken pipe") || 
		   strings.Contains(errMsg, "connection reset by peer") || 
		   strings.Contains(errMsg, "use of closed network connection")
}

// StreamAudioHandler creates HTTP handler for audio streaming
func (s *Server) StreamAudioHandler(route string) http.HandlerFunc {
	log.Printf("Creating audio stream handler for route %s", route)
	
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
	
	log.Printf("Audio stream format for route %s: %s (MIME: %s)", route, s.streamFormat, contentType)

	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Received %s request for audio stream %s from %s", r.Method, route, r.RemoteAddr)
		
		s.mutex.RLock()
		stream, exists := s.streams[route]
		s.mutex.RUnlock()

		if !exists {
			errorMsg := fmt.Sprintf("Stream %s not found", route)
			log.Printf("ERROR: %s", errorMsg)
			sentry.CaptureMessage(errorMsg) // Save as this is an error
			http.Error(w, errorMsg, http.StatusNotFound)
			return
		}
		
		log.Printf("Stream %s found, setting up headers for streaming", route)

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
		if r.Method == "HEAD" {
			log.Printf("Handled HEAD request for stream %s from %s", route, r.RemoteAddr)
			w.WriteHeader(http.StatusOK)
			return
		}
		
		// IMPORTANT: DO NOT set Content-Length for streams, otherwise browser will wait for exact byte count

		// Check for Flusher support
		flusher, ok := w.(http.Flusher)
		if !ok {
			errorMsg := "Streaming not supported"
			log.Printf("ERROR: %s", errorMsg)
			sentry.CaptureMessage(errorMsg) // Save as this is an error
			http.Error(w, errorMsg, http.StatusInternalServerError)
			return
		}
		
		// Immediately send headers - CRITICAL IMPORTANT!
		// This signals to the browser that streaming is starting
		flusher.Flush()
		
		log.Printf("Adding client to stream %s...", route)

		// Get data channel and client ID
		clientCh, clientID, err := stream.AddClient()
		if err != nil {
			log.Printf("Error adding client to stream %s: %s", route, err)
			sentry.CaptureException(err) // Save as this is an error
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer stream.RemoveClient(clientID)

		// Update metrics ONLY AFTER successful connection and headers sent
		listenerCount.WithLabelValues(route).Inc()
		defer listenerCount.WithLabelValues(route).Dec()

		// Log client connection
		remoteAddr := r.RemoteAddr
		log.Printf("Client connected to stream %s: %s (ID: %d)", route, remoteAddr, clientID)

		// Check for connection closure
		clientClosed := r.Context().Done()
		
		log.Printf("Starting data transmission to client %d for stream %s", clientID, route)

		// Counter for tracking sent data
		var totalBytesSent int64
		var lastLogTime time.Time = time.Now()
		const logEveryBytes = 1024 * 1024 // Log every sent megabyte
		
		// Send data to client
		for {
			select {
			case <-clientClosed:
				// Client disconnected
				log.Printf("Client disconnected from stream %s: %s (ID: %d). Total sent: %d bytes", 
					route, remoteAddr, clientID, totalBytesSent)
				return
			case data, ok := <-clientCh:
				if !ok {
					// Channel closed
					log.Printf("Channel closed for client %s (ID: %d). Total sent: %d bytes", 
						remoteAddr, clientID, totalBytesSent)
					return
				}
				
				// Send data to client
				n, err := w.Write(data)
				if err != nil {
					if isConnectionClosedError(err) {
						// Just log, don't send to Sentry
						log.Printf("Client %d disconnected: %s. Total sent: %d bytes", 
							clientID, err, totalBytesSent)
					} else {
						// Send to Sentry only for unusual errors
						log.Printf("Error sending data to client %d: %s. Total sent: %d bytes", 
							clientID, err, totalBytesSent)
						sentry.CaptureException(fmt.Errorf("error sending data to client %d: %w", clientID, err))
					}
					return
				}
				
				// Update counter and metrics
				totalBytesSent += int64(n)
				bytesSent.WithLabelValues(route).Add(float64(n))
				
				// Periodically log sent data amount
				if totalBytesSent >= logEveryBytes && time.Since(lastLogTime) > 5*time.Second {
					log.Printf("Sent %d Mbytes of data to client %d (IP: %s)", 
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

// ReloadPlaylist reloads playlist for specified route
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
	tmpl, err := template.ParseFiles(
		"templates/login.html",
		"templates/partials/head.html",
	)
	if err != nil {
		log.Printf("ERROR: Unable to load login.html template: %v", err)
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	data := map[string]interface{}{
		"Title": "Login - Audio Stream Status",
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("ERROR: Unable to execute template: %v", err)
	}
}

// statusLoginSubmitHandler handles login form submission
func (s *Server) statusLoginSubmitHandler(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()
	if err != nil {
		log.Printf("ERROR: Unable to parse login form: %v", err)
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
			Expires:  time.Now().Add(24 * time.Hour),
			HttpOnly: true,
		})

		// Redirect to status page
		http.Redirect(w, r, "/status-page", http.StatusFound)
		return
	}

	// If password is incorrect, show error message
	tmpl, err := template.ParseFiles(
		"templates/login.html",
		"templates/partials/head.html",
	)
	if err != nil {
		log.Printf("ERROR: Unable to load login.html template: %v", err)
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	data := map[string]interface{}{
		"Title":        "Login - Audio Stream Status",
		"ErrorMessage": "Invalid password",
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("ERROR: Unable to execute template: %v", err)
	}
}

// checkAuth checks authentication for accessing status page
func (s *Server) checkAuth(r *http.Request) bool {
	cookie, err := r.Cookie("status_auth")
	return err == nil && cookie.Value == s.statusPassword
}

// statusPageHandler displays status page for all streams
func (s *Server) statusPageHandler(w http.ResponseWriter, r *http.Request) {
	// Check authentication
	if !s.checkAuth(r) {
		http.Redirect(w, r, "/status", http.StatusFound)
		return
	}

	s.mutex.RLock()
	// Copy streams and playlists
	streams := make(map[string]StreamHandler)
	for k, v := range s.streams {
		streams[k] = v
	}
	
	playlists := make(map[string]PlaylistManager)
	for k, v := range s.playlists {
		playlists[k] = v
	}
	s.mutex.RUnlock()
	
	s.trackMutex.RLock()
	currentTracks := make(map[string]string)
	for k, v := range s.currentTracks {
		currentTracks[k] = v
	}
	s.trackMutex.RUnlock()
	
	// Sort route keys for stable display order
	var routes []string
	for route := range streams {
		routes = append(routes, route)
	}
	sort.Strings(routes)

	// Prepare data for template
	type StreamInfo struct {
		Route       string
		RouteID     string
		DisplayName string
		StartTime   string
		CurrentTrack string
		Listeners   int
		HistoryHTML template.HTML
	}

	streamInfos := make([]StreamInfo, 0, len(routes))

	for _, route := range routes {
		stream := streams[route]
		playlist, exists := playlists[route]
		if !exists {
			continue
		}
		
		currentTrack := "Unknown"
		if track, exists := currentTracks[route]; exists {
			currentTrack = track
		}
		
		// Get track history with timeout retry protection
		var history []interface{}
		// Try to get history with simple timeout protection
		historyObtained := false
		for attempt := 0; attempt < 3; attempt++ {
			// Try to get history, might return nil or empty on timeout
			history = playlist.GetHistory()
			if history != nil {
				historyObtained = true
				break
			}
			log.Printf("WARNING: GetHistory timeout for route %s, attempt %d/3", route, attempt+1)
			time.Sleep(50 * time.Millisecond) // Small delay before retry
		}
		
		historyHtml := "<ul>"
		// Track history in reverse order (newest on top)
		if historyObtained && len(history) > 0 {
			for i := len(history) - 1; i >= 0; i-- {
				// Use type assertion to get track name
				if track, ok := history[i].(interface{ GetPath() string }); ok {
					trackPath := track.GetPath()
					trackName := filepath.Base(trackPath)
					historyHtml += "<li>" + trackName + "</li>"
				}
			}
		} else {
			historyHtml += "<li>History unavailable - playlist busy</li>"
		}
		historyHtml += "</ul>"
		
		// Format start time with protection against nil/timeout
		startTime := "Unknown"
		// Try to get start time with timeout protection
		for attempt := 0; attempt < 3; attempt++ {
			// Get playlist start time or fallback to current time
			t := playlist.GetStartTime()
			if !t.IsZero() {
				startTime = t.Format("02.01.2006 15:04:05 MST")
				break
			}
			log.Printf("WARNING: GetStartTime returned zero time for route %s, attempt %d/3", route, attempt+1)
			time.Sleep(50 * time.Millisecond) // Small delay before retry
		}
		
		// Get route ID for JS functions, removing leading slash
		routeID := route[1:]
		
		// Remove leading slash for display name
		displayName := route[1:]
		
		streamInfos = append(streamInfos, StreamInfo{
			Route:       route,
			RouteID:     routeID,
			DisplayName: displayName,
			StartTime:   startTime,
			CurrentTrack: currentTrack,
			Listeners:   stream.GetClientCount(),
			HistoryHTML: template.HTML(historyHtml),
		})
	}

	// Load and render template
	tmpl, err := template.ParseFiles(
		"templates/status.html",
		"templates/partials/head.html",
	)
	if err != nil {
		log.Printf("ERROR: Unable to load status.html template: %v", err)
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	data := map[string]interface{}{
		"Title":   "Audio Stream Status",
		"Streams": streamInfos,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("ERROR: Unable to execute template: %v", err)
	}
}

// nextTrackHandler handles request for track switching forward
func (s *Server) nextTrackHandler(w http.ResponseWriter, r *http.Request) {
	// Check authentication
	if !s.checkAuth(r) {
		// Determine if this is AJAX request
		isAjax := r.Header.Get("X-Requested-With") == "XMLHttpRequest" || r.URL.Query().Get("ajax") == "1"
		
		if isAjax {
			// Return JSON error for AJAX requests
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"route":   "",
				"error":   "Authentication required",
			})
			return
		}
		
		// Redirect to login page for regular requests
		http.Redirect(w, r, "/status", http.StatusFound)
		return
	}
	
	// Get route from URL
	vars := mux.Vars(r)
	route := "/" + vars["route"]
	
	// Lock for access to playlist
	s.mutex.RLock()
	playlist, exists := s.playlists[route]
	s.mutex.RUnlock()
	
	if !exists {
		// Check if AJAX request
		isAjax := r.Header.Get("X-Requested-With") == "XMLHttpRequest" || r.URL.Query().Get("ajax") == "1"
		
		if isAjax {
			// Return JSON error for AJAX requests
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"route":   route,
				"error":   "Route not found",
			})
			return
		}
		
		http.Error(w, "Route not found", http.StatusNotFound)
		return
	}
	
	// Switch track (check return value)
	nextTrack := playlist.NextTrack()
	
	// Handle possible nil return (timeout or error)
	if nextTrack == nil {
		log.Printf("WARNING: NextTrack returned nil for route %s, likely timeout", route)
		
		// Determine if this is AJAX request
		isAjax := r.Header.Get("X-Requested-With") == "XMLHttpRequest" || r.URL.Query().Get("ajax") == "1"
		
		if isAjax {
			// Return error for AJAX requests
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			response := map[string]interface{}{
				"success": false,
				"route":   route,
				"error":   "Failed to switch track, operation timed out",
			}
			json.NewEncoder(w).Encode(response)
		} else {
			// Show error for regular requests
			http.Error(w, "Failed to switch track, operation timed out", http.StatusInternalServerError)
		}
		return
	}
	
	log.Printf("DIAGNOSTICS: Manual track switch to next track for route %s", route)
	
	// Call restart playback, if station manager is available
	success := false
	newTrackName := "Unknown"
	
	if s.stationManager != nil {
		success = s.stationManager.RestartPlayback(route)
		if success {
			log.Printf("DIAGNOSTICS: Restart playback for route %s completed successfully", route)
			
			// Get new track name
			if track, ok := nextTrack.(interface{ GetPath() string }); ok {
				newTrackName = filepath.Base(track.GetPath())
				
				// Immediately update current track information
				s.trackMutex.Lock()
				s.currentTracks[route] = newTrackName
				s.trackMutex.Unlock()
				
				log.Printf("DIAGNOSTICS: Immediately updated current track information for %s: %s", route, newTrackName)
			}
		} else {
			log.Printf("ERROR: Unable to restart playback for route %s", route)
		}
	} else {
		log.Printf("WARNING: Station manager not set, restart playback not possible")
	}
	
	// Determine if this is AJAX request
	isAjax := r.Header.Get("X-Requested-With") == "XMLHttpRequest" || r.URL.Query().Get("ajax") == "1"
	
	if isAjax {
		// Send JSON response for AJAX requests
		w.Header().Set("Content-Type", "application/json")
		response := map[string]interface{}{
			"success": success,
			"route":   route,
			"track":   newTrackName,
		}
		json.NewEncoder(w).Encode(response)
	} else {
		// Redirect back to status page for regular requests
		http.Redirect(w, r, "/status-page", http.StatusFound)
	}
}

// prevTrackHandler handles request for track switching backward
func (s *Server) prevTrackHandler(w http.ResponseWriter, r *http.Request) {
	// Check authentication
	if !s.checkAuth(r) {
		// Determine if this is AJAX request
		isAjax := r.Header.Get("X-Requested-With") == "XMLHttpRequest" || r.URL.Query().Get("ajax") == "1"
		
		if isAjax {
			// Return JSON error for AJAX requests
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"route":   "",
				"error":   "Authentication required",
			})
			return
		}
		
		// Redirect to login page for regular requests
		http.Redirect(w, r, "/status", http.StatusFound)
		return
	}
	
	// Get route from URL
	vars := mux.Vars(r)
	route := "/" + vars["route"]
	
	// Lock for access to playlist
	s.mutex.RLock()
	playlist, exists := s.playlists[route]
	s.mutex.RUnlock()
	
	if !exists {
		// Check if AJAX request
		isAjax := r.Header.Get("X-Requested-With") == "XMLHttpRequest" || r.URL.Query().Get("ajax") == "1"
		
		if isAjax {
			// Return JSON error for AJAX requests
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"route":   route,
				"error":   "Route not found",
			})
			return
		}
		
		http.Error(w, "Route not found", http.StatusNotFound)
		return
	}
	
	// Switch track (check return value)
	prevTrack := playlist.PreviousTrack()
	
	// Handle possible nil return (timeout or error)
	if prevTrack == nil {
		log.Printf("WARNING: PreviousTrack returned nil for route %s, likely timeout", route)
		
		// Determine if this is AJAX request
		isAjax := r.Header.Get("X-Requested-With") == "XMLHttpRequest" || r.URL.Query().Get("ajax") == "1"
		
		if isAjax {
			// Return error for AJAX requests
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			response := map[string]interface{}{
				"success": false,
				"route":   route,
				"error":   "Failed to switch track, operation timed out",
			}
			json.NewEncoder(w).Encode(response)
		} else {
			// Show error for regular requests
			http.Error(w, "Failed to switch track, operation timed out", http.StatusInternalServerError)
		}
		return
	}
	
	log.Printf("DIAGNOSTICS: Manual track switch to previous track for route %s", route)
	
	// Call restart playback, if station manager is available
	success := false
	newTrackName := "Unknown"
	
	if s.stationManager != nil {
		success = s.stationManager.RestartPlayback(route)
		if success {
			log.Printf("DIAGNOSTICS: Restart playback for route %s completed successfully", route)
			
			// Get new track name
			if track, ok := prevTrack.(interface{ GetPath() string }); ok {
				newTrackName = filepath.Base(track.GetPath())
				
				// Immediately update current track information
				s.trackMutex.Lock()
				s.currentTracks[route] = newTrackName
				s.trackMutex.Unlock()
				
				log.Printf("DIAGNOSTICS: Immediately updated current track information for %s: %s", route, newTrackName)
			}
		} else {
			log.Printf("ERROR: Unable to restart playback for route %s", route)
		}
	} else {
		log.Printf("WARNING: Station manager not set, restart playback not possible")
	}
	
	// Determine if this is AJAX request
	isAjax := r.Header.Get("X-Requested-With") == "XMLHttpRequest" || r.URL.Query().Get("ajax") == "1"
	
	if isAjax {
		// Send JSON response for AJAX requests
		w.Header().Set("Content-Type", "application/json")
		response := map[string]interface{}{
			"success": success,
			"route":   route,
			"track":   newTrackName,
		}
		json.NewEncoder(w).Encode(response)
	} else {
		// Redirect back to status page for regular requests
		http.Redirect(w, r, "/status-page", http.StatusFound)
	}
}

// notFoundHandler handles requests to non-existent routes
func (s *Server) notFoundHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("404 - route not found: %s", r.URL.Path)
	
	// Load and render 404 template page
	tmpl, err := template.ParseFiles(
		"templates/404.html",
		"templates/partials/head.html",
	)
	if err != nil {
		log.Printf("ERROR: Unable to load 404.html template: %v", err)
		http.Error(w, "Page not found", http.StatusNotFound)
		return
	}

	data := map[string]interface{}{
		"Title": "404 - Page not found",
		"Path":  r.URL.Path,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("ERROR: Unable to execute template: %v", err)
	}
}

// SetStationManager sets station manager for restarting playback
func (s *Server) SetStationManager(manager interface { RestartPlayback(string) bool }) {
	s.stationManager = manager
}

// redirectToLogin redirects user to login page
func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/status", http.StatusFound)
}

// handleShufflePlaylist handles the request to manually shuffle a playlist
func (s *Server) handleShufflePlaylist(w http.ResponseWriter, r *http.Request) {
	// Check authentication (same as for status page)
	if !s.checkAuth(r) {
		// Проверяем, является ли запрос AJAX
		ajax := r.URL.Query().Get("ajax")
		isAjax := r.Header.Get("X-Requested-With") == "XMLHttpRequest" || ajax == "1"
		
		if isAjax {
			// Для AJAX запросов отправляем JSON с ошибкой
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "Authentication required",
				"route":   "",
			})
			return
		}
		
		// Для обычных запросов делаем редирект
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
	log.Printf("Manual shuffle requested for route %s", route)
	
	// Execute shuffle in goroutine with timeout protection
	shuffleDone := make(chan bool, 1)
	
	go func() {
		defer func() {
			// Catch any panics from Shuffle operation
			if r := recover(); r != nil {
				log.Printf("ERROR: Shuffle operation panicked: %v", r)
				shuffleDone <- false
			}
		}()
		
		// Call shuffle
		playlist.Shuffle()
		shuffleDone <- true
	}()
	
	// Wait for shuffle to complete with timeout
	var success bool
	select {
	case success = <-shuffleDone:
		if success {
			log.Printf("Manual shuffle completed successfully for route %s", route)
		} else {
			log.Printf("Manual shuffle failed for route %s", route)
		}
	case <-time.After(1 * time.Second):
		log.Printf("WARNING: Shuffle operation timed out for route %s", route)
		success = false
	}
	
	// Check if need to return JSON or perform redirect
	ajax := r.URL.Query().Get("ajax")
	if ajax == "1" {
		w.Header().Set("Content-Type", "application/json")
		if success {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"message": "Playlist shuffled successfully",
				"route": route,
			})
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "Playlist shuffle failed or timed out",
				"route": route,
			})
		}
	} else {
		// Redirect back to status page
		http.Redirect(w, r, "/status-page", http.StatusSeeOther)
	}
} 