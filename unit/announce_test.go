package unit_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aposazhennikov/stream-audio-to-web/announce"
	"github.com/aposazhennikov/stream-audio-to-web/playlist"
	"github.com/aposazhennikov/stream-audio-to-web/radio"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSpeech is a speech service that returns "VOICE[<text>]" instead of audio and remembers
// the sample rate each text was asked for.
type fakeSpeechService struct {
	*httptest.Server
	mu    sync.Mutex
	rates map[string]string // text -> ?rate
}

func (f *fakeSpeechService) rate(text string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rates[text]
}

func fakeSpeech(t *testing.T) *fakeSpeechService {
	t.Helper()
	f := &fakeSpeechService{rates: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		text, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.rates[string(text)] = r.URL.Query().Get("rate")
		f.mu.Unlock()
		_, _ = w.Write([]byte("VOICE[" + string(text) + "]"))
	}))
	return f
}

// mp3Frames48k writes a file of silent MPEG-1 Layer III frames, 128 kbps, at 48 kHz.
func mp3Frames48k(t *testing.T, dir, name string, seconds int) string {
	t.Helper()
	const frameSize = 144 * 128000 / 48000 // 384 bytes, 0.024 s
	frames := seconds * 1000 / 24
	data := make([]byte, frames*frameSize)
	for i := 0; i < frames; i++ {
		copy(data[i*frameSize:], []byte{0xFF, 0xFB, 0x94, 0x64})
	}
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, data, 0o644))
	return path
}

// firstBytes opens a stream as a client with this User-Agent and returns what it got first.
func firstBytes(t *testing.T, url, userAgent string, size int) []byte {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("User-Agent", userAgent)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	buf := make([]byte, size)
	n, _ := io.ReadAtLeast(resp.Body, buf, size)
	return buf[:n]
}

func voices(data []byte) []string {
	var out []string
	for {
		i := bytes.Index(data, []byte("VOICE["))
		if i < 0 {
			return out
		}
		data = data[i+6:]
		j := bytes.IndexByte(data, ']')
		if j < 0 {
			return out
		}
		out = append(out, string(data[:j]))
		data = data[j+1:]
	}
}

