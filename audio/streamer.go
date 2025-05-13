package audio

import (
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"errors"

	sentry "github.com/getsentry/sentry-go"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	defaultBufferSize = 16384 // 16KB (reduced from 32KB to minimize gap when switching tracks).
	gracePeriodMs     = 50    // 50ms buffer between tracks (reduced from 100ms).
	// Minimum delay between buffer sends.
	minPlaybackDelayMs         = 10  // 10ms (reduced from 20ms).
	id3v2HeaderSize            = 10  // Size of ID3v2 header in bytes.
	id3v1TagSize               = 128 // Size of ID3v1 tag in bytes.
	tagCheckSize               = 3   // Size for checking 'TAG' string.
	bitsPerByte                = 8   // Количество бит в байте.
	id3v2SyncSafeShift21       = 21  // Bit shift for sync-safe integer (ID3v2).
	id3v2SyncSafeShift14       = 14
	id3v2SyncSafeShift7        = 7
	id3v2SyncSafeMask          = 0x7F // Mask for sync-safe integer (ID3v2).
	logIntervalSec             = 10   // Interval for logging delay info (seconds).
	defaultClientChannelBuffer = 32
	firstTimeoutMs             = 200
	secondTimeoutMs            = 200
	batchSize                  = 50
	streamerDefaultSampleRate  = 44100
	defaultChannels            = 2
	defaultBitDepth            = 16
)

// Streamer manages audio streaming for a single "radio" stream.
type Streamer struct {
	bufferPool         *sync.Pool
	bufferSize         int
	clientCounter      int32
	maxClients         int
	quit               chan struct{}
	currentTrackCh     chan string
	clientChannels     map[int]chan []byte
	clientMutex        sync.RWMutex
	transcodeFormat    string
	bitrate            int
	lastChunk          []byte                 // Last sent chunk of audio data.
	lastChunkMutex     sync.RWMutex           // Mutex for protecting lastChunk.
	normalizeVolume    bool                   // Flag to enable/disable volume normalization.
	lastClientCount    int                    // Для отслеживания количества клиентов при логировании.
	lastLogTime        time.Time              // Время последнего логирования.
	lastDelayLogTime   time.Time              // Время последнего логирования задержки.
	trackSecondsMetric *prometheus.CounterVec // Метрика для подсчета времени воспроизведения трека.
	metricMutex        sync.RWMutex           // Мьютекс для защиты метрики.
	logger             *slog.Logger           // Logger for streamer operations
}

// NewStreamer creates a new audio streamer.
func NewStreamer(bufferSize, maxClients int, transcodeFormat string, bitrate int) *Streamer {
	if bufferSize <= 0 {
		bufferSize = defaultBufferSize
	}

	return &Streamer{
		bufferPool: &sync.Pool{
			New: func() interface{} {
				return make([]byte, bufferSize)
			},
		},
		bufferSize:         bufferSize,
		maxClients:         maxClients,
		quit:               make(chan struct{}),
		currentTrackCh:     make(chan string, 1),
		clientChannels:     make(map[int]chan []byte),
		clientMutex:        sync.RWMutex{},
		transcodeFormat:    transcodeFormat,
		bitrate:            bitrate,
		lastChunk:          nil,
		lastChunkMutex:     sync.RWMutex{},
		normalizeVolume:    true, // Enable volume normalization by default.
		lastClientCount:    0,
		lastLogTime:        time.Time{},
		lastDelayLogTime:   time.Time{},
		trackSecondsMetric: nil,
		metricMutex:        sync.RWMutex{},
		logger:             slog.Default(),
	}
}

// SetVolumeNormalization enables or disables volume normalization.
func (s *Streamer) SetVolumeNormalization(enabled bool) {
	s.logger.Info("DIAGNOSTICS: Volume normalization", "enabled", enabled)
}

