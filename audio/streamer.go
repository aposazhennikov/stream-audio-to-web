package audio

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
	"golang.org/x/sync/errgroup"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	defaultBufferSize = 16384 // 16KB (reduced from 32KB to minimize gap when switching tracks)
	gracePeriodMs     = 50    // 50ms buffer between tracks (reduced from 100ms)
	// Minimum delay between buffer sends
	minPlaybackDelayMs = 10   // 10ms (reduced from 20ms)
)

// Streamer manages audio streaming for a single "radio" stream
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
	lastChunk       []byte       // Last sent chunk of audio data
	lastChunkMutex  sync.RWMutex // Mutex for protecting lastChunk
}

// NewStreamer creates a new audio streamer
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
	}
}

// Close closes the streamer and all client connections
func (s *Streamer) Close() {
	// Signal closing
	close(s.quit)
	
	// Clear the last buffer
	s.lastChunkMutex.Lock()
	s.lastChunk = nil
	s.lastChunkMutex.Unlock()
	
	// Close all client channels
	s.clientMutex.Lock()
	defer s.clientMutex.Unlock()

	for clientID, ch := range s.clientChannels {
		close(ch)
		log.Printf("DIAGNOSTICS: Client channel %d closed when closing streamer", clientID)
	}
	
	// Clear the channel map
	s.clientChannels = make(map[int]chan []byte)
	
	log.Printf("DIAGNOSTICS: Streamer completely closed")
}

// StopCurrentTrack immediately stops the playback of the current track
// Used when manually switching tracks
func (s *Streamer) StopCurrentTrack() {
	// First signal the need to stop
	// Safe check for already closed channel
	select {
	case <-s.quit:
		// Channel already closed, create a new one
		log.Printf("DIAGNOSTICS: Quit channel already closed, skipping closure")
	default:
		// Channel not yet closed, close it
		close(s.quit)
	}
	
	// Clear the last buffer - this is critically important to prevent
	// sending data from the old track to new clients
	s.lastChunkMutex.Lock()
	s.lastChunk = nil
	s.lastChunkMutex.Unlock()
	
	// DO NOT close client channels to maintain connections
	// s.clientMutex.Lock()
	// instead of closing channels just log
	log.Printf("DIAGNOSTICS: Stopping current track without closing client connections")
	// s.clientMutex.Unlock()
	
	// Create new channel for next track
	s.quit = make(chan struct{})
	
	log.Printf("DIAGNOSTICS: Current track stopped, buffer cleared")
}

// GetCurrentTrackChannel returns a channel with information about the current track
func (s *Streamer) GetCurrentTrackChannel() <-chan string {
	return s.currentTrackCh
}

