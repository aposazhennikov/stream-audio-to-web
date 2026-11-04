package unit_test

import (
	"bufio"
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aposazhennikov/stream-audio-to-web/playlist"
	"github.com/aposazhennikov/stream-audio-to-web/radio"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// remoteSignalStart is how the remote control signals begin (MPEG-1 Layer III frame header).
// The test audio files never contain these bytes.
var remoteSignalStart = []byte{0xFF, 0xFB, 0x90, 0x64}

const (
	remoteTestHold       = 800 * time.Millisecond
	remoteTestSeriesHold = 100 * time.Millisecond
	remoteTestMinListen  = 300 * time.Millisecond
	remoteTestPass       = 50 * time.Millisecond // a preset passed while scrolling
)

// remoteListener is a receiver that asks for titles; it reads the stream in the background.
type remoteListener struct {
	mu     sync.Mutex
	audio  []byte
	titles []string
	body   io.Closer
	done   chan struct{}
}

func listenRemote(t *testing.T, url string) *remoteListener {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("Icy-MetaData", "1")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	metaInt, err := strconv.Atoi(resp.Header.Get("icy-metaint"))
	require.NoError(t, err)

	l := &remoteListener{body: resp.Body, done: make(chan struct{})}
	go func() {
		defer close(l.done)
		reader := bufio.NewReader(resp.Body)
		block := make([]byte, metaInt)
		for {
			if _, readErr := io.ReadFull(reader, block); readErr != nil {
				return
			}
			lengthByte, readErr := reader.ReadByte()
			if readErr != nil {
				return
			}
			meta := make([]byte, int(lengthByte)*16)
			if _, readErr = io.ReadFull(reader, meta); readErr != nil {
				return
			}
			l.mu.Lock()
			l.audio = append(l.audio, block...)
			if lengthByte > 0 {
				title := strings.TrimSuffix(string(bytes.TrimRight(meta, "\x00")), "';")
				l.titles = append(l.titles, strings.TrimPrefix(title, "StreamTitle='"))
			}
			l.mu.Unlock()
		}
	}()
	return l
}

// pass opens a preset for a moment, as when scrolling through the presets.
func passRemote(t *testing.T, url string) {
	t.Helper()
	l := listenRemote(t, url)
	time.Sleep(remoteTestPass)
	l.close()
}

func (l *remoteListener) close() {
	l.body.Close()
	<-l.done
}

// heardSignal reports whether a remote control signal was received.
func (l *remoteListener) heardSignal() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.Contains(l.audio, remoteSignalStart)
}

func (l *remoteListener) lastTitle() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.titles) == 0 {
		return ""
	}
	return l.titles[len(l.titles)-1]
}

func currentTrackName(t *testing.T, pl *playlist.Playlist) string {
	t.Helper()
	track, ok := pl.GetCurrentTrack().(interface{ GetPath() string })
	require.True(t, ok)
	name := filepath.Base(track.GetPath())
	return strings.TrimSuffix(name, filepath.Ext(name))
}