// Close closes the streamer and all client connections.
func (s *Streamer) Close() {
	// Signal closing.
	close(s.quit)

	// Clear the last buffer.
	s.lastChunkMutex.Lock()
	s.lastChunk = nil
	s.lastChunkMutex.Unlock()

	// Close all client channels.
	s.clientMutex.Lock()
	defer s.clientMutex.Unlock()

	for clientID, ch := range s.clientChannels {
		close(ch)
		s.logger.Info(
			"DIAGNOSTICS: Client channel",
			"clientID", clientID,
			"status", "closed when closing streamer",
		)
	}

	// Clear the channel map.
	s.clientChannels = make(map[int]chan []byte)

	s.logger.Info("DIAGNOSTICS: Streamer completely closed")
}

// StopCurrentTrack immediately stops the playback of the current track.
// Used when manually switching tracks.
func (s *Streamer) StopCurrentTrack() {
	// First signal the need to stop.
	// Safe check for already closed channel.
	select {
	case <-s.quit:
		// Channel already closed, create a new one.
		s.logger.Info("DIAGNOSTICS: Quit channel already closed, skipping closure")
	default:
		// Channel not yet closed, close it.
		close(s.quit)
	}

	// Clear the last buffer - this is critically important to prevent.
	// sending data from the old track to new clients.
	s.lastChunkMutex.Lock()
	s.lastChunk = nil
	s.lastChunkMutex.Unlock()

	// DO NOT close client channels to maintain connections.
	// s.clientMutex.Lock().
	// instead of closing channels just log.
	s.logger.Info("DIAGNOSTICS: Stopping current track without closing client connections")
	// s.clientMutex.Unlock().

	// Create new channel for next track.
	s.quit = make(chan struct{})

	s.logger.Info("DIAGNOSTICS: Current track stopped, buffer cleared")
}

// GetCurrentTrackChannel returns a channel with information about the current track.
func (s *Streamer) GetCurrentTrackChannel() <-chan string {
	return s.currentTrackCh
}

// StreamTrack streams a track to all connected clients.
func (s *Streamer) StreamTrack(trackPath string) error {
	// Check for empty path.
	if trackPath == "" {
		sentryErr := errors.New("empty audio file path")
		sentry.CaptureException(sentryErr)
		return sentryErr
	}

	s.logger.Info("DIAGNOSTICS: Attempting to play file", "trackPath", trackPath)

	// Check if file exists.
	fileInfo, statErr := os.Stat(trackPath)
	if statErr != nil {
		s.logger.Info("DIAGNOSTICS: ERROR checking file",
			"trackPath", trackPath,
			"error", statErr.Error())
		sentryErr := errors.New("error checking file")
		sentry.CaptureException(sentryErr)
		return sentryErr
	}

	s.logger.Info("DIAGNOSTICS: File exists",
		"trackPath", trackPath,
		"size", fileInfo.Size(),
		"mode", fileInfo.Mode().String())

	// Check that it's not a directory.
	if fileInfo.IsDir() {
		sentryErr := fmt.Errorf("specified path %s is a directory, not a file", trackPath)
		sentry.CaptureException(sentryErr)
		return sentryErr
	}

	// Open the file.
	s.logger.Info("DIAGNOSTICS: Attempting to open file", "trackPath", trackPath)
	file, openErr := os.Open(trackPath)
	if openErr != nil {
		s.logger.Info("DIAGNOSTICS: ERROR opening file",
			"trackPath", trackPath,
			"error", openErr.Error())
		sentryErr := errors.New("error opening file")
		sentry.CaptureException(sentryErr)
		return openErr
	}
	defer file.Close()

	// Send information about current track to channel.
	select {
	case s.currentTrackCh <- filepath.Base(trackPath):
		s.logger.Info("DIAGNOSTICS: Current track information updated", "track", filepath.Base(trackPath))
	default:
		s.logger.Info("DIAGNOSTICS: Failed to update current track information: channel full")
	}

	startTime := time.Now()

	// Check if normalization should be used.
	if !s.normalizeVolume {
		return s.processRawAudio(file, fileInfo, trackPath, startTime)
	}

	// --- Нормализация ---.
	return s.processNormalizedAudio(file, trackPath, startTime)
}

