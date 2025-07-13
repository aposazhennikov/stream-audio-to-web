package http

import (
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/user/stream-audio-to-web/telegram"
)

// getTimeInTimezone returns current time in the configured timezone
func getTimeInTimezone() time.Time {
	timezone := os.Getenv("TG_ALERT_TIMEZONE")
	if timezone == "" {
		timezone = "UTC"
	}
	
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		// If timezone loading fails, use UTC
		loc = time.UTC
	}
	
	return time.Now().In(loc)
}

// telegramAlertsHandler handles the telegram alerts management page
func (s *Server) telegramAlertsHandler(w http.ResponseWriter, r *http.Request) {
	// Check authentication
	if !s.checkAuth(r) {
		// Redirect to login with telegram-alerts as return URL
		originalURL := r.URL.Path
		if r.URL.RawQuery != "" {
			originalURL += "?" + r.URL.RawQuery
		}
		encodedURL := url.QueryEscape(originalURL)
		http.Redirect(w, r, "/status?redirect="+encodedURL, http.StatusFound)
		return
	}

	if s.telegramManager == nil {
		http.Error(w, "Telegram alerts not enabled", http.StatusServiceUnavailable)
		return
	}

	// Get current configuration
	config := s.telegramManager.GetConfig()

	// Get available routes from registered streams
	type RouteInfo struct {
		Route string
		ID    string // Safe ID for HTML attributes
	}
	availableRoutes := make([]RouteInfo, 0)
	s.mutex.RLock()
	for route := range s.streams {
		routeID := strings.ReplaceAll(route, "/", "_")
		if strings.HasPrefix(routeID, "_") {
			routeID = routeID[1:] // Remove leading underscore
		}
		availableRoutes = append(availableRoutes, RouteInfo{
			Route: route,
			ID:    routeID,
		})
	}
	s.mutex.RUnlock()

	// Get available relay routes
	availableRelayRoutes := make([]string, 0)
	if s.relayManager != nil {
		relayList := s.relayManager.GetLinks()
		for i := range relayList {
			availableRelayRoutes = append(availableRelayRoutes, strconv.Itoa(i))
		}
	}

	// Process any error/success messages
	errorMessage := r.URL.Query().Get("error")
	successMessage := r.URL.Query().Get("success")

	// Load template
	tmpl, err := template.ParseFiles(
		"templates/telegram_alerts.html",
		"templates/partials/head.html",
	)
	if err != nil {
		s.logger.Error("Unable to load telegram_alerts.html template", slog.String("error", err.Error()))
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	// Prepare template data
	data := map[string]interface{}{
		"Title":                "Telegram Alerts - Audio Stream Server",
		"Config":               config,
		"AvailableRoutes":      availableRoutes,
		"AvailableRelayRoutes": availableRelayRoutes,
		"ErrorMessage":         errorMessage,
		"SuccessMessage":       successMessage,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		s.logger.Error("Failed to execute telegram alerts template", slog.String("error", err.Error()))
		return
	}
}

// telegramAlertsUpdateHandler handles updates to telegram alerts configuration
func (s *Server) telegramAlertsUpdateHandler(w http.ResponseWriter, r *http.Request) {
	// Check authentication
	if !s.checkAuth(r) {
		// Redirect to login with telegram-alerts as return URL
		originalURL := r.URL.Path
		if r.URL.RawQuery != "" {
			originalURL += "?" + r.URL.RawQuery
		}
		encodedURL := url.QueryEscape(originalURL)
		http.Redirect(w, r, "/status?redirect="+encodedURL, http.StatusFound)
		return
	}

	if s.telegramManager == nil {
		http.Redirect(w, r, "/telegram-alerts?error=Telegram+alerts+not+enabled", http.StatusSeeOther)
		return
	}

	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/telegram-alerts?error=Invalid+request+method", http.StatusSeeOther)
		return
	}

	// Parse form
	if err := r.ParseForm(); err != nil {
		s.logger.Error("Unable to parse telegram alerts form", slog.String("error", err.Error()))
		http.Redirect(w, r, "/telegram-alerts?error=Failed+to+parse+form", http.StatusSeeOther)
		return
	}

	// Get current config
	config := s.telegramManager.GetConfig()

	// Update basic settings
	config.Enabled = r.FormValue("enabled") == "on"
	config.BotToken = strings.TrimSpace(r.FormValue("bot_token"))
	config.ChatID = strings.TrimSpace(r.FormValue("chat_id"))

	// Update route monitoring settings
	config.Routes = make(map[string]bool)
	s.mutex.RLock()
	for route := range s.streams {
		routeID := strings.ReplaceAll(route, "/", "_")
		if strings.HasPrefix(routeID, "_") {
			routeID = routeID[1:] // Remove leading underscore
		}
		config.Routes[route] = r.FormValue("route_"+routeID) == "on"
	}
	s.mutex.RUnlock()

	// Update relay route monitoring settings
	config.RelayRoutes = make(map[string]bool)
	if s.relayManager != nil {
		relayList := s.relayManager.GetLinks()
		for i := range relayList {
			indexStr := strconv.Itoa(i)
			config.RelayRoutes[indexStr] = r.FormValue("relay_"+indexStr) == "on"
		}
	}

	// Save configuration
	if err := s.telegramManager.UpdateConfig(config); err != nil {
		s.logger.Error("Failed to save telegram alerts config", slog.String("error", err.Error()))
		http.Redirect(w, r, "/telegram-alerts?error=Failed+to+save+configuration", http.StatusSeeOther)
		return
	}

	s.logger.Info("Telegram alerts configuration updated",
		"enabled", config.Enabled,
		"routes", len(config.Routes),
		"relay_routes", len(config.RelayRoutes))

	http.Redirect(w, r, "/telegram-alerts?success=Configuration+saved+successfully", http.StatusSeeOther)
}

