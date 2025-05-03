package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVolumeNormalization(t *testing.T) {
	// Skip if not running manual tests
	if os.Getenv("MANUAL_TEST") == "" {
		t.Skip("Skipping manual test. Set MANUAL_TEST=1 to run")
	}
	
	// Get test audio directory from environment or use a default
	audioDir := os.Getenv("TEST_AUDIO_DIR")
	if audioDir == "" {
		t.Skip("TEST_AUDIO_DIR environment variable not set")
	}
	
	// Temporary directory for test
	tempDir, err := os.MkdirTemp("", "volume-norm-test")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tempDir)
	
	// Create test directories
	quietDir := filepath.Join(tempDir, "quiet")
	loudDir := filepath.Join(tempDir, "loud")
	
	if err := os.Mkdir(quietDir, 0755); err != nil {
		t.Fatalf("Failed to create quiet directory: %v", err)
	}
	if err := os.Mkdir(loudDir, 0755); err != nil {
		t.Fatalf("Failed to create loud directory: %v", err)
	}
	
	// Port for test server
	const testPort = "8765"
	
	// Copy audio files from test directory
	// We assume there are different volume files in the test directory
	copyTestAudioFiles(t, audioDir, quietDir, loudDir)
	
	// Start the server with normalization enabled
	normalizedCmd := startServerWithNormalization(t, testPort, quietDir, loudDir, true)
	defer stopServer(normalizedCmd)
	
	// Wait for server to start
	time.Sleep(2 * time.Second)
	
	// Test volume normalization enabled
	t.Log("Testing with volume normalization enabled")
	testNormalizedStreams(t, testPort)
	
	// Stop the normalized server
	stopServer(normalizedCmd)
	
	// Wait a moment before starting the next server
	time.Sleep(1 * time.Second)
	
	// Start server with normalization disabled
	unnormalizedCmd := startServerWithNormalization(t, testPort, quietDir, loudDir, false)
	defer stopServer(unnormalizedCmd)
	
	// Wait for server to start
	time.Sleep(2 * time.Second)
	
	// Test without volume normalization
	t.Log("Testing with volume normalization disabled")
	testUnnormalizedStreams(t, testPort)
}

// startServerWithNormalization starts the audio streaming server with or without normalization
func startServerWithNormalization(t *testing.T, port string, quietDir, loudDir string, normalize bool) *exec.Cmd {
	// Build the binary if needed
	buildCmd := exec.Command("go", "build", "-o", "temp_audio_server")
	buildCmd.Dir = ".." // Assuming e2e directory is in project root
	
	if err := buildCmd.Run(); err != nil {
		t.Fatalf("Failed to build server: %v", err)
	}
	
	// Start server with appropriate settings
	serverCmd := exec.Command(
		"../temp_audio_server",
		"--port", port,
		"--directory-routes", fmt.Sprintf(`{"quiet":"%s","loud":"%s"}`, quietDir, loudDir),
		"--normalize-volume", fmt.Sprintf("%t", normalize),
	)
	
	serverCmd.Stdout = os.Stdout
	serverCmd.Stderr = os.Stderr
	
	if err := serverCmd.Start(); err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	
	return serverCmd
}

// stopServer stops the running server
func stopServer(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
		cmd.Wait()
	}
}

// copyTestAudioFiles copies audio files to test directories
func copyTestAudioFiles(t *testing.T, sourceDir, quietDir, loudDir string) {
	// Find audio files in source directory
	files, err := os.ReadDir(sourceDir)
	if err != nil {
		t.Fatalf("Failed to read source directory: %v", err)
	}
	
	// Count of copied files
	var quietCount, loudCount int
	
	// Copy files
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		
		filename := file.Name()
		
		// Only copy audio files
		if !strings.HasSuffix(strings.ToLower(filename), ".mp3") &&
		   !strings.HasSuffix(strings.ToLower(filename), ".ogg") &&
		   !strings.HasSuffix(strings.ToLower(filename), ".aac") {
			continue
		}
		
		// Read source file
		data, err := os.ReadFile(filepath.Join(sourceDir, filename))
		if err != nil {
			t.Logf("Failed to read file %s: %v", filename, err)
			continue
		}
		
		// Choose destination based on file index
		// Even files go to quiet, odd files go to loud
		if quietCount < 3 {
			dest := filepath.Join(quietDir, filename)
			if err := os.WriteFile(dest, data, 0644); err != nil {
				t.Logf("Failed to write file %s: %v", dest, err)
				continue
			}
			quietCount++
		} else if loudCount < 3 {
			dest := filepath.Join(loudDir, filename)
			if err := os.WriteFile(dest, data, 0644); err != nil {
				t.Logf("Failed to write file %s: %v", dest, err)
				continue
			}
			loudCount++
		}
		
		// Stop after copying enough files
		if quietCount >= 3 && loudCount >= 3 {
			break
		}
	}
	
	t.Logf("Copied %d files to quiet directory and %d files to loud directory",
		quietCount, loudCount)
}

// testNormalizedStreams tests the streams with normalization enabled
func testNormalizedStreams(t *testing.T, port string) {
	// This is a manual test - just output instructions for manual verification
	t.Logf("MANUAL VERIFICATION REQUIRED:")
	t.Logf("1. Open a web browser and navigate to http://localhost:%s/quiet", port)
	t.Logf("2. Listen to the stream for a minute")
	t.Logf("3. Then navigate to http://localhost:%s/loud", port)
	t.Logf("4. Listen to the stream and verify that volume levels are consistent between streams")
	t.Logf("5. The volume should be normalized so that quiet and loud streams have similar volume")
}

// testUnnormalizedStreams tests the streams without normalization
func testUnnormalizedStreams(t *testing.T, port string) {
	// This is a manual test - just output instructions for manual verification
	t.Logf("MANUAL VERIFICATION REQUIRED:")
	t.Logf("1. Open a web browser and navigate to http://localhost:%s/quiet", port)
	t.Logf("2. Listen to the stream for a minute")
	t.Logf("3. Then navigate to http://localhost:%s/loud", port)
	t.Logf("4. Listen to the stream and verify that volume levels are DIFFERENT between streams")
	t.Logf("5. The volume should NOT be normalized - loud streams should sound significantly louder")
} 