// StreamTrack streams a track to all connected clients
func (s *Streamer) StreamTrack(trackPath string) error {
	// Check for empty path
	if trackPath == "" {
		sentryErr := fmt.Errorf("empty audio file path")
		sentry.CaptureException(sentryErr) // This is an error, send to Sentry
		return sentryErr
	}

	log.Printf("DIAGNOSTICS: Attempting to play file: %s", trackPath)
	
	// Check if file exists
	fileInfo, err := os.Stat(trackPath)
	if err != nil {
		log.Printf("DIAGNOSTICS: ERROR checking file %s: %v", trackPath, err)
		sentryErr := fmt.Errorf("error checking file: %w", err)
		sentry.CaptureException(sentryErr) // This is an error, send to Sentry
		return sentryErr
	}
	
	log.Printf("DIAGNOSTICS: File %s exists, size: %d bytes, access rights: %v", 
		trackPath, fileInfo.Size(), fileInfo.Mode())
	
	// Check that it's not a directory
	if fileInfo.IsDir() {
		sentryErr := fmt.Errorf("specified path %s is a directory, not a file", trackPath)
		sentry.CaptureException(sentryErr) // This is an error, send to Sentry
		return sentryErr
	}
	
	// Open the file
	log.Printf("DIAGNOSTICS: Attempting to open file: %s", trackPath)
	file, err := os.Open(trackPath)
	if err != nil {
		log.Printf("DIAGNOSTICS: ERROR opening file %s: %v", trackPath, err)
		sentryErr := fmt.Errorf("error opening file: %w", err)
		sentry.CaptureException(sentryErr) // This is an error, send to Sentry
		return sentryErr
	}
	defer file.Close()
	
	// skip ID3v2 if it exists
	header := make([]byte, 10)
	if _, err := io.ReadFull(file, header); err == nil && string(header[0:3]) == "ID3" {
		// size is stored in 4 sync-safe bytes (6..9)
		tagSize := int(header[6]&0x7F)<<21 | int(header[7]&0x7F)<<14 | int(header[8]&0x7F)<<7 | int(header[9]&0x7F)
		log.Printf("DIAGNOSTICS: ID3v2 tag detected with size %d bytes, skipping", tagSize)
		if _, err := file.Seek(int64(tagSize), io.SeekCurrent); err != nil {
			log.Printf("WARNING: Error skipping ID3 tag: %v", err)
		}
	} else {
		// no tag - rewind to beginning
		file.Seek(0, io.SeekStart)
		log.Printf("DIAGNOSTICS: ID3v2 tag not detected, starting reading from file beginning")
	}
	
	// Check for ID3v1 tag (128 bytes at end of file)
	fileSize := fileInfo.Size()
	hasID3v1 := false
	if fileSize > 128 {
		// Save current position
		currentPos, err := file.Seek(0, io.SeekCurrent)
		if err == nil {
			// Move to end of file minus 128 bytes
			_, err = file.Seek(fileSize-128, io.SeekStart)
			if err == nil {
				// Read 3 bytes to check tag
				tagCheck := make([]byte, 3)
				if _, err := io.ReadFull(file, tagCheck); err == nil && string(tagCheck) == "TAG" {
					hasID3v1 = true
					log.Printf("DIAGNOSTICS: ID3v1 tag detected at end of file, will ignore last 128 bytes")
				}
			}
			// Return pointer to previous position
			file.Seek(currentPos, io.SeekStart)
		}
	}
	
	// Set effective file size (without ID3v1 tag)
	effectiveFileSize := fileSize
	if hasID3v1 {
		effectiveFileSize -= 128
	}
	
	// Initialize buffer and counter
	buffer := s.bufferPool.Get().([]byte)
	defer s.bufferPool.Put(buffer)
	
	// For tracking progress
	var bytesRead int64
	
	// Send information about current track to channel
	select {
	case s.currentTrackCh <- filepath.Base(trackPath):
		log.Printf("DIAGNOSTICS: Current track information updated: %s", filepath.Base(trackPath))
	default:
		log.Printf("DIAGNOSTICS: Failed to update current track information: channel full")
	}
	
	startTime := time.Now()
	log.Printf("DIAGNOSTICS: Starting to read file %s", trackPath)
	
	for {
		// IMPORTANT: Check completion signal before reading and sending data
		select {
		case <-s.quit:
			log.Printf("DIAGNOSTICS: Playback of file %s interrupted (read %d bytes out of %d)", 
				trackPath, bytesRead, effectiveFileSize)
			return nil
		default:
			// Continue execution
		}
		
		n, err := file.Read(buffer)
		if err == io.EOF {
			log.Printf("DIAGNOSTICS: End of file %s reached", trackPath)
			break
		}
		if err != nil {
			log.Printf("DIAGNOSTICS: ERROR reading file %s: %v", trackPath, err)
			sentryErr := fmt.Errorf("error reading file: %w", err)
			sentry.CaptureException(sentryErr) // This is an error, send to Sentry
			return sentryErr
		}

		bytesRead += int64(n)
		
		// Save copy of last chunk for new clients
		dataCopy := make([]byte, n)
		copy(dataCopy, buffer[:n])
		
		// Save copy of last chunk for new clients
		s.lastChunkMutex.Lock()
		s.lastChunk = dataCopy
		s.lastChunkMutex.Unlock()
		
		// Send data to all clients
		if err := s.broadcastToClients(buffer[:n]); err != nil {
			log.Printf("DIAGNOSTICS: ERROR sending data to clients: %v", err)
			sentry.CaptureException(err) // This is an error, send to Sentry
			return err
		}

		// Calculate delay based on bitrate and buffer size
		// Formula: (buffer size in bytes * 8) / (bitrate in kbit/s * 1000) * 1000 ms
		// Simplify: (buffer size in bytes * 8 * 1000) / (bitrate in kbit/s * 1000)
		// Final formula: (buffer size in bytes * 8) / bitrate in kbit/s
		delayMs := (n * 8) / s.bitrate
		
		// Limit minimum delay
		if delayMs < minPlaybackDelayMs {
			delayMs = minPlaybackDelayMs
		}
		
		// Log delay information once every 10 seconds
		if time.Since(lastDelayLogTime) > 10*time.Second {
			log.Printf("DIAGNOSTICS: Calculated delay: %d ms for %d bytes of data at bitrate %d kbit/s", 
				delayMs, n, s.bitrate)
			lastDelayLogTime = time.Now()
		}
		
		// Use time.After to check completion signal during wait
		select {
		case <-time.After(time.Duration(delayMs) * time.Millisecond):
			// Continue processing
		case <-s.quit:
			log.Printf("DIAGNOSTICS: Playback of file %s interrupted during delay (read %d bytes out of %d)", 
				trackPath, bytesRead, effectiveFileSize)
			return nil
		}
	}
	
	duration := time.Since(startTime)
	log.Printf("DIAGNOSTICS: Playback of file %s completed (read %d bytes in %.2f sec)", 
		trackPath, bytesRead, duration.Seconds())

	// Increment playback time metric for Prometheus
	// Metric should be defined in http/server.go
	if trackSecondsTotal, ok := GetTrackSecondsMetric(); ok {
		routeName := getRouteFromTrackPath(trackPath)
		trackSecondsTotal.WithLabelValues(routeName).Add(duration.Seconds())
		log.Printf("DIAGNOSTICS: trackSecondsTotal metric increased by %.2f sec for %s", 
			duration.Seconds(), routeName)
	}

	// Add pause between tracks to avoid cutting the beginning - reduced from 200 to 100 ms
	log.Printf("DIAGNOSTICS: Adding pause of %d ms between tracks", gracePeriodMs)
	time.Sleep(gracePeriodMs * time.Millisecond)

	return nil
}