// processRawAudio handles streaming of raw (non-normalized) audio.
func (s *Streamer) processRawAudio(file *os.File, fileInfo os.FileInfo, trackPath string, startTime time.Time) error {
	// Skip ID3v2 if it exists.
	header := make([]byte, id3v2HeaderSize)
	_, headerErr := io.ReadFull(file, header)

	// Check for ID3v2 tag and skip if present.
	if headerErr == nil && string(header[0:3]) == "ID3" {
		tagSize := int(header[6]&id3v2SyncSafeMask)<<id3v2SyncSafeShift21 |
			int(header[7]&id3v2SyncSafeMask)<<id3v2SyncSafeShift14 |
			int(header[8]&id3v2SyncSafeMask)<<id3v2SyncSafeShift7 |
			int(header[9]&id3v2SyncSafeMask)
		s.logger.Info("DIAGNOSTICS: ID3v2 tag detected", "size", tagSize, "action", "skipping")
		if _, seekErr := file.Seek(int64(tagSize), io.SeekCurrent); seekErr != nil {
			s.logger.Info("WARNING: Error skipping ID3 tag", "error", seekErr.Error())
		}
	} else {
		// No ID3v2 tag or error reading header, reset to beginning.
		if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
			s.logger.Info("WARNING: Error seeking to start", "error", seekErr.Error())
		}
		s.logger.Info("DIAGNOSTICS: ID3v2 tag not detected, starting reading from file beginning")
	}

	// Check for ID3v1 tag.
	fileSize := fileInfo.Size()
	hasID3v1, err := s.checkID3v1Tag(file, fileSize)
	if err != nil {
		return err
	}

	// Set effective file size.
	var effectiveFileSize int64
	if hasID3v1 {
		effectiveFileSize = fileSize - id3v1TagSize
		s.logger.Info(
			"ID3v1 tag detected, adjusting file size",
			"original", fileSize,
			"effective", effectiveFileSize,
		)
	}

	// For tracking progress.
	var bytesRead int64
	s.logger.Info("DIAGNOSTICS: Starting to read file", "trackPath", trackPath)

	// Process file in streaming loop.
	err = s.streamAudioLoop(file, trackPath, &bytesRead)
	if err != nil {
		return err
	}

	// Log playback completion.
	duration := time.Since(startTime)
	s.logPlaybackCompletion(trackPath, bytesRead, duration)

	// Add pause between tracks.
	pauseMs := gracePeriodMs
	s.logger.Info("DIAGNOSTICS: Adding pause between tracks", "pauseMs", pauseMs)
	select {
	case <-time.After(time.Duration(pauseMs) * time.Millisecond):
		// Continue processing.
	case <-s.quit:
		s.logger.Info("DIAGNOSTICS: Pause between tracks interrupted", "trackPath", trackPath)
	}

	return nil
}

// processNormalizedAudio handles streaming of normalized audio.
func (s *Streamer) processNormalizedAudio(file *os.File, trackPath string, startTime time.Time) error {
	// Extract route name from track path for metrics.
	route := getRouteFromTrackPath(trackPath)

	// Create pipe for normalized audio.
	pr, pw := io.Pipe()
	defer pr.Close() // Always close the reader when function exits.

	// Start normalization in a separate goroutine.
	go s.runNormalization(file, pw, route)

	// Process audio streaming.
	bytesRead, err := s.streamFromPipe(pr, trackPath)
	if err != nil {
		return err
	}

	// Handle playback completion.
	duration := time.Since(startTime)
	s.logAndRecordPlaybackCompletion(trackPath, bytesRead, duration, route)

	// Add pause between tracks.
	s.addPauseBetweenTracks(trackPath)
	return nil
}

