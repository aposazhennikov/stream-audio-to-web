package unit_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/aposazhennikov/stream-audio-to-web/playlist"
	"github.com/aposazhennikov/stream-audio-to-web/unit/testdata"
	"github.com/stretchr/testify/require"
)

func playlistNames(pl *playlist.Playlist) []string {
	var names []string
	for _, track := range pl.GetTracks() {
		names = append(names, track.Name)
	}
	sort.Strings(names)
	return names
}

// TestPlaylistFollowsDirectoryChanges: files added, renamed and removed while the station runs
// (the daily download, renaming for announcements) reach the playlist without a restart.
func TestPlaylistFollowsDirectoryChanges(t *testing.T) {
	dir := t.TempDir()
	data := testdata.GetMinimumMP3Data()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "01 first.mp3"), data, 0o644))

	pl, err := playlist.NewPlaylist(dir, nil, false, slog.Default(), createTestSentryHelper())
	require.NoError(t, err)
	defer pl.Close()
	require.Equal(t, []string{"01 first.mp3"}, playlistNames(pl))

	eventuallyNames := func(want []string, msg string) {
		t.Helper()
		require.Eventually(t, func() bool {
			got := playlistNames(pl)
			if len(got) != len(want) {
				return false
			}
			for i := range want {
				if got[i] != want[i] {
					return false
				}
			}
			return true
		}, 3*time.Second, 20*time.Millisecond, msg)
	}

	require.NoError(t, os.WriteFile(filepath.Join(dir, "02 second.mp3"), data, 0o644))
	eventuallyNames([]string{"01 first.mp3", "02 second.mp3"}, "a new file must appear")

	require.NoError(t, os.Rename(filepath.Join(dir, "02 second.mp3"), filepath.Join(dir, "02 Второй.mp3")))
	eventuallyNames([]string{"01 first.mp3", "02 Второй.mp3"}, "a renamed file must appear under its new name")

	require.NoError(t, os.Remove(filepath.Join(dir, "01 first.mp3")))
	eventuallyNames([]string{"02 Второй.mp3"}, "a removed file must disappear")
}