// TestAnnouncements: the listener's receiver hears the station and the record on connecting,
// the button on arriving there and the new record after a press; other clients hear nothing,
// unless they ask with ?voice=1.
func TestAnnouncements(t *testing.T) {
	logger := slog.Default()
	speech := fakeSpeech(t)
	defer speech.Close()

	stations := radio.NewRadioStationManager(logger, createTestSentryHelper())
	defer stations.StopAll()
	server := getTestServer()
	server.SetStationManager(stations)
	server.SetRemoteControlRoutes("/an-next", "/an-prev")
	server.SetRemoteControlTiming(remoteTestRepeatGap, remoteTestMinListen)
	defer server.SetRemoteControlTiming(0, 0)
	defer server.SetRemoteControlRoutes("", "")

	files := map[string][]string{
		"/an-station": {"2019 Пси-заклинания 1", "2019 Пси-заклинания 2"},
		"/an-next":    {"next own"},
		"/an-prev":    {"prev own"},
	}
	files["/an-48k"] = []string{"Наука 48"}
	for route, names := range files {
		dir := t.TempDir()
		for i, name := range names {
			if route == "/an-48k" {
				mp3Frames48k(t, dir, name+".mp3", 60)
				continue
			}
			patternFile(t, dir, name+".mp3", 60*qualityBytesPerSec, byte(i+1))
		}
		pl, err := playlist.NewPlaylist(dir, nil, false, logger, createTestSentryHelper())
		require.NoError(t, err)
		defer pl.Close()
		streamer := newQualityStreamer()
		defer streamer.Close()
		stations.AddStation(route, streamer, pl)
		server.RegisterStream(route, streamer, pl)
	}
	announcer := announce.New(announce.Config{
		TTSURL:     speech.URL,
		CacheDir:   t.TempDir(),
		Stations:   map[string]announce.Station{"/an-station": {Preset: 32, Name: "Целительство"}},
		UserAgents: []string{"WR-230SF"},
		Logger:     logger,
	})
	server.SetAnnouncer(announcer)
	defer server.SetAnnouncer(nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	time.Sleep(300 * time.Millisecond)

	// Another client hears no announcement, unless it asks for it.
	assert.Empty(t, voices(firstBytes(t, ts.URL+"/an-station", "SomePlayer/1.0", 4096)))
	got := voices(firstBytes(t, ts.URL+"/an-station?voice=1", "SomePlayer/1.0", 4096))
	require.NotEmpty(t, got, "?voice=1 turns announcements on")

	// The receiver connecting to the station: station, record, time.
	got = voices(firstBytes(t, ts.URL+"/an-station", "WR-230SF/1.0", 4096))
	require.GreaterOrEqual(t, len(got), 2, "station and record: %v", got)
	assert.Equal(t, "Тридцать вторая станция. Целительство.", got[0])
	assert.Equal(t, "Сейчас звучит: две тысячи девятнадцать, Пси-заклинания один.", got[1])
	assert.True(t, len(got) == 2 || strings.HasPrefix(got[2], "Запись") || strings.HasPrefix(got[2], "Прошл"), "time: %v", got)

	// Listen long enough for the station to count, then arrive on the "next" button.
	listening := listenRemoteAs(t, ts.URL+"/an-station", "WR-230SF/1.0")
	time.Sleep(remoteTestMinListen + 100*time.Millisecond)
	listening.close()
	arrive := firstBytes(t, ts.URL+"/an-next", "WR-230SF/1.0", 64*1024)
	got = voices(arrive)
	require.Len(t, got, 1, "the button introduces itself: %v", got)
	assert.Equal(t, "Кнопка вперёд. Целительство. Нажмите ещё раз — следующая запись.", got[0])

	// Pressing it again: the new record is announced.
	time.Sleep(remoteTestRepeatGap)
	pressed := firstBytes(t, ts.URL+"/an-next", "WR-230SF/1.0", 32*1024)
	got = voices(pressed)
	require.NotEmpty(t, got, "the new record is announced after a press")
	assert.Equal(t, "Сейчас звучит: две тысячи девятнадцать, Пси-заклинания два.", got[0])
	assert.Equal(t, "44100", speech.rate(got[0]), "records of unknown rate get 44.1 kHz clips")

	// A station whose records are 48 kHz: the receiver goes silent when the rate changes
	// inside the stream, so the speech and the button sound are 48 kHz too.
	const ua48 = "WR-230SF/1.0 (48k test)"
	got = voices(firstBytes(t, ts.URL+"/an-48k", ua48, 4096))
	require.NotEmpty(t, got)
	assert.Equal(t, "Сейчас звучит: Наука сорок восемь.", got[0])
	assert.Equal(t, "48000", speech.rate(got[0]))

	listening = listenRemoteAs(t, ts.URL+"/an-48k", ua48)
	time.Sleep(remoteTestMinListen + 100*time.Millisecond)
	listening.close()
	arrive = firstBytes(t, ts.URL+"/an-next", ua48, 64*1024)
	button48, err := os.ReadFile(filepath.Join("..", "http", "sounds", "button-48000.mp3"))
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(arrive, button48), "the button sound is the 48 kHz one")
	got = voices(arrive)
	require.Len(t, got, 1, "%v", got)
	assert.Equal(t, "48000", speech.rate(got[0]))
}

// readFor reads a stream as a client with this User-Agent for d.
func readFor(t *testing.T, url, userAgent string, d time.Duration) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("User-Agent", userAgent)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return data
}

