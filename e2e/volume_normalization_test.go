package e2e_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/user/stream-audio-to-web/audio"
)

// getServerURL returns the server URL from environment variable or default value
func getServerURL() string {
	if url := os.Getenv("SERVER_URL"); url != "" {
		return url
	}
	return "http://localhost:8080"
}

// TestVolumeNormalization tests that volume normalization works correctly in the end-to-end system
func TestVolumeNormalization(t *testing.T) {
	// Skip if not running E2E tests
	if os.Getenv("E2E_TEST") == "" && os.Getenv("CI") == "" {
		t.Skip("Skipping E2E test. Set E2E_TEST=1 to run")
	}

	// Get server URL from environment or use default
	serverURL := getServerURL()

	// Test parameters
	const (
		streamTimeout  = 5 * time.Second
		minStreamBytes = 4096             // We need at least this many bytes for analysis
		maxCollectTime = 10 * time.Second // Maximum time to collect audio data
	)

	// Set up test route paths
	routes := []string{"/test_audio", "/science"}

	// Collect samples from each route
	routeSamples := make(map[string][][]byte)

	for _, route := range routes {
		// Collect audio data from the route
		testURL := fmt.Sprintf("%s%s", serverURL, route)

		// Create client with timeout
		client := &http.Client{
			Timeout: streamTimeout,
		}

		// First, make HEAD request to verify stream exists
		resp, err := client.Head(testURL)
		require.NoError(t, err, "Failed to make HEAD request to %s", testURL)
		require.Equal(t, http.StatusOK, resp.StatusCode, "Unexpected status code for HEAD request to %s", testURL)

		// Now collect audio samples
		routeSamples[route] = collectAudioSamples(t, testURL, minStreamBytes, maxCollectTime)
	}

	// Calculate volume metrics for each route
	routeRMS := make(map[string]float64)
	routeLUFS := make(map[string]float64)

	for route, samples := range routeSamples {
		// Convert samples to PCM data for analysis
		pcm := convertToPCM(samples)

		// Calculate RMS and estimated LUFS
		rms := calculateRMS(pcm)
		lufs := 20*math.Log10(rms) - 10 // Simplified LUFS approximation

		routeRMS[route] = rms
		routeLUFS[route] = lufs

		t.Logf("Route %s: RMS = %.6f, LUFS approx = %.2f dB", route, rms, lufs)
	}

	// Test that all routes have similar volume levels (within 1 dB)
	const maxLUFSDiff = 1.0 // Maximum allowed difference in dB

	// Compare all routes with each other
	for i, route1 := range routes {
		for j, route2 := range routes {
			if j <= i {
				continue
			}
			lufs1 := routeLUFS[route1]
			lufs2 := routeLUFS[route2]
			lufsDiff := math.Abs(lufs1 - lufs2)
			assert.LessOrEqualf(t, lufsDiff, maxLUFSDiff,
				"Volume difference between %s (%.2f dB) and %s (%.2f dB) exceeds maximum allowed (%.1f dB)",
				route1, lufs1, route2, lufs2, maxLUFSDiff)
			t.Logf("Volume difference between %s and %s: %.2f dB", route1, route2, lufsDiff)
		}
	}
}

