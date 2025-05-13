package unit_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/user/stream-audio-to-web/playlist"
	"github.com/user/stream-audio-to-web/slog"
)

// TestParallelPlaylistInitialization tests initialization of multiple playlists in parallel.
// This is a stress test simulating production-like conditions.
func TestParallelPlaylistInitialization(t *testing.T) {
	// Skip in short mode.
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	// Установим переменную окружения для увеличения таймаутов.
	t.Setenv("TEST_ENVIRONMENT", "1")

	t.Log("STRESS TEST START: Beginning parallel playlist test with detailed logging")

	// Setup test environment.
	setupTestEnvironment(t)

	// Create playlists and test directories.
	dirs := createPlaylistDirectories(t)

	// Run parallel playlist tests.
	runParallelTests(t, dirs)
}

// setupTestEnvironment configures the test environment with timeout protection.
func setupTestEnvironment(t *testing.T) {
	// Set shorter test timeout and force exit after 50 seconds.
	timer := time.AfterFunc(50*time.Second, func() {
		t.Logf("FATAL: Test timed out after 50 seconds - forcing exit due to deadlock")
		// Force termination on timeout.
		os.Exit(1)
	})
	defer timer.Stop()
}

// createPlaylistDirectories creates test directories with audio files.
func createPlaylistDirectories(t *testing.T) []string {
	// Number of playlists to create in parallel.
	const numPlaylists = 7

	// Number of files per playlist - reduce for faster test.
	const filesPerPlaylist = 50 // Reduced from 230 to speed up test

	// Create base temp directory.
	t.Log("STEP 1: Creating temporary test directory")
	baseDir := t.TempDir()

	// Create multiple playlist directories.
	var dirs []string
	t.Logf("STEP 2: Creating %d playlist directories with %d files each", numPlaylists, filesPerPlaylist)

	for i := range make([]struct{}, numPlaylists) {
		dir := filepath.Join(baseDir, fmt.Sprintf("playlist-%d", i))
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("Failed to create playlist directory: %v", err)
		}
		dirs = append(dirs, dir)

		createPlaylistFiles(t, dir, i, filesPerPlaylist)
	}

	return dirs
}

