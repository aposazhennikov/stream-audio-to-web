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
	Route         string     `json:"route"`
	IsAvailable   bool       `json:"is_available"`
	LastCheck     time.Time  `json:"last_check"`
	DownSince     *time.Time `json:"down_since,omitempty"`
	LastAlert     *time.Time `json:"last_alert,omitempty"`
	LastAlertType *bool      `json:"last_alert_type,omitempty"` // true = up alert, false = down alert
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
	timezone         string          // Timezone for alert timestamps
	isInitialCheck   bool // Flag to skip alerts on first check after startup
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
		timezone:       "Europe/Moscow",  // Default timezone for alerts
		isInitialCheck: true, // Set to true initially
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
	m.logger.Debug("TELEGRAM CONFIG: GetConfig called - acquiring RLock")
	m.mutex.RLock()
	m.logger.Debug("TELEGRAM CONFIG: RLock acquired successfully")
	defer func() {
		m.logger.Debug("TELEGRAM CONFIG: Releasing RLock")
		m.mutex.RUnlock()
		m.logger.Debug("TELEGRAM CONFIG: RLock released")
	}()
	
	if m.config == nil {
		m.logger.Debug("TELEGRAM CONFIG: Config is nil, returning default")
		return &AlertConfig{
			Enabled:     false,
			Routes:      make(map[string]bool),
			RelayRoutes: make(map[string]bool),
		}
	}
	
	m.logger.Debug("TELEGRAM CONFIG: Creating config copy")
	// Return a copy to prevent race conditions
	config := *m.config
	config.Routes = make(map[string]bool)
	config.RelayRoutes = make(map[string]bool)
	
	m.logger.Debug("TELEGRAM CONFIG: Copying routes", "routes_count", len(m.config.Routes))
	for k, v := range m.config.Routes {
		config.Routes[k] = v
	}
	
	m.logger.Debug("TELEGRAM CONFIG: Copying relay routes", "relay_routes_count", len(m.config.RelayRoutes))
	for k, v := range m.config.RelayRoutes {
		config.RelayRoutes[k] = v
	}
	
	m.logger.Debug("TELEGRAM CONFIG: Config copy completed successfully")
	return &config
}

// GetConfigFast returns config copy quickly without blocking background operations
func (m *Manager) GetConfigFast() *AlertConfig {
	m.logger.Debug("TELEGRAM CONFIG: GetConfigFast called - acquiring RLock")
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	
	if m.config == nil {
		m.logger.Debug("TELEGRAM CONFIG: No config available")
		return &AlertConfig{
			Enabled:     false,
			Routes:      make(map[string]bool),
			RelayRoutes: make(map[string]bool),
		}
	}
	
	// Make fast copy
	configCopy := *m.config
	configCopy.Routes = make(map[string]bool)
	configCopy.RelayRoutes = make(map[string]bool)
	for k, v := range m.config.Routes {
		configCopy.Routes[k] = v
	}
	for k, v := range m.config.RelayRoutes {
		configCopy.RelayRoutes[k] = v
	}
	
	m.logger.Debug("TELEGRAM CONFIG: GetConfigFast completed")
	return &configCopy
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
	m.logger.Debug("TELEGRAM CONFIG: IsEnabled called - acquiring RLock")
	m.mutex.RLock()
	defer func() {
		m.logger.Debug("TELEGRAM CONFIG: IsEnabled releasing RLock")
		m.mutex.RUnlock()
	}()
	m.logger.Debug("TELEGRAM CONFIG: IsEnabled RLock acquired")
	result := m.config != nil && m.config.Enabled
	m.logger.Debug("TELEGRAM CONFIG: IsEnabled result", "enabled", result)
	return result
}

// SetRouteCheckFunc sets the function to check route availability
func (m *Manager) SetRouteCheckFunc(fn func(string) bool) {
	m.routeCheckFunc = fn
}

// SetRelayCheckFunc sets the function to check relay availability
func (m *Manager) SetRelayCheckFunc(fn func(string) bool) {
	m.relayCheckFunc = fn
}

// SetTimezone sets the timezone for alert timestamps.
func (m *Manager) SetTimezone(timezone string) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.timezone = timezone
	m.logger.Info("Telegram alert timezone updated", "timezone", timezone)
}