// runNormalization runs audio normalization in a separate goroutine.
func (s *Streamer) runNormalization(file *os.File, writer *io.PipeWriter, route string) {
	defer writer.Close()
	if normErr := NormalizeMP3Stream(file, writer, route); normErr != nil {
		// Проверяем, содержит ли ошибка строку "EOF", которая обычно не является критической.
		if strings.Contains(normErr.Error(), "EOF") {
			// Для EOF-ошибок только логируем без отправки в Sentry.
			s.logger.Info("DIAGNOSTICS: Non-critical normalization error",
				"error", normErr.Error(),
				"route", route)
		} else {
			// Для более серьезных ошибок и логируем, и отправляем в Sentry.
			s.logger.Info("DIAGNOSTICS: ERROR during audio normalization",
				"error", normErr.Error())
			sentry.CaptureException(normErr)
		}
	}
}

// streamFromPipe streams audio data from a pipe.
func (s *Streamer) streamFromPipe(reader *io.PipeReader, trackPath string) (int64, error) {
	// Get buffer from pool.
	buffer, okPipe := s.bufferPool.Get().([]byte)
	if !okPipe {
		return 0, errors.New("failed to get buffer from pool")
	}
	defer s.bufferPool.Put(&buffer)

	var bytesRead int64
	s.logger.Info("DIAGNOSTICS: Starting to read normalized audio data", "trackPath", trackPath)

	for {
		// Check completion signal before reading.
		if s.isPlaybackInterrupted(trackPath) {
			return bytesRead, nil
		}

		// Read from pipe.
		n, readErr := reader.Read(buffer)

		// Handle EOF or error.
		if readErr != nil {
			// Check for EOF.
			if errors.Is(readErr, io.EOF) {
				s.logger.Info("DIAGNOSTICS: End of normalized audio data reached", "trackPath", trackPath)
				break
			}

			// Check for closed pipe.
			if s.isPipeClosed(readErr, trackPath) {
				return bytesRead, nil
			}

			// Handle other errors.
			s.logger.Info("DIAGNOSTICS: ERROR reading normalized audio data", "error", readErr.Error())
			sentry.CaptureException(readErr)
			return bytesRead, fmt.Errorf("error reading normalized audio data: %w", readErr)
		}

		bytesRead += int64(n)

		// Process audio chunk.
		s.processAudioChunk(buffer[:n], bytesRead, trackPath)
	}

	return bytesRead, nil
}

// isPlaybackInterrupted checks if playback should be interrupted.
func (s *Streamer) isPlaybackInterrupted(trackPath string) bool {
	select {
	case <-s.quit:
		s.logger.Info("DIAGNOSTICS: Playback interrupted", "trackPath", trackPath)
		return true
	default:
		return false
	}
}

// isPipeClosed checks if pipe was closed.
func (s *Streamer) isPipeClosed(err error, trackPath string) bool {
	if errors.Is(err, io.ErrClosedPipe) || strings.Contains(err.Error(), "closed pipe") {
		s.logger.Info("DIAGNOSTICS: Pipe was closed during playback",
			"trackPath", trackPath,
			"action", "stopping")
		return true
	}
	return false
}

// processAudioChunk processes a chunk of audio data.
func (s *Streamer) processAudioChunk(data []byte, bytesRead int64, trackPath string) {
	// Save copy of last chunk for new clients.
	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)

	s.lastChunkMutex.Lock()
	s.lastChunk = dataCopy
	s.lastChunkMutex.Unlock()

	// Send data to all clients.
	s.broadcastToClients(data)

	// Calculate delay based on bitrate and buffer size.
	delayMs := s.calculateAndLogDelay(len(data))

	// Wait for delay with completion check.
	s.waitForDelayOrQuit(delayMs, trackPath, bytesRead)
}