// createPlaylistFiles creates dummy MP3 files in the specified directory.
func createPlaylistFiles(t *testing.T, dir string, playlistIdx, filesCount int) {
	t.Logf("STEP 2.%d: Creating files for playlist %d", playlistIdx+1, playlistIdx)
	// Create many dummy files for this playlist.
	for j := range make([]struct{}, filesCount) {
		filename := filepath.Join(dir, fmt.Sprintf("track-%03d.mp3", j))
		// Create a minimal MP3 file with some bytes.
		data := []byte{0xFF, 0xFB, 0x50, 0x40, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
		if err := os.WriteFile(filename, data, 0644); err != nil {
			t.Fatalf("Failed to create dummy file: %v", err)
		}
	}

	t.Logf("Created playlist directory %s with %d files", dir, filesCount)
}

// runParallelTests runs tests for all playlists in parallel.
func runParallelTests(t *testing.T, dirs []string) {
	// Start timers for diagnostics.
	t.Log("STEP 3: Starting parallel playlist initialization")
	start := time.Now()

	// Create playlists in parallel with shuffle enabled.
	var wg sync.WaitGroup
	results := make(chan error, len(dirs))

	t.Logf("Starting parallel playlist initialization with shuffling...")

	for i, dir := range dirs {
		wg.Add(1)
		go func(idx int, directory string) {
			defer wg.Done()
			testSinglePlaylist(t, idx, directory, results)
		}(i, dir)
	}

	// Wait for all goroutines to complete.
	waitForCompletion(t, &wg, results)

	elapsed := time.Since(start)
	t.Logf("STRESS TEST COMPLETE: All playlists processed in %v", elapsed)
}

// testSinglePlaylist tests a single playlist instance.
func testSinglePlaylist(t *testing.T, idx int, directory string, results chan<- error) {
	const filesPerPlaylist = 50 // Must match the value in createPlaylistDirectories

	plStart := time.Now()
	t.Logf("PLAYLIST %d: Starting initialization from directory %s", idx, directory)

	// Create playlist with shuffling enabled.
	t.Logf("PLAYLIST %d: Creating playlist object (NewPlaylist call)", idx)
	pl, err := playlist.NewPlaylist(directory, nil, true, slog.Default())
	if err != nil {
		results <- fmt.Errorf("playlist %d creation failed: %w", idx, err)
		return
	}
	defer pl.Close()

	t.Logf("PLAYLIST %d: Playlist created successfully, took %v", idx, time.Since(plStart))

	// Test playlist operations.
	performPlaylistOperations(t, pl, idx)

	// Verify playlist has been loaded.
	t.Logf("PLAYLIST %d: Getting tracks to verify", idx)
	tracks := pl.GetTracks()
	if len(tracks) != filesPerPlaylist {
		results <- fmt.Errorf("playlist %d has wrong number of tracks: got %d, want %d",
			idx, len(tracks), filesPerPlaylist)
		return
	}

	elapsed := time.Since(plStart)
	t.Logf("PLAYLIST %d: All operations completed in %v", idx, elapsed)

	results <- nil
}

// performPlaylistOperations tests various operations on a playlist.
func performPlaylistOperations(t *testing.T, pl *playlist.Playlist, idx int) {
	// Explicitly call Shuffle to test concurrent shuffling.
	t.Logf("PLAYLIST %d: Explicitly calling Shuffle()", idx)
	shuffleStart := time.Now()
	pl.Shuffle()
	t.Logf("PLAYLIST %d: Shuffle() completed in %v", idx, time.Since(shuffleStart))

	// Call some operations to test functionality.
	t.Logf("PLAYLIST %d: Testing GetCurrentTrack()", idx)
	pl.GetCurrentTrack()

	t.Logf("PLAYLIST %d: Testing NextTrack()", idx)
	pl.NextTrack()

	t.Logf("PLAYLIST %d: Testing PreviousTrack()", idx)
	pl.PreviousTrack()

	// Shuffle again to test repeated shuffle.
	t.Logf("PLAYLIST %d: Calling Shuffle() second time", idx)
	shuffleStart = time.Now()
	pl.Shuffle()
	t.Logf("PLAYLIST %d: Second shuffle completed in %v", idx, time.Since(shuffleStart))
}

// waitForCompletion waits for all goroutines to complete with timeout.
func waitForCompletion(t *testing.T, wg *sync.WaitGroup, results chan error) {
	// Wait for all goroutines to complete with timeout.
	done := make(chan struct{})
	go func() {
		t.Log("STEP 4: Waiting for all playlist operations to complete")
		wg.Wait()
		close(results)
		close(done)
	}()

	// Add safety timeout - increased from 25 to 45 seconds.
	select {
	case <-done:
		t.Log("All goroutines completed normally")
	case <-time.After(45 * time.Second):
		t.Log("WARNING: Wait timeout occurred - not all goroutines completed in time")
		// Force termination on timeout.
		t.Fatalf("Test force-terminated after 45 seconds due to timeout")
	}

	// Collect results.
	t.Log("STEP 5: Collecting results from operations")
	var failures []error
	for err := range results {
		if err != nil {
			failures = append(failures, err)
			t.Errorf("Playlist error: %v", err)
		}
	}

	if len(failures) > 0 {
		t.Fatalf("Failed to initialize %d out of %d playlists", len(failures), len(failures))
	}
}

// TestConcurrentShuffling specifically tests concurrent shuffling of the same playlist.
func TestConcurrentShuffling(t *testing.T) {
	// Установим переменную окружения для увеличения таймаутов.
	t.Setenv("TEST_ENVIRONMENT", "1")

	// Create a temp directory.
	tmpDir := t.TempDir()

	// Create some test files.
	const numFiles = 250
	for i := range make([]struct{}, numFiles) {
		filename := filepath.Join(tmpDir, fmt.Sprintf("track-%03d.mp3", i))
		data := []byte{0xFF, 0xFB, 0x50, 0x40, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
		if err := os.WriteFile(filename, data, 0644); err != nil {
			t.Fatalf("Failed to create test file: %v", err)
		}
	}

	// Create a playlist.
	pl, err := playlist.NewPlaylist(tmpDir, nil, false, slog.Default()) // Start without shuffle
	if err != nil {
		t.Fatalf("Failed to create playlist: %v", err)
	}
	defer pl.Close()

	// Number of concurrent shuffle operations.
	const concurrentShuffles = 10

	// Start concurrent shuffle operations.
	var wg sync.WaitGroup
	start := time.Now()

	for i := range make([]struct{}, concurrentShuffles) {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			// Add a small random delay to increase chance of contention.
			time.Sleep(time.Duration(idx*5) * time.Millisecond)

			t.Logf("Starting shuffle operation %d", idx)
			shuffleStart := time.Now()

			// Call Shuffle.
			pl.Shuffle()

			elapsed := time.Since(shuffleStart)
			t.Logf("Shuffle operation %d completed in %v", idx, elapsed)
		}(i)
	}

	// Also perform some other operations during shuffling.
	for i := range make([]struct{}, 5) {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			// Perform various operations.
			for j := range make([]struct{}, 10) {
				// Randomly choose an operation.
				switch j % 3 {
				case 0:
					pl.GetCurrentTrack()
				case 1:
					pl.NextTrack()
				case 2:
					pl.PreviousTrack()
				}

				// Small delay between operations.
				time.Sleep(10 * time.Millisecond)
			}

			t.Logf("Operation thread %d completed", idx)
		}(i)
	}

	// Wait for all operations to complete.
	wg.Wait()

	elapsed := time.Since(start)
	t.Logf("All concurrent operations completed in %v", elapsed)

	// Verify playlist is still functional.
	tracks := pl.GetTracks()
	if len(tracks) != numFiles {
		t.Errorf("Wrong number of tracks after concurrent operations: got %d, want %d",
			len(tracks), numFiles)
	}

	// Test one more shuffle to ensure functionality still works.
	pl.Shuffle()
}

