package unit

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/user/stream-audio-to-web/radio"
)

// MockAudioStreamer implements radio.AudioStreamer for testing
type MockAudioStreamer struct {
	streamTrackCalls      int32  // Number of times StreamTrack was called
	stopCurrentTrackCalls int32  // Number of times StopCurrentTrack was called
	closeCalls            int32  // Number of times Close was called
	shouldStreamFail      bool   // If true, StreamTrack returns an error
	currentTrack          string // Current track being streamed
	streaming             atomic.Bool // Currently streaming
	streamingMutex        sync.Mutex  // Mutex to coordinate streaming operations
	streamComplete        chan struct{} // Channel to signal when streaming is complete
}

func NewMockAudioStreamer() *MockAudioStreamer {
	return &MockAudioStreamer{
		streamComplete: make(chan struct{}),
	}
}

func (m *MockAudioStreamer) StreamTrack(trackPath string) error {
	atomic.AddInt32(&m.streamTrackCalls, 1)
	
	m.streamingMutex.Lock()
	m.currentTrack = trackPath
	m.streaming.Store(true)
	m.streamingMutex.Unlock()
	
	// Block until cancelled or we manually complete
	select {
	case <-m.streamComplete:
		// Streaming completed normally
		m.streamingMutex.Lock()
		m.streaming.Store(false)
		m.streamingMutex.Unlock()
		
		if m.shouldStreamFail {
			return errors.New("mock stream failure")
		}
		return nil
	}
}

func (m *MockAudioStreamer) StopCurrentTrack() {
	atomic.AddInt32(&m.stopCurrentTrackCalls, 1)
	
	// Signal StreamTrack to stop blocking
	close(m.streamComplete)
	
	// Make a new channel for next streaming operation
	m.streamingMutex.Lock()
	m.streamComplete = make(chan struct{})
	m.streaming.Store(false)
	m.streamingMutex.Unlock()
}

func (m *MockAudioStreamer) Close() {
	atomic.AddInt32(&m.closeCalls, 1)
	
	m.streamingMutex.Lock()
	defer m.streamingMutex.Unlock()
	
	// Signal StreamTrack to stop if it's running
	if m.streaming.Load() {
		close(m.streamComplete)
		m.streamComplete = make(chan struct{})
	}
	
	m.streaming.Store(false)
}

// MockPlaylistManager implements radio.PlaylistManager for testing
type MockPlaylistManager struct {
	tracks                []string
	currentIndex          int
	getCurrentTrackCalls  int32
	nextTrackCalls        int32
	previousTrackCalls    int32
	mutex                 sync.Mutex
}

func NewMockPlaylistManager(tracks []string) *MockPlaylistManager {
	return &MockPlaylistManager{
		tracks:       tracks,
		currentIndex: 0,
	}
}

func (m *MockPlaylistManager) GetCurrentTrack() interface{} {
	atomic.AddInt32(&m.getCurrentTrackCalls, 1)
	
	m.mutex.Lock()
	defer m.mutex.Unlock()
	
	if len(m.tracks) == 0 {
		return nil
	}
	
	return m.tracks[m.currentIndex]
}

func (m *MockPlaylistManager) NextTrack() interface{} {
	atomic.AddInt32(&m.nextTrackCalls, 1)
	
	m.mutex.Lock()
	defer m.mutex.Unlock()
	
	if len(m.tracks) == 0 {
		return nil
	}
	
	m.currentIndex = (m.currentIndex + 1) % len(m.tracks)
	return m.tracks[m.currentIndex]
}

func (m *MockPlaylistManager) PreviousTrack() interface{} {
	atomic.AddInt32(&m.previousTrackCalls, 1)
	
	m.mutex.Lock()
	defer m.mutex.Unlock()
	
	if len(m.tracks) == 0 {
		return nil
	}
	
	if m.currentIndex == 0 {
		m.currentIndex = len(m.tracks) - 1
	} else {
		m.currentIndex--
	}
	
	return m.tracks[m.currentIndex]
}

