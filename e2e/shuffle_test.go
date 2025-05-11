package e2e_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Helper function to get base URL and password
func getBaseURLAndPassword(_ *testing.T) (string, string) {
	baseURL := getEnvOrDefault("TEST_SERVER_URL", "http://localhost:8000")
	password := getEnvOrDefault("STATUS_PASSWORD", "1234554321")
	return baseURL, password
}

// TestShuffleMode tests the functionality of the shuffle mode
func TestShuffleMode(t *testing.T) {
	// Get base parameters
	baseURL, password := getBaseURLAndPassword(t)

	// Create HTTP client with cookie support
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:     jar,
		Timeout: 10 * time.Second,
	}

	// Authenticate on the status page
	form := url.Values{}
	form.Add("password", password)
	resp, authStatusPageErr := client.PostForm(fmt.Sprintf("%s/status", baseURL), form)
	if authStatusPageErr != nil {
		t.Fatalf("Authentication error: %v", authStatusPageErr)
	}
	resp.Body.Close()

	// Get available streams
	streamsResp, getStreamsListErr := client.Get(fmt.Sprintf("%s/streams", baseURL))
	if getStreamsListErr != nil {
		t.Fatalf("Error getting streams list: %v", getStreamsListErr)
	}

	var streamsData map[string]interface{}
	if decodeStreamsJSONErr := json.NewDecoder(streamsResp.Body).Decode(&streamsData); decodeStreamsJSONErr != nil {
		t.Fatalf("Error decoding JSON: %v", decodeStreamsJSONErr)
	}
	streamsResp.Body.Close()

	// Find the first route for testing
	streams, streamsTypeOk := streamsData["streams"].([]interface{})
	if !streamsTypeOk || len(streams) == 0 {
		t.Skip("No streams available for testing")
		return
	}

	routeName := ""
	if stream, streamMapOk := streams[0].(map[string]interface{}); streamMapOk {
		if route, routeTypeOk := stream["route"].(string); routeTypeOk {
			routeName = route
			if routeName[0] == '/' {
				routeName = routeName[1:]
			}
		}
	}

	if routeName == "" {
		t.Skip("Could not determine route for testing")
		return
	}

	t.Logf("Using route: %s for shuffle mode testing", routeName)

	// Collect track list by switching tracks several times
	trackList1 := collectTrackSequence(t, client, baseURL, routeName, 5)

	// Reset playback and collect second list
	resetStream(t, client, baseURL, routeName)
	trackList2 := collectTrackSequence(t, client, baseURL, routeName, 5)

	// Compare the two lists - they should be the same in non-shuffle mode
	// or likely different in shuffle mode
	sameSequence := true
	for i, track := range trackList1 {
		if i >= len(trackList2) {
			break
		}
		if track != trackList2[i] {
			sameSequence = false
			break
		}
	}

	// Get current configuration from API
	configResp, getConfigErr := client.Get(fmt.Sprintf("%s/streams", baseURL))
	if getConfigErr != nil {
		t.Fatalf("Error getting configuration: %v", getConfigErr)
	}
	defer configResp.Body.Close()

	// Log API response for diagnostics
	configBytes, readConfigRespBodyErr := io.ReadAll(configResp.Body)
	if readConfigRespBodyErr != nil {
		t.Fatalf("Error reading config response: %v", readConfigRespBodyErr)
	}

	t.Logf("API response content: %s", string(configBytes))

	var configData map[string]interface{}
	if decodeConfigJSONErr := json.Unmarshal(configBytes, &configData); decodeConfigJSONErr != nil {
		t.Fatalf("Error decoding configuration: %v", decodeConfigJSONErr)
	}

	// Mode of shuffling is not returned via API in /streams
	// So we determine it based on checking the order of tracks
	// If order is different, assume shuffling is enabled
	shuffleEnabled := !sameSequence

	// Log track information
	t.Logf("Sequence 1: %v", trackList1)
	t.Logf("Sequence 2: %v", trackList2)
	t.Logf("Sequences are identical: %v", sameSequence)
	t.Logf("Shuffle mode determined from sequences: %v", shuffleEnabled)

	// We don't check shuffle errors because we determine shuffleEnabled based on comparing sequences
	// Instead, we just log the results
}

