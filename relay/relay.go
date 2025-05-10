package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
)

const (
	relayBufferSize = 4096
)

// RelayManager handles the relay streaming functionality
type RelayManager struct {
	RelayLinks  []string     // List of URLs to relay
	mutex       sync.RWMutex // For thread safety
	configFile  string       // Path to store relay list configuration
	relayActive bool         // Flag to enable/disable relay functionality
	logger      *slog.Logger // Инстанс логгера
}

// NewRelayManager creates a new RelayManager
func NewRelayManager(configFile string, logger *slog.Logger) *RelayManager {
	if logger == nil {
		logger = slog.Default()
	}
	manager := &RelayManager{
		RelayLinks:  make([]string, 0),
		configFile:  configFile,
		relayActive: false,
		logger:      logger,
	}

	// Load existing configuration if file exists
	if _, statConfigFileErr := os.Stat(configFile); statConfigFileErr == nil {
		if loadLinksErr := manager.LoadLinksFromFile(); loadLinksErr != nil {
			manager.logger.Error("Failed to load relay links from file", slog.String("file", configFile), slog.String("error", loadLinksErr.Error()))
		}
	}

	return manager
}

// LoadLinksFromFile loads relay links from JSON file
func (rm *RelayManager) LoadLinksFromFile() error {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	data, readConfigFileErr := os.ReadFile(rm.configFile)
	if readConfigFileErr != nil {
		return fmt.Errorf("failed to read relay configuration file: %w", readConfigFileErr)
	}

	if unmarshalConfigErr := json.Unmarshal(data, &rm.RelayLinks); unmarshalConfigErr != nil {
		return fmt.Errorf("failed to parse relay configuration: %w", unmarshalConfigErr)
	}

	rm.logger.Info("Loaded relay links from file", slog.Int("count", len(rm.RelayLinks)), slog.String("file", rm.configFile))
	return nil
}

// SaveLinksToFile saves relay links to JSON file
func (rm *RelayManager) SaveLinksToFile() error {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	data, marshalLinksErr := json.MarshalIndent(rm.RelayLinks, "", "  ")
	if marshalLinksErr != nil {
		return fmt.Errorf("failed to marshal relay links to JSON: %w", marshalLinksErr)
	}

	if writeConfigFileErr := os.WriteFile(rm.configFile, data, 0600); writeConfigFileErr != nil {
		return fmt.Errorf("failed to write relay configuration to file: %w", writeConfigFileErr)
	}

	rm.logger.Info("Saved relay links to file", slog.Int("count", len(rm.RelayLinks)), slog.String("file", rm.configFile))
	return nil
}

// GetLinks returns the current list of relay links
func (rm *RelayManager) GetLinks() []string {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	links := make([]string, len(rm.RelayLinks))
	copy(links, rm.RelayLinks)
	return links
}

// AddLink adds a new link to relay list
func (rm *RelayManager) AddLink(link string) error {
	// Validate URL format
	if !strings.HasPrefix(link, "http://") && !strings.HasPrefix(link, "https://") {
		return errors.New("invalid URL format, must start with http:// or https://")
	}

	rm.mutex.Lock()

	// Check for duplicates
	for _, existingLink := range rm.RelayLinks {
		if existingLink == link {
			rm.mutex.Unlock()
			return fmt.Errorf("duplicate link: %s already exists in relay list", link)
		}
	}

	rm.RelayLinks = append(rm.RelayLinks, link)
	rm.logger.Info("Added relay link", slog.String("link", link))

	// Создаем копию ссылок для сохранения
	links := make([]string, len(rm.RelayLinks))
	copy(links, rm.RelayLinks)
	configFile := rm.configFile

	// Разблокируем мьютекс перед сохранением
	rm.mutex.Unlock()

	// Сохраняем данные в файл
	data, marshalLinksErr := json.MarshalIndent(links, "", "  ")
	if marshalLinksErr != nil {
		return fmt.Errorf("failed to marshal relay links to JSON: %w", marshalLinksErr)
	}

	if writeConfigFileErr := os.WriteFile(configFile, data, 0600); writeConfigFileErr != nil {
		return fmt.Errorf("failed to write relay configuration to file: %w", writeConfigFileErr)
	}

	rm.logger.Info("Saved relay links to file", slog.Int("count", len(links)), slog.String("file", configFile))
	return nil
}

