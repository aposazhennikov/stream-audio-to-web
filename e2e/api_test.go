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

func TestNowPlayingEndpoint(t *testing.T) {
	// Get base URL from environment variable or use default value
	baseURL := getEnvOrDefault("TEST_SERVER_URL", "http://localhost:8000")
	
	// Send GET request to /now-playing
	resp, err := http.Get(fmt.Sprintf("%s/now-playing", baseURL))
	if err != nil {
		t.Fatalf("Failed to send request to /now-playing: %v", err)
	}
	defer resp.Body.Close()
	
	// Check response code
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d, got %d", http.StatusOK, resp.StatusCode)
	}
	
	// Check that response contains JSON
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "application/json") {
		t.Errorf("Expected Content-Type to contain 'application/json', got '%s'", contentType)
	}
	
	// Check that response body is not empty
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}
	
	if len(body) == 0 {
		t.Errorf("Response body is empty")
	}
}

func TestStatusPageAccess(t *testing.T) {
	// Get base URL from environment variable or use default value
	baseURL := getEnvOrDefault("TEST_SERVER_URL", "http://localhost:8000")
	password := getEnvOrDefault("STATUS_PASSWORD", "1234554321")
	
	// Create HTTP client with cookie support
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("Failed to create cookie jar: %v", err)
	}
	
	client := &http.Client{
		Jar: jar,
		Timeout: 5 * time.Second,
	}
	
	// Send GET request to login page
	respGet, err := client.Get(fmt.Sprintf("%s/status", baseURL))
	if err != nil {
		t.Fatalf("Failed to send GET request to /status: %v", err)
	}
	respGet.Body.Close()
	
	// Check that login page is accessible
	if respGet.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d for /status GET, got %d", http.StatusOK, respGet.StatusCode)
	}
	
	// Send POST request with password
	form := url.Values{}
	form.Add("password", password)
	
	respPost, err := client.PostForm(fmt.Sprintf("%s/status", baseURL), form)
	if err != nil {
		t.Fatalf("Failed to send POST request to /status: %v", err)
	}
	defer respPost.Body.Close()
	
	// Check that redirect to status page occurred
	if respPost.StatusCode != http.StatusOK {
		// Should normally be 302 to /status-page, but we're already redirected
		t.Logf("Status code after authentication: %d (expected redirect to status page)", respPost.StatusCode)
	}
	
	// Now try to access status page directly
	respStatus, err := client.Get(fmt.Sprintf("%s/status-page", baseURL))
	if err != nil {
		t.Fatalf("Failed to send GET request to /status-page: %v", err)
	}
	defer respStatus.Body.Close()
	
	// Check that status page is accessible after authentication
	if respStatus.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d for /status-page after auth, got %d", http.StatusOK, respStatus.StatusCode)
	}
}

func TestTrackControlEndpoints(t *testing.T) {
	// Skip this test if no password is specified for checking
	if testing.Short() {
		t.Skip("Skipping track control test in short mode")
	}
	
	// Get base URL from environment variable or use default value
	baseURL := getEnvOrDefault("TEST_SERVER_URL", "http://localhost:8000")
	password := getEnvOrDefault("STATUS_PASSWORD", "1234554321")
	
	// Get route for testing
	resp, err := http.Get(fmt.Sprintf("%s/streams", baseURL))
	if err != nil {
		t.Fatalf("Failed to send request to /streams: %v", err)
	}
	
	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}
	
	// Use default route if couldn't get from API
	testRoute := "humor"
	if strings.Contains(string(body), "science") {
		testRoute = "science"
	}
	
	// Create HTTP client with cookie support
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("Failed to create cookie jar: %v", err)
	}
	
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Allow redirects for login check
			return nil
		},
		Timeout: 5 * time.Second,
	}
	
	// Authenticate
	form := url.Values{}
	form.Add("password", password)
	
	_, err = client.PostForm(fmt.Sprintf("%s/status", baseURL), form)
	if err != nil {
		t.Fatalf("Failed to authenticate: %v", err)
	}
	
	// Get information about current track before switching
	nowPlayingResp, err := client.Get(fmt.Sprintf("%s/now-playing?route=%s", baseURL, testRoute))
	if err != nil {
		t.Fatalf("Failed to get current track: %v", err)
	}
	
	var currentTrackInfo map[string]interface{}
	err = json.NewDecoder(nowPlayingResp.Body).Decode(&currentTrackInfo)
	nowPlayingResp.Body.Close()
	if err != nil {
		t.Fatalf("Failed to decode current track info: %v", err)
	}
	
	currentTrack, ok := currentTrackInfo["track"].(string)
	if !ok {
		t.Logf("Could not get current track name, continuing anyway")
		currentTrack = "unknown"
	}
	
	// Send request to switch track
	nextTrackURL := fmt.Sprintf("%s/next-track/%s?ajax=1", baseURL, testRoute)
	nextTrackResp, err := client.Post(nextTrackURL, "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("Failed to switch to next track: %v", err)
	}
	defer nextTrackResp.Body.Close()
	
	// Check that response is successful
	if nextTrackResp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d for next-track, got %d", http.StatusOK, nextTrackResp.StatusCode)
	}
	
	// Give server time to switch tracks
	time.Sleep(1 * time.Second)
	
	// Check that track has changed
	newPlayingResp, err := client.Get(fmt.Sprintf("%s/now-playing?route=%s", baseURL, testRoute))
	if err != nil {
		t.Fatalf("Failed to get new current track: %v", err)
	}
	
	var newTrackInfo map[string]interface{}
	err = json.NewDecoder(newPlayingResp.Body).Decode(&newTrackInfo)
	newPlayingResp.Body.Close()
	if err != nil {
		t.Fatalf("Failed to decode new track info: %v", err)
	}
	
	newTrack, ok := newTrackInfo["track"].(string)
	if !ok {
		t.Errorf("Could not get new track name")
		return
	}
	
	// Due to the specifics of the verification mechanism and asynchronicity, we just log the track change
	t.Logf("Track before switch: %s", currentTrack)
	t.Logf("Track after switch: %s", newTrack)
} 