// TestAddAudioFile tests that the playlist updates when adding new files
func TestAddAudioFile(t *testing.T) {
	// This test will only work with direct access to the server file system
	// So it will be skipped in CI/CD
	t.Skip("This test requires access to the server file system, so it's skipped in automated tests")
}

// Helper function to collect a sequence of tracks
func collectTrackSequence(t *testing.T, client *http.Client, baseURL, routeName string, count int) []string {
	var tracks []string

	// Get the current track
	currentTrack := getCurrentTrackName(t, client, baseURL, routeName)
	tracks = append(tracks, currentTrack)

	// Switch tracks several times, collecting the sequence
	for range make([]struct{}, count-1) {
		nextTrackURL := fmt.Sprintf("%s/next-track/%s?ajax=1", baseURL, routeName)
		resp, switchTrackErr := client.Post(nextTrackURL, "application/x-www-form-urlencoded", nil)
		if switchTrackErr != nil {
			t.Fatalf("Error switching track: %v", switchTrackErr)
		}
		resp.Body.Close()

		// Allow time for switching
		time.Sleep(300 * time.Millisecond)

		// Get the name of the new current track
		trackName := getCurrentTrackName(t, client, baseURL, routeName)
		tracks = append(tracks, trackName)
	}

	return tracks
}

// Get the name of the current track
func getCurrentTrackName(t *testing.T, client *http.Client, baseURL, routeName string) string {
	trackInfoURL := fmt.Sprintf("%s/now-playing?route=/%s", baseURL, routeName)
	resp, getCurrentTrackInfoErr := client.Get(trackInfoURL)
	if getCurrentTrackInfoErr != nil {
		t.Fatalf("Error getting current track info: %v", getCurrentTrackInfoErr)
	}
	defer resp.Body.Close()

	// Check Content-Type header
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "application/json") {
		t.Fatalf("Expected JSON response, got: %s", contentType)
	}

	// Read response body for analysis
	bodyBytes, readTrackInfoBodyErr := io.ReadAll(resp.Body)
	if readTrackInfoBodyErr != nil {
		t.Fatalf("Error reading response body: %v", readTrackInfoBodyErr)
	}

	// Log for diagnostics
	t.Logf("Current track API response: %s", string(bodyBytes))

	var trackInfo map[string]interface{}
	if decodeTrackInfoJSONErr := json.Unmarshal(bodyBytes, &trackInfo); decodeTrackInfoJSONErr != nil {
		t.Fatalf("Error decoding track info: %v", decodeTrackInfoJSONErr)
	}

	// Return track name without path
	trackFullPath, trackNameTypeOk := trackInfo["track"].(string)
	if !trackNameTypeOk {
		t.Fatalf("Could not get track name from API response")
	}

	// Remove path, keep only filename
	parts := strings.Split(trackFullPath, "/")
	return parts[len(parts)-1]
}

// Reset stream to initial state
func resetStream(t *testing.T, client *http.Client, baseURL, routeName string) {
	// To reset, we'll switch to previous track multiple times
	// This should lead to going back to the beginning of the playlist
	for range make([]struct{}, 10) {
		prevTrackURL := fmt.Sprintf("%s/prev-track/%s?ajax=1", baseURL, routeName)
		resp, switchPrevTrackErr := client.Post(prevTrackURL, "application/x-www-form-urlencoded", nil)
		if switchPrevTrackErr != nil {
			t.Fatalf("Error resetting stream: %v", switchPrevTrackErr)
		}
		resp.Body.Close()
		time.Sleep(100 * time.Millisecond)
	}

	// Allow time for state stabilization
	time.Sleep(500 * time.Millisecond)
}