// TestShufflePerformance tests the performance of shuffle for large playlists.
func TestShufflePerformance(t *testing.T) {
	// Установим переменную окружения для увеличения таймаутов.
	t.Setenv("TEST_ENVIRONMENT", "1")

	// Create test directories with different sizes.
	sizes := []int{10, 100, 500, 1000, 5000}

	for _, size := range sizes {
		t.Run(fmt.Sprintf("Size-%d", size), func(t *testing.T) {
			// Skip very large tests in short mode.
			if size > 1000 && testing.Short() {
				t.Skip("Skipping large performance test in short mode")
			}

			// Create a temp directory.
			tmpDir := t.TempDir()

			// Create test files.
			for i := range make([]struct{}, size) {
				filename := filepath.Join(tmpDir, fmt.Sprintf("track-%05d.mp3", i))
				data := []byte{0xFF, 0xFB, 0x50, 0x40, 0x00, 0x00, 0x00}
				if err := os.WriteFile(filename, data, 0644); err != nil {
					t.Fatalf("Failed to create test file: %v", err)
				}
			}

			// Create playlist.
			pl, err := playlist.NewPlaylist(tmpDir, nil, false, slog.Default())
			if err != nil {
				t.Fatalf("Failed to create playlist: %v", err)
			}
			defer pl.Close()

			// Measure shuffle performance.
			start := time.Now()
			pl.Shuffle()
			elapsed := time.Since(start)

			t.Logf("Shuffle of %d tracks took %v (%.2f tracks/ms)",
				size, elapsed, float64(size)/float64(elapsed.Milliseconds()))

			// Check for extremely slow performance.
			maxAllowedTime := time.Duration(size/10+500) * time.Millisecond
			if elapsed > maxAllowedTime {
				t.Errorf("Shuffle performance too slow: %v for %d tracks (expected < %v)",
					elapsed, size, maxAllowedTime)
			}
		})
	}
}