// TestRemoteControlButtons: staying on a button URL moves the station the listener was
// listening to (not the ones passed on the way) and keeps playing it; passing the buttons
// while scrolling, reconnects and duplicate connections do not move it; a receiver turned on
// straight on a button gets the button's own station.
func TestRemoteControlButtons(t *testing.T) {
	logger := slog.Default()
	stations := radio.NewRadioStationManager(logger, createTestSentryHelper())
	defer stations.StopAll()
	server := getTestServer()
	server.SetStationManager(stations)
	server.SetRemoteControlRoutes("/rc-next", "/rc-prev")
	server.SetRemoteControlTiming(remoteTestHold, remoteTestSeriesHold, remoteTestMinListen)
	defer server.SetRemoteControlTiming(0, 0, 0)
	defer server.SetRemoteControlRoutes("", "")

	// The station the listener uses, another one passed while scrolling, and the two
	// stations stored on the button presets.
	files := map[string][]string{
		"/rc-station": {"01 first", "02 second", "03 third"},
		"/rc-other":   {"other 1", "other 2"},
		"/rc-next":    {"next own"},
		"/rc-prev":    {"prev own"},
	}
	playlists := map[string]*playlist.Playlist{}
	for route, names := range files {
		dir := t.TempDir()
		for i, name := range names {
			patternFile(t, dir, name+".mp3", 60*qualityBytesPerSec, byte(i+1))
		}
		pl, err := playlist.NewPlaylist(dir, nil, false, logger, createTestSentryHelper())
		require.NoError(t, err)
		defer pl.Close()
		playlists[route] = pl
		streamer := newQualityStreamer()
		defer streamer.Close()
		stations.AddStation(route, streamer, pl)
		server.RegisterStream(route, streamer, pl)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	time.Sleep(300 * time.Millisecond)
	station := playlists["/rc-station"]
	afterHold := func() { time.Sleep(remoteTestHold + 400*time.Millisecond) }
	eventually := func(cond func() bool, msg string) {
		t.Helper()
		require.Eventually(t, cond, 3*time.Second, 20*time.Millisecond, msg)
	}

	// The receiver plays its station long enough for it to count as listened.
	listening := listenRemote(t, ts.URL+"/rc-station")
	eventually(func() bool { return listening.lastTitle() == "01 first" }, "station title")
	time.Sleep(remoteTestMinListen + 100*time.Millisecond)

	// A HEAD request (some receivers probe first) is not a press.
	head, err := http.Head(ts.URL + "/rc-next")
	require.NoError(t, err)
	head.Body.Close()

	// Scrolling through the presets: passing both buttons quickly moves nothing.
	listening.close()
	passRemote(t, ts.URL+"/rc-next")
	passRemote(t, ts.URL+"/rc-prev")
	listening = listenRemote(t, ts.URL+"/rc-station")
	afterHold()
	assert.Equal(t, "01 first", currentTrackName(t, station), "scrolling past the buttons must not switch")
	assert.False(t, listening.heardSignal())

	// Scrolling to the "next" button past another station and staying there: the station
	// the listener was listening to keeps playing, then moves forward with a signal.
	listening.close()
	passRemote(t, ts.URL+"/rc-other")
	pressed := listenRemote(t, ts.URL+"/rc-next")
	eventually(func() bool { return pressed.lastTitle() == "01 first" },
		"until the hold time the listener hears the station it was listening to")
	assert.Equal(t, "01 first", currentTrackName(t, station))
	eventually(pressed.heardSignal, "signal after the hold time")
	assert.Equal(t, "02 second", currentTrackName(t, station))
	assert.Equal(t, "other 1", currentTrackName(t, playlists["/rc-other"]), "a passed station must not move")
	eventually(func() bool { return pressed.lastTitle() == "02 second" }, "new track title")

	// A duplicate connection while the first is open is not a second press.
	duplicate := listenRemote(t, ts.URL+"/rc-next")
	afterHold()
	assert.Equal(t, "02 second", currentTrackName(t, station))
	assert.False(t, duplicate.heardSignal())
	assert.Equal(t, "02 second", duplicate.lastTitle())
	duplicate.close()
	pressed.close()

	// A reconnect after a dropped connection is not a press either.
	reconnect := listenRemote(t, ts.URL+"/rc-next")
	afterHold()
	assert.Equal(t, "02 second", currentTrackName(t, station))
	assert.False(t, reconnect.heardSignal())
	assert.Equal(t, "02 second", reconnect.lastTitle())
	reconnect.close()

	// Staying on "previous" right after: the station goes back.
	back := listenRemote(t, ts.URL+"/rc-prev")
	eventually(back.heardSignal, "signal on previous")
	assert.Equal(t, "01 first", currentTrackName(t, station))
	eventually(func() bool { return back.lastTitle() == "01 first" }, "previous track title")
	back.close()

	// "Next" twice: step to the neighbouring preset and back. The repeat right after a
	// switch acts after the short series hold.
	again := listenRemote(t, ts.URL+"/rc-next")
	eventually(again.heardSignal, "first next")
	again.close()
	assert.Equal(t, "02 second", currentTrackName(t, station))
	passRemote(t, ts.URL+"/rc-prev")
	again = listenRemote(t, ts.URL+"/rc-next")
	require.Eventually(t, again.heardSignal, remoteTestHold/2, 10*time.Millisecond,
		"a repeated press in a series must act sooner than the full hold")
	assert.Equal(t, "03 third", currentTrackName(t, station))
	again.close()

	// Leaving the buttons by scrolling past the other one: it keeps the full hold, so a
	// moment longer than the series hold on it does not switch back.
	leaving := listenRemote(t, ts.URL+"/rc-prev")
	time.Sleep(3 * remoteTestSeriesHold)
	leaving.close()
	afterHold()
	assert.Equal(t, "03 third", currentTrackName(t, station), "the other button must not use the series hold")

	// Turned on straight on the preset (nothing listened to recently): its own station plays.
	server.SetRemoteControlRoutes("/rc-next", "/rc-prev") // forgets recent listening
	own := listenRemote(t, ts.URL+"/rc-next")
	eventually(func() bool { return own.lastTitle() == "next own" }, "own station title")
	afterHold()
	assert.False(t, own.heardSignal())
	assert.Equal(t, "03 third", currentTrackName(t, station))
	own.close()
}