// telegramAlertsTestHandler handles testing telegram alerts
func (s *Server) telegramAlertsTestHandler(w http.ResponseWriter, r *http.Request) {
	// Check authentication
	if !s.checkAuth(r) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Authentication required",
		})
		return
	}

	if s.telegramManager == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Telegram alerts not enabled",
		})
		return
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Invalid request method",
		})
		return
	}

	// Parse JSON request
	var request struct {
		BotToken string `json:"bot_token"`
		ChatID   string `json:"chat_id"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Invalid request format",
		})
		return
	}

	// Validate inputs
	if request.BotToken == "" || request.ChatID == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Bot token and chat ID are required",
		})
		return
	}

	// Create test manager temporarily
	testManager := telegram.NewManager("", s.logger)
	
	// Create test message
	message := "🧪 *Telegram Alerts Test*\n\n"
	message += "✅ Bot configuration is working correctly!\n"
	message += "📅 *Test Time:* " + getTimeInTimezone().Format("15:04:05") + "\n"
	message += "🤖 *From:* Audio Stream Server"

	// Send test message
	if err := testManager.SendTelegramMessage(request.BotToken, request.ChatID, message); err != nil {
		s.logger.Error("Failed to send test telegram message", slog.String("error", err.Error()))
		
		// Provide helpful error messages
		errorMsg := err.Error()
		if strings.Contains(errorMsg, "chat not found") {
			errorMsg = "Chat not found. Make sure:\n" +
				"1. You sent /start to your bot first\n" +
				"2. For groups: add \"-\" before Chat ID (e.g., -1001234567890)\n" +
				"3. Bot is added to the group (if using group chat)\n" +
				"4. Chat ID is correct (use @userinfobot to verify)"
		} else if strings.Contains(errorMsg, "bot was blocked") {
			errorMsg = "Bot was blocked by user. Please unblock the bot and try again."
		} else if strings.Contains(errorMsg, "Unauthorized") {
			errorMsg = "Invalid bot token. Please check your bot token from @BotFather."
		}
		
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   errorMsg,
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Test message sent successfully!",
	})
} 