// RemoveLink removes a link from relay list by index
func (rm *RelayManager) RemoveLink(index int) error {
	rm.mutex.Lock()

	if index < 0 || index >= len(rm.RelayLinks) {
		rm.mutex.Unlock()
		return fmt.Errorf("invalid index: %d, valid range is 0-%d", index, len(rm.RelayLinks)-1)
	}

	// Remove link by index
	rm.RelayLinks = append(rm.RelayLinks[:index], rm.RelayLinks[index+1:]...)
	rm.logger.Info("Removed relay link at index", slog.Int("index", index))

	// Создаем копию ссылок для сохранения
	links := make([]string, len(rm.RelayLinks))
	copy(links, rm.RelayLinks)
	configFile := rm.configFile

	// Разблокируем мьютекс перед сохранением
	rm.mutex.Unlock()

	// Сохраняем данные в файл
	data, marshalLinksErr := json.MarshalIndent(links, "", "  ")
	if marshalLinksErr != nil {
		return fmt.Errorf("failed to marshal relay links to JSON: %w", marshalLinksErr)
	}

	if writeConfigFileErr := os.WriteFile(configFile, data, 0600); writeConfigFileErr != nil {
		return fmt.Errorf("failed to write relay configuration to file: %w", writeConfigFileErr)
	}

	rm.logger.Info("Saved relay links to file", slog.Int("count", len(links)), slog.String("file", configFile))
	return nil
}

// SetActive sets the active state of relay functionality
func (rm *RelayManager) SetActive(active bool) {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()
	rm.relayActive = active
	rm.logger.Info("Relay functionality", slog.Bool("active", active))
}

// IsActive returns the current active state of relay functionality
func (rm *RelayManager) IsActive() bool {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()
	return rm.relayActive
}

// RelayAudioStream relays audio stream from source to client
func (rm *RelayManager) RelayAudioStream(w http.ResponseWriter, r *http.Request, index int) error {
	// Check if relay is active
	if !rm.IsActive() {
		return errors.New("relay functionality is disabled")
	}

	links := rm.GetLinks()
	if index < 0 || index >= len(links) {
		return fmt.Errorf("invalid stream index: %d", index)
	}

	sourceURL := links[index]
	rm.logger.Info("Relaying audio stream from", slog.String("url", sourceURL))

	// Create request to source
	req, createSourceRequestErr := http.NewRequestWithContext(r.Context(), http.MethodGet, sourceURL, nil)
	if createSourceRequestErr != nil {
		return fmt.Errorf("failed to create request: %w", createSourceRequestErr)
	}

	// Copy essential headers from the original request
	req.Header.Set("User-Agent", r.Header.Get("User-Agent"))
	req.Header.Set("Accept", r.Header.Get("Accept"))
	req.Header.Set("Range", r.Header.Get("Range"))

	// Execute the request to the source
	client := &http.Client{}
	resp, fetchSourceErr := client.Do(req)
	if fetchSourceErr != nil {
		return fmt.Errorf("failed to fetch from source: %w", fetchSourceErr)
	}
	defer resp.Body.Close()

	// Copy headers from source response to client response
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	// Ensure content type header is set
	if contentType := resp.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	} else {
		w.Header().Set("Content-Type", "audio/mpeg") // Default to mp3 if not specified
	}

	// Set cache control headers
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	// Set status code from source response
	w.WriteHeader(resp.StatusCode)

	// Stream the content from source to client
	buf := make([]byte, relayBufferSize) // 4KB buffer
	for {
		n, readSourceStreamErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeToClientErr := w.Write(buf[:n]); writeToClientErr != nil {
				if !isConnectionClosedError(writeToClientErr) {
					rm.logger.Error("Error writing to client", slog.String("error", writeToClientErr.Error()))
				}
				return nil // Client disconnected
			}
			if f, flusherOk := w.(http.Flusher); flusherOk {
				f.Flush()
			}
		}
		if readSourceStreamErr != nil {
			if readSourceStreamErr == io.EOF {
				return nil // End of stream
			}
			return fmt.Errorf("error reading from source: %w", readSourceStreamErr)
		}
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
