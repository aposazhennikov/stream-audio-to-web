package relay

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
)

// RelayManager handles the relay streaming functionality
type RelayManager struct {
	RelayLinks  []string          // List of URLs to relay
	mutex       sync.RWMutex      // For thread safety
	configFile  string            // Path to store relay list configuration
	relayActive bool              // Flag to enable/disable relay functionality
}

// NewRelayManager creates a new RelayManager
func NewRelayManager(configFile string) *RelayManager {
	manager := &RelayManager{
		RelayLinks:  make([]string, 0),
		configFile:  configFile,
		relayActive: false,
	}

	// Load existing configuration if file exists
	if _, err := os.Stat(configFile); err == nil {
		if err := manager.LoadLinksFromFile(); err != nil {
			log.Printf("ERROR: Failed to load relay links from %s: %v", configFile, err)
		}
	}

	return manager
}

// LoadLinksFromFile loads relay links from JSON file
func (rm *RelayManager) LoadLinksFromFile() error {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	data, err := os.ReadFile(rm.configFile)
	if err != nil {
		return fmt.Errorf("failed to read relay configuration file: %w", err)
	}

	if err := json.Unmarshal(data, &rm.RelayLinks); err != nil {
		return fmt.Errorf("failed to parse relay configuration: %w", err)
	}

	log.Printf("Loaded %d relay links from %s", len(rm.RelayLinks), rm.configFile)
	return nil
}

// SaveLinksToFile saves relay links to JSON file
func (rm *RelayManager) SaveLinksToFile() error {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	data, err := json.MarshalIndent(rm.RelayLinks, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal relay links to JSON: %w", err)
	}

	if err := os.WriteFile(rm.configFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write relay configuration to file: %w", err)
	}

	log.Printf("Saved %d relay links to %s", len(rm.RelayLinks), rm.configFile)
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
		return fmt.Errorf("invalid URL format, must start with http:// or https://")
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
	log.Printf("Added relay link: %s", link)
	
	// Создаем копию ссылок для сохранения
	links := make([]string, len(rm.RelayLinks))
	copy(links, rm.RelayLinks)
	configFile := rm.configFile
	
	// Разблокируем мьютекс перед сохранением
	rm.mutex.Unlock()

	// Сохраняем данные в файл
	data, err := json.MarshalIndent(links, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal relay links to JSON: %w", err)
	}

	if err := os.WriteFile(configFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write relay configuration to file: %w", err)
	}

	log.Printf("Saved %d relay links to %s", len(links), configFile)
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
	log.Printf("Removed relay link at index %d", index)
	
	// Создаем копию ссылок для сохранения
	links := make([]string, len(rm.RelayLinks))
	copy(links, rm.RelayLinks)
	configFile := rm.configFile
	
	// Разблокируем мьютекс перед сохранением
	rm.mutex.Unlock()

	// Сохраняем данные в файл
	data, err := json.MarshalIndent(links, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal relay links to JSON: %w", err)
	}

	if err := os.WriteFile(configFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write relay configuration to file: %w", err)
	}

	log.Printf("Saved %d relay links to %s", len(links), configFile)
	return nil
}

// SetActive sets the active state of relay functionality
func (rm *RelayManager) SetActive(active bool) {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()
	rm.relayActive = active
	log.Printf("Relay functionality %s", map[bool]string{true: "enabled", false: "disabled"}[active])
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
		return fmt.Errorf("relay functionality is disabled")
	}
	
	links := rm.GetLinks()
	if index < 0 || index >= len(links) {
		return fmt.Errorf("invalid stream index: %d", index)
	}

	sourceURL := links[index]
	log.Printf("Relaying audio stream from %s", sourceURL)

	// Create request to source
	req, err := http.NewRequest("GET", sourceURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Copy essential headers from the original request
	req.Header.Set("User-Agent", r.Header.Get("User-Agent"))
	req.Header.Set("Accept", r.Header.Get("Accept"))
	req.Header.Set("Range", r.Header.Get("Range"))

	// Execute the request to the source
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch from source: %w", err)
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
	buf := make([]byte, 4096) // 4KB buffer
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				if !isConnectionClosedError(err) {
					log.Printf("Error writing to client: %v", err)
				}
				return nil // Client disconnected
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil // End of stream
			}
			return fmt.Errorf("error reading from source: %w", err)
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