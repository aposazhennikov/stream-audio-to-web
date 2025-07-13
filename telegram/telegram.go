package telegram

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AlertConfig represents the configuration for telegram alerts
type AlertConfig struct {
	Enabled     bool                    `json:"enabled"`
	BotToken    string                  `json:"bot_token"`
	ChatID      string                  `json:"chat_id"`
	Routes      map[string]bool         `json:"routes"`       // route -> enabled
	RelayRoutes map[string]bool         `json:"relay_routes"` // relay index -> enabled
	UpdatedAt   time.Time              `json:"updated_at"`
}

// StreamStatus represents the status of a stream
type StreamStatus struct {
	Route       string    `json:"route"`
	IsAvailable bool      `json:"is_available"`
	LastCheck   time.Time `json:"last_check"`
	DownSince   *time.Time `json:"down_since,omitempty"`
	LastAlert   *time.Time `json:"last_alert,omitempty"`
}

// Manager handles telegram alerts
type Manager struct {
	config           *AlertConfig
	configFile       string
	streamStatuses   map[string]*StreamStatus
	relayStatuses    map[string]*StreamStatus
	mutex            sync.RWMutex
	logger           *slog.Logger
	httpClient       *http.Client
	alertCooldown    time.Duration
	checkInterval    time.Duration
	routeCheckFunc   func(string) bool
	relayCheckFunc   func(string) bool
	stopChan         chan struct{}
	running          bool
}

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

// NewManager creates a new telegram alert manager
func NewManager(configFile string, logger *slog.Logger) *Manager {
	return &Manager{
		configFile:     configFile,
		streamStatuses: make(map[string]*StreamStatus),
		relayStatuses:  make(map[string]*StreamStatus),
		logger:         logger,
		httpClient:     &http.Client{Timeout: 10 * time.Second},
		alertCooldown:  5 * time.Minute,  // Don't spam alerts
		checkInterval:  30 * time.Second, // Check every 30 seconds
		stopChan:       make(chan struct{}),
	}
}

// LoadConfig loads the telegram alert configuration
func (m *Manager) LoadConfig() error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Create default config if file doesn't exist
	if _, err := os.Stat(m.configFile); os.IsNotExist(err) {
		m.config = &AlertConfig{
			Enabled:     false,
			Routes:      make(map[string]bool),
			RelayRoutes: make(map[string]bool),
			UpdatedAt:   time.Now(),
		}
		return m.saveConfigUnsafe()
	}

	data, err := os.ReadFile(m.configFile)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	m.config = &AlertConfig{}
	if err := json.Unmarshal(data, m.config); err != nil {
		return fmt.Errorf("failed to unmarshal config: %w", err)
	}

	if m.config.Routes == nil {
		m.config.Routes = make(map[string]bool)
	}
	if m.config.RelayRoutes == nil {
		m.config.RelayRoutes = make(map[string]bool)
	}

	m.logger.Info("Telegram alerts config loaded", 
		"enabled", m.config.Enabled,
		"routes", len(m.config.Routes),
		"relay_routes", len(m.config.RelayRoutes))

	return nil
}

// SaveConfig saves the telegram alert configuration
func (m *Manager) SaveConfig() error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.saveConfigUnsafe()
}

func (m *Manager) saveConfigUnsafe() error {
	// Create directory if it doesn't exist
	dir := filepath.Dir(m.configFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	m.config.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(m.config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	return os.WriteFile(m.configFile, data, 0644)
}

// GetConfig returns the current configuration
func (m *Manager) GetConfig() *AlertConfig {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	
	if m.config == nil {
		return &AlertConfig{
			Enabled:     false,
			Routes:      make(map[string]bool),
			RelayRoutes: make(map[string]bool),
		}
	}
	
	// Return a copy to prevent race conditions
	config := *m.config
	config.Routes = make(map[string]bool)
	config.RelayRoutes = make(map[string]bool)
	
	for k, v := range m.config.Routes {
		config.Routes[k] = v
	}
	for k, v := range m.config.RelayRoutes {
		config.RelayRoutes[k] = v
	}
	
	return &config
}

// UpdateConfig updates the configuration
func (m *Manager) UpdateConfig(newConfig *AlertConfig) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	
	m.config = newConfig
	return m.saveConfigUnsafe()
}

// IsEnabled returns whether telegram alerts are enabled
func (m *Manager) IsEnabled() bool {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.config != nil && m.config.Enabled
}

// SetRouteCheckFunc sets the function to check route availability
func (m *Manager) SetRouteCheckFunc(fn func(string) bool) {
	m.routeCheckFunc = fn
}

// SetRelayCheckFunc sets the function to check relay availability
func (m *Manager) SetRelayCheckFunc(fn func(string) bool) {
	m.relayCheckFunc = fn
}

// Start starts the monitoring loop
func (m *Manager) Start() {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	
	if m.running {
		return
	}
	
	m.running = true
	go m.monitoringLoop()
	m.logger.Info("TELEGRAM ALERTS: Monitoring started")
}

// Stop stops the monitoring loop
func (m *Manager) Stop() {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	
	if !m.running {
		return
	}
	
	m.running = false
	close(m.stopChan)
	m.logger.Info("Telegram alerts monitoring stopped")
}

// monitoringLoop runs the monitoring loop
func (m *Manager) monitoringLoop() {
	ticker := time.NewTicker(m.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopChan:
			return
		case <-ticker.C:
			if m.IsEnabled() {
				m.checkStreams()
			}
		}
	}
}

