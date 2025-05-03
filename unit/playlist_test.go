package unit

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/user/stream-audio-to-web/playlist"
)

// Minimal valid audio files for testing
var (
	// Minimal valid MP3 file (data frame)
	minimumMP3Data = []byte{
		0xFF, 0xFB, 0x90, 0x64, // MPEG-1 Layer 3 header
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // Minimal data
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}

	// Minimal valid WAV file (44 bytes header + minimal data)
	minimumWAVData = []byte{
		// RIFF header
		0x52, 0x49, 0x46, 0x46, // "RIFF"
		0x24, 0x00, 0x00, 0x00, // Chunk size (36 + data size)
		0x57, 0x41, 0x56, 0x45, // "WAVE"
		// fmt subchunk
		0x66, 0x6D, 0x74, 0x20, // "fmt "
		0x10, 0x00, 0x00, 0x00, // Subchunk size (16 bytes)
		0x01, 0x00,             // Audio format (1 = PCM)
		0x01, 0x00,             // Number of channels (1 = mono)
		0x44, 0xAC, 0x00, 0x00, // Sample rate (44100 Hz)
		0x88, 0x58, 0x01, 0x00, // Byte rate (44100 * 1 * 16/8)
		0x02, 0x00,             // Block align (channels * bits/sample / 8)
		0x10, 0x00,             // Bits per sample (16 bits)
		// data subchunk
		0x64, 0x61, 0x74, 0x61, // "data"
		0x00, 0x00, 0x00, 0x00, // Data size (0 bytes)
		// Minimal data (1 sample)
		0x00, 0x00,
	}

	// Minimal valid OGG file (header only without data)
	minimumOGGData = []byte{
		// OGG header
		0x4F, 0x67, 0x67, 0x53, // "OggS"
		0x00,                   // Version
		0x02,                   // Header type
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // Granule position (0)
		0x01, 0x02, 0x03, 0x04, // Serial number (arbitrary)
		0x00, 0x00, 0x00, 0x00, // Page number
		0x01, 0x00, 0x00, 0x00, // CRC checksum
		0x01,                   // Number of segments
		0x1E,                   // Segment size (30 bytes)
		// Vorbis header (simplified)
		0x01, 0x76, 0x6F, 0x72, 0x62, 0x69, 0x73, // "\x01vorbis"
		0x00, 0x00, 0x00, 0x00, // Vorbis version
		0x01,                   // Channels (1)
		0x44, 0xAC, 0x00, 0x00, // Sample rate (44100 Hz)
		0x00, 0x00, 0x00, 0x00, // Bitrate maximum
		0x00, 0x00, 0x00, 0x00, // Bitrate nominal
		0x00, 0x00, 0x00, 0x00, // Bitrate minimum
		0x00,                   // Blocksize
	}
)

// Creating different audio file types for testing
func createTestAudioFiles(dir string) error {
	files := []struct {
		name string
		data []byte
	}{
		{"test1.mp3", minimumMP3Data},
		{"test2.mp3", minimumMP3Data},
		{"test3.wav", minimumWAVData},
		{"test4.ogg", minimumOGGData},
		{"test5.mp3", minimumMP3Data},
	}

	for _, file := range files {
		filePath := filepath.Join(dir, file.name)
		if err := ioutil.WriteFile(filePath, file.data, 0644); err != nil {
			return err
		}
	}
	return nil
}

