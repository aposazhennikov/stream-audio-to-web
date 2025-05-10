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

	"github.com/getsentry/sentry-go"
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
	bitsPerByte                = 8   // Количество бит в байте
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
	bufferPool      *sync.Pool
	bufferSize      int
	clientCounter   int32
	maxClients      int
	quit            chan struct{}
	currentTrackCh  chan string
	clientChannels  map[int]chan []byte
	clientMutex     sync.RWMutex
	transcodeFormat string
	bitrate         int
	lastChunk       []byte       // Last sent chunk of audio data.
	lastChunkMutex  sync.RWMutex // Mutex for protecting lastChunk.
	normalizeVolume bool         // Flag to enable/disable volume normalization.
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
		bufferSize:      bufferSize,
		maxClients:      maxClients,
		quit:            make(chan struct{}),
		currentTrackCh:  make(chan string, 1),
		clientChannels:  make(map[int]chan []byte),
		clientMutex:     sync.RWMutex{},
		transcodeFormat: transcodeFormat,
		bitrate:         bitrate,
		lastChunk:       nil,
		lastChunkMutex:  sync.RWMutex{},
		normalizeVolume: true, // Enable volume normalization by default.
	}
}

// SetVolumeNormalization enables or disables volume normalization.
func (s *Streamer) SetVolumeNormalization(enabled bool) {
	slog.Info("DIAGNOSTICS: Volume normalization", slog.Bool("enabled", enabled))
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
		slog.Info("DIAGNOSTICS: Client channel", slog.Int("clientID", clientID), "status", "closed when closing streamer")
	}

	// Clear the channel map.
	s.clientChannels = make(map[int]chan []byte)

	slog.Info("DIAGNOSTICS: Streamer completely closed")
}

