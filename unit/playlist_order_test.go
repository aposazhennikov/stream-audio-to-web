package unit_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aposazhennikov/stream-audio-to-web/playlist"
	"github.com/aposazhennikov/stream-audio-to-web/unit/testdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPlaylistNewestFirst: a station with daily downloads plays the newest file first.
func TestPlaylistNewestFirst(t *testing.T) {
	dir := t.TempDir()
	data := testdata.GetMinimumMP3Data()
	base := time.Date(2026, 11, 5, 2, 0, 0, 0, time.UTC)
	files := map[string]time.Duration{ // name -> age
		"A channel - old.mp3":    72 * time.Hour,
		"Z channel - newest.mp3": 0,
		"M channel - middle.mp3": 24 * time.Hour,
	}
	for name, age := range files {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, data, 0o644))
		require.NoError(t, os.Chtimes(path, base.Add(-age), base.Add(-age)))
	}

	pl, err := playlist.NewPlaylist(dir, nil, false, slog.Default(), createTestSentryHelper())
	require.NoError(t, err)
	defer pl.Close()
	assert.Equal(t, "A channel - old.mp3", currentTrackName(t, pl)+".mp3", "by name before the switch")

	pl.SetNewestFirst(true)
	names := []string{}
	for _, track := range pl.GetTracks() {
		names = append(names, track.Name)
	}
	assert.Equal(t, []string{"Z channel - newest.mp3", "M channel - middle.mp3", "A channel - old.mp3"}, names)
	assert.Equal(t, "A channel - old", currentTrackName(t, pl), "the current track is kept when the order changes")
}

// TestPlaylistSetCurrentTrackByName: a station can be put on a track by its file name.
func TestPlaylistSetCurrentTrackByName(t *testing.T) {
	dir := t.TempDir()
	data := testdata.GetMinimumMP3Data()
	for _, name := range []string{"01 a.mp3", "02 b.mp3", "03 c.mp3"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o644))
	}
	pl, err := playlist.NewPlaylist(dir, nil, false, slog.Default(), createTestSentryHelper())
	require.NoError(t, err)
	defer pl.Close()

	assert.True(t, pl.SetCurrentTrackByName("03 c.mp3"))
	assert.Equal(t, "03 c", currentTrackName(t, pl))
	assert.False(t, pl.SetCurrentTrackByName("gone.mp3"))
	assert.Equal(t, "03 c", currentTrackName(t, pl), "an unknown name keeps the current track")
}
