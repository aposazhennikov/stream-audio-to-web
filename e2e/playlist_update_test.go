package e2e_test

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPlaylistUpdate tests playlist updating when adding new files.
// This test requires access to the server's filesystem,.
// so it's intended only for manual execution.
func TestPlaylistUpdate(t *testing.T) {
	// Create a logger for the test.
	logger := slog.Default()

	// Check that test is run in manual mode with file access.
	manualTest := os.Getenv("MANUAL_TEST")
	if manualTest != "1" {
		t.Skip("This test requires manual execution with filesystem access. Run it with MANUAL_TEST=1")
		return
	}

	// Test directory path must be specified in environment variable.
	testDir := os.Getenv("TEST_AUDIO_DIR")
	if testDir == "" {
		t.Fatal("You must specify TEST_AUDIO_DIR for testing")
	}

	// Check that directory exists and is accessible for writing.
	info, dirStatErr := os.Stat(testDir)
	if dirStatErr != nil {
		t.Fatalf("Error accessing directory %s: %v", testDir, dirStatErr)
	}

	if !info.IsDir() {
		t.Fatalf("%s is not a directory", testDir)
	}

	// Path to test audio file.
	testAudioFile := os.Getenv("TEST_AUDIO_FILE")
	if testAudioFile == "" {
		t.Fatal("You must specify TEST_AUDIO_FILE - path to a test MP3/OGG/AAC file")
	}

	// Check that file exists.
	_, fileStatErr := os.Stat(testAudioFile)
	if fileStatErr != nil {
		t.Fatalf("Error accessing file %s: %v", testAudioFile, fileStatErr)
	}

	// Test server URL.
	baseURL := getEnvOrDefault("TEST_SERVER_URL", "http://localhost:8000")
	password := getEnvOrDefault("STATUS_PASSWORD", "1234554321")

	// Create HTTP client with cookie support.
	jar, cookieJarErr := cookiejar.New(nil)
	if cookieJarErr != nil {
		t.Fatalf("Failed to create cookie jar: %v", cookieJarErr)
	}
	client := &http.Client{
		Jar:     jar,
		Timeout: 10 * time.Second,
	}

	// Authenticate on status page.
	form := url.Values{}
	form.Add("password", password)
	authResp, authRequestErr := client.PostForm(fmt.Sprintf("%s/status", baseURL), form)
	if authRequestErr != nil {
		t.Fatalf("Authentication error: %v", authRequestErr)
	}
	authResp.Body.Close()

	// Find route corresponding to test directory.
	route := getRouteForDirectory(t, client, baseURL, testDir)
	if route == "" {
		t.Fatalf("Could not find route for directory %s", testDir)
	}

	// Get initial track list for verification.
	logger.Info("Getting initial track list for route", slog.String("route", route))
	initialTracks := getCurrentTracks(t, client, baseURL, route)

	// Create unique name for test file.
	destFileName := fmt.Sprintf("test_file_%d.mp3", time.Now().Unix())
	destFilePath := filepath.Join(testDir, destFileName)

	// Copy test file to directory.
	logger.Info("Copying test file", slog.String("src", testAudioFile), slog.String("dst", destFilePath))
	if copyErr := copyFile(testAudioFile, destFilePath); copyErr != nil {
		t.Fatalf("Error copying file: %v", copyErr)
	}

	// Remove file after test completes.
	defer func() {
		logger.Info("Removing test file", slog.String("file", destFilePath))
		if removeErr := os.Remove(destFilePath); removeErr != nil {
			t.Logf("Error removing test file: %v", removeErr)
		}
	}()

	// Allow time for file detection and playlist update.
	logger.Info("Waiting for playlist update...")
	time.Sleep(3 * time.Second)

	// Get updated track list.
	updatedTracks := getCurrentTracks(t, client, baseURL, route)

	// Check that track list has changed.
	if len(updatedTracks) <= len(initialTracks) {
		t.Errorf("Track list did not increase after adding file: was %d, now %d",
			len(initialTracks), len(updatedTracks))
	}

	// Check that new file appears in the list.
	foundNewFile := false
	for _, track := range updatedTracks {
		if filepath.Base(track) == destFileName {
			foundNewFile = true
			break
		}
	}

	if !foundNewFile {
		t.Errorf("New file %s not found in updated track list", destFileName)
	} else {
		logger.Info("New file successfully added to playlist", slog.String("file", destFileName))
	}
}

// Helper function to copy a file.
func copyFile(src, dst string) error {
	// Open source file.
	sourceFile, openErr := os.Open(src)
	if openErr != nil {
		return openErr
	}
	defer sourceFile.Close()

	// Create destination file.
	destFile, createErr := os.Create(dst)
	if createErr != nil {
		return createErr
	}
	defer destFile.Close()

	// Copy contents.
	_, copyErr := destFile.ReadFrom(sourceFile)
	return copyErr
}

// Helper function to get route by directory.
func getRouteForDirectory(t *testing.T, client *http.Client, baseURL, directory string) string {
	// Get stream information.
	streamsResp, getStreamsErr := client.Get(fmt.Sprintf("%s/streams", baseURL))
	if getStreamsErr != nil {
		t.Fatalf("Error getting stream information: %v", getStreamsErr)
	}
	defer streamsResp.Body.Close()

	var streamsData map[string]interface{}
	if decodeStreamsJSONErr := json.NewDecoder(streamsResp.Body).Decode(&streamsData); decodeStreamsJSONErr != nil {
		t.Fatalf("Error decoding JSON: %v", decodeStreamsJSONErr)
	}

	// Find corresponding route.
	streams, streamsTypeOk := streamsData["streams"].([]interface{})
	if !streamsTypeOk {
		return ""
	}

	for _, stream := range streams {
		streamMap, streamMapOk := stream.(map[string]interface{})
		if !streamMapOk {
			continue
		}
		dir, dirTypeOk := streamMap["directory"].(string)
		if !dirTypeOk {
			continue
		}
		dir = filepath.Clean(dir)
		directory = filepath.Clean(directory)
		if dir == directory || filepath.Base(dir) == filepath.Base(directory) {
			route, routeTypeOk := streamMap["route"].(string)
			if routeTypeOk {
				return route
			}
		}
	}

	return ""
}

// Get track list.
func getCurrentTracks(t *testing.T, client *http.Client, baseURL, route string) []string {
	// Remove leading slash for API request.
	if route[0] == '/' {
		route = route[1:]
	}

	// Request playlist information via API.
	resp, getPlaylistInfoErr := client.Get(fmt.Sprintf("%s/playlist-info?route=/%s", baseURL, route))
	if getPlaylistInfoErr != nil {
		t.Fatalf("Error getting playlist information: %v", getPlaylistInfoErr)
	}
	defer resp.Body.Close()

	// Check response.
	var response map[string]interface{}
	if decodePlaylistJSONErr := json.NewDecoder(resp.Body).Decode(&response); decodePlaylistJSONErr != nil {
		t.Fatalf("Failed to decode response: %v", decodePlaylistJSONErr)
	}

	// Extract track list.
	tracks, tracksTypeOk := response["tracks"].([]interface{})
	if !tracksTypeOk {
		t.Fatalf("Track list missing in API response")
	}

	// Convert to string list.
	var tracksList []string
	for _, track := range tracks {
		if trackStr, trackStrOk := track.(string); trackStrOk {
			tracksList = append(tracksList, trackStr)
		}
	}

	return tracksList
}