// StopCurrentTrack immediately stops the playback of the current track.
// Used when manually switching tracks.
func (s *Streamer) StopCurrentTrack() {
	// First signal the need to stop.
	// Safe check for already closed channel.
	select {
	case <-s.quit:
		// Channel already closed, create a new one.
		slog.Info("DIAGNOSTICS: Quit channel already closed, skipping closure")
	default:
		// Channel not yet closed, close it.
		close(s.quit)
	}

	// Clear the last buffer - this is critically important to prevent
	// sending data from the old track to new clients.
	s.lastChunkMutex.Lock()
	s.lastChunk = nil
	s.lastChunkMutex.Unlock()

	// DO NOT close client channels to maintain connections.
	// s.clientMutex.Lock()
	// instead of closing channels just log.
	slog.Info("DIAGNOSTICS: Stopping current track without closing client connections")
	// s.clientMutex.Unlock()

	// Create new channel for next track.
	s.quit = make(chan struct{})

	slog.Info("DIAGNOSTICS: Current track stopped, buffer cleared")
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

	slog.Info("DIAGNOSTICS: Attempting to play file: %s", trackPath)

	// Check if file exists.
	fileInfo, statErr := os.Stat(trackPath)
	if statErr != nil {
		slog.Info("DIAGNOSTICS: ERROR checking file %s: %v", trackPath, statErr)
		sentryErr := errors.New("error checking file")
		sentry.CaptureException(sentryErr)
		return sentryErr
	}

	slog.Info("DIAGNOSTICS: File %s exists, size: %d bytes, access rights: %v",
		trackPath, fileInfo.Size(), fileInfo.Mode())

	// Check that it's not a directory.
	if fileInfo.IsDir() {
		sentryErr := fmt.Errorf("specified path %s is a directory, not a file", trackPath)
		sentry.CaptureException(sentryErr)
		return sentryErr
	}

	// Open the file.
	slog.Info("DIAGNOSTICS: Attempting to open file: %s", trackPath)
	file, openErr := os.Open(trackPath)
	if openErr != nil {
		slog.Info("DIAGNOSTICS: ERROR opening file %s: %v", trackPath, openErr)
		sentryErr := errors.New("error opening file")
		sentry.CaptureException(sentryErr)
		return sentryErr
	}
	defer file.Close()

	// Send information about current track to channel.
	select {
	case s.currentTrackCh <- filepath.Base(trackPath):
		slog.Info("DIAGNOSTICS: Current track information updated: %s", filepath.Base(trackPath))
	default:
		slog.Info("DIAGNOSTICS: Failed to update current track information: channel full")
	}

	startTime := time.Now()

	// Create pipe for normalized audio.
	pr, pw := io.Pipe()
	defer pr.Close() // Always close the reader when function exits.

	// Check if normalization should be used.
	if !s.normalizeVolume {
		// Non-normalized mode (raw streaming).
		// Skip ID3v2 if it exists.
		header := make([]byte, id3v2HeaderSize)
		_, headerErr := io.ReadFull(file, header)
		hasID3v2 := headerErr == nil && string(header[0:3]) == "ID3"
		if hasID3v2 {
			tagSize := int(header[6]&id3v2SyncSafeMask)<<id3v2SyncSafeShift21 |
				int(header[7]&id3v2SyncSafeMask)<<id3v2SyncSafeShift14 |
				int(header[8]&id3v2SyncSafeMask)<<id3v2SyncSafeShift7 |
				int(header[9]&id3v2SyncSafeMask)
			slog.Info("DIAGNOSTICS: ID3v2 tag detected with size %d bytes, skipping", tagSize)
			if _, seekErr := file.Seek(int64(tagSize), io.SeekCurrent); seekErr != nil {
				slog.Info("WARNING: Error skipping ID3 tag: %v", seekErr)
			}
		} else {
			if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
				slog.Info("WARNING: Error seeking to start: %v", seekErr)
			}
			slog.Info("DIAGNOSTICS: ID3v2 tag not detected, starting reading from file beginning")
		}

		// Check for ID3v1 tag (128 bytes at end of file).
		fileSize := fileInfo.Size()
		hasID3v1 := false
		if fileSize > id3v1TagSize {
			currentPos, posErr := file.Seek(0, io.SeekCurrent)
			if posErr != nil {
				slog.Info("WARNING: Error getting current file position: %v", posErr)
			} else {
				seekEndOk := false
				if _, seekErr := file.Seek(fileSize-id3v1TagSize, io.SeekStart); seekErr != nil {
					slog.Info("WARNING: Error seeking to end for ID3v1: %v", seekErr)
				} else {
					tagCheck := make([]byte, tagCheckSize)
					if _, readErr := io.ReadFull(file, tagCheck); readErr != nil {
						slog.Info("WARNING: Error reading ID3v1 tag: %v", readErr)
					} else if string(tagCheck) == "TAG" {
						hasID3v1 = true
						slog.Info("DIAGNOSTICS: ID3v1 tag detected at end of file, will ignore last %d bytes", id3v1TagSize)
					}
					seekEndOk = true
				}
				if seekEndOk {
					if _, seekErr := file.Seek(currentPos, io.SeekStart); seekErr != nil {
						slog.Info("WARNING: Error seeking back to position: %v", seekErr)
					}
				}
			}
		}

		// Set effective file size (without ID3v1 tag).
		effectiveFileSize := fileSize
		if hasID3v1 {
			effectiveFileSize -= id3v1TagSize
		}

		// Initialize buffer and counter.
		buffer, okBuf := s.bufferPool.Get().([]byte)
		if !okBuf {
			return fmt.Errorf("failed to get buffer from pool")
		}
		defer s.bufferPool.Put(&buffer)

		// For tracking progress.
		var bytesRead int64

		slog.Info("DIAGNOSTICS: Starting to read file %s", trackPath)

		for {
			// Check completion signal before reading.
			select {
			case <-s.quit:
				slog.Info("DIAGNOSTICS: Playback of file %s interrupted (read %d bytes out of %d)",
					trackPath, bytesRead, effectiveFileSize)
				return nil
			default:
				// Continue execution.
			}

			n, readErr := file.Read(buffer)
			if errors.Is(readErr, io.EOF) {
				slog.Info("DIAGNOSTICS: End of file %s reached", trackPath)
				break
			}
			if readErr != nil {
				slog.Info("DIAGNOSTICS: ERROR reading file %s: %v", trackPath, readErr)
				sentryErr := fmt.Errorf("error reading file: %w", readErr)
				sentry.CaptureException(sentryErr)
				return sentryErr
			}

			bytesRead += int64(n)

			// Save copy of last chunk for new clients.
			dataCopy := make([]byte, n)
			copy(dataCopy, buffer[:n])

			// Save copy of last chunk for new clients.
			s.lastChunkMutex.Lock()
			s.lastChunk = dataCopy
			s.lastChunkMutex.Unlock()

			// Send data to all clients.
			if broadcastErr := s.broadcastToClients(buffer[:n]); broadcastErr != nil {
				slog.Info("DIAGNOSTICS: ERROR sending data to clients: %v", broadcastErr)
				sentry.CaptureException(broadcastErr)
				return broadcastErr
			}

			// Calculate delay based on bitrate and buffer size.
			delayMs := (n * bitsPerByte) / s.bitrate

			// Limit minimum delay.
			if delayMs < minPlaybackDelayMs {
				delayMs = minPlaybackDelayMs
			}

			// Log delay information once every 10 seconds.
			if time.Since(lastDelayLogTime) > 10*time.Second {
				slog.Info("DIAGNOSTICS: Calculated delay: %d ms for %d bytes of data at bitrate %d kbit/s",
					delayMs, n, s.bitrate)
				lastDelayLogTime = time.Now()
			}

			// Wait for delay with completion check.
			select {
			case <-time.After(time.Duration(delayMs) * time.Millisecond):
				// Continue processing.
			case <-s.quit:
				slog.Info("DIAGNOSTICS: Playback of file %s interrupted during delay (read %d bytes out of %d)",
					trackPath, bytesRead, effectiveFileSize)
				return nil
			}
		}

		duration := time.Since(startTime)
		slog.Info("DIAGNOSTICS: Playback of file %s completed (read %d bytes in %.2f sec)",
			trackPath, bytesRead, duration.Seconds())

		// Increment playback time metric for Prometheus.
		if trackSecondsTotal, ok := GetTrackSecondsMetric(); ok {
			routeName := getRouteFromTrackPath(trackPath)
			trackSecondsTotal.WithLabelValues(routeName).Add(duration.Seconds())
			slog.Info("DIAGNOSTICS: trackSecondsTotal metric increased by %.2f sec for %s",
				duration.Seconds(), routeName)
		}

		// Add pause between tracks.
		pauseMs := gracePeriodMs
		slog.Info("DIAGNOSTICS: Adding pause of %d ms between tracks", pauseMs)
		select {
		case <-time.After(time.Duration(pauseMs) * time.Millisecond):
			// Continue processing.
		case <-s.quit:
			slog.Info("DIAGNOSTICS: Pause between tracks interrupted for %s", trackPath)
			return nil
		}

		return nil
	}

	// --- Нормализация ---
	// Extract route name from track path for metrics.
	route := getRouteFromTrackPath(trackPath)
	// Start normalization в отдельной горутине.
	go func() {
		defer pw.Close()
		if normErr := NormalizeMP3Stream(file, pw, route); normErr != nil {
			slog.Info("DIAGNOSTICS: ERROR during audio normalization: %v", normErr)
			sentry.CaptureException(normErr)
		}
	}()

	// Stream from pipe.
	buffer, okPipe := s.bufferPool.Get().([]byte)
	if !okPipe {
		return fmt.Errorf("failed to get buffer from pool")
	}
	defer s.bufferPool.Put(&buffer)

	// For tracking progress.
	var bytesRead int64

	slog.Info("DIAGNOSTICS: Starting to read normalized audio data from %s", trackPath)

	for {
		// Check completion signal before reading.
		select {
		case <-s.quit:
			slog.Info("DIAGNOSTICS: Playback of %s interrupted", trackPath)
			return nil
		default:
			// Continue execution.
		}

		// Read from pipe.
		n, readErr := pr.Read(buffer)
		if errors.Is(readErr, io.EOF) {
			slog.Info("DIAGNOSTICS: End of normalized audio data reached for %s", trackPath)
			break
		}
		if readErr != nil {
			// If pipe was closed, this might be the result of normal termination.
			if errors.Is(readErr, io.ErrClosedPipe) || strings.Contains(readErr.Error(), "closed pipe") {
				slog.Info("DIAGNOSTICS: Pipe was closed during playback of %s, stopping", trackPath)
				return nil
			}

			slog.Info("DIAGNOSTICS: ERROR reading normalized audio data: %v", readErr)
			sentry.CaptureException(readErr)
			return fmt.Errorf("error reading normalized audio data: %w", readErr)
		}

		bytesRead += int64(n)

		// Save copy of last chunk for new clients.
		dataCopy := make([]byte, n)
		copy(dataCopy, buffer[:n])

		// Save copy of last chunk for new clients.
		s.lastChunkMutex.Lock()
		s.lastChunk = dataCopy
		s.lastChunkMutex.Unlock()

		// Send data to all clients.
		if broadcastErr := s.broadcastToClients(buffer[:n]); broadcastErr != nil {
			slog.Info("DIAGNOSTICS: ERROR sending data to clients: %v", broadcastErr)
			sentry.CaptureException(broadcastErr)
			return broadcastErr
		}

		// Calculate delay based on bitrate and buffer size.
		delayMs := (n * bitsPerByte) / s.bitrate

		// Limit minimum delay.
		if delayMs < minPlaybackDelayMs {
			delayMs = minPlaybackDelayMs
		}

		// Log delay information once every 10 seconds.
		if time.Since(lastDelayLogTime) > 10*time.Second {
			slog.Info("DIAGNOSTICS: Calculated delay: %d ms for %d bytes of data at bitrate %d kbit/s",
				delayMs, n, s.bitrate)
			lastDelayLogTime = time.Now()
		}

		// Wait for delay with completion check.
		select {
		case <-time.After(time.Duration(delayMs) * time.Millisecond):
			// Continue processing.
		case <-s.quit:
			slog.Info("DIAGNOSTICS: Playback of %s interrupted during delay", trackPath)
			return nil
		}
	}

	duration := time.Since(startTime)
	slog.Info("DIAGNOSTICS: Playback of %s completed (read %d bytes in %.2f sec)",
		trackPath, bytesRead, duration.Seconds())

	// Increment playback time metric for Prometheus
	if trackSecondsTotal, ok := GetTrackSecondsMetric(); ok {
		routeName := getRouteFromTrackPath(trackPath)
		trackSecondsTotal.WithLabelValues(routeName).Add(duration.Seconds())
		slog.Info("DIAGNOSTICS: trackSecondsTotal metric increased by %.2f sec for %s",
			duration.Seconds(), routeName)
	}

	// Add pause between tracks
	pauseMs := gracePeriodMs
	slog.Info("DIAGNOSTICS: Adding pause of %d ms between tracks", pauseMs)
	select {
	case <-time.After(time.Duration(pauseMs) * time.Millisecond):
		// Continue processing
	case <-s.quit:
		slog.Info("DIAGNOSTICS: Pause between tracks interrupted for %s", trackPath)
		return nil
	}

	return nil
}

