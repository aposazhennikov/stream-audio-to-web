package unit_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aposazhennikov/stream-audio-to-web/audio"
	"github.com/aposazhennikov/stream-audio-to-web/unit/testdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSlowClientDoesNotFreezeStream checks that a client which stops reading its channel
// is disconnected without blocking the whole stream. Before the fix, disconnecting such a
// client from inside the broadcast deadlocked the streamer: the station went silent for all
// listeners and new clients could not connect.
func TestSlowClientDoesNotFreezeStream(t *testing.T) {
	// High bitrate gives the minimal 10ms delay per 1KB chunk, so the client channel
	// (32 chunks) overflows in well under a second.
	streamer := audio.NewStreamer(1024, 10, 100000, slog.Default(), createTestSentryHelper())
	streamer.SetVolumeNormalization(false)
	defer streamer.Close()

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.mp3")
	data := make([]byte, 100*1024)
	copy(data, testdata.GetMinimumMP3Data())
	require.NoError(t, os.WriteFile(testFile, data, 0644))

	// The client is never read from, like a listener whose connection hung.
	_, stuckID, err := streamer.AddClient()
	require.NoError(t, err)

	streamDone := make(chan error, 1)
	go func() {
		streamDone <- streamer.StreamTrack(testFile)
	}()

	select {
	case streamErr := <-streamDone:
		require.NoError(t, streamErr)
	case <-time.After(10 * time.Second):
		t.Fatal("StreamTrack is stuck: the stream froze because of a client that stopped reading")
	}

	assert.Equal(t, 0, streamer.GetClientCount(), "stuck client should be disconnected")

	// The streamer must still accept new clients.
	added := make(chan error, 1)
	go func() {
		_, id, addErr := streamer.AddClient()
		if addErr == nil {
			streamer.RemoveClient(id)
		}
		added <- addErr
	}()
	select {
	case addErr := <-added:
		require.NoError(t, addErr)
	case <-time.After(2 * time.Second):
		t.Fatal("AddClient is stuck after a slow client was disconnected")
	}

	// Removing an already disconnected client must be harmless (the HTTP handler does this in defer).
	streamer.RemoveClient(stuckID)
}
