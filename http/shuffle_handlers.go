package http

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gorilla/mux"
)

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