// calculateAndLogDelay calculates delay based on bitrate and logs it periodically.
func (s *Streamer) calculateAndLogDelay(dataSize int) int {
	delayMs := (dataSize * bitsPerByte) / s.bitrate

	// Limit minimum delay.
	if delayMs < minPlaybackDelayMs {
		delayMs = minPlaybackDelayMs
	}

	// Log delay information once every 10 seconds.
	if time.Since(s.lastDelayLogTime) > 10*time.Second {
		s.logger.Info("DIAGNOSTICS: Calculated delay",
			"delayMs", delayMs,
			"bytesCount", dataSize,
			"bitrate", s.bitrate)
		s.lastDelayLogTime = time.Now()
	}

	return delayMs
}

// logAndRecordPlaybackCompletion logs playback completion and updates metrics.
func (s *Streamer) logAndRecordPlaybackCompletion(
	trackPath string,
	bytesRead int64,
	duration time.Duration,
	route string,
) {
	s.logger.Info("DIAGNOSTICS: Playback completed",
		"trackPath", trackPath,
		"bytesRead", bytesRead,
		"duration", duration.Seconds())

	// Increment playback time metric for Prometheus.
	if trackSecondsTotal, ok := s.GetTrackSecondsMetric(); ok {
		trackSecondsTotal.WithLabelValues(route).Add(duration.Seconds())
		s.logger.Info("DIAGNOSTICS: trackSecondsTotal metric increased",
			"seconds", duration.Seconds(),
			"route", route)
	}
}

// addPauseBetweenTracks adds a pause between tracks.
func (s *Streamer) addPauseBetweenTracks(trackPath string) {
	pauseMs := gracePeriodMs
	s.logger.Info("DIAGNOSTICS: Adding pause between tracks", "pauseMs", pauseMs)
	select {
	case <-time.After(time.Duration(pauseMs) * time.Millisecond):
		// Continue processing.
	case <-s.quit:
		s.logger.Info("DIAGNOSTICS: Pause between tracks interrupted", "trackPath", trackPath)
	}
}

// getRouteFromTrackPath tries to extract route name from file path.
func getRouteFromTrackPath(trackPath string) string {
	// Extract directory path.
	dir := filepath.Dir(trackPath)

	// Get last component of path, which usually corresponds to route name.
	route := filepath.Base(dir)

	// Add leading slash if it doesn't exist.
	if !strings.HasPrefix(route, "/") {
		route = "/" + route
	}

	return route
}

// AddClient adds a new client and returns a channel for receiving data.
func (s *Streamer) AddClient() (<-chan []byte, int, error) {
	// Check if maximum number of clients has been exceeded.
	if s.maxClients > 0 {
		if s.maxClients > math.MaxInt32 {
			s.logger.Info(
				"ERROR: maxClients exceeds int32 max value, limiting",
				"maxClients", s.maxClients,
				"limit", math.MaxInt32,
			)
			s.maxClients = math.MaxInt32
		}
		if atomic.LoadInt32(&s.clientCounter) >= int32(s.maxClients) {
			err := fmt.Errorf("maximum number of clients exceeded (%d)", s.maxClients)
			sentry.CaptureException(err) // This is an error, send to Sentry
			return nil, 0, err
		}
	}

	s.clientMutex.Lock()
	defer s.clientMutex.Unlock()

	// Increase client counter.
	clientID := int(atomic.AddInt32(&s.clientCounter, 1))

	// Create channel for client with buffer.
	// Buffered channel is needed to prevent blocking.
	// with slow clients.
	clientChannel := make(chan []byte, defaultClientChannelBuffer)
	s.clientChannels[clientID] = clientChannel

	s.logger.Info(
		"Client connected",
		"clientID", clientID,
		"totalClients", int(atomic.LoadInt32(&s.clientCounter)),
	)

	// IMPORTANT: If we have the last data buffer, send it immediately to the new client.
	// so they don't have to wait for the next file read.
	s.lastChunkMutex.RLock()
	if s.lastChunk != nil {
		// Use append to create a new copy for the client - one allocation instead of two.
		dataCopy := append([]byte(nil), s.lastChunk...)

		s.logger.Info(
			"DIAGNOSTICS: Sending last buffer to new client",
			"bytes", len(dataCopy),
			"clientID", clientID,
		)

		// Send data to new client's channel.
		select {
		case clientChannel <- dataCopy:
			s.logger.Info("DIAGNOSTICS: Last buffer successfully sent to client", "clientID", clientID)
		default:
			s.logger.Info("DIAGNOSTICS: Unable to send last buffer to client, channel full", "clientID", clientID)
		}
	} else {
		s.logger.Info("DIAGNOSTICS: No last buffer to send to client", "clientID", clientID)
	}
	s.lastChunkMutex.RUnlock()

	return clientChannel, clientID, nil
}