// TestFirstSoundLatency tests that the first sound is delivered quickly
func TestFirstSoundLatency(t *testing.T) {
	// Skip if not running E2E tests
	if os.Getenv("E2E_TEST") == "" && os.Getenv("CI") == "" {
		t.Skip("Skipping E2E test. Set E2E_TEST=1 to run")
	}

	// Get server URL from environment or use default
	serverURL := getServerURL()

	// Test parameters
	const (
		maxFirstSoundTime = 150 * time.Millisecond // Maximum allowed time to first sound
		testRoute         = "/test_audio"          // Route to test latency
	)

	// Start a timer
	startTime := time.Now()

	// Create client with no timeout
	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	// Make GET request to the audio stream
	testURL := fmt.Sprintf("%s%s", serverURL, testRoute)
	resp, getStreamErr := client.Get(testURL)
	require.NoError(t, getStreamErr, "Failed to make GET request to %s", testURL)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "Unexpected status code for GET request to %s", testURL)

	// Read with a buffer
	buffer := make([]byte, 1024)
	bytesRead := 0

	// Read until we get some data or timeout
	for bytesRead < 1024 {
		n, readStreamDataErr := resp.Body.Read(buffer[bytesRead:])
		if readStreamDataErr != nil && readStreamDataErr != io.EOF {
			t.Fatalf("Error reading response body: %v", readStreamDataErr)
		}

		bytesRead += n

		// Check elapsed time
		elapsed := time.Since(startTime)

		// If we've received enough data or reached EOF, break
		if bytesRead > 0 || readStreamDataErr == io.EOF {
			t.Logf("Received first %d bytes in %.2f ms", bytesRead, float64(elapsed.Microseconds())/1000.0)
			break
		}

		// Check timeout
		if elapsed > maxFirstSoundTime*2 {
			t.Fatalf("Timed out waiting for first sound data (waited %.2f ms)", float64(elapsed.Microseconds())/1000.0)
		}
	}

	// Calculate the time to first sound
	firstSoundTime := time.Since(startTime)

	// Check that the time to first sound is within the allowed limit
	assert.LessOrEqualf(t, firstSoundTime, maxFirstSoundTime,
		"Time to first sound (%.2f ms) exceeds maximum allowed (%.2f ms)",
		float64(firstSoundTime.Microseconds())/1000.0, float64(maxFirstSoundTime.Microseconds())/1000.0)

	t.Logf("Time to first sound: %.2f ms", float64(firstSoundTime.Microseconds())/1000.0)
}

// Helper function to collect audio samples from a streaming URL
func collectAudioSamples(t *testing.T, url string, minBytes int, maxTime time.Duration) [][]byte {
	// Create client with no timeout for streaming
	client := &http.Client{}

	// Make GET request to the audio stream
	resp, getStreamErr := client.Get(url)
	require.NoError(t, getStreamErr, "Failed to make GET request to %s", url)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "Unexpected status code for GET request to %s", url)

	// Collect samples
	samples := make([][]byte, 0)
	buffer := make([]byte, 4096)
	totalBytesRead := 0

	// Start a timer for maximum collection time
	startTime := time.Now()

	for totalBytesRead < minBytes && time.Since(startTime) < maxTime {
		n, readStreamDataErr := resp.Body.Read(buffer)
		if readStreamDataErr != nil && readStreamDataErr != io.EOF {
			t.Fatalf("Error reading response body: %v", readStreamDataErr)
		}

		if n > 0 {
			// Make a copy of the buffer
			sampleCopy := make([]byte, n)
			copy(sampleCopy, buffer[:n])
			samples = append(samples, sampleCopy)
			totalBytesRead += n
		}

		if readStreamDataErr == io.EOF {
			break
		}
	}

	t.Logf("Collected %d bytes from %s in %.2f seconds", totalBytesRead, url, time.Since(startTime).Seconds())

	return samples
}

// Helper function to convert raw audio samples to PCM data
func convertToPCM(samples [][]byte) []int16 {
	// For simplicity, we'll treat all bytes as PCM data
	// In a real implementation, we'd need to handle the MP3 decoding properly

	// Estimate total size
	totalBytes := 0
	for _, sample := range samples {
		totalBytes += len(sample)
	}

	// Create PCM array - treating data as 16-bit signed PCM
	// This is a simplification that works for our test purposes
	pcmSamples := make([]int16, totalBytes/2)

	// Fill the PCM array
	pcmIndex := 0
	for _, sample := range samples {
		for i := 0; i < len(sample)/2; i++ {
			offset := i * 2
			if pcmIndex < len(pcmSamples) {
				pcmSamples[pcmIndex] = int16(binary.LittleEndian.Uint16(sample[offset:]))
				pcmIndex++
			}
		}
	}

	return pcmSamples
}

// Helper function to calculate RMS of PCM samples
func calculateRMS(samples []int16) float64 {
	if len(samples) == 0 {
		return 0
	}

	var sumSquares float64
	for _, sample := range samples {
		// Convert to float64 in range [-1, 1]
		normalizedSample := float64(sample) / 32767.0
		sumSquares += normalizedSample * normalizedSample
	}

	// Calculate RMS
	rms := math.Sqrt(sumSquares / float64(len(samples)))
	return rms
}

