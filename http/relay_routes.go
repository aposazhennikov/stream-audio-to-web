// Package http provides HTTP server implementation.
// This file contains relay-related route handlers for the HTTP server.
package http

import (
	"html/template"
	"net/http"
	"strconv"

	"log/slog"

	"github.com/gorilla/mux"
)

// setupRelayRoutes configures all relay-related routes in the router.
func (s *Server) setupRelayRoutes() {
	// Only setup relay routes if a relay manager is available
	if s.relayManager == nil {
		s.logger.Info("Relay manager not available, skipping relay routes setup")
		return
	}

	s.logger.Info("Setting up relay routes")

	// Management page
	s.router.HandleFunc("/relay-management", s.relayManagementHandler).Methods("GET")

	// Add relay URL
	s.router.HandleFunc("/relay/add", s.addRelayHandler).Methods("POST")

	// Remove relay URL
	s.router.HandleFunc("/relay/remove", s.removeRelayHandler).Methods("POST")

	// Toggle relay functionality
	s.router.HandleFunc("/relay/toggle", s.toggleRelayHandler).Methods("POST")

	// Relay stream
	s.router.HandleFunc("/relay/stream/{index}", s.relayStreamHandler).Methods("GET")

	s.logger.Info("Relay routes setup complete")
}

// relayManagementHandler handles the relay management page requests.
func (s *Server) relayManagementHandler(w http.ResponseWriter, r *http.Request) {
	// Check authentication
	if !s.checkAuth(r) {
		http.Redirect(w, r, "/status", http.StatusFound)
		return
	}

	s.logger.Info("Rendering relay management page")

	// Get success or error message from query parameters
	successMsg := r.URL.Query().Get("success")
	errorMsg := r.URL.Query().Get("error")

	// Parse the relay template
	tmpl, parseErr := template.ParseFiles(
		"templates/layout.html",
		"templates/relay.html",
	)
	if parseErr != nil {
		s.logger.Error("Failed to parse relay template", slog.String("error", parseErr.Error()))
		http.Error(w, "Server error: "+parseErr.Error(), http.StatusInternalServerError)
		return
	}

	// Prepare data for the template
	data := map[string]interface{}{
		"Title":          "Relay Management",
		"RelayLinks":     s.relayManager.GetLinks(),
		"RelayActive":    s.relayManager.IsActive(),
		"SuccessMessage": successMsg,
		"ErrorMessage":   errorMsg,
	}

	// Execute the template
	if executeErr := tmpl.Execute(w, data); executeErr != nil {
		s.logger.Error("Failed to execute relay template", slog.String("error", executeErr.Error()))
		http.Error(w, "Server error: "+executeErr.Error(), http.StatusInternalServerError)
		return
	}

	s.logger.Info("Relay management page rendered successfully")
}