func TestPlaylist_GetCurrentTrack(t *testing.T) {
	// Create a temporary directory for tests
	tmpDir, err := ioutil.TempDir("", "playlist-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create various audio files for testing
	if err := createTestAudioFiles(tmpDir); err != nil {
		t.Fatalf("Failed to create test audio files: %v", err)
	}

	// Initialize playlist
	pl, err := playlist.NewPlaylist(tmpDir, nil, false)
	if err != nil {
		t.Fatalf("Failed to create playlist: %v", err)
	}

	// Allow time for playlist initialization
	time.Sleep(200 * time.Millisecond)

	// Check that the current track is not empty
	track := pl.GetCurrentTrack()
	if track == nil {
		t.Fatalf("Expected current track to not be nil")
	}

	// Check that the track has a path
	if track, ok := track.(interface{ GetPath() string }); !ok || track.GetPath() == "" {
		t.Fatalf("Expected track to have a valid path")
	}

	// Check that history starts with the current track
	history := pl.GetHistory()
	if len(history) == 0 {
		t.Fatalf("Expected history to contain at least one item")
	}

	t.Logf("Current track: %v", track)
	t.Logf("History: %v", history)
}

func TestPlaylist_NextTrack(t *testing.T) {
	// Create a temporary directory for tests
	tmpDir, err := ioutil.TempDir("", "playlist-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create various audio files for testing
	if err := createTestAudioFiles(tmpDir); err != nil {
		t.Fatalf("Failed to create test audio files: %v", err)
	}

	// Initialize playlist
	pl, err := playlist.NewPlaylist(tmpDir, nil, false)
	if err != nil {
		t.Fatalf("Failed to create playlist: %v", err)
	}

	// Allow time for playlist initialization
	time.Sleep(200 * time.Millisecond)

	// Get current track
	currentTrack := pl.GetCurrentTrack()
	if currentTrack == nil {
		t.Fatalf("Expected current track to not be nil before switching")
	}

	// Log current state for debugging
	t.Logf("Initial track: %v", currentTrack)
	t.Logf("Initial history: %v", pl.GetHistory())

	// Move to next track
	nextTrack := pl.NextTrack()

	// Allow time for history to update
	time.Sleep(200 * time.Millisecond)

	// Check that the next track is not empty
	if nextTrack == nil {
		t.Fatalf("Expected next track to not be nil")
	}

	// Check that the track has a path
	if track, ok := nextTrack.(interface{ GetPath() string }); !ok || track.GetPath() == "" {
		t.Fatalf("Expected next track to have a valid path")
	}

	// Check that the next track is different from the current track
	if nextTrack == currentTrack {
		if currentTrackPath, ok := currentTrack.(interface{ GetPath() string }); ok {
			if nextTrackPath, ok := nextTrack.(interface{ GetPath() string }); ok {
				if currentTrackPath.GetPath() == nextTrackPath.GetPath() {
					t.Fatalf("Expected next track to be different from current track. Both have path: %s", currentTrackPath.GetPath())
				}
			}
		}
	}

	// Check that history has been updated
	history := pl.GetHistory()
	if len(history) < 2 {
		t.Fatalf("Expected history to contain at least two items, but got %d", len(history))
	}

	// Log results for debugging
	t.Logf("After next track operation:")
	t.Logf("Next track: %v", nextTrack)
	t.Logf("Updated history: %v", history)
}

func TestPlaylist_PreviousTrack(t *testing.T) {
	// Create a temporary directory for tests
	tmpDir, err := ioutil.TempDir("", "playlist-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create various audio files for testing
	if err := createTestAudioFiles(tmpDir); err != nil {
		t.Fatalf("Failed to create test audio files: %v", err)
	}

	// Initialize playlist
	pl, err := playlist.NewPlaylist(tmpDir, nil, false)
	if err != nil {
		t.Fatalf("Failed to create playlist: %v", err)
	}

	// Allow time for playlist initialization
	time.Sleep(200 * time.Millisecond)

	// Remember current track
	currentTrack := pl.GetCurrentTrack()
	currentTrackPath := ""
	if track, ok := currentTrack.(interface{ GetPath() string }); ok {
		currentTrackPath = track.GetPath()
		t.Logf("Initial track path: %s", currentTrackPath)
	}

	// Move to next track
	pl.NextTrack()

	// Allow time for playlist to update
	time.Sleep(200 * time.Millisecond)

	// Check that the track has changed
	midTrack := pl.GetCurrentTrack()
	midTrackPath := ""
	if track, ok := midTrack.(interface{ GetPath() string }); ok {
		midTrackPath = track.GetPath()
		t.Logf("Middle track path (after next): %s", midTrackPath)
	}

	// Move back to previous track
	previousTrack := pl.PreviousTrack()

	// Allow time for playlist to update
	time.Sleep(200 * time.Millisecond)

	// Check that the previous track is not empty
	if previousTrack == nil {
		t.Fatalf("Expected previous track to not be nil")
	}

	// Get previous track path
	previousTrackPath := ""
	if track, ok := previousTrack.(interface{ GetPath() string }); ok {
		previousTrackPath = track.GetPath()
		t.Logf("Previous track path (after prev): %s", previousTrackPath)
	}

	// Check that the previous track matches the original track
	if previousTrackPath != currentTrackPath {
		t.Fatalf("Expected previous track path (%s) to be the same as original track path (%s)",
			previousTrackPath, currentTrackPath)
	}

	// Log history state
	t.Logf("Final history: %v", pl.GetHistory())
}

func TestPlaylist_ShuffleMode(t *testing.T) {
	// Create a temporary directory for tests
	tmpDir, err := ioutil.TempDir("", "playlist-test-shuffle")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create more audio files for better shuffle testing
	files := []struct {
		name string
		data []byte
	}{
		{"01_track.mp3", minimumMP3Data},
		{"02_track.mp3", minimumMP3Data},
		{"03_track.mp3", minimumMP3Data},
		{"04_track.mp3", minimumMP3Data},
		{"05_track.mp3", minimumMP3Data},
		{"06_track.mp3", minimumMP3Data},
		{"07_track.mp3", minimumMP3Data},
		{"08_track.mp3", minimumMP3Data},
		{"09_track.mp3", minimumMP3Data},
		{"10_track.mp3", minimumMP3Data},
	}

	// Create files
	for _, file := range files {
		filePath := filepath.Join(tmpDir, file.name)
		if err := ioutil.WriteFile(filePath, file.data, 0644); err != nil {
			t.Fatalf("Failed to create test file %s: %v", file.name, err)
		}
	}

	// Initialize playlist without shuffling to preserve original order
	regularPl, err := playlist.NewPlaylist(tmpDir, nil, false)
	if err != nil {
		t.Fatalf("Failed to create regular playlist: %v", err)
	}

	// Allow time for initialization
	time.Sleep(200 * time.Millisecond)

	// Get tracks from regular playlist (should be in alphabetical order)
	var regularTracks []string
	currentTrack := regularPl.GetCurrentTrack()
	if track, ok := currentTrack.(interface{ GetPath() string }); ok {
		regularTracks = append(regularTracks, filepath.Base(track.GetPath()))
	}

	// Iterate through tracks in sequence
	for i := 0; i < 9; i++ {
		nextTrack := regularPl.NextTrack()
		time.Sleep(50 * time.Millisecond)
		if track, ok := nextTrack.(interface{ GetPath() string }); ok {
			regularTracks = append(regularTracks, filepath.Base(track.GetPath()))
		}
	}

	// Manually create playlist with shuffle instead of using the constructor with shuffle=true
	shufflePl, err := playlist.NewPlaylist(tmpDir, nil, false)
	if err != nil {
		t.Fatalf("Failed to create shuffle playlist: %v", err)
	}
	
	// Allow time for initialization
	time.Sleep(200 * time.Millisecond)

	// Manually shuffle with timeout to identify potential deadlocks
	t.Log("Starting manual shuffle operation...")
	
	// Create a channel to signal completion
	done := make(chan bool)
	
	// Start goroutine to execute shuffle
	go func() {
		shufflePl.Shuffle()
		done <- true
	}()
	
	// Wait for shuffle to complete with timeout
	select {
	case <-done:
		t.Log("Shuffle operation completed successfully")
	case <-time.After(10 * time.Second):
		t.Fatalf("Shuffle operation timed out after 10 seconds")
	}

	// Get tracks from shuffled playlist
	var shuffleTracks []string
	currentTrack = shufflePl.GetCurrentTrack()
	if track, ok := currentTrack.(interface{ GetPath() string }); ok {
		shuffleTracks = append(shuffleTracks, filepath.Base(track.GetPath()))
	}

	// Iterate through tracks in sequence
	for i := 0; i < 9; i++ {
		nextTrack := shufflePl.NextTrack()
		time.Sleep(50 * time.Millisecond)
		if track, ok := nextTrack.(interface{ GetPath() string }); ok {
			shuffleTracks = append(shuffleTracks, filepath.Base(track.GetPath()))
		}
	}

	// Log track order for debugging
	t.Logf("Regular playlist tracks order: %v", regularTracks)
	t.Logf("Shuffled playlist tracks order: %v", shuffleTracks)

	// Check that both playlists have the same number of tracks
	if len(regularTracks) != len(shuffleTracks) {
		t.Fatalf("Expected both playlists to have the same number of tracks, but got %d and %d",
			len(regularTracks), len(shuffleTracks))
	}

	// Check that the track order is different in SHUFFLE mode
	different := false
	for i, track := range regularTracks {
		if i < len(shuffleTracks) && track != shuffleTracks[i] {
			different = true
			break
		}
	}

	if !different {
		t.Errorf("Expected shuffled playlist to have different order than regular playlist, but they appear identical")
	}

	// Check that all files are present in both playlists
	regularMap := make(map[string]bool)
	shuffleMap := make(map[string]bool)

	for _, track := range regularTracks {
		regularMap[track] = true
	}

	for _, track := range shuffleTracks {
		shuffleMap[track] = true
	}

	// All files from regular playlist should be in shuffled playlist
	for track := range regularMap {
		if !shuffleMap[track] {
			t.Errorf("Track %s is missing in shuffled playlist", track)
		}
	}

	// All files from shuffled playlist should be in regular playlist
	for track := range shuffleMap {
		if !regularMap[track] {
			t.Errorf("Track %s is missing in regular playlist", track)
		}
	}
} 