// getRouteFromTrackPath tries to extract route name from file path.
func getRouteFromTrackPath(trackPath string) string {
	// Extract directory path
	dir := filepath.Dir(trackPath)

	// Get last component of path, which usually corresponds to route name
	route := filepath.Base(dir)

	// Add leading slash if it doesn't exist
	if !strings.HasPrefix(route, "/") {
		route = "/" + route
	}

	return route
}

// For tracking logs when sending data.
var (
	lastClientCount  int
	lastLogTime      time.Time
	lastDelayLogTime time.Time
)

// AddClient adds a new client and returns a channel for receiving data.
func (s *Streamer) AddClient() (<-chan []byte, int, error) {
	// Check if maximum number of clients has been exceeded
	if s.maxClients > 0 {
		if s.maxClients > math.MaxInt32 {
			slog.Info("ERROR: maxClients (%d) exceeds int32 max value, limiting to %d", s.maxClients, math.MaxInt32)
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

	// Increase client counter
	clientID := int(atomic.AddInt32(&s.clientCounter, 1))

	// Create channel for client with buffer
	// Buffered channel is needed to prevent blocking
	// with slow clients
	clientChannel := make(chan []byte, defaultClientChannelBuffer)
	s.clientChannels[clientID] = clientChannel

	slog.Info("Client %d connected. Total clients: %d", clientID, atomic.LoadInt32(&s.clientCounter))

	// IMPORTANT: If we have the last data buffer, send it immediately to the new client
	// so they don't have to wait for the next file read
	s.lastChunkMutex.RLock()
	if s.lastChunk != nil {
		// Use append to create a new copy for the client - one allocation instead of two
		dataCopy := append([]byte(nil), s.lastChunk...)

		slog.Info("DIAGNOSTICS: Sending last buffer (%d bytes) to new client %d", len(dataCopy), clientID)

		// Send data to new client's channel
		select {
		case clientChannel <- dataCopy:
			slog.Info("DIAGNOSTICS: Last buffer successfully sent to client %d", clientID)
		default:
			slog.Info("DIAGNOSTICS: Unable to send last buffer to client %d, channel full", clientID)
		}
	} else {
		slog.Info("DIAGNOSTICS: No last buffer to send to client %d", clientID)
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
		slog.Info("Client %d disconnected. Total clients: %d", clientID, atomic.LoadInt32(&s.clientCounter))
	}
}

// GetClientCount returns the current number of clients.
func (s *Streamer) GetClientCount() int {
	return int(atomic.LoadInt32(&s.clientCounter))
}

// broadcastToClients sends data to all connected clients.
func (s *Streamer) broadcastToClients(data []byte) error {
	s.clientMutex.RLock()
	defer s.clientMutex.RUnlock()

	// If there are no clients, just return
	clientCount := len(s.clientChannels)
	if clientCount == 0 {
		return nil
	}

	// Reduce number of diagnostic messages - only output when client count changes
	// or once every 10 seconds
	if clientCount > 0 {
		// Output message only when client count changes
		// or not more than once every 10 seconds
		if clientCount != lastClientCount || time.Since(lastLogTime) > 10*time.Second {
			slog.Info("DIAGNOSTICS: Sending %d bytes of data to %d clients", len(data), clientCount)
			lastClientCount = clientCount
			lastLogTime = time.Now()
		}
	}

	// If there are many clients, use batch approach
	// Create list of all clients
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

	// Process clients in batches
	var wg sync.WaitGroup
	for i := 0; i < clientCount; i += batchSize {
		end := i + batchSize
		if end > clientCount {
			end = clientCount
		}

		wg.Add(1)
		go func(batch []struct {
			id int
			ch chan []byte
		}) {
			defer wg.Done()

			for _, client := range batch {
				select {
				case client.ch <- data: // Use original buffer, not a copy
					// Data successfully sent
				case <-time.After(time.Duration(firstTimeoutMs) * time.Millisecond):
					// Timeout sending data - try one more time
					slog.Info("First timeout sending data to client %d, retrying...", client.id)

					select {
					case client.ch <- data:
						slog.Info("Resending data to client %d successful", client.id)
					case <-time.After(time.Duration(secondTimeoutMs) * time.Millisecond):
						// Second timeout, now disconnect client
						slog.Info("Second timeout sending data to client %d, disconnecting...", client.id)
						s.RemoveClient(client.id)
					}
				}
			}
		}(clients[i:end])
	}

	wg.Wait()
	return nil
}

// In-place min function.
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// For exchanging metrics between packages

var (
	trackSecondsMetric *prometheus.CounterVec
	metricMutex        sync.RWMutex
)

// SetTrackSecondsMetric sets the metric from outside.
func SetTrackSecondsMetric(metric *prometheus.CounterVec) {
	metricMutex.Lock()
	defer metricMutex.Unlock()

	trackSecondsMetric = metric
	slog.Info("DIAGNOSTICS: SetTrackSecondsMetric saved pointer to metric")
}

// GetTrackSecondsMetric returns the metric if it's set.
func GetTrackSecondsMetric() (*prometheus.CounterVec, bool) {
	metricMutex.RLock()
	defer metricMutex.RUnlock()

	if trackSecondsMetric == nil {
		return nil, false
	}

	return trackSecondsMetric, true
}