// RemoveClient removes a client.
func (s *Streamer) RemoveClient(clientID int) {
	s.clientMutex.Lock()
	defer s.clientMutex.Unlock()

	if channel, exists := s.clientChannels[clientID]; exists {
		close(channel)
		delete(s.clientChannels, clientID)
		atomic.AddInt32(&s.clientCounter, -1)
		s.logger.Info(
			"Client disconnected",
			"clientID", clientID,
			"totalClients", int(atomic.LoadInt32(&s.clientCounter)),
		)
	}
}

// GetClientCount returns the current number of clients.
func (s *Streamer) GetClientCount() int {
	return int(atomic.LoadInt32(&s.clientCounter))
}

// broadcastToClients sends data to all connected clients.
func (s *Streamer) broadcastToClients(data []byte) {
	s.clientMutex.RLock()
	defer s.clientMutex.RUnlock()

	// If there are no clients, just return.
	clientCount := len(s.clientChannels)
	if clientCount == 0 {
		return
	}

	// Log client status if needed.
	s.logClientStatusIfNeeded(clientCount, len(data))

	// Get list of all clients.
	clients := s.getClientsList(clientCount)

	// Process clients in batches.
	s.processBatches(clients, data)
}

// logClientStatusIfNeeded logs client status information periodically.
func (s *Streamer) logClientStatusIfNeeded(clientCount, dataSize int) {
	// Output message only when client count changes or not more than once every 10 seconds.
	if clientCount > 0 && (clientCount != s.lastClientCount || time.Since(s.lastLogTime) > 10*time.Second) {
		s.logger.Info(
			"DIAGNOSTICS: Sending data to clients",
			"bytes", dataSize,
			"clients", clientCount,
		)
		s.lastClientCount = clientCount
		s.lastLogTime = time.Now()
	}
}

// getClientsList creates a list of clients for processing.
func (s *Streamer) getClientsList(clientCount int) []struct {
	id int
	ch chan []byte
} {
	clients := make([]struct {
		id int
		ch chan []byte
	}, 0, clientCount)

	for id, ch := range s.clientChannels {
		clients = append(clients, struct {
			id int
			ch chan []byte
		}{id, ch})
	}

	return clients
}

// processBatches processes clients in batches for better performance.
func (s *Streamer) processBatches(clients []struct {
	id int
	ch chan []byte
}, data []byte) {
	clientCount := len(clients)
	var wg sync.WaitGroup

	for i := 0; i < clientCount; i += batchSize {
		end := i + batchSize
		if end > clientCount {
			end = clientCount
		}

		wg.Add(1)
		go s.processBatch(clients[i:end], data, &wg)
	}

	wg.Wait()
}

// processBatch processes a batch of clients.
func (s *Streamer) processBatch(
	batch []struct {
		id int
		ch chan []byte
	},
	data []byte,
	wg *sync.WaitGroup,
) {
	defer wg.Done()

	for _, client := range batch {
		s.sendDataToClient(client.id, client.ch, data)
	}
}

// sendDataToClient sends data to a specific client with retry logic.
func (s *Streamer) sendDataToClient(clientID int, clientCh chan []byte, data []byte) {
	select {
	case clientCh <- data: // Use original buffer, not a copy
		// Data successfully sent.
	case <-time.After(time.Duration(firstTimeoutMs) * time.Millisecond):
		// First timeout, retry.
		s.handleFirstTimeout(clientID, clientCh, data)
	}
}