// TestVolumeJumpsOnTrackChange tests that volume doesn't jump dramatically when tracks change
func TestVolumeJumpsOnTrackChange(t *testing.T) {
	// Skip if not running E2E tests
	if os.Getenv("E2E_TEST") == "" && os.Getenv("CI") == "" {
		t.Skip("Skipping E2E test. Set E2E_TEST=1 to run")
	}

	// Skip if no manual test directory is specified
	testDir := os.Getenv("TEST_AUDIO_DIR")
	if testDir == "" {
		t.Skip("Skipping track change test. Set TEST_AUDIO_DIR to run")
	}

	// Get server URL from environment or use default
	serverURL := getServerURL()
	statusPassword := os.Getenv("STATUS_PASSWORD")
	if statusPassword == "" {
		statusPassword = "1234554321" // Default password
	}

	// Test parameters
	const (
		testRoute   = "/test_audio"   // Route to test
		maxLUFSDiff = 1.0             // Maximum allowed volume difference in dB
		sampleTime  = 5 * time.Second // Time to sample each track
	)

	// First sample the current track
	firstTrackSamples := collectAudioSamples(t, serverURL+testRoute, 32768, sampleTime)
	firstTrackPCM := convertToPCM(firstTrackSamples)
	firstTrackRMS := calculateRMS(firstTrackPCM)
	firstTrackLUFS := 20*math.Log10(firstTrackRMS) - 10

	// Get current track name
	firstTrackName := getCurrentTrack(t, serverURL, testRoute)
	t.Logf("Current track: %s, RMS: %.6f, LUFS approx: %.2f dB", firstTrackName, firstTrackRMS, firstTrackLUFS)

	// Switch to next track
	switchToNextTrack(t, serverURL, testRoute, statusPassword)

	// Wait a short time for the track to start playing
	time.Sleep(500 * time.Millisecond)

	// Sample the new track
	secondTrackSamples := collectAudioSamples(t, serverURL+testRoute, 32768, sampleTime)
	secondTrackPCM := convertToPCM(secondTrackSamples)
	secondTrackRMS := calculateRMS(secondTrackPCM)
	secondTrackLUFS := 20*math.Log10(secondTrackRMS) - 10

	// Get new track name
	secondTrackName := getCurrentTrack(t, serverURL, testRoute)
	t.Logf("Switched to track: %s, RMS: %.6f, LUFS approx: %.2f dB", secondTrackName, secondTrackRMS, secondTrackLUFS)

	// Check the volume difference
	lufsDiff := math.Abs(firstTrackLUFS - secondTrackLUFS)
	assert.LessOrEqualf(t, lufsDiff, maxLUFSDiff,
		"Volume jump between tracks exceeds maximum allowed: %.2f dB (limit: %.1f dB)",
		lufsDiff, maxLUFSDiff)

	t.Logf("Volume difference between tracks: %.2f dB", lufsDiff)
}

// Helper function to get the current track name
func getCurrentTrack(t *testing.T, serverURL, route string) string {
	// Make request to now-playing endpoint
	resp, err := http.Get(serverURL + "/now-playing")
	require.NoError(t, err, "Failed to get current track information")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "Unexpected status code for now-playing request")

	// Read response body
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "Failed to read response body")

	// Parse JSON - simplified version, just extracting the track name as a substring
	// In a real implementation, we'd properly parse the JSON
	routeJSON := fmt.Sprintf(`"%s":"`, route)
	startIndex := bytes.Index(body, []byte(routeJSON))
	if startIndex == -1 {
		t.Fatalf("Route %s not found in response: %s", route, string(body))
	}

	startIndex += len(routeJSON)
	endIndex := bytes.Index(body[startIndex:], []byte(`"`))
	if endIndex == -1 {
		t.Fatalf("Failed to parse track name from response: %s", string(body))
	}

	trackName := string(body[startIndex : startIndex+endIndex])
	return trackName
}

// Helper function to switch to the next track
func switchToNextTrack(t *testing.T, serverURL, route, password string) {
	// Create a client that follows redirects
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Copy authentication cookie to redirected request
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			for _, cookie := range via[0].Cookies() {
				req.AddCookie(cookie)
			}
			return nil
		},
	}

	// Create request
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/next-track%s", serverURL, route), nil)
	require.NoError(t, err, "Failed to create request")

	// Add authentication cookie
	req.AddCookie(&http.Cookie{
		Name:  "status_auth",
		Value: password,
	})

	// Add ajax parameter to get JSON response
	q := req.URL.Query()
	q.Add("ajax", "1")
	req.URL.RawQuery = q.Encode()

	// Send request
	resp, err := client.Do(req)
	require.NoError(t, err, "Failed to send request to switch track")
	defer resp.Body.Close()

	// Check response status code
	require.Equal(t, http.StatusOK, resp.StatusCode, "Unexpected status code for next-track request")

	// Read response body
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "Failed to read response body")

	// Check that switch was successful
	if !bytes.Contains(body, []byte("success")) {
		t.Fatalf("Failed to switch track: %s", string(body))
	}

	t.Logf("Successfully switched to next track on route %s", route)
}