// TestNextRecordAnnouncement: when the station moves on to the next record while the receiver
// listens, the receiver hears "Дальше: <title>" in its stream; music stations stay silent and
// the record named on connecting is not named again.
func TestNextRecordAnnouncement(t *testing.T) {
	logger := slog.Default()
	speech := fakeSpeech(t)
	defer speech.Close()

	stations := radio.NewRadioStationManager(logger, createTestSentryHelper())
	defer stations.StopAll()
	server := getTestServer()
	server.SetStationManager(stations)

	files := map[string][]string{
		"/nx-lecture": {"Лекция 1", "Лекция 2"},
		"/nx-music":   {"Песня 1", "Песня 2"},
	}
	for route, names := range files {
		dir := t.TempDir()
		for i, name := range names {
			patternFile(t, dir, name+".mp3", 2*qualityBytesPerSec, byte(i+1))
		}
		pl, err := playlist.NewPlaylist(dir, nil, false, logger, createTestSentryHelper())
		require.NoError(t, err)
		defer pl.Close()
		streamer := newQualityStreamer()
		defer streamer.Close()
		stations.AddStation(route, streamer, pl)
		server.RegisterStream(route, streamer, pl)
	}
	server.SetAnnouncer(announce.New(announce.Config{
		TTSURL:   speech.URL,
		CacheDir: t.TempDir(),
		Stations: map[string]announce.Station{
			"/nx-lecture": {Preset: 5, Name: "Лекции"},
			"/nx-music":   {Preset: 6, Name: "Песни", Music: true},
		},
		UserAgents: []string{"WR-230SF"},
		Logger:     logger,
	}))
	defer server.SetAnnouncer(nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	time.Sleep(300 * time.Millisecond)

	// Two-second records: listening for five seconds passes at least two record starts.
	got := voices(readFor(t, ts.URL+"/nx-lecture", "WR-230SF/1.0 (next test)", 5500*time.Millisecond))
	require.GreaterOrEqual(t, len(got), 3, "connect announcement and the next records: %v", got)
	assert.Equal(t, "Пятая станция. Лекции.", got[0])
	require.True(t, strings.HasPrefix(got[1], "Сейчас звучит: Лекция "), "%v", got)
	first := strings.TrimSuffix(strings.TrimPrefix(got[1], "Сейчас звучит: "), ".")
	var nexts []string
	for _, v := range got {
		if strings.HasPrefix(v, "Дальше: ") {
			nexts = append(nexts, strings.TrimSuffix(strings.TrimPrefix(v, "Дальше: "), "."))
		}
	}
	require.GreaterOrEqual(t, len(nexts), 2, "each next record is named: %v", got)
	assert.NotEqual(t, first, nexts[0], "the record named on connecting is not named again: %v", got)
	for i := 1; i < len(nexts); i++ {
		assert.NotEqual(t, nexts[i-1], nexts[i], "records alternate: %v", got)
	}

	// Other players and music stations hear no "Дальше".
	for _, v := range voices(readFor(t, ts.URL+"/nx-lecture", "SomePlayer/1.0", 4500*time.Millisecond)) {
		assert.NotContains(t, v, "Дальше")
	}
	for _, v := range voices(readFor(t, ts.URL+"/nx-music", "WR-230SF/1.0 (next test)", 4500*time.Millisecond)) {
		assert.NotContains(t, v, "Дальше", "music stations do not announce each song")
	}
}

// TestSampleRate: the rate is read from the first confirmed Layer III frame, after an ID3 tag.
func TestSampleRate(t *testing.T) {
	dir := t.TempDir()
	assert.Equal(t, 48000, announce.SampleRate(mp3Frames48k(t, dir, "a.mp3", 1)))

	tagged := append([]byte("ID3\x04\x00\x00\x00\x00\x00\x20"), make([]byte, 32)...)
	plain, err := os.ReadFile(filepath.Join(dir, "a.mp3"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.mp3"), append(tagged, plain...), 0o644))
	assert.Equal(t, 48000, announce.SampleRate(filepath.Join(dir, "b.mp3")))

	for _, name := range []string{"next.mp3", "next-48000.mp3", "next-32000.mp3", "next-16000.mp3"} {
		want := map[string]int{"next.mp3": 44100, "next-48000.mp3": 48000, "next-32000.mp3": 32000, "next-16000.mp3": 16000}[name]
		assert.Equal(t, want, announce.SampleRate(filepath.Join("..", "http", "sounds", name)), name)
	}
	patternFile(t, dir, "c.mp3", 64*1024, 1)
	assert.Equal(t, 0, announce.SampleRate(filepath.Join(dir, "c.mp3")), "not MP3 audio")
	assert.Equal(t, 0, announce.SampleRate(filepath.Join(dir, "missing.mp3")))
}
