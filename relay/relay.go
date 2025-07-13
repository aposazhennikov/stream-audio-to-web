// Package relay implements functionality for relaying audio streams from external sources.
// It includes components for managing relay links, streaming from external sources, and handling relay configuration.
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
	"time"
)

const (
	relayBufferSize = 4096
)

// Manager handles the relay streaming functionality.
type Manager struct {
	RelayLinks  []string     // List of URLs to relay
	mutex       sync.RWMutex // For thread safety
	configFile  string       // Path to store relay list configuration
	relayActive bool         // Flag to enable/disable relay functionality
	logger      *slog.Logger
}

// NewRelayManager creates a new RelayManager.
func NewRelayManager(configFile string, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	manager := &Manager{
		RelayLinks:  make([]string, 0),
		configFile:  configFile,
		relayActive: false,
		logger:      logger,
	}

	// Load existing configuration if file exists.
	if _, statConfigFileErr := os.Stat(configFile); statConfigFileErr == nil {
		if loadLinksErr := manager.LoadLinksFromFile(); loadLinksErr != nil {
			manager.logger.Error(
				"Failed to load relay links from file",
				slog.String("file", configFile),
				slog.String("error", loadLinksErr.Error()),
			)
		}
	}

	return manager
}

// LoadLinksFromFile loads relay links from JSON file.
func (rm *Manager) LoadLinksFromFile() error {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	data, readConfigFileErr := os.ReadFile(rm.configFile)
	if readConfigFileErr != nil {
		return fmt.Errorf("failed to read relay configuration file: %w", readConfigFileErr)
	}

	if unmarshalConfigErr := json.Unmarshal(data, &rm.RelayLinks); unmarshalConfigErr != nil {
		return fmt.Errorf("failed to parse relay configuration: %w", unmarshalConfigErr)
	}

	rm.logger.Info("Loaded relay links from file",
		slog.Int("count", len(rm.RelayLinks)),
		slog.String("file", rm.configFile))
	return nil
}

// SaveLinksToFile saves relay links to JSON file.
func (rm *Manager) SaveLinksToFile() error {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	data, marshalLinksErr := json.MarshalIndent(rm.RelayLinks, "", "  ")
	if marshalLinksErr != nil {
		return fmt.Errorf("failed to marshal relay links to JSON: %w", marshalLinksErr)
	}

	if writeConfigFileErr := os.WriteFile(rm.configFile, data, 0600); writeConfigFileErr != nil {
		return fmt.Errorf("failed to write relay configuration to file: %w", writeConfigFileErr)
	}

	rm.logger.Info(
		"Saved relay links to file",
		slog.Int("count", len(rm.RelayLinks)),
		slog.String("file", rm.configFile),
	)
	return nil
}

// GetLinks returns the current list of relay links.
func (rm *Manager) GetLinks() []string {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	links := make([]string, len(rm.RelayLinks))
	copy(links, rm.RelayLinks)
	return links
}

