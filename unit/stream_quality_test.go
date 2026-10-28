package unit_test

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aposazhennikov/stream-audio-to-web/audio"
	"github.com/aposazhennikov/stream-audio-to-web/playlist"
	"github.com/aposazhennikov/stream-audio-to-web/radio"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 128 kbps = 16000 bytes per second; 4000-byte chunks are 250 ms of audio.
const (
	qualityBitrate     = 128
	qualityBytesPerSec = 16000
	qualityChunk       = 4000
)

// patternFile writes a file whose bytes encode their own offset, so a client can check
// that it received the stream without gaps, duplicates or overwritten chunks.
func patternFile(t *testing.T, dir, name string, size int, seed byte) string {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i/qualityChunk) + seed
	}
	// Must not look like an ID3v2/ID3v1 tag.
	data[0] = 0xFF
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, data, 0644))
	return path
}

func newQualityStreamer() *audio.Streamer {
	streamer := audio.NewStreamer(qualityChunk, 10, qualityBitrate, slog.Default(), createTestSentryHelper())
	streamer.SetVolumeNormalization(false)
	return streamer
}

// collect reads a client channel in the background.
type collector struct {
	mu       sync.Mutex
	data     []byte
	firstAt  time.Time
	lastAt   time.Time
	maxGap   time.Duration
	finished chan struct{}
}

func collect(ch <-chan []byte, delay time.Duration) *collector {
	c := &collector{finished: make(chan struct{})}
	go func() {
		defer close(c.finished)
		for chunk := range ch {
			if delay > 0 {
				time.Sleep(delay)
			}
			now := time.Now()
			c.mu.Lock()
			if c.firstAt.IsZero() {
				c.firstAt = now
			} else if gap := now.Sub(c.lastAt); gap > c.maxGap {
				c.maxGap = gap
			}
			c.lastAt = now
			c.data = append(c.data, chunk...)
			c.mu.Unlock()
		}
	}()
	return c
}

func (c *collector) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.data...)
}

// TestStreamerSendsAtRealTimeRate checks that audio goes out at the audio's own rate:
// not slower (players run dry and go silent after a while) and not faster.
func TestStreamerSendsAtRealTimeRate(t *testing.T) {
	streamer := newQualityStreamer()
	defer streamer.Close()

	path := patternFile(t, t.TempDir(), "rate.mp3", 4*qualityBytesPerSec, 1)
	ch, _, err := streamer.AddClient()
	require.NoError(t, err)
	c := collect(ch, 0)

	start := time.Now()
	require.NoError(t, streamer.StreamTrack(path))
	elapsed := time.Since(start)

	// 4 s of audio: the last chunk waits for its time, then a 50 ms pause between tracks.
	assert.InDelta(t, 4.05, elapsed.Seconds(), 0.3, "track of 4 s must take about 4 s to stream")
	assert.Equal(t, 4*qualityBytesPerSec, len(c.bytes()))
}

// TestStreamerNoDriftAcrossTracks plays many short tracks back to back and checks that the
// total time matches the total audio length: no accumulated lag and no accumulated lead.
func TestStreamerNoDriftAcrossTracks(t *testing.T) {
	streamer := newQualityStreamer()
	defer streamer.Close()

	dir := t.TempDir()
	var tracks []string
	for i := 0; i < 12; i++ {
		// Half a second each.
		tracks = append(tracks, patternFile(t, dir, fmt.Sprintf("%02d.mp3", i), qualityBytesPerSec/2, byte(i*10)))
	}

	ch, _, err := streamer.AddClient()
	require.NoError(t, err)
	c := collect(ch, 0)

	start := time.Now()
	for _, track := range tracks {
		require.NoError(t, streamer.StreamTrack(track))
	}
	elapsed := time.Since(start)

	// 6 s of audio in total.
	assert.InDelta(t, 6.0, elapsed.Seconds(), 0.5, "12 tracks of 0.5 s must take about 6 s")
	assert.Equal(t, 12*qualityBytesPerSec/2, len(c.bytes()))
	c.mu.Lock()
	maxGap := c.maxGap
	c.mu.Unlock()
	// Chunks are 250 ms; a transition adds a short pause that the pace clock catches up.
	assert.Less(t, maxGap, time.Second, "no long silence between tracks")
}

// TestLargeBufferSizeIsCapped: with the old 64 KB default a chunk held 4 s of audio and clients
// got data in 4-second bursts, leaving the player almost empty before each burst.
func TestLargeBufferSizeIsCapped(t *testing.T) {
	streamer := audio.NewStreamer(65536, 10, qualityBitrate, slog.Default(), createTestSentryHelper())
	defer streamer.Close()

	path := patternFile(t, t.TempDir(), "big.mp3", 5*qualityBytesPerSec, 1)
	ch, _, err := streamer.AddClient()
	require.NoError(t, err)
	c := collect(ch, 0)
	require.NoError(t, streamer.StreamTrack(path))

	c.mu.Lock()
	maxGap := c.maxGap
	c.mu.Unlock()
	assert.LessOrEqual(t, maxGap, 1100*time.Millisecond, "data must arrive at least once a second")
}

