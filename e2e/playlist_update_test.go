package e2e

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPlaylistUpdate tests playlist updating when adding new files
// This test requires access to the server's filesystem,
// so it's intended only for manual execution
func TestPlaylistUpdate(t *testing.T) {
	// Check that test is run in manual mode with file access
	manualTest := os.Getenv("MANUAL_TEST")
	if manualTest != "1" {
		t.Skip("This test requires manual execution with filesystem access. Run it with MANUAL_TEST=1")
		return
	}
	
	// Test directory path must be specified in environment variable
	testDir := os.Getenv("TEST_AUDIO_DIR")
	if testDir == "" {
		t.Fatal("You must specify TEST_AUDIO_DIR for testing")
	}
	
	// Check that directory exists and is accessible for writing
	info, err := os.Stat(testDir)
	if err != nil {
		t.Fatalf("Error accessing directory %s: %v", testDir, err)
	}
	
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", testDir)
	}
	
	// Path to test audio file
	testAudioFile := os.Getenv("TEST_AUDIO_FILE")
	if testAudioFile == "" {
		t.Fatal("You must specify TEST_AUDIO_FILE - path to a test MP3/OGG/AAC file")
	}
	
	// Check that file exists
	_, err = os.Stat(testAudioFile)
	if err != nil {
		t.Fatalf("Error accessing file %s: %v", testAudioFile, err)
	}
	
	// Test server URL
	baseURL := getEnvOrDefault("TEST_SERVER_URL", "http://localhost:8000")
	password := getEnvOrDefault("STATUS_PASSWORD", "1234554321")
	
	// Create HTTP client with cookie support
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		Timeout: 10 * time.Second,
	}
	
	// Authenticate on status page
	form := url.Values{}
	form.Add("password", password)
	resp, err := client.PostForm(fmt.Sprintf("%s/status", baseURL), form)
	if err != nil {
		t.Fatalf("Authentication error: %v", err)
	}
	resp.Body.Close()
	
	// Find route corresponding to test directory
	route := getRouteForDirectory(t, client, baseURL, testDir)
	if route == "" {
		t.Fatalf("Could not find route for directory %s", testDir)
	}
	
	// Get initial track list for verification
	log.Printf("Getting initial track list for route %s", route)
	initialTracks := getCurrentTracks(t, client, baseURL, route)
	
	// Create unique name for test file
	destFileName := fmt.Sprintf("test_file_%d.mp3", time.Now().Unix())
	destFilePath := filepath.Join(testDir, destFileName)
	
	// Copy test file to directory
	log.Printf("Copying test file %s -> %s", testAudioFile, destFilePath)
	if err := copyFile(testAudioFile, destFilePath); err != nil {
		t.Fatalf("Error copying file: %v", err)
	}
	
	// Remove file after test completes
	defer func() {
		log.Printf("Removing test file %s", destFilePath)
		if err := os.Remove(destFilePath); err != nil {
			t.Logf("Error removing test file: %v", err)
		}
	}()
	
	// Allow time for file detection and playlist update
	log.Println("Waiting for playlist update...")
	time.Sleep(3 * time.Second)
	
	// Get updated track list
	updatedTracks := getCurrentTracks(t, client, baseURL, route)
	
	// Check that track list has changed
	if len(updatedTracks) <= len(initialTracks) {
		t.Errorf("Track list did not increase after adding file: was %d, now %d", 
			len(initialTracks), len(updatedTracks))
	}
	
	// Check that new file appears in the list
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
		log.Printf("New file %s successfully added to playlist", destFileName)
	}
}

// Helper function to copy a file
func copyFile(src, dst string) error {
	// Open source file
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()
	
	// Create destination file
	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destFile.Close()
	
	// Copy contents
	_, err = destFile.ReadFrom(sourceFile)
	return err
}

// Helper function to get route by directory
func getRouteForDirectory(t *testing.T, client *http.Client, baseURL, directory string) string {
	// Get stream information
	resp, err := client.Get(fmt.Sprintf("%s/streams", baseURL))
	if err != nil {
		t.Fatalf("Error getting stream information: %v", err)
	}
	defer resp.Body.Close()
	
	var streamsData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&streamsData); err != nil {
		t.Fatalf("Error decoding JSON: %v", err)
	}
	
	// Find corresponding route
	streams, ok := streamsData["streams"].([]interface{})
	if !ok {
		return ""
	}
	
	for _, stream := range streams {
		if streamMap, ok := stream.(map[string]interface{}); ok {
			if dir, ok := streamMap["directory"].(string); ok {
				// Normalize paths for comparison
				dir = filepath.Clean(dir)
				directory = filepath.Clean(directory)
				
				// Check exact match or match by base name
				if dir == directory || filepath.Base(dir) == filepath.Base(directory) {
					if route, ok := streamMap["route"].(string); ok {
						return route
					}
				}
			}
		}
	}
	
	return ""
}

// Get track list
func getCurrentTracks(t *testing.T, client *http.Client, baseURL, route string) []string {
	// Remove leading slash for API request
	if route[0] == '/' {
		route = route[1:]
	}
	
	// Request playlist information via API
	resp, err := client.Get(fmt.Sprintf("%s/playlist-info?route=/%s", baseURL, route))
	if err != nil {
		t.Fatalf("Error getting playlist information: %v", err)
	}
	defer resp.Body.Close()
	
	var playlistData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&playlistData); err != nil {
		t.Fatalf("Error decoding playlist information: %v", err)
	}
	
	// Extract track list
	tracks, ok := playlistData["tracks"].([]interface{})
	if !ok {
		t.Fatalf("Track list missing in API response")
	}
	
	// Convert to string list
	var tracksList []string
	for _, track := range tracks {
		if trackStr, ok := track.(string); ok {
			tracksList = append(tracksList, trackStr)
		}
	}
	
	return tracksList
} 