// AddLink adds a new link to relay list.
func (rm *Manager) AddLink(link string) error {
	// Validate URL format.
	if !strings.HasPrefix(link, "http://") && !strings.HasPrefix(link, "https://") {
		return errors.New("invalid URL format, must start with http:// or https://")
	}

	rm.mutex.Lock()

	// Check for duplicates.
	for _, existingLink := range rm.RelayLinks {
		if existingLink == link {
			rm.mutex.Unlock()
			return fmt.Errorf("duplicate link: %s already exists in relay list", link)
		}
	}

	rm.RelayLinks = append(rm.RelayLinks, link)
	rm.logger.Info("Added relay link", slog.String("link", link))

	// Создаем копию ссылок для сохранения.
	links := make([]string, len(rm.RelayLinks))
	copy(links, rm.RelayLinks)
	configFile := rm.configFile

	// Разблокируем мьютекс перед сохранением.
	rm.mutex.Unlock()

	// Сохраняем данные в файл.
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

// RemoveLink removes a link from relay list by index.
func (rm *Manager) RemoveLink(index int) error {
	rm.mutex.Lock()

	if index < 0 || index >= len(rm.RelayLinks) {
		rm.mutex.Unlock()
		return fmt.Errorf("invalid index: %d, valid range is 0-%d", index, len(rm.RelayLinks)-1)
	}

	// Remove link by index.
	rm.RelayLinks = append(rm.RelayLinks[:index], rm.RelayLinks[index+1:]...)
	rm.logger.Info("Removed relay link at index", slog.Int("index", index))

	// Создаем копию ссылок для сохранения.
	links := make([]string, len(rm.RelayLinks))
	copy(links, rm.RelayLinks)
	configFile := rm.configFile

	// Разблокируем мьютекс перед сохранением.
	rm.mutex.Unlock()

	// Сохраняем данные в файл.
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

// SetActive sets the active state of relay functionality.
func (rm *Manager) SetActive(active bool) {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()
	rm.relayActive = active
	rm.logger.Info("Relay functionality", slog.Bool("active", active))
}

// IsActive returns the current active state of relay functionality.
func (rm *Manager) IsActive() bool {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()
	return rm.relayActive
}

// RelayAudioStream relays audio stream from source to client.
func (rm *Manager) RelayAudioStream(w http.ResponseWriter, r *http.Request, index int) error {
	// Проверяем параметры запроса.
	sourceURL, err := rm.validateRelayRequest(index)
	if err != nil {
		return err
	}

	rm.logger.Info("Relaying audio stream from", slog.String("url", sourceURL))

	// Создаем запрос к источнику.
	req, err := rm.createSourceRequest(r, sourceURL)
	if err != nil {
		return err
	}

	// Выполняем запрос к источнику.
	resp, err := rm.executeSourceRequest(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Копируем заголовки из ответа источника.
	rm.copyResponseHeaders(w, resp)

	// Устанавливаем код статуса из ответа источника.
	w.WriteHeader(resp.StatusCode)

	// Стримим контент от источника к клиенту.
	return rm.streamFromSourceToClient(w, resp)
}

// validateRelayRequest проверяет, активна ли функция ретрансляции и валиден ли индекс.
func (rm *Manager) validateRelayRequest(index int) (string, error) {
	// Проверяем, активна ли ретрансляция.
	if !rm.IsActive() {
		return "", errors.New("relay functionality is disabled")
	}

	links := rm.GetLinks()
	if index < 0 || index >= len(links) {
		return "", fmt.Errorf("invalid stream index: %d", index)
	}

	return links[index], nil
}

// createSourceRequest создает запрос к источнику на основе исходного запроса.
func (rm *Manager) createSourceRequest(r *http.Request, sourceURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Копируем важные заголовки из исходного запроса.
	req.Header.Set("User-Agent", r.Header.Get("User-Agent"))
	req.Header.Set("Accept", r.Header.Get("Accept"))
	req.Header.Set("Range", r.Header.Get("Range"))

	return req, nil
}

// executeSourceRequest выполняет запрос к источнику.
func (rm *Manager) executeSourceRequest(req *http.Request) (*http.Response, error) {
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch from source: %w", err)
	}
	return resp, nil
}

// copyResponseHeaders копирует заголовки из ответа источника в ответ клиенту.
func (rm *Manager) copyResponseHeaders(w http.ResponseWriter, resp *http.Response) {
	// Копируем заголовки из ответа источника.
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	// Убеждаемся, что заголовок типа контента установлен.
	if contentType := resp.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	} else {
		w.Header().Set("Content-Type", "audio/mpeg") // По умолчанию mp3, если не указано
	}

	// Устанавливаем заголовки управления кешированием.
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

// streamFromSourceToClient стримит контент от источника к клиенту.
func (rm *Manager) streamFromSourceToClient(w http.ResponseWriter, resp *http.Response) error {
	buf := make([]byte, relayBufferSize) // 4KB буфер
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				if !isConnectionClosedError(writeErr) {
					rm.logger.Error("Error writing to client", slog.String("error", writeErr.Error()))
				}
				return nil // Клиент отключился
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil // Конец потока
			}
			return fmt.Errorf("error reading from source: %w", readErr)
		}
	}
}

// isConnectionClosedError checks if error is result of client closing connection.
func isConnectionClosedError(err error) bool {
	if err == nil {
		return false
	}

	errMsg := err.Error()
	return strings.Contains(errMsg, "broken pipe") ||
		strings.Contains(errMsg, "connection reset by peer") ||
		strings.Contains(errMsg, "use of closed network connection")
}

// StreamStatus represents the status of a stream.
type StreamStatus struct {
	Index  int    `json:"index"`
	URL    string `json:"url"`
	Status string `json:"status"` // "online", "offline", "unknown"
	Error  string `json:"error,omitempty"`
}