// GetTimeInConfiguredTimezone returns current time in the configured timezone.
func (m *Manager) GetTimeInConfiguredTimezone() time.Time {
	m.mutex.RLock()
	timezone := m.timezone
	m.mutex.RUnlock()
	
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		// If timezone loading fails, use UTC
		m.logger.Warn("Failed to load timezone, using UTC", "timezone", timezone, "error", err)
		loc = time.UTC
	}
	
	return time.Now().In(loc)
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
	
	// CRITICAL FIX: Perform initial state check for all enabled streams
	// This ensures that offline streams are detected and alerts sent immediately on startup
	go func() {
		// Small delay to allow check functions to be properly initialized
		time.Sleep(2 * time.Second)
		m.logger.Info("TELEGRAM ALERTS: Performing initial state check for all enabled streams")
		
		isEnabled := m.IsEnabled()
		m.logger.Info("TELEGRAM ALERTS: Initial check - IsEnabled result", "enabled", isEnabled)
		
		if isEnabled {
			m.logger.Info("TELEGRAM ALERTS: Initial check - calling checkStreams()")
			m.checkStreams()
		} else {
			m.logger.Info("TELEGRAM ALERTS: Initial check - skipping checkStreams because IsEnabled=false")
		}
	}()
	
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
	// CRITICAL DEADLOCK FIX: Use quick config copy instead of long-running GetConfig()
	// GetConfig() holds RLock which blocks HTTP handlers during long relay checks
	m.logger.Debug("TELEGRAM ALERTS: checkStreams ENTRY - getting config copy")
	
	// Quick copy without holding lock during network operations
	m.mutex.RLock()
	if m.config == nil {
		m.mutex.RUnlock()
		m.logger.Debug("TELEGRAM ALERTS: No config available, skipping check")
		return
	}
	
	// Make fast copy
	configCopy := *m.config
	configCopy.Routes = make(map[string]bool)
	configCopy.RelayRoutes = make(map[string]bool)
	for k, v := range m.config.Routes {
		configCopy.Routes[k] = v
	}
	for k, v := range m.config.RelayRoutes {
		configCopy.RelayRoutes[k] = v
	}
	m.mutex.RUnlock()
	
	m.logger.Debug("TELEGRAM ALERTS: Config copy completed, RLock released")
	config := &configCopy
	
	// Log check start  
	m.logger.Info("TELEGRAM ALERTS: Starting stream check cycle", 
		"routes", len(config.Routes),
		"relay_routes", len(config.RelayRoutes),
		"routeCheckFunc_set", m.routeCheckFunc != nil,
		"relayCheckFunc_set", m.relayCheckFunc != nil)
	
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
		
		// CRITICAL FIX: Pass config to avoid re-locking
		m.updateStreamStatusWithConfig(route, isAvailable, false, config)
	}
	
	// Check relay routes
	m.logger.Info("TELEGRAM ALERTS: Starting relay routes check", "total_relays", len(config.RelayRoutes))
	for relayIndex, enabled := range config.RelayRoutes {
		if !enabled {
			m.logger.Debug("TELEGRAM ALERTS: Skipping disabled relay", "relay", relayIndex)
			continue
		}
		m.logger.Info("TELEGRAM ALERTS: Checking relay", "relay", relayIndex)
		
		isAvailable := false
		if m.relayCheckFunc != nil {
			isAvailable = m.relayCheckFunc(relayIndex)
		}
		
		m.logger.Info("TELEGRAM ALERTS: Relay check result", 
			"relay", relayIndex, 
			"available", isAvailable)
		
		// CRITICAL FIX: Pass config to avoid re-locking
		m.updateStreamStatusWithConfig("relay/"+relayIndex, isAvailable, true, config)
	}
	
	// Reset initial check flag after first complete check
	if m.isInitialCheck {
		m.mutex.Lock()
		m.isInitialCheck = false
		m.mutex.Unlock()
		m.logger.Info("TELEGRAM ALERTS: Initial check completed - future checks will send alerts for state changes")
	}
}

// updateStreamStatus updates the status of a stream and sends alerts if needed
func (m *Manager) updateStreamStatus(route string, isAvailable bool, isRelay bool) {
	config := m.GetConfig()
	m.updateStreamStatusWithConfig(route, isAvailable, isRelay, config)
}

