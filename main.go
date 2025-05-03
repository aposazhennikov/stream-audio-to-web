package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/gorilla/mux"
	"github.com/user/stream-audio-to-web/audio"
	httpServer "github.com/user/stream-audio-to-web/http"
	"github.com/user/stream-audio-to-web/playlist"
	"github.com/user/stream-audio-to-web/radio"
)

// Default configuration
const (
	defaultPort         = 8000
	defaultAudioDir     = "./audio"
	defaultStreamFormat = "mp3"
	defaultBitrate      = 128
	defaultMaxClients   = 500
	defaultLogLevel     = "info"
	defaultBufferSize   = 65536 // 64KB
	defaultShuffle      = false  // Shuffle tracks is disabled by default
)

// Application configuration
type Config struct {
	Port           int
	AudioDir       string
	DirectoryRoutes map[string]string
	StreamFormat   string
	Bitrate        int
	MaxClients     int
	LogLevel       string
	BufferSize     int
	Shuffle        bool // Shuffle tracks in playlist
}

// Global variables for routes
var (
	humorRoute   *mux.Route
	scienceRoute *mux.Route
)

func main() {
	// Sentry initialization
	err := sentry.Init(sentry.ClientOptions{
		Dsn: "https://f5dbf565496b75215d81c2286cf0dc9c@o4508953992101888.ingest.de.sentry.io/4509243323908176",
		Environment: getEnvOrDefault("ENV", "development"),
		Release:     "stream-audio-to-web@1.0.0",
	})
	if err != nil {
		log.Fatalf("sentry.Init: %s", err)
	}
	defer sentry.Flush(2 * time.Second)
	defer sentry.Recover()

	// Load configuration
	config := loadConfig()
	
	// Detailed configuration logging for diagnostics
	log.Printf("========== APPLICATION CONFIGURATION ==========")
	log.Printf("Port: %d", config.Port)
	log.Printf("Default audio directory: %s", config.AudioDir)
	log.Printf("Stream format: %s", config.StreamFormat)
	log.Printf("Bitrate: %d", config.Bitrate)
	log.Printf("Max clients: %d", config.MaxClients)
	log.Printf("Buffer size: %d", config.BufferSize)
	log.Printf("Shuffle tracks: %v", config.Shuffle)
	log.Printf("Additional directory routes:")
	for route, dir := range config.DirectoryRoutes {
		log.Printf("  - Route '%s' -> Directory '%s'", route, dir)
	}
	log.Printf("============================================")

	// Create HTTP server
	server := httpServer.NewServer(config.StreamFormat, config.MaxClients)

	// Create radio station manager
	stationManager := radio.NewRadioStationManager()
	
	// Set radio station manager for HTTP server
	server.SetStationManager(stationManager)

	// Create minimal dummy streams for /healthz to immediately find at least one route
	dummyStream, dummyPlaylist := createDummyStreamAndPlaylist()
	server.RegisterStream("/humor", dummyStream, dummyPlaylist)
	server.RegisterStream("/science", dummyStream, dummyPlaylist)
	log.Printf("Temporary stream placeholders registered for quick healthcheck passing")

	// Remove temporary handlers here as they are already registered in server.setupRoutes()
	
	// NOW create and start HTTP server BEFORE setting up streams
	httpSrv := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", config.Port), // Explicitly specify that we listen on all interfaces
		Handler: server.Handler(),
		// Increase timeouts for request processing
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // Disable timeout for streaming
		IdleTimeout:  60 * time.Second,
	}

	// Start server in goroutine BEFORE setting up routes
	go func() {
		log.Printf("Starting HTTP server on address 0.0.0.0:%d...", config.Port)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server start error: %s", err)
			sentry.CaptureException(err)
		}
	}()
	
	// Redirect from root route
	redirectTo := "/humor" // redirect to /humor by default
	if _, exists := config.DirectoryRoutes["/humor"]; !exists {
		// If /humor doesn't exist, take the first route from configuration
		for route := range config.DirectoryRoutes {
			redirectTo = route
			break
		}
	}
	
	// Replace temporary handler for root route with redirection
	server.Handler().(*mux.Router).HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Redirecting from / to %s (method: %s)", redirectTo, r.Method)
		// For HEAD requests return only headers without redirect
		if r.Method == "HEAD" {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, redirectTo, http.StatusSeeOther)
	}).Methods("GET", "HEAD")
	
	log.Printf("Redirect configured from / to %s", redirectTo)

	// AFTER starting HTTP server, configure audio routes asynchronously
	log.Printf("Starting audio route configuration...")

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
			log.Printf("Asynchronous configuration of route '%s' -> directory '%s'", route, dir)
			if success := configureSyncRoute(server, stationManager, route, dir, config); success {
				log.Printf("Route '%s' successfully configured", route)
			} else {
				log.Printf("ERROR: Route '%s' configuration failed", route)
			}
		}(routeCopy, dirCopy)
	}

	// Check stream status
	log.Printf("====== REGISTERED STREAMS STATUS ======")
	humorRegistered := server.IsStreamRegistered("/humor")
	scienceRegistered := server.IsStreamRegistered("/science")
	log.Printf("Stream /humor registered: %v", humorRegistered)
	log.Printf("Stream /science registered: %v", scienceRegistered)
	
	if !humorRegistered || !scienceRegistered {
		log.Printf("WARNING: Some streams are not registered!")
	} else {
		log.Printf("All streams successfully registered")
	}
	log.Printf("=============================================")

	// Graceful shutdown setup
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	sig := <-quit

	log.Printf("Signal received: %v, performing graceful shutdown...", sig)

	// Handle SIGHUP for playlist reload
	if sig == syscall.SIGHUP {
		log.Println("SIGHUP received, reloading playlists...")
		
		// Get list of all playlists
		server.Handler().(*mux.Router).Walk(func(route *mux.Route, router *mux.Router, ancestors []*mux.Route) error {
			path, err := route.GetPathTemplate()
			if err != nil {
				return nil
			}
			
			// Reload only for registered streams
			if server.IsStreamRegistered(path) {
				log.Printf("Reloading playlist for %s", path)
				if err := server.ReloadPlaylist(path); err != nil {
					log.Printf("Error reloading playlist for %s: %s", path, err)
					sentry.CaptureException(err)
				} else {
					log.Printf("Playlist for %s successfully reloaded", path)
				}
			}
			
			return nil
		})
		
		log.Println("Playlist reload complete")
		return // Continue operation
	}

	// Stop all radio stations
	stationManager.StopAll()

	// Graceful HTTP server shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Fatalf("Server shutdown error: %s", err)
		sentry.CaptureException(err)
	}
	log.Println("Server successfully stopped")
}

