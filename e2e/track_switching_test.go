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

func TestTrackSwitchingEndpoints(t *testing.T) {
	// Get base URL from environment variable or use default value
	baseURL := getEnvOrDefault("TEST_SERVER_URL", "http://localhost:8000")
	password := getEnvOrDefault("STATUS_PASSWORD", "1234554321")

	// Get information about available streams
	streamsResp, getStreamsInfoErr := http.Get(fmt.Sprintf("%s/streams", baseURL))
	if getStreamsInfoErr != nil {
		t.Fatalf("Failed to get streams info: %v", getStreamsInfoErr)
	}
	defer streamsResp.Body.Close()

	// Decode JSON response
	var streamsData map[string]interface{}
	if decodeStreamsResponseErr := json.NewDecoder(streamsResp.Body).Decode(&streamsData); decodeStreamsResponseErr != nil {
		t.Fatalf("Failed to decode streams response: %v", decodeStreamsResponseErr)
		return
	}

	// Find first available route
	streams, streamsTypeOk := streamsData["streams"].([]interface{})
	if !streamsTypeOk || len(streams) == 0 {
		t.Skip("No streams available for testing")
		return
	}

	// Take first available stream for test
	var routeName string
	if stream, streamMapOk := streams[0].(map[string]interface{}); streamMapOk {
		if route, routeTypeOk := stream["route"].(string); routeTypeOk {
			routeName = route
			// Remove leading slash if present
			if routeName[0] == '/' {
				routeName = routeName[1:]
			}
		}
	}

	if routeName == "" {
		t.Skip("Could not determine stream route for testing")
		return
	}

	t.Logf("Using stream route: %s for track switching tests", routeName)

	// Create client with cookie support for authentication
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:     jar,
		Timeout: 10 * time.Second,
	}

	// Authenticate
	form := url.Values{}
	form.Add("password", password)

	_, authStatusPageErr := client.PostForm(fmt.Sprintf("%s/status", baseURL), form)
	if authStatusPageErr != nil {
		t.Fatalf("Failed to authenticate: %v", authStatusPageErr)
	}

	// Test 1: Check next track API
	testNextTrackAPI(t, client, baseURL, routeName)

	// Test 2: Check previous track API
	testPrevTrackAPI(t, client, baseURL, routeName)
}

func testNextTrackAPI(t *testing.T, client *http.Client, baseURL, routeName string) {
	// Get current track information
	initialTrackInfo := getCurrentTrackInfo(t, client, baseURL, routeName)
	initialTrackName := initialTrackInfo["track"].(string)

	t.Logf("Initial track: %s", initialTrackName)

	// Call API to switch to next track (use AJAX=1 to get JSON)
	nextTrackURL := fmt.Sprintf("%s/next-track/%s?ajax=1", baseURL, routeName)
	resp, callNextTrackAPIErr := client.Post(nextTrackURL, "application/x-www-form-urlencoded", nil)
	if callNextTrackAPIErr != nil {
		t.Fatalf("Failed to call next track API: %v", callNextTrackAPIErr)
	}
	defer resp.Body.Close()

	// Check response code
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d for next-track API, got %d", http.StatusOK, resp.StatusCode)
		body, _ := io.ReadAll(resp.Body)
		t.Logf("Response body: %s", string(body))
		return
	}

	// Check that content type is JSON
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "application/json") {
		t.Errorf("Expected Content-Type to be application/json, got %s", contentType)
	}

	// Decode JSON response
	var responseData map[string]interface{}
	if decodeNextTrackJSONErr := json.NewDecoder(resp.Body).Decode(&responseData); decodeNextTrackJSONErr != nil {
		t.Errorf("Failed to decode JSON response: %v", decodeNextTrackJSONErr)
		return
	}

	// Check that response contains data about successful switching
	success, successTypeOk := responseData["success"].(bool)
	if !successTypeOk || !success {
		t.Errorf("Next track API did not report success")
	}

	// Give server time to switch
	time.Sleep(500 * time.Millisecond)

	// Get information about new current track
	newTrackInfo := getCurrentTrackInfo(t, client, baseURL, routeName)
	newTrackName := newTrackInfo["track"].(string)

	t.Logf("New track after next: %s", newTrackName)

	// Check that track has actually changed
	if newTrackName == initialTrackName {
		t.Errorf("Track did not change after calling next-track API")
	}
}

func testPrevTrackAPI(t *testing.T, client *http.Client, baseURL, routeName string) {
	// Get current track information
	initialTrackInfo := getCurrentTrackInfo(t, client, baseURL, routeName)
	initialTrackName := initialTrackInfo["track"].(string)

	t.Logf("Initial track before prev: %s", initialTrackName)

	// Call API to switch to previous track
	prevTrackURL := fmt.Sprintf("%s/prev-track/%s?ajax=1", baseURL, routeName)
	resp, callPrevTrackAPIErr := client.Post(prevTrackURL, "application/x-www-form-urlencoded", nil)
	if callPrevTrackAPIErr != nil {
		t.Fatalf("Failed to call previous track API: %v", callPrevTrackAPIErr)
	}
	defer resp.Body.Close()

	// Check response code
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d for prev-track API, got %d", http.StatusOK, resp.StatusCode)
		return
	}

	// Give server time to switch
	time.Sleep(500 * time.Millisecond)

	// Get information about new current track
	newTrackInfo := getCurrentTrackInfo(t, client, baseURL, routeName)
	newTrackName := newTrackInfo["track"].(string)

	t.Logf("New track after prev: %s", newTrackName)

	// Check that track has actually changed
	if newTrackName == initialTrackName {
		t.Errorf("Track did not change after calling prev-track API")
	}
}

// Helper function to get current track information
func getCurrentTrackInfo(t *testing.T, client *http.Client, baseURL, routeName string) map[string]interface{} {
	trackInfoURL := fmt.Sprintf("%s/now-playing?route=/%s", baseURL, routeName)
	resp, getCurrentTrackInfoErr := client.Get(trackInfoURL)
	if getCurrentTrackInfoErr != nil {
		t.Fatalf("Failed to get current track info: %v", getCurrentTrackInfoErr)
	}
	defer resp.Body.Close()

	var trackInfo map[string]interface{}
	if decodeTrackInfoResponseErr := json.NewDecoder(resp.Body).Decode(&trackInfo); decodeTrackInfoResponseErr != nil {
		t.Fatalf("Failed to decode track info response: %v", decodeTrackInfoResponseErr)
	}

	return trackInfo
}