// checkStreams checks all monitored streams
func (m *Manager) checkStreams() {
	config := m.GetConfig()
	
	// Log check start
	m.logger.Debug("TELEGRAM ALERTS: Starting stream check cycle", 
		"routes", len(config.Routes),
		"relay_routes", len(config.RelayRoutes))
	
	// Check regular routes
	for route, enabled := range config.Routes {
		if !enabled {
			continue
		}
		
		isAvailable := false
		if m.routeCheckFunc != nil {
			isAvailable = m.routeCheckFunc(route)
		}
		
		m.logger.Debug("TELEGRAM ALERTS: Route check result", 
			"route", route, 
			"available", isAvailable)
		
		m.updateStreamStatus(route, isAvailable, false)
	}
	
	// Check relay routes
	for relayIndex, enabled := range config.RelayRoutes {
		if !enabled {
			continue
		}
		
		isAvailable := false
		if m.relayCheckFunc != nil {
			isAvailable = m.relayCheckFunc(relayIndex)
		}
		
		m.logger.Debug("TELEGRAM ALERTS: Relay check result", 
			"relay", relayIndex, 
			"available", isAvailable)
		
		m.updateStreamStatus("relay/"+relayIndex, isAvailable, true)
	}
}

// updateStreamStatus updates the status of a stream and sends alerts if needed
func (m *Manager) updateStreamStatus(route string, isAvailable bool, isRelay bool) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	
	var statusMap map[string]*StreamStatus
	if isRelay {
		statusMap = m.relayStatuses
	} else {
		statusMap = m.streamStatuses
	}
	
	status, exists := statusMap[route]
	if !exists {
		status = &StreamStatus{
			Route:       route,
			IsAvailable: isAvailable,
			LastCheck:   time.Now(),
		}
		statusMap[route] = status
	}
	
	now := time.Now()
	wasAvailable := status.IsAvailable
	status.IsAvailable = isAvailable
	status.LastCheck = now
	
	// Handle state changes
	if wasAvailable && !isAvailable {
		// Stream went down
		status.DownSince = &now
		m.logger.Info("TELEGRAM ALERTS: Stream went DOWN", 
			"route", route, 
			"is_relay", isRelay)
		m.sendAlert(route, false, nil, isRelay)
	} else if !wasAvailable && isAvailable {
		// Stream came back up
		var downtime *time.Duration
		if status.DownSince != nil {
			dt := now.Sub(*status.DownSince)
			downtime = &dt
		}
		status.DownSince = nil
		m.logger.Info("TELEGRAM ALERTS: Stream came UP", 
			"route", route, 
			"is_relay", isRelay, 
			"downtime", downtime)
		m.sendAlert(route, true, downtime, isRelay)
	}
}

