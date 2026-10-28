package http

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gorilla/mux"
)

// handleTrackSwitchHandler handles track switching.
func (s *Server) handleTrackSwitchHandler(w http.ResponseWriter, r *http.Request, direction string) {
	vars := mux.Vars(r)
	route := "/" + vars["route"]

	// Check authentication.
	if !s.checkAuth(r) {
		s.logger.Info("Track switch rejected: not authenticated",
			slog.String("route", route),
			slog.String("remoteAddr", r.RemoteAddr))
		s.handleUnauthenticatedTrackSwitch(w, r)
		return
	}

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
	if direction == "next" {
		return playlist.NextTrack()
	}
	return playlist.PreviousTrack()
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
