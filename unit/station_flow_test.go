package unit_test

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aposazhennikov/stream-audio-to-web/playlist"
	"github.com/aposazhennikov/stream-audio-to-web/radio"
	"github.com/aposazhennikov/stream-audio-to-web/unit/testdata"
	"github.com/stretchr/testify/require"
)

// recordingStreamer records which tracks the station plays and lets the test finish
// the current track "naturally". StopCurrentTrack interrupts it like the real streamer.
type recordingStreamer struct {
	mu      sync.Mutex
	played  []string
	finish  chan struct{} // closed by FinishTrack: the track reached its end
	stopped chan struct{} // closed by StopCurrentTrack: manual interruption
	active  int
	maxRun  int
}

func newRecordingStreamer() *recordingStreamer {
	return &recordingStreamer{stopped: make(chan struct{})}
}

func (r *recordingStreamer) StreamTrack(trackPath string) error {
	r.mu.Lock()
	r.played = append(r.played, filepath.Base(trackPath))
	finish := make(chan struct{})
	r.finish = finish
	stopped := r.stopped
	r.active++
	if r.active > r.maxRun {
		r.maxRun = r.active
	}
	r.mu.Unlock()

	select {
	case <-finish:
	case <-stopped:
	}

	r.mu.Lock()
	r.active--
	r.mu.Unlock()
	return nil
}

func (r *recordingStreamer) StopCurrentTrack() {
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-r.stopped:
	default:
		close(r.stopped)
	}
	r.stopped = make(chan struct{})
}

func (r *recordingStreamer) Close()              {}
func (r *recordingStreamer) GetClientCount() int { return 0 }

// FinishTrack makes the currently playing track end as if the file was fully played.
func (r *recordingStreamer) FinishTrack(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	finish := r.finish
	r.finish = nil
	r.mu.Unlock()
	require.NotNil(t, finish, "no track is playing")
	close(finish)
}

func (r *recordingStreamer) Played() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.played...)
}

// waitPlayed waits until the streamer has started n tracks and returns the list.
func (r *recordingStreamer) waitPlayed(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if played := r.Played(); len(played) >= n {
			// Give the station a moment: a wrong extra start would show up here.
			time.Sleep(150 * time.Millisecond)
			return r.Played()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d started tracks, got %v", n, r.Played())
	return nil
}

// newTestStation creates a station over a real playlist with tracks 1.mp3..n.mp3.
func newTestStation(t *testing.T, n int) (*radio.Station, *recordingStreamer, *playlist.Playlist) {
	t.Helper()
	dir := t.TempDir()
	for i := 1; i <= n; i++ {
		path := filepath.Join(dir, fmt.Sprintf("%d.mp3", i))
		require.NoError(t, os.WriteFile(path, testdata.GetMinimumMP3Data(), 0644))
	}
	pl, err := playlist.NewPlaylist(dir, nil, false, slog.Default(), createTestSentryHelper())
	require.NoError(t, err)

	streamer := newRecordingStreamer()
	station := radio.NewRadioStation("/flow-test", streamer, pl, slog.Default(), createTestSentryHelper())
	station.Start()
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() {
			station.Stop()
			close(done)
		}()
		streamer.StopCurrentTrack()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("station did not stop")
		}
		streamer.StopCurrentTrack()
		_ = pl.Close()
	})
	return station, streamer, pl
}

func TestStationPlaysTracksInOrderAndWraps(t *testing.T) {
	_, streamer, _ := newTestStation(t, 3)

	streamer.waitPlayed(t, 1)
	for i := 0; i < 3; i++ {
		streamer.FinishTrack(t)
		streamer.waitPlayed(t, i+2)
	}

	require.Equal(t, []string{"1.mp3", "2.mp3", "3.mp3", "1.mp3"}, streamer.Played())
}

func TestStationPreviousTrackThenNaturalEndGoesForward(t *testing.T) {
	station, streamer, pl := newTestStation(t, 4)

	streamer.waitPlayed(t, 1)
	streamer.FinishTrack(t)
	streamer.waitPlayed(t, 2)
	streamer.FinishTrack(t)
	streamer.waitPlayed(t, 3) // 3.mp3 is playing

	pl.PreviousTrack()
	station.RestartPlayback()
	streamer.waitPlayed(t, 4) // 2.mp3 after "Previous track"

	streamer.FinishTrack(t)
	played := streamer.waitPlayed(t, 5)

	require.Equal(t, []string{"1.mp3", "2.mp3", "3.mp3", "2.mp3", "3.mp3"}, played,
		"after a manual switch the next natural transition must go to the following track")
}

func TestStationNextTrackThenNaturalEndGoesForward(t *testing.T) {
	station, streamer, pl := newTestStation(t, 4)

	streamer.waitPlayed(t, 1)
	pl.NextTrack()
	station.RestartPlayback()
	streamer.waitPlayed(t, 2) // 2.mp3

	streamer.FinishTrack(t)
	played := streamer.waitPlayed(t, 3)

	require.Equal(t, []string{"1.mp3", "2.mp3", "3.mp3"}, played)
}

func TestStationRepeatedPreviousTrackClicks(t *testing.T) {
	station, streamer, pl := newTestStation(t, 10)

	streamer.waitPlayed(t, 1)
	for i := 0; i < 9; i++ {
		pl.NextTrack()
	}
	station.RestartPlayback()
	streamer.waitPlayed(t, 2) // 10.mp3

	// Like pressing "Previous track" nine times, about once per second (here faster).
	for i := 0; i < 9; i++ {
		pl.PreviousTrack()
		station.RestartPlayback()
		time.Sleep(30 * time.Millisecond)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		played := streamer.Played()
		if played[len(played)-1] == "1.mp3" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	played := streamer.Played()
	require.Equal(t, "1.mp3", played[len(played)-1], "station must end up on track 1, played: %v", played)

	streamer.mu.Lock()
	maxRun := streamer.maxRun
	streamer.mu.Unlock()
	require.Equal(t, 1, maxRun, "two tracks played at the same time")

	// The stream keeps going after the clicks.
	n := len(played)
	streamer.FinishTrack(t)
	played = streamer.waitPlayed(t, n+1)
	require.Equal(t, "2.mp3", played[len(played)-1])
}
