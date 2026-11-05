package unit_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aposazhennikov/stream-audio-to-web/announce"
	"github.com/aposazhennikov/stream-audio-to-web/playlist"
	"github.com/aposazhennikov/stream-audio-to-web/radio"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSpeech is a speech service that returns "VOICE[<text>]" instead of audio.
func fakeSpeech(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		text, _ := io.ReadAll(r.Body)
		_, _ = w.Write([]byte("VOICE[" + string(text) + "]"))
	}))
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
	for route, names := range files {
		dir := t.TempDir()
		for i, name := range names {
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
}
