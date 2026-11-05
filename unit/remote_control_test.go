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

// remoteSoundStart is how the remote control sounds begin (MPEG-1 Layer III frame header).
// The test audio files never contain these bytes.
var remoteSoundStart = []byte{0xFF, 0xFB, 0x90, 0x64}

const (
	remoteTestRepeatGap = 400 * time.Millisecond
	remoteTestMinListen = 300 * time.Millisecond
	remoteTestPass      = 50 * time.Millisecond // a preset passed while scrolling
	remoteTestReceiver  = "TestReceiver/1.0"
)

// remoteListener is a receiver that asks for titles; it reads the stream in the background.
type remoteListener struct {
	mu        sync.Mutex
	audio     []byte
	titles    []string
	body      io.Closer
	done      chan struct{}
	closeOnce sync.Once
}

func listenRemote(t *testing.T, url string) *remoteListener {
	t.Helper()
	return listenRemoteAs(t, url, remoteTestReceiver)
}

func listenRemoteAs(t *testing.T, url, userAgent string) *remoteListener {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("Icy-MetaData", "1")
	req.Header.Set("User-Agent", userAgent)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	metaInt, err := strconv.Atoi(resp.Header.Get("icy-metaint"))
	require.NoError(t, err)

	l := &remoteListener{body: resp.Body, done: make(chan struct{})}
	// A failed assertion must not leave the connection open: the test server would wait for it forever.
	t.Cleanup(l.close)
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

// passRemote opens a preset for a moment, as when scrolling through the presets.
func passRemote(t *testing.T, url string) {
	t.Helper()
	l := listenRemote(t, url)
	time.Sleep(remoteTestPass)
	l.close()
}

func (l *remoteListener) close() {
	l.closeOnce.Do(func() {
		l.body.Close()
		<-l.done
	})
}

// heardSound reports whether a remote control sound (button ticks or switch beeps) was received.
func (l *remoteListener) heardSound() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.Contains(l.audio, remoteSoundStart)
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

// TestRemoteControlButtons: arriving on a button only introduces it, however long the
// listener stays; pressing the same preset again moves the station listened to (not the ones
// passed on the way); duplicate connections do not; the other button undoes it the same way;
// another device behind the same address has its own session; a receiver turned on straight
// on a button gets the button's own station.
func TestRemoteControlButtons(t *testing.T) {
	logger := slog.Default()
	stations := radio.NewRadioStationManager(logger, createTestSentryHelper())
	defer stations.StopAll()
	server := getTestServer()
	server.SetStationManager(stations)
	server.SetRemoteControlRoutes("/rc-next", "/rc-prev")
	server.SetRemoteControlTiming(remoteTestRepeatGap, remoteTestMinListen)
	defer server.SetRemoteControlTiming(0, 0)
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
	eventually := func(cond func() bool, msg string) {
		t.Helper()
		require.Eventually(t, cond, 3*time.Second, 20*time.Millisecond, msg)
	}
	afterGap := func() { time.Sleep(remoteTestRepeatGap + 100*time.Millisecond) }

	// The receiver plays its station long enough for it to count as listened.
	listening := listenRemote(t, ts.URL+"/rc-station")
	eventually(func() bool { return listening.lastTitle() == "01 first" }, "station title")
	assert.False(t, listening.heardSound(), "an ordinary station has no sound inserted")
	time.Sleep(remoteTestMinListen + 100*time.Millisecond)

	// A HEAD request (some receivers probe first) does nothing.
	head, err := http.Head(ts.URL + "/rc-next")
	require.NoError(t, err)
	head.Body.Close()

	// Scrolling past another station to the "next" button and staying there to listen: the
	// button introduces itself, the station listened to keeps playing and does not move.
	listening.close()
	passRemote(t, ts.URL+"/rc-other")
	arrived := listenRemote(t, ts.URL+"/rc-next")
	duplicate := listenRemote(t, ts.URL+"/rc-next") // the receiver's duplicate for the same arrival
	eventually(arrived.heardSound, "the button introduces itself with a sound")
	eventually(func() bool { return arrived.lastTitle() == "01 first" }, "the listened station keeps playing on the button")
	time.Sleep(3 * remoteTestRepeatGap) // stays on the button much longer than any gap
	assert.Equal(t, "01 first", currentTrackName(t, station), "staying on a button must not switch")
	assert.Equal(t, "other 1", currentTrackName(t, playlists["/rc-other"]), "a passed station must not move")
	assert.False(t, duplicate.heardSound(), "a duplicate connection is not introduced again")
	duplicate.close()
	arrived.close()

	// Pressing the same preset again: the station moves forward with a signal. The receiver
	// opens a duplicate connection for the same press right away; it does not switch again.
	pressed := listenRemote(t, ts.URL+"/rc-next")
	pressedDuplicate := listenRemote(t, ts.URL+"/rc-next")
	eventually(pressed.heardSound, "signal after the press")
	assert.Equal(t, "02 second", currentTrackName(t, station))
	eventually(func() bool { return pressed.lastTitle() == "02 second" }, "new track title")
	afterGap()
	assert.Equal(t, "02 second", currentTrackName(t, station))
	pressedDuplicate.close()
	pressed.close()

	// And again: one more record.
	again := listenRemote(t, ts.URL+"/rc-next")
	eventually(again.heardSound, "signal after the second press")
	assert.Equal(t, "03 third", currentTrackName(t, station))
	again.close()

	// Undo with the other button: arriving introduces it, pressing it again goes back.
	undoArrive := listenRemote(t, ts.URL+"/rc-prev")
	eventually(undoArrive.heardSound, "the other button introduces itself")
	afterGap()
	assert.Equal(t, "03 third", currentTrackName(t, station), "arriving on the other button must not switch")
	undoArrive.close()
	undo := listenRemote(t, ts.URL+"/rc-prev")
	eventually(undo.heardSound, "signal after the undo press")
	assert.Equal(t, "02 second", currentTrackName(t, station))
	eventually(func() bool { return undo.lastTitle() == "02 second" }, "previous track title")
	undo.close()

	// Another device behind the same address (a phone at home) has a session of its own:
	// it never listened to anything, so the preset plays its own station and moves nothing.
	phone := listenRemoteAs(t, ts.URL+"/rc-next", "SomePhone/2.0")
	eventually(func() bool { return phone.lastTitle() == "next own" }, "own station for another device")
	afterGap()
	assert.False(t, phone.heardSound())
	assert.Equal(t, "02 second", currentTrackName(t, station))
	phone.close()

	// Turned on straight on the preset (nothing listened to recently): its own station plays.
	// Even after listening to it for a while, pressing it again does not switch the button's
	// own station (a button station is never the one listened to).
	server.SetRemoteControlRoutes("/rc-next", "/rc-prev") // forgets recent listening
	own := listenRemote(t, ts.URL+"/rc-next")
	eventually(func() bool { return own.lastTitle() == "next own" }, "own station title")
	time.Sleep(remoteTestMinListen + 100*time.Millisecond)
	own.close()
	afterGap()
	ownAgain := listenRemote(t, ts.URL+"/rc-next")
	eventually(func() bool { return ownAgain.lastTitle() == "next own" }, "own station again")
	assert.False(t, ownAgain.heardSound())
	assert.Equal(t, "02 second", currentTrackName(t, station))
	ownAgain.close()
}