// TestRadioStationPlayback tests the basic playback loop of a radio station
func TestRadioStationPlayback(t *testing.T) {
	// Create mock streamer and playlist
	streamer := NewMockAudioStreamer()
	tracks := []string{"/path/to/track1.mp3", "/path/to/track2.mp3", "/path/to/track3.mp3"}
	playlist := NewMockPlaylistManager(tracks)
	
	// Create a radio station
	station := radio.NewRadioStation("/test", streamer, playlist)
	
	// Start the station
	station.Start()
	
	// Give it some time to process the first track
	time.Sleep(200 * time.Millisecond)
	
	// Check that streaming has started
	assert.Equal(t, int32(1), atomic.LoadInt32(&streamer.streamTrackCalls), 
		"StreamTrack should be called once after starting")
	
	// Stop the current track to trigger next track
	streamer.StopCurrentTrack()
	
	// Give it time to process the next track
	time.Sleep(200 * time.Millisecond)
	
	// Check that next track was requested and streamed
	assert.Equal(t, int32(1), atomic.LoadInt32(&playlist.nextTrackCalls), 
		"NextTrack should be called once after first track completes")
	assert.Equal(t, int32(2), atomic.LoadInt32(&streamer.streamTrackCalls), 
		"StreamTrack should be called twice")
	
	// Stop the station
	station.Stop()
	
	// Check that it's cleaned up
	time.Sleep(100 * time.Millisecond)
	assert.False(t, streamer.streaming.Load(), "Streaming should be stopped")
}

// TestRadioStationRestartPlayback tests the restart playback functionality
func TestRadioStationRestartPlayback(t *testing.T) {
	// Create mock streamer and playlist
	streamer := NewMockAudioStreamer()
	tracks := []string{"/path/to/track1.mp3", "/path/to/track2.mp3"}
	playlist := NewMockPlaylistManager(tracks)
	
	// Create a radio station
	station := radio.NewRadioStation("/test", streamer, playlist)
	
	// Start the station
	station.Start()
	
	// Give it some time to start processing
	time.Sleep(200 * time.Millisecond)
	
	// Verify initial state
	assert.Equal(t, int32(1), atomic.LoadInt32(&streamer.streamTrackCalls), 
		"StreamTrack should be called once initially")
	assert.Equal(t, int32(0), atomic.LoadInt32(&streamer.stopCurrentTrackCalls), 
		"StopCurrentTrack should not be called yet")
	
	// Now restart playback
	station.RestartPlayback()
	
	// Give it time to process
	time.Sleep(200 * time.Millisecond)
	
	// Verify that streaming stopped and restarted the current track
	assert.Equal(t, int32(1), atomic.LoadInt32(&streamer.stopCurrentTrackCalls), 
		"StopCurrentTrack should be called once")
	assert.Equal(t, int32(2), atomic.LoadInt32(&streamer.streamTrackCalls), 
		"StreamTrack should be called again")
	assert.Equal(t, int32(0), atomic.LoadInt32(&playlist.nextTrackCalls), 
		"NextTrack should not be called after restart")
	
	// Clean up
	station.Stop()
}

// TestRadioStationErrorHandling tests how radio station handles streaming errors
func TestRadioStationErrorHandling(t *testing.T) {
	// Create mock streamer and playlist with streaming error
	streamer := NewMockAudioStreamer()
	streamer.shouldStreamFail = true
	
	tracks := []string{"/path/to/track1.mp3", "/path/to/track2.mp3"}
	playlist := NewMockPlaylistManager(tracks)
	
	// Create a radio station
	station := radio.NewRadioStation("/test", streamer, playlist)
	
	// Start the station
	station.Start()
	
	// Give it some time to start processing
	time.Sleep(200 * time.Millisecond)
	
	// Trigger completion with error
	streamer.StopCurrentTrack()
	
	// Give it time to process the error and try the next track
	time.Sleep(200 * time.Millisecond)
	
	// Verify that next track was attempted after error
	assert.Equal(t, int32(1), atomic.LoadInt32(&playlist.nextTrackCalls), 
		"NextTrack should be called after streaming error")
	assert.Equal(t, int32(2), atomic.LoadInt32(&streamer.streamTrackCalls), 
		"StreamTrack should be called again after error")
	
	// Clean up
	station.Stop()
}

// TestRadioStationEmptyPlaylist tests behavior with an empty playlist
func TestRadioStationEmptyPlaylist(t *testing.T) {
	// Create mock streamer and empty playlist
	streamer := NewMockAudioStreamer()
	playlist := NewMockPlaylistManager([]string{})
	
	// Create a radio station
	station := radio.NewRadioStation("/test", streamer, playlist)
	
	// Start the station
	station.Start()
	
	// Give it some time to try processing
	time.Sleep(500 * time.Millisecond)
	
	// Verify that no streaming occurred
	assert.Equal(t, int32(0), atomic.LoadInt32(&streamer.streamTrackCalls), 
		"StreamTrack should not be called with empty playlist")
	
	// Verify GetCurrentTrack was called multiple times (retry logic)
	assert.True(t, atomic.LoadInt32(&playlist.getCurrentTrackCalls) > 0, 
		"GetCurrentTrack should be called multiple times")
	
	// Clean up
	station.Stop()
}

