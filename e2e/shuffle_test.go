package e2e

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Helper function to get base URL and password
func getBaseURLAndPassword(t *testing.T) (string, string) {
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
		Jar: jar,
		Timeout: 10 * time.Second,
	}
	
	// Authenticate on the status page
	form := url.Values{}
	form.Add("password", password)
	resp, err := client.PostForm(fmt.Sprintf("%s/status", baseURL), form)
	if err != nil {
		t.Fatalf("Authentication error: %v", err)
	}
	resp.Body.Close()
	
	// Get available streams
	streamsResp, err := client.Get(fmt.Sprintf("%s/streams", baseURL))
	if err != nil {
		t.Fatalf("Error getting streams list: %v", err)
	}
	
	var streamsData map[string]interface{}
	if err := json.NewDecoder(streamsResp.Body).Decode(&streamsData); err != nil {
		t.Fatalf("Error decoding JSON: %v", err)
	}
	streamsResp.Body.Close()
	
	// Find the first route for testing
	streams, ok := streamsData["streams"].([]interface{})
	if !ok || len(streams) == 0 {
		t.Skip("No streams available for testing")
		return
	}
	
	routeName := ""
	if stream, ok := streams[0].(map[string]interface{}); ok {
		if route, ok := stream["route"].(string); ok {
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
	for i := 0; i < len(trackList1) && i < len(trackList2); i++ {
		if trackList1[i] != trackList2[i] {
			sameSequence = false
			break
		}
	}
	
	// Get current configuration from API
	configResp, err := client.Get(fmt.Sprintf("%s/streams", baseURL))
	if err != nil {
		t.Fatalf("Error getting configuration: %v", err)
	}
	defer configResp.Body.Close()
	
	var configData map[string]interface{}
	if err := json.NewDecoder(configResp.Body).Decode(&configData); err != nil {
		t.Fatalf("Error decoding configuration: %v", err)
	}
	
	// Check if shuffle mode is enabled
	shuffleEnabled := false
	for _, stream := range streams {
		if streamMap, ok := stream.(map[string]interface{}); ok {
			if streamRoute, ok := streamMap["route"].(string); ok {
				if strings.TrimPrefix(streamRoute, "/") == routeName {
					if shuffle, ok := streamMap["shuffle"].(bool); ok {
						shuffleEnabled = shuffle
						break
					}
				}
			}
		}
	}
	
	// Make conclusions based on sequence comparison and shuffle setting
	t.Logf("Sequence 1: %v", trackList1)
	t.Logf("Sequence 2: %v", trackList2)
	t.Logf("Shuffle mode enabled: %v", shuffleEnabled)
	
	if shuffleEnabled && sameSequence {
		t.Errorf("Shuffle mode is enabled, but track sequences are identical")
	} else if !shuffleEnabled && !sameSequence {
		t.Errorf("Shuffle mode is disabled, but track sequences are different")
	}
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
	for i := 0; i < count-1; i++ {
		nextTrackURL := fmt.Sprintf("%s/next-track/%s?ajax=1", baseURL, routeName)
		resp, err := client.Post(nextTrackURL, "application/x-www-form-urlencoded", nil)
		if err != nil {
			t.Fatalf("Error switching track: %v", err)
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
	resp, err := client.Get(trackInfoURL)
	if err != nil {
		t.Fatalf("Error getting current track info: %v", err)
	}
	defer resp.Body.Close()
	
	var trackInfo map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&trackInfo); err != nil {
		t.Fatalf("Error decoding track info: %v", err)
	}
	
	// Return track name without path
	trackFullPath, ok := trackInfo["track"].(string)
	if !ok {
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
	for i := 0; i < 10; i++ {
		prevTrackURL := fmt.Sprintf("%s/prev-track/%s?ajax=1", baseURL, routeName)
		resp, err := client.Post(prevTrackURL, "application/x-www-form-urlencoded", nil)
		if err != nil {
			t.Fatalf("Error resetting stream: %v", err)
		}
		resp.Body.Close()
		time.Sleep(100 * time.Millisecond)
	}
	
	// Allow time for state stabilization
	time.Sleep(500 * time.Millisecond)
} 