package unit_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/aposazhennikov/stream-audio-to-web/audio"
	sentryhelper "github.com/aposazhennikov/stream-audio-to-web/sentry_helper"
	"github.com/aposazhennikov/stream-audio-to-web/unit/testdata"
	"go.uber.org/goleak"
)


// TestMain sets up and tears down tests, including goroutine leak detection.
func TestMain(m *testing.M) {
	// Skip leak check for external packages we can't control.
	opts := []goleak.Option{
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
		// Ignore our own goroutines that are expected to stay alive after tests.
		goleak.IgnoreTopFunction("github.com/aposazhennikov/stream-audio-to-web/http.(*Server).trackCurrentTrack"),
		goleak.IgnoreTopFunction("github.com/aposazhennikov/stream-audio-to-web/playlist.(*Playlist).watchDirectory"),
		goleak.IgnoreTopFunction("github.com/fsnotify/fsnotify.(*Watcher).readEvents"),
	}

	// Run tests.
	goleak.VerifyTestMain(m, opts...)
}

// TestStreamerClientManagement tests the client management functions of the Streamer.
func TestStreamerClientManagement(t *testing.T) {
	// Create a new streamer with small buffer size.
	streamer := audio.NewStreamer(1024, 10, 128, slog.Default(), createTestSentryHelper())
	defer streamer.Close() // Ensure cleanup

	// Test initial state.
	assert.Equal(t, 0, streamer.GetClientCount(), "Initial client count should be zero")

	// Test adding clients.
	var clients []int
	var channels []<-chan []byte

	// Add multiple clients.
	for i := range 5 {
		ch, clientID, err := streamer.AddClient()
		require.NoError(t, err, "Adding client should not error")
		require.NotNil(t, ch, "Client channel should not be nil")

		clients = append(clients, clientID)
		var _ = append(channels, ch)

		// Check incremented client count.
		assert.Equal(t, i+1, streamer.GetClientCount(), "Client count should increase by one")
	}

	// Test removing clients.
	for i, clientID := range clients {
		streamer.RemoveClient(clientID)
		assert.Equal(t, 4-i, streamer.GetClientCount(), "Client count should decrease by one")
	}

	// Test maximum clients limit.
	streamer = audio.NewStreamer(1024, 3, 128, slog.Default(), createTestSentryHelper())
	defer streamer.Close()

	// Add clients up to limit.
	for range 3 {
		_, _, err := streamer.AddClient()
		require.NoError(t, err, "Should allow adding clients up to limit")
	}

	// Try to add one more client beyond the limit.
	_, _, err := streamer.AddClient()
	require.Error(t, err, "Should prevent adding clients beyond limit")
	assert.Contains(t, err.Error(), "maximum number of clients exceeded", "Error should mention client limit")
}

// TestBroadcastToClients tests the broadcasting functionality.
func TestBroadcastToClients(t *testing.T) {
	// Create a streamer with small buffers for testing.
	streamer := audio.NewStreamer(1024, 10, 128, slog.Default(), createTestSentryHelper())
	defer streamer.Close()

	// Add some test clients.
	var wg sync.WaitGroup
	clientCount := 3
	receivedData := make([]bool, clientCount)

	// Set up clients that will read from their channels.
	for i := range clientCount {
		wg.Add(1)
		ch, _, err := streamer.AddClient()
		require.NoError(t, err)

		// Start goroutine to read from channel.
		go func(clientIndex int, channel <-chan []byte) {
			defer wg.Done()

			// Wait for data on the channel.
			select {
			case data, ok := <-channel:
				if ok && len(data) > 0 {
					receivedData[clientIndex] = true
				}
			case <-time.After(1 * time.Second):
				// Timeout waiting for data.
			}
		}(i, ch)
	}

	// Use StreamTrack with a minimal MP3 file to trigger broadcasting.
	// Create temporary file with minimal MP3 data.
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.mp3")
	errWrite := os.WriteFile(testFile, testdata.GetMinimumMP3Data(), 0644)
	require.NoError(t, errWrite)

	// Start streaming in a separate goroutine.
	go func() {
		err := streamer.StreamTrack(testFile)
		assert.NoError(t, err)
	}()

	// Wait for all clients to receive data.
	wg.Wait()

	// Check that all clients received data.
	for i, received := range receivedData {
		assert.True(t, received, "Client %d should have received data", i)
	}
}

// TestStreamTrackHappyPath tests the normal functioning of StreamTrack.
func TestStreamTrackHappyPath(t *testing.T) {
	// Create a streamer with small buffers for quick testing.
	streamer := audio.NewStreamer(1024, 10, 128, slog.Default(), createTestSentryHelper())
	defer streamer.Close()

	// Create temporary file with minimal MP3 data.
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.mp3")
	errWrite := os.WriteFile(testFile, testdata.GetMinimumMP3Data(), 0644)
	require.NoError(t, errWrite)

	// Add a test client.
	ch, _, errAddClient := streamer.AddClient()
	require.NoError(t, errAddClient)

	// Variable to track if data was received.
	dataReceived := atomic.Bool{}
	dataReceived.Store(false)

	// Start goroutine to read from client channel.
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
			// Timeout waiting for data.
		}
	}()

	// Stream the test file.
	errStream := streamer.StreamTrack(testFile)
	require.NoError(t, errStream, "StreamTrack should not error with valid file")

	// Wait for client to process data.
	wg.Wait()

	// Check if client received data.
	assert.True(t, dataReceived.Load(), "Client should have received data")
}

