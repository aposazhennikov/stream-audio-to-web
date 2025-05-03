package unit

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/user/stream-audio-to-web/playlist"
)

// TestParallelPlaylistInitialization tests initialization of multiple playlists in parallel
// This is a stress test simulating production-like conditions
func TestParallelPlaylistInitialization(t *testing.T) {
	// Skip in short mode
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	// Number of playlists to create in parallel
	const numPlaylists = 7
	
	// Number of files per playlist
	const filesPerPlaylist = 230
	
	// Create base temp directory
	baseDir, err := ioutil.TempDir("", "playlist-stress-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(baseDir)
	
	// Create multiple playlist directories
	var dirs []string
	for i := 0; i < numPlaylists; i++ {
		dir := filepath.Join(baseDir, fmt.Sprintf("playlist-%d", i))
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("Failed to create playlist directory: %v", err)
		}
		dirs = append(dirs, dir)
		
		// Create many dummy files for this playlist
		for j := 0; j < filesPerPlaylist; j++ {
			filename := filepath.Join(dir, fmt.Sprintf("track-%03d.mp3", j))
			// Create a minimal MP3 file with some bytes (could use minimumMP3Data from playlist_test.go)
			data := []byte{0xFF, 0xFB, 0x50, 0x40, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
			if err := ioutil.WriteFile(filename, data, 0644); err != nil {
				t.Fatalf("Failed to create dummy file: %v", err)
			}
		}
		
		t.Logf("Created playlist directory %s with %d files", dir, filesPerPlaylist)
	}
	
	// Start timers for diagnostics
	start := time.Now()
	
	// Create playlists in parallel with shuffle enabled
	var wg sync.WaitGroup
	results := make(chan error, numPlaylists)
	
	t.Logf("Starting parallel playlist initialization with shuffling...")
	
	for i, dir := range dirs {
		wg.Add(1)
		go func(idx int, directory string) {
			defer wg.Done()
			
			t.Logf("Starting initialization of playlist %d from directory %s", idx, directory)
			playlistStart := time.Now()
			
			// Create playlist with shuffling enabled
			pl, err := playlist.NewPlaylist(directory, nil, true)
			if err != nil {
				results <- fmt.Errorf("playlist %d creation failed: %v", idx, err)
				return
			}
			defer pl.Close()
			
			// Explicitly call Shuffle to test concurrent shuffling
			t.Logf("Explicitly shuffling playlist %d", idx)
			pl.Shuffle()
			
			// Verify playlist has been loaded
			tracks := pl.GetTracks()
			if len(tracks) != filesPerPlaylist {
				results <- fmt.Errorf("playlist %d has wrong number of tracks: got %d, want %d", 
					idx, len(tracks), filesPerPlaylist)
				return
			}
			
			// Call some operations to test functionality
			pl.GetCurrentTrack()
			pl.NextTrack()
			pl.PreviousTrack()
			
			// Shuffle again to test repeated shuffle
			pl.Shuffle()
			
			elapsed := time.Since(playlistStart)
			t.Logf("Playlist %d initialized and shuffled in %v", idx, elapsed)
			
			results <- nil
		}(i, dir)
	}
	
	// Wait for all goroutines to complete
	go func() {
		wg.Wait()
		close(results)
	}()
	
	// Collect results
	var failures []error
	for err := range results {
		if err != nil {
			failures = append(failures, err)
			t.Errorf("Playlist error: %v", err)
		}
	}
	
	elapsed := time.Since(start)
	t.Logf("All playlists processed in %v", elapsed)
	
	if len(failures) > 0 {
		t.Fatalf("Failed to initialize %d out of %d playlists", len(failures), numPlaylists)
	}
}

// TestConcurrentShuffling specifically tests concurrent shuffling of the same playlist
func TestConcurrentShuffling(t *testing.T) {
	// Create a temp directory
	tmpDir, err := ioutil.TempDir("", "concurrent-shuffle-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)
	
	// Create some test files
	const numFiles = 250
	for i := 0; i < numFiles; i++ {
		filename := filepath.Join(tmpDir, fmt.Sprintf("track-%03d.mp3", i))
		data := []byte{0xFF, 0xFB, 0x50, 0x40, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
		if err := ioutil.WriteFile(filename, data, 0644); err != nil {
			t.Fatalf("Failed to create test file: %v", err)
		}
	}
	
	// Create a playlist
	pl, err := playlist.NewPlaylist(tmpDir, nil, false) // Start without shuffle
	if err != nil {
		t.Fatalf("Failed to create playlist: %v", err)
	}
	defer pl.Close()
	
	// Number of concurrent shuffle operations
	const concurrentShuffles = 10
	
	// Start concurrent shuffle operations
	var wg sync.WaitGroup
	start := time.Now()
	
	for i := 0; i < concurrentShuffles; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			
			// Add a small random delay to increase chance of contention
			time.Sleep(time.Duration(idx * 5) * time.Millisecond)
			
			t.Logf("Starting shuffle operation %d", idx)
			shuffleStart := time.Now()
			
			// Call Shuffle
			pl.Shuffle()
			
			elapsed := time.Since(shuffleStart)
			t.Logf("Shuffle operation %d completed in %v", idx, elapsed)
		}(i)
	}
	
	// Also perform some other operations during shuffling
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			
			// Perform various operations
			for j := 0; j < 10; j++ {
				// Randomly choose an operation
				switch j % 3 {
				case 0:
					pl.GetCurrentTrack()
				case 1:
					pl.NextTrack()
				case 2:
					pl.PreviousTrack()
				}
				
				// Small delay between operations
				time.Sleep(10 * time.Millisecond)
			}
			
			t.Logf("Operation thread %d completed", idx)
		}(i)
	}
	
	// Wait for all operations to complete
	wg.Wait()
	
	elapsed := time.Since(start)
	t.Logf("All concurrent operations completed in %v", elapsed)
	
	// Verify playlist is still functional
	tracks := pl.GetTracks()
	if len(tracks) != numFiles {
		t.Errorf("Wrong number of tracks after concurrent operations: got %d, want %d", 
			len(tracks), numFiles)
	}
	
	// Test one more shuffle to ensure functionality still works
	pl.Shuffle()
}