// getRouteFromTrackPath tries to extract route name from file path
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

// For exchanging metrics between packages

var (
	trackSecondsMetric *prometheus.CounterVec
	metricMutex        sync.RWMutex
)

// SetTrackSecondsMetric sets the metric from outside
func SetTrackSecondsMetric(metric *prometheus.CounterVec) {
	metricMutex.Lock()
	defer metricMutex.Unlock()
	
	trackSecondsMetric = metric
	log.Printf("DIAGNOSTICS: SetTrackSecondsMetric saved pointer to metric")
}

// GetTrackSecondsMetric returns the metric if it's set
func GetTrackSecondsMetric() (*prometheus.CounterVec, bool) {
	metricMutex.RLock()
	defer metricMutex.RUnlock()
	
	if trackSecondsMetric == nil {
		return nil, false
	}
	
	return trackSecondsMetric, true
}

// For tracking logs when sending data
var (
	lastClientCount int
	lastLogTime     time.Time
	lastDelayLogTime time.Time
)

// AddClient adds a new client and returns a channel for receiving data
func (s *Streamer) AddClient() (<-chan []byte, int, error) {
	// Check if maximum number of clients has been exceeded
	if s.maxClients > 0 && atomic.LoadInt32(&s.clientCounter) >= int32(s.maxClients) {
		err := fmt.Errorf("maximum number of clients exceeded (%d)", s.maxClients)
		sentry.CaptureException(err) // This is an error, send to Sentry
		return nil, 0, err
	}

	s.clientMutex.Lock()
	defer s.clientMutex.Unlock()

	// Increase client counter
	clientID := int(atomic.AddInt32(&s.clientCounter, 1))
	
	// Create channel for client with buffer
	// Buffered channel is needed to prevent blocking
	// with slow clients
	clientChannel := make(chan []byte, 32) // Buffer size increased from 10 to 32
	s.clientChannels[clientID] = clientChannel

	log.Printf("Client %d connected. Total clients: %d", clientID, atomic.LoadInt32(&s.clientCounter))

	// IMPORTANT: If we have the last data buffer, send it immediately to the new client
	// so they don't have to wait for the next file read
	s.lastChunkMutex.RLock()
	if s.lastChunk != nil {
		// Use append to create a new copy for the client - one allocation instead of two
		dataCopy := append([]byte(nil), s.lastChunk...)
		
		log.Printf("DIAGNOSTICS: Sending last buffer (%d bytes) to new client %d", len(dataCopy), clientID)
		
		// Send data to new client's channel
		select {
		case clientChannel <- dataCopy:
			log.Printf("DIAGNOSTICS: Last buffer successfully sent to client %d", clientID)
		default:
			log.Printf("DIAGNOSTICS: Unable to send last buffer to client %d, channel full", clientID)
		}
	} else {
		log.Printf("DIAGNOSTICS: No last buffer to send to client %d", clientID)
	}
	s.lastChunkMutex.RUnlock()

	return clientChannel, clientID, nil
}

