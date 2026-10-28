package unit_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aposazhennikov/stream-audio-to-web/audio"
	"github.com/aposazhennikov/stream-audio-to-web/radio"
	"github.com/aposazhennikov/stream-audio-to-web/unit/testdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// concurrencyCountingStreamer wraps the real streamer and records how many StreamTrack calls run at once.
type concurrencyCountingStreamer struct {
	*audio.Streamer
	active    int32
	maxActive int32
}

func (c *concurrencyCountingStreamer) StreamTrack(trackPath string) error {
	n := atomic.AddInt32(&c.active, 1)
	for {
		prev := atomic.LoadInt32(&c.maxActive)
		if n <= prev || atomic.CompareAndSwapInt32(&c.maxActive, prev, n) {
			break
		}
	}
	defer atomic.AddInt32(&c.active, -1)
	return c.Streamer.StreamTrack(trackPath)
}

// TestRapidTrackSwitchKeepsSingleStream switches tracks many times in a row (like repeatedly
// pressing "Previous track") and checks that two tracks never stream into the same output at once
// and that the stream keeps sending data afterwards.
func TestRapidTrackSwitchKeepsSingleStream(t *testing.T) {
	tmpDir := t.TempDir()
	data := make([]byte, 512*1024)
	copy(data, testdata.GetMinimumMP3Data())
	var tracks []string
	for _, name := range []string{"1.mp3", "2.mp3", "3.mp3"} {
		path := filepath.Join(tmpDir, name)
		require.NoError(t, os.WriteFile(path, data, 0644))
		tracks = append(tracks, path)
	}

	streamer := &concurrencyCountingStreamer{
		Streamer: audio.NewStreamer(1024, 10, 128, slog.Default(), createTestSentryHelper()),
	}
	streamer.SetVolumeNormalization(false)
	playlist := NewMockPlaylistManager(tracks)

	station := radio.NewRadioStation("/switch-test", streamer, playlist, slog.Default(), createTestSentryHelper())
	station.Start()

	clientCh, clientID, err := streamer.AddClient()
	require.NoError(t, err)

	// Drain the client so it never becomes a slow client.
	var received int64
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for chunk := range clientCh {
			atomic.AddInt64(&received, int64(len(chunk)))
		}
	}()

	time.Sleep(200 * time.Millisecond)
	for i := range 40 {
		station.RestartPlayback()
		time.Sleep(time.Duration(i%7) * 15 * time.Millisecond)
	}

	// After switching the stream must keep going.
	time.Sleep(500 * time.Millisecond)
	before := atomic.LoadInt64(&received)
	time.Sleep(1500 * time.Millisecond)
	after := atomic.LoadInt64(&received)

	assert.Equal(t, int32(1), atomic.LoadInt32(&streamer.maxActive),
		"two StreamTrack calls ran at the same time")
	assert.Greater(t, after, before, "stream stopped sending data after track switches")

	streamer.RemoveClient(clientID)
	<-drainDone

	stopped := make(chan struct{})
	go func() {
		station.Stop()
		close(stopped)
	}()
	streamer.StopCurrentTrack()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("station did not stop")
	}
	streamer.StopCurrentTrack()
	streamer.Close()
}