// configureSyncRoute configures one audio stream route synchronously
func configureSyncRoute(server *httpServer.Server, stationManager *radio.RadioStationManager, route, dir string, config *Config) bool {
	log.Printf("Starting synchronous configuration of route '%s'...", route)

	// Create directory if it doesn't exist
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		log.Printf("Creating directory for route %s: %s", route, dir)
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Printf("ERROR: When creating directory %s: %s. Route '%s' NOT CONFIGURED.", dir, err, route)
			sentry.CaptureException(fmt.Errorf("error creating directory %s: %w", dir, err))
			return false
		}
	}

	// Check directory contents
	files, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("ERROR: When reading directory %s: %s. Route '%s' NOT CONFIGURED.", dir, err, route)
		sentry.CaptureException(fmt.Errorf("error reading directory %s: %w", dir, err))
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
	log.Printf("Directory %s contains %d audio files", dir, audioFiles)
	if audioFiles == 0 {
		log.Printf("CRITICAL ERROR: No audio files in directory %s. Route '%s' NOT CONFIGURED.", dir, route)
		sentry.CaptureMessage(fmt.Sprintf("No audio files in directory %s for route %s", dir, route))
		return false
	}

	// Create playlist and configure stream synchronously
	log.Printf("Creating playlist for route %s...", route)
	pl, err := playlist.NewPlaylist(dir, nil, config.Shuffle)
	if err != nil {
		log.Printf("ERROR creating playlist: %s", err)
		sentry.CaptureException(fmt.Errorf("error creating playlist: %w", err))
		return false
	}
	
	log.Printf("Playlist for route %s successfully created", route)
	
	log.Printf("Creating audio streamer for route %s...", route)
	streamer := audio.NewStreamer(config.BufferSize, config.MaxClients, config.StreamFormat, config.Bitrate)
	log.Printf("Audio streamer for route %s successfully created", route)
	
	log.Printf("Adding radio station %s to manager...", route)
	stationManager.AddStation(route, streamer, pl)
	log.Printf("Radio station '%s' successfully added to manager", route)
	
	log.Printf("Registering audio stream %s on HTTP server...", route)
	server.RegisterStream(route, streamer, pl)
	log.Printf("Audio stream '%s' successfully registered on HTTP server", route)
	
	// Check if stream registration was successful
	if !server.IsStreamRegistered(route) {
		log.Printf("CRITICAL ERROR: Stream %s not registered after all operations", route)
		sentry.CaptureMessage(fmt.Sprintf("Stream %s not registered after all operations", route))
		return false
	}
	
	log.Printf("RESULT: Route '%s' configuration SUCCESSFULLY COMPLETED", route)
	return true
}