// RemoveClient removes a client
func (s *Streamer) RemoveClient(clientID int) {
	s.clientMutex.Lock()
	defer s.clientMutex.Unlock()

	if channel, exists := s.clientChannels[clientID]; exists {
		close(channel)
		delete(s.clientChannels, clientID)
		atomic.AddInt32(&s.clientCounter, -1)
		log.Printf("Client %d disconnected. Total clients: %d", clientID, atomic.LoadInt32(&s.clientCounter))
	}
}

// GetClientCount returns the current number of clients
func (s *Streamer) GetClientCount() int {
	return int(atomic.LoadInt32(&s.clientCounter))
}

// broadcastToClients sends data to all connected clients
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
			log.Printf("DIAGNOSTICS: Sending %d bytes of data to %d clients", len(data), clientCount)
			lastClientCount = clientCount
			lastLogTime = time.Now()
		}
	}

	// If there are many clients, use batch approach
	const batchSize = 50
	
	// For small number of clients, use direct sending
	if clientCount <= batchSize {
		var g errgroup.Group
		
		// Send data to each client in a separate goroutine
		for clientID, clientChan := range s.clientChannels {
			clientID := clientID
			clientChan := clientChan
			
			g.Go(func() error {
				select {
				case clientChan <- data: // Use original buffer, not a copy
					// Data successfully sent
					return nil
				case <-time.After(200 * time.Millisecond):
					// Timeout sending data - try one more time
					log.Printf("First timeout sending data to client %d, retrying...", clientID)
					
					select {
					case clientChan <- data:
						log.Printf("Resending data to client %d successful", clientID)
						return nil
					case <-time.After(200 * time.Millisecond):
						// Second timeout, now disconnect client
						log.Printf("Second timeout sending data to client %d, disconnecting...", clientID)
						s.RemoveClient(clientID)
						return nil
					}
				}
			})
		}

		return g.Wait()
	}
	
	// For large number of clients use batch processing
	// Create list of all clients
	clients := make([]struct {
		id  int
		ch  chan []byte
	}, 0, clientCount)
	
	for id, ch := range s.clientChannels {
		clients = append(clients, struct {
			id  int
			ch  chan []byte
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
			id  int
			ch  chan []byte
		}) {
			defer wg.Done()
			
			for _, client := range batch {
				select {
				case client.ch <- data: // Use original buffer, not a copy
					// Data successfully sent
				case <-time.After(200 * time.Millisecond):
					// Timeout sending data - try one more time
					log.Printf("First timeout sending data to client %d, retrying...", client.id)
					
					select {
					case client.ch <- data:
						log.Printf("Resending data to client %d successful", client.id)
					case <-time.After(200 * time.Millisecond):
						// Second timeout, now disconnect client
						log.Printf("Second timeout sending data to client %d, disconnecting...", client.id)
						s.RemoveClient(client.id)
					}
				}
			}
		}(clients[i:end])
	}
	
	wg.Wait()
	return nil
} 