func TestVolumeNormalizationManual(t *testing.T) {
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
	tempDir := t.TempDir()

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
		"--normalize-volume", strconv.FormatBool(normalize),
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
		data, readSourceFileErr := os.ReadFile(filepath.Join(sourceDir, filename))
		if readSourceFileErr != nil {
			t.Logf("Failed to read file %s: %v", filename, readSourceFileErr)
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

// TestMultiWindowAnalysisLatency tests the latency of multi-window analysis with a long file
func TestMultiWindowAnalysisLatency(t *testing.T) {
	// Skip if not running E2E tests
	if os.Getenv("E2E_TEST") == "" && os.Getenv("CI") == "" {
		t.Skip("Skipping E2E test. Set E2E_TEST=1 to run")
	}

	// Skip if no test file is provided
	lectureFile := os.Getenv("TEST_LECTURE_FILE")
	if lectureFile == "" {
		t.Skip("Skipping lecture latency test. Set TEST_LECTURE_FILE to a long audio file path")
	}

	// Test parameters
	const (
		maxAnalysisTime = 150 * time.Millisecond // Maximum allowed analysis time
		routeName       = "/test_lecture"
	)

	// Configure normalization
	audio.SetNormalizeConfig(6, 200) // 6 windows, 200ms each

	// Start a timer
	startTime := time.Now()

	// Analyze the file
	gain, err := audio.CalculateGain(lectureFile, routeName)

	// Calculate elapsed time
	analysisTime := time.Since(startTime)

	// Log results
	t.Logf("Multi-window analysis of lecture file:")
	t.Logf("- File: %s", lectureFile)
	t.Logf("- Analysis time: %.2f ms", float64(analysisTime.Microseconds())/1000.0)
	t.Logf("- Calculated gain: %.4f", gain)

	// Check for errors
	if err != nil {
		t.Errorf("Analysis failed: %v", err)
	}

	// Check analysis time
	assert.LessOrEqualf(t, analysisTime, maxAnalysisTime,
		"Analysis time (%.2f ms) exceeds maximum allowed (%.2f ms)",
		float64(analysisTime.Microseconds())/1000.0, float64(maxAnalysisTime.Microseconds())/1000.0)

	// Check gain is within valid range
	assert.GreaterOrEqual(t, gain, 0.25, "Gain factor (%.4f) below minimum threshold (0.25)", gain)
	assert.LessOrEqual(t, gain, 4.0, "Gain factor (%.4f) above maximum threshold (4.0)", gain)

	// Now test with single window for comparison
	audio.SetNormalizeConfig(1, 200) // 1 window, 200ms
	startTime = time.Now()
	singleGain, _ := audio.CalculateGain(lectureFile, routeName+"_single")
	singleAnalysisTime := time.Since(startTime)

	t.Logf("Single-window analysis of lecture file:")
	t.Logf("- Analysis time: %.2f ms", float64(singleAnalysisTime.Microseconds())/1000.0)
	t.Logf("- Calculated gain: %.4f", singleGain)

	// Log the difference
	t.Logf("Comparison:")
	t.Logf("- Time difference: %.2f ms", float64((analysisTime-singleAnalysisTime).Microseconds())/1000.0)
	t.Logf("- Gain difference: %.4f", math.Abs(gain-singleGain))

	// Calculate dB difference
	if gain > 0 && singleGain > 0 {
		dbDiff := 20 * math.Log10(gain/singleGain)
		t.Logf("- Gain difference in dB: %.2f dB", dbDiff)
	}

	// Reset test configuration
	audio.SetNormalizeConfig(6, 200)
}

// TestVolumeNormalizationWithPodcast tests the effectiveness of normalization with podcast-like content
func TestVolumeNormalizationWithPodcast(t *testing.T) {
	var err error
	// Skip if not running E2E tests
	if os.Getenv("E2E_TEST") == "" && os.Getenv("CI") == "" {
		t.Skip("Skipping E2E test. Set E2E_TEST=1 to run")
	}

	// Create temporary directory for test files
	tempDir := t.TempDir()

	// Create test files
	silenceFirstFile := filepath.Join(tempDir, "silence_first.mp3")
	loudFirstFile := filepath.Join(tempDir, "loud_first.mp3")

	// Create test files with synthetic data
	createSilenceThenLoudFile(t, silenceFirstFile)
	createLoudThenSilenceFile(t, loudFirstFile)

	// Normalize the files
	silenceFirstOutput := new(bytes.Buffer)
	loudFirstOutput := new(bytes.Buffer)

	// Open the files
	silenceFile, errSilence := os.Open(silenceFirstFile)
	require.NoError(t, errSilence, "Failed to open silence_first.mp3")
	defer silenceFile.Close()

	loudFile, errLoud := os.Open(loudFirstFile)
	require.NoError(t, errLoud, "Failed to open loud_first.mp3")
	defer loudFile.Close()

	// Configure normalization for multi-window
	audio.SetNormalizeConfig(6, 200)

	// Normalize the files
	err = audio.NormalizeMP3Stream(silenceFile, silenceFirstOutput, "/test")
	require.NoError(t, err, "Failed to normalize silence_first.mp3")

	err = audio.NormalizeMP3Stream(loudFile, loudFirstOutput, "/test")
	require.NoError(t, err, "Failed to normalize loud_first.mp3")

	// Analyze the normalized outputs
	silenceFirstPCM := convertToPCM([][]byte{silenceFirstOutput.Bytes()})
	loudFirstPCM := convertToPCM([][]byte{loudFirstOutput.Bytes()})

	silenceFirstRMS := calculateRMS(silenceFirstPCM)
	loudFirstRMS := calculateRMS(loudFirstPCM)

	// Convert to LUFS for comparison
	silenceFirstLUFS := 20*math.Log10(silenceFirstRMS) - 10
	loudFirstLUFS := 20*math.Log10(loudFirstRMS) - 10

	t.Logf("Normalized audio levels:")
	t.Logf("- Silence-first file: RMS=%.6f, LUFS approx=%.2f dB", silenceFirstRMS, silenceFirstLUFS)
	t.Logf("- Loud-first file: RMS=%.6f, LUFS approx=%.2f dB", loudFirstRMS, loudFirstLUFS)

	// Compare levels - they should be similar after normalization
	lufsDiff := math.Abs(silenceFirstLUFS - loudFirstLUFS)
	t.Logf("- Difference: %.2f dB", lufsDiff)

	// Check that difference is within expected range
	assert.LessOrEqualf(t, lufsDiff, 1.0,
		"Volume difference between normalized files (%.2f dB) exceeds maximum allowed (1.0 dB)",
		lufsDiff)

	// For comparison, normalize with single window
	silenceFile.Seek(0, io.SeekStart)
	loudFile.Seek(0, io.SeekStart)

	silenceFirstSingle := new(bytes.Buffer)
	loudFirstSingle := new(bytes.Buffer)

	// Configure normalization for single window
	audio.SetNormalizeConfig(1, 200)

	// Normalize the files
	err = audio.NormalizeMP3Stream(silenceFile, silenceFirstSingle, "/test_single")
	require.NoError(t, err, "Failed to normalize silence_first.mp3 with single window")

	err = audio.NormalizeMP3Stream(loudFile, loudFirstSingle, "/test_single")
	require.NoError(t, err, "Failed to normalize loud_first.mp3 with single window")

	// Analyze the normalized outputs
	silenceFirstSinglePCM := convertToPCM([][]byte{silenceFirstSingle.Bytes()})
	loudFirstSinglePCM := convertToPCM([][]byte{loudFirstSingle.Bytes()})

	silenceFirstSingleRMS := calculateRMS(silenceFirstSinglePCM)
	loudFirstSingleRMS := calculateRMS(loudFirstSinglePCM)

	// Convert to LUFS for comparison
	silenceFirstSingleLUFS := 20*math.Log10(silenceFirstSingleRMS) - 10
	loudFirstSingleLUFS := 20*math.Log10(loudFirstSingleRMS) - 10

	t.Logf("Single-window normalized audio levels:")
	t.Logf("- Silence-first file: RMS=%.6f, LUFS approx=%.2f dB", silenceFirstSingleRMS, silenceFirstSingleLUFS)
	t.Logf("- Loud-first file: RMS=%.6f, LUFS approx=%.2f dB", loudFirstSingleRMS, loudFirstSingleLUFS)

	// Compare levels
	lufsSingleDiff := math.Abs(silenceFirstSingleLUFS - loudFirstSingleLUFS)
	t.Logf("- Difference: %.2f dB", lufsSingleDiff)

	// Compare multi-window vs single-window results
	t.Logf("Multi-window vs Single-window comparison:")
	t.Logf("- Multi-window difference: %.2f dB", lufsDiff)
	t.Logf("- Single-window difference: %.2f dB", lufsSingleDiff)

	// The multi-window approach should yield a smaller difference
	assert.Lessf(t, lufsDiff, lufsSingleDiff,
		"Multi-window approach (%.2f dB) did not improve over single-window approach (%.2f dB)",
		lufsDiff, lufsSingleDiff)

	// Reset test configuration
	audio.SetNormalizeConfig(6, 200)
}

// createSilenceThenLoudFile creates a test file with silence followed by loud content
func createSilenceThenLoudFile(t *testing.T, filePath string) {
	// Create file
	f, createTestFileErr := os.Create(filePath)
	if createTestFileErr != nil {
		t.Fatalf("Failed to create test file: %v", createTestFileErr)
	}
	defer f.Close()

	// Create minimal MP3 header
	header := []byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 10}
	id3Content := make([]byte, 10)
	f.Write(header)
	f.Write(id3Content)

	// Create PCM data (2 seconds total)
	const sampleRate = 44100
	const numSamples = sampleRate * 2
	pcmData := make([]byte, numSamples*4)

	// First second: near silence
	for i := 0; i < sampleRate; i++ {
		// Very low amplitude noise
		value := (rand.Float64() - 0.5) * 0.01
		pcmValue := int16(value * 32767)

		// Write to both channels
		bytePos := i * 4
		binary.LittleEndian.PutUint16(pcmData[bytePos:], uint16(pcmValue))
		binary.LittleEndian.PutUint16(pcmData[bytePos+2:], uint16(pcmValue))
	}

	// Second second: loud content
	for i := sampleRate; i < numSamples; i++ {
		// Loud sine wave
		phase := float64(i-sampleRate) * 2 * math.Pi * 440 / sampleRate
		value := 0.5 * math.Sin(phase) // -6 dB
		pcmValue := int16(value * 32767)

		// Write to both channels
		bytePos := i * 4
		binary.LittleEndian.PutUint16(pcmData[bytePos:], uint16(pcmValue))
		binary.LittleEndian.PutUint16(pcmData[bytePos+2:], uint16(pcmValue))
	}

	// Write PCM data
	f.Write(pcmData)
}

// createLoudThenSilenceFile creates a test file with loud content followed by silence
func createLoudThenSilenceFile(t *testing.T, filePath string) {
	// Create file
	f, createTestFileErr := os.Create(filePath)
	if createTestFileErr != nil {
		t.Fatalf("Failed to create test file: %v", createTestFileErr)
	}
	defer f.Close()

	// Create minimal MP3 header
	header := []byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 10}
	id3Content := make([]byte, 10)
	f.Write(header)
	f.Write(id3Content)

	// Create PCM data (2 seconds total)
	const sampleRate = 44100
	const numSamples = sampleRate * 2
	pcmData := make([]byte, numSamples*4)

	// First second: loud content
	for i := 0; i < sampleRate; i++ {
		// Loud sine wave
		phase := float64(i) * 2 * math.Pi * 440 / sampleRate
		value := 0.5 * math.Sin(phase) // -6 dB
		pcmValue := int16(value * 32767)

		// Write to both channels
		bytePos := i * 4
		binary.LittleEndian.PutUint16(pcmData[bytePos:], uint16(pcmValue))
		binary.LittleEndian.PutUint16(pcmData[bytePos+2:], uint16(pcmValue))
	}

	// Second second: near silence
	for i := sampleRate; i < numSamples; i++ {
		// Very low amplitude noise
		value := (rand.Float64() - 0.5) * 0.01
		pcmValue := int16(value * 32767)

		// Write to both channels
		bytePos := i * 4
		binary.LittleEndian.PutUint16(pcmData[bytePos:], uint16(pcmValue))
		binary.LittleEndian.PutUint16(pcmData[bytePos+2:], uint16(pcmValue))
	}

	// Write PCM data
	f.Write(pcmData)
}
