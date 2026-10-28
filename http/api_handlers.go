package http

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	html "html"
)

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
		Route        string `json:"route"`
		Listeners    int    `json:"listeners"`
		CurrentTrack string `json:"current_track"`
		HistoryHTML  string `json:"history_html"`
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
				case *struct {
					Path string
					Name string
				}:
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
					`<li><span>%s</span><button class="copy-button" data-track="%s"><span class="copy-icon">📋</span><span class="copy-feedback"></span></button></li>`,
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

// IsStreamAvailable checks if a stream is available by making a HEAD request to the route
func (s *Server) IsStreamAvailable(route string) bool {
	s.mutex.RLock()
	stream, exists := s.streams[route]
	s.mutex.RUnlock()

	// If stream doesn't exist, it's not available
	if !exists {
		return false
	}

	// Check if stream is actually playing audio
	s.trackMutex.RLock()
	currentTrack := s.currentTracks[route]
	s.trackMutex.RUnlock()

	// A station can have a current track and still send nothing (stuck streamer).
	// If the stream reports its last activity, treat a long silence as unavailable.
	if activity, ok := stream.(interface{ GetLastActivity() time.Time }); ok {
		lastActivity := activity.GetLastActivity()
		if lastActivity.IsZero() || time.Since(lastActivity) > streamSilenceThreshold {
			s.logger.Warn("Stream sends no audio data",
				slog.String("route", route),
				slog.String("currentTrack", currentTrack),
				slog.Time("lastActivity", lastActivity))
			return false
		}
	}

	// Stream is available only if it exists, has a current track and is properly initialized
	return currentTrack != "" && stream.GetClientCount() >= 0
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