// TestNewClientGetsBurst checks that a client connecting mid-track immediately gets
// a few seconds of audio, so the player starts with a buffer.
func TestNewClientGetsBurst(t *testing.T) {
	streamer := newQualityStreamer()
	defer streamer.Close()

	path := patternFile(t, t.TempDir(), "burst.mp3", 10*qualityBytesPerSec, 1)
	done := make(chan error, 1)
	go func() { done <- streamer.StreamTrack(path) }()
	defer func() {
		streamer.StopCurrentTrack()
		<-done
	}()

	time.Sleep(5 * time.Second)
	ch, _, err := streamer.AddClient()
	require.NoError(t, err)

	received := 0
	timeout := time.After(100 * time.Millisecond)
loop:
	for {
		select {
		case chunk := <-ch:
			received += len(chunk)
		case <-timeout:
			break loop
		}
	}
	assert.GreaterOrEqual(t, received, 3*qualityBytesPerSec, "new client must get at least ~3 s of audio at once")
}

// TestClientIDsAreNotReused: a new client must not take over the channel of a connected one.
func TestClientIDsAreNotReused(t *testing.T) {
	streamer := newQualityStreamer()
	defer streamer.Close()

	_, id1, err := streamer.AddClient()
	require.NoError(t, err)
	ch2, id2, err := streamer.AddClient()
	require.NoError(t, err)
	streamer.RemoveClient(id1)
	_, id3, err := streamer.AddClient()
	require.NoError(t, err)

	require.NotEqual(t, id2, id3, "client ID reused while that client is still connected")
	assert.Equal(t, 2, streamer.GetClientCount())

	path := patternFile(t, t.TempDir(), "ids.mp3", qualityChunk, 1)
	require.NoError(t, streamer.StreamTrack(path))
	select {
	case chunk, ok := <-ch2:
		require.True(t, ok, "channel of client 2 was closed")
		assert.Len(t, chunk, qualityChunk)
	case <-time.After(time.Second):
		t.Fatal("client 2 stopped receiving audio after another client reconnected")
	}
}

// TestLaggingClientGetsIntactData: a client that reads with a delay must still get the exact
// bytes of the file in order (chunks must not be overwritten by later reads).
func TestLaggingClientGetsIntactData(t *testing.T) {
	streamer := newQualityStreamer()
	defer streamer.Close()

	path := patternFile(t, t.TempDir(), "intact.mp3", 3*qualityBytesPerSec, 1)
	want, err := os.ReadFile(path)
	require.NoError(t, err)

	ch, id, err := streamer.AddClient()
	require.NoError(t, err)
	c := collect(ch, 120*time.Millisecond) // falls behind by several chunks, but less than the buffer

	require.NoError(t, streamer.StreamTrack(path))
	time.Sleep(2 * time.Second)
	streamer.RemoveClient(id)
	<-c.finished

	got := c.bytes()
	require.Equal(t, len(want), len(got))
	assert.True(t, bytes.Equal(want, got), "lagging client received corrupted audio")
}

// TestICYMetadataWithRussianTitle checks that players asking for ICY metadata get the
// current track title in UTF-8, and that ordinary clients get plain audio.
func TestICYMetadataWithRussianTitle(t *testing.T) {
	dir := t.TempDir()
	patternFile(t, dir, "Владимир Высоцкий - Песня о 'друге'.mp3", 20*qualityBytesPerSec, 1)

	pl, err := playlist.NewPlaylist(dir, nil, false, slog.Default(), createTestSentryHelper())
	require.NoError(t, err)
	defer pl.Close()
	streamer := newQualityStreamer()
	defer streamer.Close()
	station := radio.NewRadioStation("/icy-test", streamer, pl, slog.Default(), createTestSentryHelper())
	station.Start()
	defer func() {
		station.Stop()
	}()

	server := getTestServer()
	server.RegisterStream("/icy-test", streamer, pl)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	time.Sleep(300 * time.Millisecond)

	// Player that asks for metadata.
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/icy-test", nil)
	require.NoError(t, err)
	req.Header.Set("Icy-MetaData", "1")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	metaInt, err := strconv.Atoi(resp.Header.Get("icy-metaint"))
	require.NoError(t, err, "icy-metaint header missing")
	require.Positive(t, metaInt)

	reader := bufio.NewReader(resp.Body)
	audioPart := make([]byte, metaInt)
	_, err = io.ReadFull(reader, audioPart)
	require.NoError(t, err)
	lengthByte, err := reader.ReadByte()
	require.NoError(t, err)
	require.Positive(t, int(lengthByte), "first metadata block must carry the title")
	meta := make([]byte, int(lengthByte)*16)
	_, err = io.ReadFull(reader, meta)
	require.NoError(t, err)
	meta = bytes.TrimRight(meta, "\x00")
	assert.Equal(t, "StreamTitle='Владимир Высоцкий - Песня о ’друге’';", string(meta))

	// The next block for the same title is a single zero byte.
	_, err = io.ReadFull(reader, audioPart)
	require.NoError(t, err)
	lengthByte, err = reader.ReadByte()
	require.NoError(t, err)
	assert.Equal(t, byte(0), lengthByte)

	// Ordinary client: no ICY header and no metadata in the audio.
	plain, err := http.Get(ts.URL + "/icy-test")
	require.NoError(t, err)
	defer plain.Body.Close()
	assert.Empty(t, plain.Header.Get("icy-metaint"))
	assert.Equal(t, "audio/mpeg", plain.Header.Get("Content-Type"))
}
