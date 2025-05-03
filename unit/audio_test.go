package unit

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/user/stream-audio-to-web/audio"
	"go.uber.org/goleak"
)

// Minimal valid MP3 file data for testing
var minimumMP3Data = []byte{
	0xFF, 0xFB, 0x90, 0x64, // MPEG-1 Layer 3 header
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // Minimal data
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
}

// TestMain sets up and tears down tests, including goroutine leak detection
func TestMain(m *testing.M) {
	// Skip leak check for external packages we can't control
	opts := []goleak.Option{
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
	}

	// Run tests
	goleak.VerifyTestMain(m, opts...)
}

// TestStreamerClientManagement tests the client management functions of the Streamer
func TestStreamerClientManagement(t *testing.T) {
	// Create a new streamer with small buffer size
	streamer := audio.NewStreamer(1024, 10, "mp3", 128)
	defer streamer.Close() // Ensure cleanup

	// Test initial state
	assert.Equal(t, 0, streamer.GetClientCount(), "Initial client count should be zero")

	// Test adding clients
	var clients []int
	var channels []<-chan []byte

	// Add multiple clients
	for i := 0; i < 5; i++ {
		ch, clientID, err := streamer.AddClient()
		require.NoError(t, err, "Adding client should not error")
		require.NotNil(t, ch, "Client channel should not be nil")
		
		clients = append(clients, clientID)
		channels = append(channels, ch)
		
		// Check incremented client count
		assert.Equal(t, i+1, streamer.GetClientCount(), "Client count should increase by one")
	}

	// Test removing clients
	for i, clientID := range clients {
		streamer.RemoveClient(clientID)
		assert.Equal(t, 4-i, streamer.GetClientCount(), "Client count should decrease by one")
	}

	// Test maximum clients limit
	streamer = audio.NewStreamer(1024, 3, "mp3", 128)
	defer streamer.Close()

	// Add clients up to limit
	for i := 0; i < 3; i++ {
		_, _, err := streamer.AddClient()
		require.NoError(t, err, "Should allow adding clients up to limit")
	}

	// Try to add one more client beyond the limit
	_, _, err := streamer.AddClient()
	require.Error(t, err, "Should prevent adding clients beyond limit")
	assert.Contains(t, err.Error(), "maximum number of clients exceeded", "Error should mention client limit")
}

// TestBroadcastToClients tests the broadcasting functionality
func TestBroadcastToClients(t *testing.T) {
	// Create a streamer with small buffers for testing
	streamer := audio.NewStreamer(1024, 10, "mp3", 128)
	defer streamer.Close()

	// Add some test clients
	var wg sync.WaitGroup
	clientCount := 3
	receivedData := make([]bool, clientCount)

	// Set up clients that will read from their channels
	for i := 0; i < clientCount; i++ {
		wg.Add(1)
		ch, _, err := streamer.AddClient()
		require.NoError(t, err)

		// Start goroutine to read from channel
		go func(clientIndex int, channel <-chan []byte) {
			defer wg.Done()
			
			// Wait for data on the channel
			select {
			case data, ok := <-channel:
				if ok && len(data) > 0 {
					receivedData[clientIndex] = true
				}
			case <-time.After(1 * time.Second):
				// Timeout waiting for data
			}
		}(i, ch)
	}

	// Use StreamTrack with a minimal MP3 file to trigger broadcasting
	// Create temporary file with minimal MP3 data
	tmpDir, err := ioutil.TempDir("", "audio-test")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	testFile := filepath.Join(tmpDir, "test.mp3")
	err = ioutil.WriteFile(testFile, minimumMP3Data, 0644)
	require.NoError(t, err)

	// Start streaming in a separate goroutine
	go func() {
		err := streamer.StreamTrack(testFile)
		assert.NoError(t, err)
	}()

	// Wait for all clients to receive data
	wg.Wait()

	// Check that all clients received data
	for i, received := range receivedData {
		assert.True(t, received, "Client %d should have received data", i)
	}
}

// TestStreamTrackHappyPath tests the normal functioning of StreamTrack
func TestStreamTrackHappyPath(t *testing.T) {
	// Create a streamer with small buffers for quick testing
	streamer := audio.NewStreamer(1024, 10, "mp3", 128)
	defer streamer.Close()

	// Create temporary file with minimal MP3 data
	tmpDir, err := ioutil.TempDir("", "audio-test")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	testFile := filepath.Join(tmpDir, "test.mp3")
	err = ioutil.WriteFile(testFile, minimumMP3Data, 0644)
	require.NoError(t, err)

	// Add a test client
	ch, _, err := streamer.AddClient()
	require.NoError(t, err)

	// Variable to track if data was received
	dataReceived := atomic.Bool{}
	dataReceived.Store(false)

	// Start goroutine to read from client channel
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case data, ok := <-ch:
			if ok && len(data) > 0 {
				dataReceived.Store(true)
			}
		case <-time.After(1 * time.Second):
			// Timeout waiting for data
		}
	}()

	// Stream the test file
	err = streamer.StreamTrack(testFile)
	require.NoError(t, err, "StreamTrack should not error with valid file")

	// Wait for client to process data
	wg.Wait()

	// Check if client received data
	assert.True(t, dataReceived.Load(), "Client should have received data")
}