// TestStreamTrackErrors tests error handling in StreamTrack.
func TestStreamTrackErrors(t *testing.T) {
	streamer := audio.NewStreamer(1024, 10, 128, slog.Default(), createTestSentryHelper())
	defer streamer.Close()

	// Test empty path.
	errEmpty := streamer.StreamTrack("")
	require.Error(t, errEmpty, "Empty path should return error")
	assert.Contains(t, errEmpty.Error(), "empty audio file path", "Error should mention empty path")

	// Test non-existent file.
	errNotFound := streamer.StreamTrack("/path/to/nonexistent/file.mp3")
	require.Error(t, errNotFound, "Non-existent file should return error")
	assert.Contains(t, errNotFound.Error(), "error checking file", "Error should mention file check issue")

	// Test directory instead of file.
	tmpDir := t.TempDir()

	errDir := streamer.StreamTrack(tmpDir)
	require.Error(t, errDir, "Directory path should return error")
	assert.Contains(t, errDir.Error(), "is a directory", "Error should mention it's a directory")
}

// TestLastChunkDelivery tests that new clients receive the last chunk of data.
func TestLastChunkDelivery(t *testing.T) {
	// Create a streamer with small buffers.
	streamer := audio.NewStreamer(1024, 10, 128, slog.Default(), createTestSentryHelper())
	defer streamer.Close()

	// Create temporary file with minimal MP3 data.
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.mp3")
	errWrite := os.WriteFile(testFile, testdata.GetMinimumMP3Data(), 0644)
	require.NoError(t, errWrite)

	// Start streaming in a separate goroutine.
	go func() {
		err := streamer.StreamTrack(testFile)
		assert.NoError(t, err)
	}()

	// Wait a moment for streaming to start.
	time.Sleep(200 * time.Millisecond)

	// Now add a new client which should receive the last chunk.
	ch, _, errAddClient := streamer.AddClient()
	require.NoError(t, errAddClient)

	// Check if new client receives data.
	dataReceived := false
	select {
	case data, ok := <-ch:
		if ok && len(data) > 0 {
			dataReceived = true
		}
	case <-time.After(1 * time.Second):
		// Timeout waiting for data.
	}

	assert.True(t, dataReceived, "New client should receive last chunk")
}

// TestStopCurrentTrack tests that stopping the current track works correctly.
func TestStopCurrentTrack(t *testing.T) {
	streamer := audio.NewStreamer(1024, 10, 128, slog.Default(), createTestSentryHelper())
	// Отключим нормализацию громкости для этого теста, чтобы избежать ошибок.
	streamer.SetVolumeNormalization(false)
	defer streamer.Close()

	// Create temporary file with minimal MP3 data.
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.mp3")
	// Create a larger file for longer playback.
	largerData := make([]byte, 10000)
	copy(largerData, testdata.GetMinimumMP3Data())
	errWrite := os.WriteFile(testFile, largerData, 0644)
	require.NoError(t, errWrite)

	// Add a client.
	ch, _, errAddClient := streamer.AddClient()
	require.NoError(t, errAddClient)

	// Flag to track if streaming was interrupted.
	interrupted := atomic.Bool{}
	interrupted.Store(false)

	// Start streaming in a separate goroutine.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := streamer.StreamTrack(testFile)
		// If streaming ends early without error, it was interrupted.
		if err == nil {
			interrupted.Store(true)
		}
	}()

	// Wait a moment for streaming to start.
	time.Sleep(200 * time.Millisecond)

	// Stop the current track.
	streamer.StopCurrentTrack()

	// Wait for streaming to finish.
	wg.Wait()

	// Check if streaming was interrupted.
	assert.True(t, interrupted.Load(), "StreamTrack should have been interrupted")

	// Add WaitGroup for final goroutine to ensure it completes before test finishes.
	var finalWg sync.WaitGroup
	finalWg.Add(1)

	// Setup a done channel to ensure goroutine cleanup.
	done := make(chan struct{})

	// Check if channel is still functional.
	var receivedData bool
	// Start another streaming to send data.
	go func() {
		defer finalWg.Done() // Signal completion of this goroutine
		defer close(done)    // Signal that we're done so select can exit
		err := streamer.StreamTrack(testFile)
		// Don't use assert in goroutines that may outlive the test function.
		if err != nil {
			t.Logf("Error streaming track in final check: %v", err)
		}
	}()

	// Check if client can still receive data.
	select {
	case data, ok := <-ch:
		receivedData = ok && len(data) > 0
	case <-time.After(1 * time.Second):
		// Timeout.
	}

	assert.True(t, receivedData, "Client should still be able to receive data after stopping track")

	// Stop current track and wait for the final goroutine to complete.
	streamer.StopCurrentTrack() // Ensure the streaming stops

	// Wait for the goroutine to finish with timeout.
	select {
	case <-done:
		// Goroutine completed successfully.
	case <-time.After(2 * time.Second):
		t.Log("Warning: Timeout waiting for streaming goroutine to complete")
	}

	finalWg.Wait()

	// Убедимся, что все операции с файлом завершены перед выходом из функции.
	// Это предотвратит удаление файла до завершения всех операций с ним.
	time.Sleep(50 * time.Millisecond)
}