// TestShufflePerformance tests the performance of shuffle for large playlists
func TestShufflePerformance(t *testing.T) {
	// Create test directories with different sizes
	sizes := []int{10, 100, 500, 1000, 5000}
	
	for _, size := range sizes {
		t.Run(fmt.Sprintf("Size-%d", size), func(t *testing.T) {
			// Skip very large tests in short mode
			if size > 1000 && testing.Short() {
				t.Skip("Skipping large performance test in short mode")
			}
			
			// Create a temp directory
			tmpDir, err := ioutil.TempDir("", fmt.Sprintf("shuffle-perf-%d", size))
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tmpDir)
			
			// Create test files
			for i := 0; i < size; i++ {
				filename := filepath.Join(tmpDir, fmt.Sprintf("track-%05d.mp3", i))
				data := []byte{0xFF, 0xFB, 0x50, 0x40, 0x00, 0x00, 0x00}
				if err := ioutil.WriteFile(filename, data, 0644); err != nil {
					t.Fatalf("Failed to create test file: %v", err)
				}
			}
			
			// Create playlist
			pl, err := playlist.NewPlaylist(tmpDir, nil, false)
			if err != nil {
				t.Fatalf("Failed to create playlist: %v", err)
			}
			defer pl.Close()
			
			// Measure shuffle performance
			start := time.Now()
			pl.Shuffle()
			elapsed := time.Since(start)
			
			t.Logf("Shuffle of %d tracks took %v (%.2f tracks/ms)", 
				size, elapsed, float64(size)/float64(elapsed.Milliseconds()))
			
			// Check for extremely slow performance
			maxAllowedTime := time.Duration(size/10+500) * time.Millisecond
			if elapsed > maxAllowedTime {
				t.Errorf("Shuffle performance too slow: %v for %d tracks (expected < %v)", 
					elapsed, size, maxAllowedTime)
			}
		})
	}
} 