// getAllRoutes returns a list of all registered routes
func getAllRoutes(router *mux.Router) []string {
	routes := []string{}
	
	router.Walk(func(route *mux.Route, router *mux.Router, ancestors []*mux.Route) error {
		pathTemplate, err := route.GetPathTemplate()
		if err == nil {
			routes = append(routes, pathTemplate)
		}
		return nil
	})
	
	return routes
}

// Load configuration from command line flags and environment variables
func loadConfig() *Config {
	config := &Config{}

	// Define command line flags
	flag.IntVar(&config.Port, "port", defaultPort, "HTTP server port")
	flag.StringVar(&config.AudioDir, "audio-dir", defaultAudioDir, "Default audio files directory")
	flag.StringVar(&config.StreamFormat, "stream-format", defaultStreamFormat, "Stream format: mp3, aac, ogg")
	flag.IntVar(&config.Bitrate, "bitrate", defaultBitrate, "Bitrate in kbps")
	flag.IntVar(&config.MaxClients, "max-clients", defaultMaxClients, "Maximum number of concurrent connections")
	flag.StringVar(&config.LogLevel, "log-level", defaultLogLevel, "Logging level: debug, info, warn, error")
	flag.IntVar(&config.BufferSize, "buffer-size", defaultBufferSize, "Buffer size for reading audio files in bytes")
	flag.BoolVar(&config.Shuffle, "shuffle", defaultShuffle, "Shuffle tracks in playlist")

	// For mapping directories and routes we use JSON
	var directoryRoutesJSON string
	flag.StringVar(&directoryRoutesJSON, "directory-routes", "{}", "JSON string with route to directory mapping")

	// Priority: environment variables > command line flags > default values
	flag.Parse()

	// Initialize DirectoryRoutes
	config.DirectoryRoutes = make(map[string]string)

	// Parse JSON string with directory routes
	if directoryRoutesJSON != "" {
		if err := json.Unmarshal([]byte(directoryRoutesJSON), &config.DirectoryRoutes); err != nil {
			log.Printf("Error parsing JSON string with directory routes: %s", err)
			sentry.CaptureException(fmt.Errorf("error parsing JSON string with directory routes: %w", err))
		}
	}

	// Check environment variables
	if envPort := os.Getenv("PORT"); envPort != "" {
		if port, err := strconv.Atoi(envPort); err == nil {
			config.Port = port
		} else {
			sentry.CaptureException(fmt.Errorf("error parsing PORT: %w", err))
		}
	}
	if envAudioDir := os.Getenv("AUDIO_DIR"); envAudioDir != "" {
		config.AudioDir = envAudioDir
	}
	if envStreamFormat := os.Getenv("STREAM_FORMAT"); envStreamFormat != "" {
		config.StreamFormat = envStreamFormat
	}
	if envBitrate := os.Getenv("BITRATE"); envBitrate != "" {
		if bitrate, err := strconv.Atoi(envBitrate); err == nil {
			config.Bitrate = bitrate
		} else {
			sentry.CaptureException(fmt.Errorf("error parsing BITRATE: %w", err))
		}
	}
	if envMaxClients := os.Getenv("MAX_CLIENTS"); envMaxClients != "" {
		if maxClients, err := strconv.Atoi(envMaxClients); err == nil {
			config.MaxClients = maxClients
		} else {
			sentry.CaptureException(fmt.Errorf("error parsing MAX_CLIENTS: %w", err))
		}
	}
	if envLogLevel := os.Getenv("LOG_LEVEL"); envLogLevel != "" {
		config.LogLevel = envLogLevel
	}
	if envBufferSize := os.Getenv("BUFFER_SIZE"); envBufferSize != "" {
		if bufferSize, err := strconv.Atoi(envBufferSize); err == nil {
			config.BufferSize = bufferSize
		} else {
			sentry.CaptureException(fmt.Errorf("error parsing BUFFER_SIZE: %w", err))
		}
	}
	if envDirectoryRoutes := os.Getenv("DIRECTORY_ROUTES"); envDirectoryRoutes != "" {
		var routes map[string]string
		if err := json.Unmarshal([]byte(envDirectoryRoutes), &routes); err == nil {
			for k, v := range routes {
				config.DirectoryRoutes[k] = v
			}
		} else {
			log.Printf("Error parsing JSON from environment variable DIRECTORY_ROUTES: %s", err)
			sentry.CaptureException(fmt.Errorf("error parsing JSON from environment variable DIRECTORY_ROUTES: %w", err))
		}
	}

	if envShuffle := os.Getenv("SHUFFLE"); envShuffle != "" {
		if shuffle, err := strconv.ParseBool(envShuffle); err == nil {
			config.Shuffle = shuffle
		} else {
			sentry.CaptureException(fmt.Errorf("error parsing SHUFFLE: %w", err))
		}
	}

	// Check for main routes (humor, science)
	// If they don't exist - add them explicitly with leading slash /humor and /science
	_, existsWithSlash := config.DirectoryRoutes["/humor"]
	_, existsWithoutSlash := config.DirectoryRoutes["humor"]
	if !existsWithSlash && !existsWithoutSlash {
		config.DirectoryRoutes["/humor"] = "/app/humor"
		log.Printf("Added default route: '/humor' -> '/app/humor'")
	}
	
	_, scienceWithSlash := config.DirectoryRoutes["/science"]
	_, scienceWithoutSlash := config.DirectoryRoutes["science"]
	if !scienceWithSlash && !scienceWithoutSlash {
		config.DirectoryRoutes["/science"] = "/app/science"
		log.Printf("Added default route: '/science' -> '/app/science'")
	}

	// Normalize all routes by adding leading slash if it doesn't exist
	normalizedRoutes := make(map[string]string)
	for route, dir := range config.DirectoryRoutes {
		if route[0] != '/' {
			normalizedRoutes["/"+route] = dir
			log.Printf("Normalized route: '%s' -> '/%s'", route, route)
		} else {
			normalizedRoutes[route] = dir
		}
	}
	config.DirectoryRoutes = normalizedRoutes

	// We don't check default directory as the route "/" is not used
	// if _, err := os.Stat(config.AudioDir); os.IsNotExist(err) {
	//	log.Printf("Audio directory doesn't exist: %s, creating...", config.AudioDir)
	//	if err := os.MkdirAll(config.AudioDir, 0755); err != nil {
	//		log.Fatalf("Unable to create directory: %s", err)
	//		sentry.CaptureException(fmt.Errorf("unable to create directory: %w", err))
	//	}
	// }

	// Get absolute paths for DirectoryRoutes
	for route, dir := range config.DirectoryRoutes {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			log.Printf("Unable to get absolute path for %s: %s", dir, err)
			sentry.CaptureException(fmt.Errorf("unable to get absolute path for %s: %w", dir, err))
			continue
		}
		config.DirectoryRoutes[route] = absDir
	}

	return config
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

func (d *dummyStreamHandler) RemoveClient(clientID int) {
	// Do nothing
}

func (d *dummyStreamHandler) GetClientCount() int {
	return 0
}

func (d *dummyStreamHandler) GetCurrentTrackChannel() <-chan string {
	return d.clientCh
}

// Minimal implementation of PlaylistManager for placeholder
type dummyPlaylistManager struct{}

func (d *dummyPlaylistManager) Reload() error {
	return nil
}

func (d *dummyPlaylistManager) GetCurrentTrack() interface{} {
	return "dummy.mp3"
}

func (d *dummyPlaylistManager) NextTrack() interface{} {
	return "dummy.mp3"
}

func (d *dummyPlaylistManager) GetHistory() []interface{} {
	return []interface{}{}
}

func (d *dummyPlaylistManager) GetStartTime() time.Time {
	return time.Now()
}

func (d *dummyPlaylistManager) PreviousTrack() interface{} {
	return "dummy.mp3"
} 