// handleFirstTimeout handles first timeout when sending data to client.
func (s *Streamer) handleFirstTimeout(clientID int, clientCh chan []byte, data []byte) {
	s.logger.Info("First timeout sending data to client, retrying...", "clientID", clientID)

	select {
	case clientCh <- data:
		s.logger.Info("Resending data to client successful", "clientID", clientID)
	case <-time.After(time.Duration(secondTimeoutMs) * time.Millisecond):
		// Second timeout, disconnect client.
		s.handleSecondTimeout(clientID)
	}
}

// handleSecondTimeout handles second timeout when sending data to client.
func (s *Streamer) handleSecondTimeout(clientID int) {
	s.logger.Info(
		"Second timeout sending data to client, disconnecting...",
		"clientID", clientID,
	)
	s.RemoveClient(clientID)
}

// SetTrackSecondsMetric sets the metric for the streamer.
func (s *Streamer) SetTrackSecondsMetric(metric *prometheus.CounterVec) {
	s.metricMutex.Lock()
	defer s.metricMutex.Unlock()

	s.trackSecondsMetric = metric
	s.logger.Info("DIAGNOSTICS: SetTrackSecondsMetric saved pointer to metric for streamer")
}

// GetTrackSecondsMetric returns the metric if it's set for the streamer.
func (s *Streamer) GetTrackSecondsMetric() (*prometheus.CounterVec, bool) {
	s.metricMutex.RLock()
	defer s.metricMutex.RUnlock()

	if s.trackSecondsMetric == nil {
		return nil, false
	}

	return s.trackSecondsMetric, true
}

// CloseClient closes a client connection.
func (s *Streamer) CloseClient(clientID int) {
	s.clientMutex.Lock()
	defer s.clientMutex.Unlock()

	ch, exists := s.clientChannels[clientID]
	if exists {
		// Try to close channel only if it exists and not previously closed.
		select {
		case <-ch:
			// Channel already closed.
			s.logger.Info(
				"DIAGNOSTICS: Client channel",
				"clientID", clientID,
				"status", "closed when closing streamer",
			)
		default:
			close(ch)
			s.logger.Info("Client closed", "clientID", clientID)
		}
		delete(s.clientChannels, clientID)
	}
}

// streamAudioLoop handles the main stream reading and broadcasting loop.
func (s *Streamer) streamAudioLoop(file io.Reader, trackPath string, bytesRead *int64) error {
	buffer, ok := s.bufferPool.Get().([]byte)
	if !ok {
		return errors.New("failed to get buffer from pool")
	}
	defer s.bufferPool.Put(&buffer)

	for {
		// Check completion signal before reading.
		select {
		case <-s.quit:
			s.logger.Info("DIAGNOSTICS: Playback interrupted", "trackPath", trackPath)
			return nil
		default:
			// Continue execution.
		}

		// Read data.
		n, readErr := file.Read(buffer)
		if errors.Is(readErr, io.EOF) {
			s.logger.Info("DIAGNOSTICS: End of data reached", "trackPath", trackPath)
			break
		}
		if readErr != nil {
			// Special handling for pipe closed.
			if errors.Is(readErr, io.ErrClosedPipe) || strings.Contains(readErr.Error(), "closed pipe") {
				s.logger.Info("DIAGNOSTICS: Pipe was closed during playback",
					"trackPath", trackPath,
					"action", "stopping")
				return nil
			}

			s.logger.Info("DIAGNOSTICS: ERROR reading data", "error", readErr.Error())
			sentry.CaptureException(readErr)
			return fmt.Errorf("error reading data: %w", readErr)
		}

		*bytesRead += int64(n)

		// Save last chunk and broadcast to clients.
		s.saveAndBroadcastChunk(buffer[:n])

		// Handle delay.
		delayMs := s.calculateDelay(n)
		s.waitForDelayOrQuit(delayMs, trackPath, *bytesRead)
	}

	return nil
}