// addRelayHandler handles requests to add a new relay URL.
func (s *Server) addRelayHandler(w http.ResponseWriter, r *http.Request) {
	// Check authentication
	if !s.checkAuth(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Parse form
	if parseErr := r.ParseForm(); parseErr != nil {
		s.logger.Error("Failed to parse relay add form", slog.String("error", parseErr.Error()))
		http.Redirect(w, r, "/relay-management?error=Failed+to+parse+form", http.StatusSeeOther)
		return
	}

	// Get URL from form
	url := r.FormValue("url")
	if url == "" {
		s.logger.Error("Empty URL provided for relay add")
		http.Redirect(w, r, "/relay-management?error=URL+cannot+be+empty", http.StatusSeeOther)
		return
	}

	// Add URL to relay manager
	if addErr := s.relayManager.AddLink(url); addErr != nil {
		s.logger.Error("Failed to add relay URL",
			slog.String("url", url),
			slog.String("error", addErr.Error()))
		http.Redirect(w, r, "/relay-management?error="+addErr.Error(), http.StatusSeeOther)
		return
	}

	s.logger.Info("Relay URL added successfully", slog.String("url", url))
	http.Redirect(w, r, "/relay-management?success=Relay+URL+added+successfully", http.StatusSeeOther)
}

// removeRelayHandler handles requests to remove a relay URL.
func (s *Server) removeRelayHandler(w http.ResponseWriter, r *http.Request) {
	// Check authentication
	if !s.checkAuth(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Parse form
	if parseErr := r.ParseForm(); parseErr != nil {
		s.logger.Error("Failed to parse relay remove form", slog.String("error", parseErr.Error()))
		http.Redirect(w, r, "/relay-management?error=Failed+to+parse+form", http.StatusSeeOther)
		return
	}

	// Get index from form
	indexStr := r.FormValue("index")
	if indexStr == "" {
		s.logger.Error("No index provided for relay remove")
		http.Redirect(w, r, "/relay-management?error=No+index+provided", http.StatusSeeOther)
		return
	}

	// Parse index
	index, parseIntErr := strconv.Atoi(indexStr)
	if parseIntErr != nil {
		s.logger.Error("Invalid index for relay remove",
			slog.String("index", indexStr),
			slog.String("error", parseIntErr.Error()))
		http.Redirect(w, r, "/relay-management?error=Invalid+index", http.StatusSeeOther)
		return
	}

	// Remove URL from relay manager
	if removeErr := s.relayManager.RemoveLink(index); removeErr != nil {
		s.logger.Error("Failed to remove relay URL",
			slog.Int("index", index),
			slog.String("error", removeErr.Error()))
		http.Redirect(w, r, "/relay-management?error="+removeErr.Error(), http.StatusSeeOther)
		return
	}

	s.logger.Info("Relay URL removed successfully", slog.Int("index", index))
	http.Redirect(w, r, "/relay-management?success=Relay+URL+removed+successfully", http.StatusSeeOther)
}

// toggleRelayHandler handles requests to toggle relay functionality.
func (s *Server) toggleRelayHandler(w http.ResponseWriter, r *http.Request) {
	// Check authentication
	if !s.checkAuth(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Parse form
	if parseErr := r.ParseForm(); parseErr != nil {
		s.logger.Error("Failed to parse relay toggle form", slog.String("error", parseErr.Error()))
		http.Redirect(w, r, "/relay-management?error=Failed+to+parse+form", http.StatusSeeOther)
		return
	}

	// Get active state from form
	activeStr := r.FormValue("active")
	if activeStr == "" {
		s.logger.Error("No active state provided for relay toggle")
		http.Redirect(w, r, "/relay-management?error=No+active+state+provided", http.StatusSeeOther)
		return
	}

	// Parse active state
	var active bool
	switch activeStr {
	case "true", "1", "yes", "on":
		active = true
	case "false", "0", "no", "off":
		active = false
	default:
		s.logger.Error("Invalid active state for relay toggle", slog.String("active", activeStr))
		http.Redirect(w, r, "/relay-management?error=Invalid+active+state", http.StatusSeeOther)
		return
	}

	// Set active state in relay manager
	s.relayManager.SetActive(active)

	// Prepare success message
	status := "enabled"
	if !active {
		status = "disabled"
	}

	s.logger.Info("Relay functionality toggled", slog.Bool("active", active))
	http.Redirect(w, r, "/relay-management?success=Relay+"+status, http.StatusSeeOther)
}

// relayStreamHandler handles requests to stream audio from a relay source.
func (s *Server) relayStreamHandler(w http.ResponseWriter, r *http.Request) {
	// Get index from URL variables
	vars := mux.Vars(r)
	indexStr := vars["index"]
	if indexStr == "" {
		s.logger.Error("No index provided for relay stream")
		http.Error(w, "Missing stream index", http.StatusBadRequest)
		return
	}

	// Parse index
	index, parseIntErr := strconv.Atoi(indexStr)
	if parseIntErr != nil {
		s.logger.Error("Invalid index for relay stream",
			slog.String("index", indexStr),
			slog.String("error", parseIntErr.Error()))
		http.Error(w, "Invalid stream index", http.StatusBadRequest)
		return
	}

	// Relay the audio stream
	if relayErr := s.relayManager.RelayAudioStream(w, r, index); relayErr != nil {
		s.logger.Error("Failed to relay audio stream",
			slog.Int("index", index),
			slog.String("error", relayErr.Error()))
		http.Error(w, "Relay error: "+relayErr.Error(), http.StatusInternalServerError)
		return
	}
}