// sendAlert sends a telegram alert
func (m *Manager) sendAlert(route string, isUp bool, downtime *time.Duration, isRelay bool) {
	config := m.GetConfig()
	
	m.logger.Debug("TELEGRAM ALERTS: Attempting to send alert", 
		"route", route, 
		"is_up", isUp, 
		"is_relay", isRelay,
		"config_enabled", config.Enabled,
		"has_bot_token", config.BotToken != "",
		"has_chat_id", config.ChatID != "")
	
	// Check if we can send alerts
	if !config.Enabled || config.BotToken == "" || config.ChatID == "" {
		m.logger.Debug("TELEGRAM ALERTS: Cannot send alert - missing configuration", 
			"enabled", config.Enabled,
			"has_bot_token", config.BotToken != "",
			"has_chat_id", config.ChatID != "")
		return
	}
	
	// Check cooldown
	m.mutex.RLock()
	var statusMap map[string]*StreamStatus
	if isRelay {
		statusMap = m.relayStatuses
	} else {
		statusMap = m.streamStatuses
	}
	
	status := statusMap[route]
	if status.LastAlert != nil && time.Since(*status.LastAlert) < m.alertCooldown {
		m.mutex.RUnlock()
		return
	}
	m.mutex.RUnlock()
	
	// Create alert message
	var message string
	if isUp {
		emoji := "✅"
		if isRelay {
			emoji = "🔄"
		}
		message = fmt.Sprintf("%s *Stream Restored*\n\n", emoji)
		message += fmt.Sprintf("🎵 *Route:* `%s`\n", route)
		message += fmt.Sprintf("⏰ *Time:* %s\n", getTimeInTimezone().Format("15:04:05"))
		
		if downtime != nil {
			message += fmt.Sprintf("⏱️ *Downtime:* %s\n", formatDuration(*downtime))
		}
		
		if isRelay {
			message += "🌐 *Type:* Relay Stream"
		} else {
			message += "📻 *Type:* Main Stream"
		}
	} else {
		emoji := "🚨"
		if isRelay {
			emoji = "⚠️"
		}
		message = fmt.Sprintf("%s *Stream Down*\n\n", emoji)
		message += fmt.Sprintf("🎵 *Route:* `%s`\n", route)
		message += fmt.Sprintf("⏰ *Time:* %s\n", getTimeInTimezone().Format("15:04:05"))
		
		if isRelay {
			message += "🌐 *Type:* Relay Stream"
		} else {
			message += "📻 *Type:* Main Stream"
		}
	}
	
	// Send the alert
	if err := m.sendTelegramMessage(config.BotToken, config.ChatID, message); err != nil {
		m.logger.Error("TELEGRAM ALERTS: Failed to send alert", 
			"route", route, 
			"error", err.Error())
	} else {
		m.logger.Info("TELEGRAM ALERTS: Alert sent successfully", 
			"route", route, 
			"is_up", isUp)
		
		// Update last alert time
		m.mutex.Lock()
		now := time.Now()
		status.LastAlert = &now
		m.mutex.Unlock()
	}
}

// SendTelegramMessage sends a message to telegram (public method for testing)
func (m *Manager) SendTelegramMessage(botToken, chatID, message string) error {
	return m.sendTelegramMessage(botToken, chatID, message)
}

// sendTelegramMessage sends a message to telegram
func (m *Manager) sendTelegramMessage(botToken, chatID, message string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
	
	// Log the attempt for debugging
	if m.logger != nil {
		m.logger.Debug("Sending telegram message", 
			"chat_id", chatID, 
			"bot_token_length", len(botToken),
			"message_length", len(message))
	}
	
	payload := map[string]interface{}{
		"chat_id":    chatID,
		"text":       message,
		"parse_mode": "Markdown",
	}
	
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}
	
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	
	req.Header.Set("Content-Type", "application/json")
	
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	
	body, _ := io.ReadAll(resp.Body)
	
	if resp.StatusCode != http.StatusOK {
		// Try to parse the error response for better error messages
		var errorResponse struct {
			OK          bool   `json:"ok"`
			ErrorCode   int    `json:"error_code"`
			Description string `json:"description"`
		}
		
		if json.Unmarshal(body, &errorResponse) == nil {
			if m.logger != nil {
				m.logger.Error("Telegram API error details", 
					"error_code", errorResponse.ErrorCode,
					"description", errorResponse.Description,
					"chat_id", chatID)
			}
			return fmt.Errorf("telegram API error: %s", errorResponse.Description)
		}
		
		return fmt.Errorf("telegram API error: %s", string(body))
	}
	
	if m.logger != nil {
		m.logger.Info("Telegram message sent successfully", 
			"chat_id", chatID,
			"message_length", len(message))
	}
	
	return nil
}

// formatDuration formats a duration in a human-readable way
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	} else if d < time.Hour {
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	} else {
		hours := int(d.Hours())
		minutes := int(d.Minutes()) % 60
		return fmt.Sprintf("%d hours %d minutes", hours, minutes)
	}
} 