// updateStreamStatusWithConfig updates the status of a stream with provided config to avoid re-locking
func (m *Manager) updateStreamStatusWithConfig(route string, isAvailable bool, isRelay bool, config *AlertConfig) {
	// Prepare variables outside the lock to minimize lock time
	var needAlert bool
	var alertIsUp bool
	var alertDowntime *time.Duration
	
	m.mutex.Lock()
	
	var statusMap map[string]*StreamStatus
	if isRelay {
		statusMap = m.relayStatuses
	} else {
		statusMap = m.streamStatuses
	}
	
	status, exists := statusMap[route]
	var wasAvailable bool
	
	if !exists {
		// For new streams during initial check, don't send alerts
		// This prevents duplicate alerts when application restarts
		if m.isInitialCheck {
			// During initial check, just record current state without alerts
			wasAvailable = isAvailable
		} else {
			// For truly new streams (discovered after startup), assume they were previously UP
			wasAvailable = true
		}
		status = &StreamStatus{
			Route:       route,
			IsAvailable: isAvailable,
			LastCheck:   time.Now(),
		}
		statusMap[route] = status
		m.logger.Info("TELEGRAM ALERTS: New stream status created", 
			"route", route, 
			"current_available", isAvailable,
			"assumed_previous", wasAvailable,
			"is_relay", isRelay,
			"is_initial_check", m.isInitialCheck)
	} else {
		wasAvailable = status.IsAvailable
	}
	
	now := time.Now()
	status.IsAvailable = isAvailable
	status.LastCheck = now
	
	// Handle state changes and prepare alert info
	if wasAvailable && !isAvailable {
		// Stream went down
		status.DownSince = &now
		needAlert = true
		alertIsUp = false
		alertDowntime = nil
		m.logger.Info("TELEGRAM ALERTS: Stream went DOWN", 
			"route", route, 
			"is_relay", isRelay)
	} else if !wasAvailable && isAvailable {
		// Stream came back up
		var downtime *time.Duration
		if status.DownSince != nil {
			dt := now.Sub(*status.DownSince)
			downtime = &dt
		}
		status.DownSince = nil
		needAlert = true
		alertIsUp = true
		alertDowntime = downtime
		m.logger.Info("TELEGRAM ALERTS: Stream came UP", 
			"route", route, 
			"is_relay", isRelay, 
			"downtime", downtime)
	}
	
	m.mutex.Unlock()
	
	// CRITICAL DEADLOCK FIX: Send alert OUTSIDE the lock to prevent deadlock
	// This prevents the case where we hold Lock() and try to acquire RLock() in sendAlertWithConfig()
	if needAlert {
		m.sendAlertWithConfig(route, alertIsUp, alertDowntime, isRelay, config)
	}
}

// sendAlert sends a telegram alert
func (m *Manager) sendAlert(route string, isUp bool, downtime *time.Duration, isRelay bool) {
	config := m.GetConfig()
	m.sendAlertWithConfig(route, isUp, downtime, isRelay, config)
}

// sendAlertWithConfig sends a telegram alert with provided config to avoid re-locking
func (m *Manager) sendAlertWithConfig(route string, isUp bool, downtime *time.Duration, isRelay bool, config *AlertConfig) {
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
	
	// Check cooldown - but allow immediate alerts for state changes (down->up or up->down)
	m.mutex.RLock()
	var statusMap map[string]*StreamStatus
	if isRelay {
		statusMap = m.relayStatuses
	} else {
		statusMap = m.streamStatuses
	}
	
	status := statusMap[route]
	shouldSkipDueToCooldown := false
	if status.LastAlert != nil && time.Since(*status.LastAlert) < m.alertCooldown {
		// Only apply cooldown if the alert type is the same (down->down or up->up)
		if status.LastAlertType != nil && *status.LastAlertType == isUp {
			shouldSkipDueToCooldown = true
			m.logger.Debug("TELEGRAM ALERTS: Skipping alert due to cooldown", 
				"route", route, 
				"is_up", isUp,
				"last_alert_type", *status.LastAlertType,
				"time_since_last", time.Since(*status.LastAlert))
		}
	}
	m.mutex.RUnlock()
	
	if shouldSkipDueToCooldown {
		return
	}
	
	// Create alert message
	var message string
	if isUp {
		emoji := "✅"
		if isRelay {
			emoji = "🔄"
		}
		message = fmt.Sprintf("%s *Stream Restored*\n\n", emoji)
		message += fmt.Sprintf("🎵 *Route:* `%s`\n", route)
		message += fmt.Sprintf("⏰ *Time:* %s\n", m.GetTimeInConfiguredTimezone().Format("15:04:05"))
		
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
		message += fmt.Sprintf("⏰ *Time:* %s\n", m.GetTimeInConfiguredTimezone().Format("15:04:05"))
		
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
		// Get status again inside the lock for safe update
		var statusMap map[string]*StreamStatus
		if isRelay {
			statusMap = m.relayStatuses
		} else {
			statusMap = m.streamStatuses
		}
		
		if status := statusMap[route]; status != nil {
			now := time.Now()
			status.LastAlert = &now
			status.LastAlertType = &isUp // Update alert type
		}
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