// CheckStreamStatus checks the status of a specific stream by making a GET request with limited data reading.
func (rm *Manager) CheckStreamStatus(index int) StreamStatus {
	links := rm.GetLinks()
	if index < 0 || index >= len(links) {
		return StreamStatus{
			Index:  index,
			URL:    "",
			Status: "unknown",
			Error:  "invalid index",
		}
	}

	url := links[index]
	status := StreamStatus{
		Index:  index,
		URL:    url,
		Status: "checking",
	}

	// Create HTTP client with timeout.
	client := &http.Client{
		Timeout: 10 * time.Second, // 10 second timeout
	}

	// Make GET request to check if stream is available.
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		status.Status = "offline"
		status.Error = fmt.Sprintf("failed to create request: %v", err)
		rm.logger.Error("Failed to create request for stream status check",
			slog.Int("index", index),
			slog.String("url", url),
			slog.String("error", err.Error()))
		return status
	}

	// Set headers to mimic a real audio player.
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "audio/mpeg, audio/*, */*")
	req.Header.Set("Range", "bytes=0-1023") // Request only first 1KB to test availability
	req.Header.Set("Connection", "close")

	resp, err := client.Do(req)
	if err != nil {
		status.Status = "offline"
		status.Error = fmt.Sprintf("request failed: %v", err)
		rm.logger.Error("Stream status check request failed",
			slog.Int("index", index),
			slog.String("url", url),
			slog.String("error", err.Error()))
		return status
	}
	defer resp.Body.Close()

	// Read a small amount of data to ensure stream is actually working.
	buffer := make([]byte, 1024)
	_, readErr := resp.Body.Read(buffer)
	if readErr != nil && readErr != io.EOF {
		status.Status = "offline"
		status.Error = fmt.Sprintf("failed to read stream data: %v", readErr)
		rm.logger.Error("Failed to read stream data",
			slog.Int("index", index),
			slog.String("url", url),
			slog.String("error", readErr.Error()))
		return status
	}

	// Check response status and content type.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		contentType := resp.Header.Get("Content-Type")

		// Check if content type looks like audio.
		if strings.Contains(contentType, "audio") ||
			strings.Contains(contentType, "mpeg") ||
			strings.Contains(contentType, "mp3") ||
			strings.Contains(contentType, "ogg") ||
			strings.Contains(contentType, "application/octet-stream") ||
			contentType == "" { // Some streams don't set content-type

			status.Status = "online"
			rm.logger.Info("Stream status check successful",
				slog.Int("index", index),
				slog.String("url", url),
				slog.Int("status_code", resp.StatusCode),
				slog.String("content_type", contentType))
		} else {
			status.Status = "offline"
			status.Error = fmt.Sprintf("invalid content type: %s", contentType)
			rm.logger.Warn("Stream has invalid content type",
				slog.Int("index", index),
				slog.String("url", url),
				slog.String("content_type", contentType))
		}
	} else if resp.StatusCode == 206 {
		// Partial content is also OK for range requests.
		status.Status = "online"
		rm.logger.Info("Stream status check successful (partial content)",
			slog.Int("index", index),
			slog.String("url", url),
			slog.Int("status_code", resp.StatusCode))
	} else {
		status.Status = "offline"
		status.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		rm.logger.Warn("Stream status check failed",
			slog.Int("index", index),
			slog.String("url", url),
			slog.Int("status_code", resp.StatusCode))
	}

	return status
}

// CheckAllStreamsStatus checks the status of all streams concurrently.
func (rm *Manager) CheckAllStreamsStatus() []StreamStatus {
	links := rm.GetLinks()
	statuses := make([]StreamStatus, len(links))

	if len(links) == 0 {
		return statuses
	}

	// Use channels and goroutines for concurrent status checking.
	type indexedStatus struct {
		index  int
		status StreamStatus
	}

	statusChan := make(chan indexedStatus, len(links))

	// Start goroutines for each stream.
	for i := range links {
		go func(index int) {
			status := rm.CheckStreamStatus(index)
			statusChan <- indexedStatus{index: index, status: status}
		}(i)
	}

	// Collect results.
	for i := 0; i < len(links); i++ {
		result := <-statusChan
		statuses[result.index] = result.status
	}

	rm.logger.Info("Completed status check for all streams",
		slog.Int("total_streams", len(links)))

	return statuses
}