// saveAndBroadcastChunk saves a copy of the last chunk and broadcasts to clients.
func (s *Streamer) saveAndBroadcastChunk(data []byte) {
	// Save copy of last chunk for new clients.
	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)

	s.lastChunkMutex.Lock()
	s.lastChunk = dataCopy
	s.lastChunkMutex.Unlock()

	// Send data to all clients.
	s.broadcastToClients(data)
}

// calculateDelay calculates delay based on bitrate and data size.
func (s *Streamer) calculateDelay(dataSize int) int {
	delayMs := (dataSize * bitsPerByte) / s.bitrate

	// Limit minimum delay.
	if delayMs < minPlaybackDelayMs {
		delayMs = minPlaybackDelayMs
	}

	// Log delay information once every 10 seconds.
	if time.Since(s.lastDelayLogTime) > 10*time.Second {
		s.logger.Info("DIAGNOSTICS: Calculated delay",
			"delayMs", delayMs,
			"bytesCount", dataSize,
			"bitrate", s.bitrate)
		s.lastDelayLogTime = time.Now()
	}

	return delayMs
}

// waitForDelayOrQuit waits for calculated delay or quit signal.
func (s *Streamer) waitForDelayOrQuit(delayMs int, trackPath string, bytesRead int64) {
	if delayMs <= 0 {
		return
	}

	select {
	case <-s.quit:
		s.logger.Info(
			"Audio streaming interrupted during delay",
			"trackPath", trackPath,
			"bytesRead", bytesRead,
		)
		return
	case <-time.After(time.Duration(delayMs) * time.Millisecond):
		// Продолжаем после задержки.
		return
	}
}

// checkID3v1Tag checks for ID3v1 tag at the end of a file.
func (s *Streamer) checkID3v1Tag(file *os.File, fileSize int64) (bool, error) {
	if fileSize <= id3v1TagSize {
		return false, nil
	}

	// Save current position.
	currentPos, posErr := file.Seek(0, io.SeekCurrent)
	if posErr != nil {
		s.logger.Info("WARNING: Error getting current file position", "error", posErr.Error())
		return false, posErr
	}

	// Defer restoring position.
	defer func() {
		if _, seekErr := file.Seek(currentPos, io.SeekStart); seekErr != nil {
			s.logger.Info("WARNING: Error seeking back to position", "error", seekErr.Error())
		}
	}()

	// Check end of file for ID3v1 tag.
	if _, seekErr := file.Seek(fileSize-id3v1TagSize, io.SeekStart); seekErr != nil {
		s.logger.Info("WARNING: Error seeking to end for ID3v1", "error", seekErr.Error())
		return false, seekErr
	}

	// Read tag identifier.
	tagCheck := make([]byte, tagCheckSize)
	if _, readErr := io.ReadFull(file, tagCheck); readErr != nil {
		s.logger.Info("WARNING: Error reading ID3v1 tag", "error", readErr.Error())
		return false, readErr
	}

	hasID3v1 := string(tagCheck) == "TAG"
	if hasID3v1 {
		s.logger.Info("DIAGNOSTICS: ID3v1 tag detected at end of file", "ignoredBytes", id3v1TagSize)
	}

	return hasID3v1, nil
}

// logPlaybackCompletion logs completion of playback and updates metrics.
func (s *Streamer) logPlaybackCompletion(trackPath string, bytesRead int64, duration time.Duration) {
	s.logger.Info("DIAGNOSTICS: Playback completed",
		"trackPath", trackPath,
		"bytesRead", bytesRead,
		"duration", duration.Seconds())

	// Increment playback time metric for Prometheus.
	if trackSecondsTotal, ok := s.GetTrackSecondsMetric(); ok {
		routeName := getRouteFromTrackPath(trackPath)
		trackSecondsTotal.WithLabelValues(routeName).Add(duration.Seconds())
		s.logger.Info("DIAGNOSTICS: trackSecondsTotal metric increased",
			"seconds", duration.Seconds(),
			"route", routeName)
	}
}
