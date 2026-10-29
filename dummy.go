package main

import (
	"time"

	httpServer "github.com/aposazhennikov/stream-audio-to-web/http"
)

// This is necessary for quickly passing healthcheck before loading real streams.
func createDummyStreamAndPlaylist() (httpServer.StreamHandler, httpServer.PlaylistManager) {
	// Placeholder for StreamHandler.
	dummyStream := &dummyStreamHandler{
		clientCounter: 0,
		clientCh:      make(chan string, 1),
	}

	// Placeholder for PlaylistManager.
	dummyPlaylist := &dummyPlaylistManager{}

	return dummyStream, dummyPlaylist
}

// Minimal implementation of StreamHandler for placeholder.
type dummyStreamHandler struct {
	clientCounter int32
	clientCh      chan string
}

func (d *dummyStreamHandler) AddClient() (<-chan []byte, int, error) {
	// Empty channel that will be replaced with real stream later.
	ch := make(chan []byte, 1)
	return ch, 0, nil
}

func (d *dummyStreamHandler) RemoveClient(_ int) {
	// Do nothing.
}

func (d *dummyStreamHandler) GetClientCount() int {
	return 0
}

func (d *dummyStreamHandler) GetCurrentTrackChannel() <-chan string {
	return d.clientCh
}

func (d *dummyStreamHandler) GetPlaybackInfo() (string, time.Time, time.Duration, time.Duration) {
	return "", time.Time{}, time.Duration(0), time.Duration(0)
}

// getDummyMP3 returns the name of a dummy MP3 file.
func getDummyMP3() string {
	return "dummy.mp3"
}

// Minimal implementation of PlaylistManager for placeholder.
type dummyPlaylistManager struct{}

func (d *dummyPlaylistManager) Reload() error {
	return nil
}

func (d *dummyPlaylistManager) GetCurrentTrack() interface{} {
	return getDummyMP3()
}

func (d *dummyPlaylistManager) NextTrack() interface{} {
	return getDummyMP3()
}

func (d *dummyPlaylistManager) GetHistory() []interface{} {
	return []interface{}{}
}

func (d *dummyPlaylistManager) GetStartTime() time.Time {
	return time.Now()
}

func (d *dummyPlaylistManager) PreviousTrack() interface{} {
	return getDummyMP3()
}

// Shuffle implements PlaylistManager.Shuffle method.
func (d *dummyPlaylistManager) Shuffle() {
	// Empty implementation for dummy placeholder.
}

// GetShuffleEnabled implements PlaylistManager.GetShuffleEnabled method.
func (d *dummyPlaylistManager) GetShuffleEnabled() bool {
	// Dummy implementation - always return false.
	return false
}

// SetShuffleEnabled implements PlaylistManager.SetShuffleEnabled method.
func (d *dummyPlaylistManager) SetShuffleEnabled(enabled bool) {
	// Empty implementation for dummy placeholder.
}