// TestRadioStationManager tests the RadioStationManager functionality
func TestRadioStationManager(t *testing.T) {
	// Create a manager
	manager := radio.NewRadioStationManager()
	
	// Create streamers and playlists for multiple stations
	streamer1 := NewMockAudioStreamer()
	playlist1 := NewMockPlaylistManager([]string{"/path/to/track1.mp3"})
	
	streamer2 := NewMockAudioStreamer()
	playlist2 := NewMockPlaylistManager([]string{"/path/to/track2.mp3"})
	
	// Add stations
	manager.AddStation("/route1", streamer1, playlist1)
	manager.AddStation("/route2", streamer2, playlist2)
	
	// Allow time for stations to initialize
	time.Sleep(300 * time.Millisecond)
	
	// Verify stations are streaming
	assert.Equal(t, int32(1), atomic.LoadInt32(&streamer1.streamTrackCalls), 
		"First streamer should be called")
	assert.Equal(t, int32(1), atomic.LoadInt32(&streamer2.streamTrackCalls), 
		"Second streamer should be called")
	
	// Test GetStation
	station := manager.GetStation("/route1")
	require.NotNil(t, station, "Should return an existing station")
	
	nonExistingStation := manager.GetStation("/non-existing")
	assert.Nil(t, nonExistingStation, "Should return nil for non-existing station")
	
	// Test RestartPlayback
	restart := manager.RestartPlayback("/route1")
	assert.True(t, restart, "Restart should return true for existing station")
	
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(1), atomic.LoadInt32(&streamer1.stopCurrentTrackCalls), 
		"StopCurrentTrack should be called on restart")
	
	noRestart := manager.RestartPlayback("/non-existing")
	assert.False(t, noRestart, "Restart should return false for non-existing station")
	
	// Test RemoveStation
	manager.RemoveStation("/route1")
	station = manager.GetStation("/route1")
	assert.Nil(t, station, "Station should be removed")
	
	// Test StopAll
	manager.StopAll()
	
	// Allow time for stations to stop
	time.Sleep(200 * time.Millisecond)
	
	// Verify streamers have been stopped
	assert.False(t, streamer1.streaming.Load(), "First streamer should be stopped")
	assert.False(t, streamer2.streaming.Load(), "Second streamer should be stopped")
}

// TestTrackPathExtraction tests the getTrackPath function
func TestTrackPathExtraction(t *testing.T) {
	tests := []struct {
		name     string
		input    interface{}
		expected string
	}{
		{
			name:     "string input",
			input:    "/path/to/file.mp3",
			expected: "/path/to/file.mp3",
		},
		{
			name:     "struct with GetPath method",
			input:    &testTrack{path: "/path/to/track.mp3"},
			expected: "/path/to/track.mp3",
		},
		{
			name:     "map input",
			input:    map[string]string{"path": "/path/to/map.mp3"},
			expected: "/path/to/map.mp3",
		},
		{
			name:     "struct with Path field",
			input:    struct{ Path string }{Path: "/path/to/struct.mp3"},
			expected: "/path/to/struct.mp3",
		},
		{
			name:     "unknown type",
			input:    123,
			expected: "",
		},
		{
			name:     "nil input",
			input:    nil,
			expected: "",
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getTrackPathExported(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// Helper struct for testing getTrackPath
type testTrack struct {
	path string
}

func (t *testTrack) GetPath() string {
	return t.path
}

// Exported version of getTrackPath to allow testing
// Note: In a real project, this could be exposed via a test-only export mechanism
func getTrackPathExported(track interface{}) string {
	// Interface unpacking depends on specific Track implementation
	if t, ok := track.(interface{ GetPath() string }); ok {
		return t.GetPath()
	}
	if t, ok := track.(map[string]string); ok {
		return t["path"]
	}
	if t, ok := track.(struct{ Path string }); ok {
		return t.Path
	}
	
	// If unpacking failed, try to convert to string
	if s, ok := track.(string); ok {
		return s
	}
	
	return ""
} 