// TestStreamTrackErrors tests error handling in StreamTrack
func TestStreamTrackErrors(t *testing.T) {
	streamer := audio.NewStreamer(1024, 10, "mp3", 128)
	defer streamer.Close()

	// Test empty path
	err := streamer.StreamTrack("")
	require.Error(t, err, "Empty path should return error")
	assert.Contains(t, err.Error(), "empty audio file path", "Error should mention empty path")

	// Test non-existent file
	err = streamer.StreamTrack("/path/to/nonexistent/file.mp3")
	require.Error(t, err, "Non-existent file should return error")
	assert.Contains(t, err.Error(), "error checking file", "Error should mention file check issue")

	// Test directory instead of file
	tmpDir, err := ioutil.TempDir("", "audio-test")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)
	
	err = streamer.StreamTrack(tmpDir)
	require.Error(t, err, "Directory path should return error")
	assert.Contains(t, err.Error(), "is a directory", "Error should mention it's a directory")
}

// TestLastChunkDelivery tests that new clients receive the last chunk of data
func TestLastChunkDelivery(t *testing.T) {
	// Create a streamer with small buffers
	streamer := audio.NewStreamer(1024, 10, "mp3", 128)
	defer streamer.Close()

	// Create temporary file with minimal MP3 data
	tmpDir, err := ioutil.TempDir("", "audio-test")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	testFile := filepath.Join(tmpDir, "test.mp3")
	err = ioutil.WriteFile(testFile, minimumMP3Data, 0644)
	require.NoError(t, err)

	// Start streaming in a separate goroutine
	go func() {
		err := streamer.StreamTrack(testFile)
		assert.NoError(t, err)
	}()

	// Wait a moment for streaming to start
	time.Sleep(200 * time.Millisecond)

	// Now add a new client which should receive the last chunk
	ch, _, err := streamer.AddClient()
	require.NoError(t, err)

	// Check if new client receives data
	dataReceived := false
	select {
	case data, ok := <-ch:
		if ok && len(data) > 0 {
			dataReceived = true
		}
	case <-time.After(1 * time.Second):
		// Timeout waiting for data
	}

	assert.True(t, dataReceived, "New client should receive last chunk")
}

// TestStopCurrentTrack tests that stopping the current track works correctly
func TestStopCurrentTrack(t *testing.T) {
	streamer := audio.NewStreamer(1024, 10, "mp3", 128)
	defer streamer.Close()

	// Create temporary file with minimal MP3 data
	tmpDir, err := ioutil.TempDir("", "audio-test")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	testFile := filepath.Join(tmpDir, "test.mp3")
	// Create a larger file for longer playback
	largerData := make([]byte, 10000)
	copy(largerData, minimumMP3Data)
	err = ioutil.WriteFile(testFile, largerData, 0644)
	require.NoError(t, err)

	// Add a client
	ch, _, err := streamer.AddClient()
	require.NoError(t, err)

	// Flag to track if streaming was interrupted
	interrupted := atomic.Bool{}
	interrupted.Store(false)

	// Start streaming in a separate goroutine
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := streamer.StreamTrack(testFile)
		// If streaming ends early without error, it was interrupted
		if err == nil {
			interrupted.Store(true)
		}
	}()

	// Wait a moment for streaming to start
	time.Sleep(200 * time.Millisecond)

	// Stop the current track
	streamer.StopCurrentTrack()

	// Wait for streaming to finish
	wg.Wait()

	// Check if streaming was interrupted
	assert.True(t, interrupted.Load(), "StreamTrack should have been interrupted")

	// Check if channel is still functional
	var receivedData bool
	// Start another streaming to send data
	go func() {
		err := streamer.StreamTrack(testFile)
		assert.NoError(t, err)
	}()

	// Check if client can still receive data
	select {
	case data, ok := <-ch:
		receivedData = ok && len(data) > 0
	case <-time.After(1 * time.Second):
		// Timeout
	}

	assert.True(t, receivedData, "Client should still be able to receive data after stopping track")
} 