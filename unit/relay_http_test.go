package unit

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	httpServer "github.com/user/stream-audio-to-web/http"
	"github.com/user/stream-audio-to-web/relay"
)

func TestRelayHTTPHandlers(t *testing.T) {
	// Create a temporary file for testing
	tempDir, err := os.MkdirTemp("", "relay-http-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	configFile := filepath.Join(tempDir, "relay_list.json")

	// Create a new relay manager
	relayManager := relay.NewRelayManager(configFile)
	
	// Add test URLs
	testURL1 := "http://example.com/stream1"
	testURL2 := "https://example.org/stream2"
	
	if err := relayManager.AddLink(testURL1); err != nil {
		t.Fatalf("Failed to add test URL1: %v", err)
	}
	if err := relayManager.AddLink(testURL2); err != nil {
		t.Fatalf("Failed to add test URL2: %v", err)
	}
	
	// Activate relay
	relayManager.SetActive(true)

	// Create a new HTTP server
	server := httpServer.NewServer("mp3", 10)
	
	// Set relay manager
	server.SetRelayManager(relayManager)
	
	// Create test server
	testServer := httptest.NewServer(server.Handler())
	defer testServer.Close()

	// Create a test cookie for authentication
	testPassword := "test-password"
	server.SetStatusPassword(testPassword)
	
	authCookie := &http.Cookie{
		Name:  "status_auth",
		Value: testPassword,
	}

	// Test relay management page
	t.Run("TestRelayManagementPage", func(t *testing.T) {
		req, err := http.NewRequest("GET", testServer.URL+"/relay-management", nil)
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}
		req.AddCookie(authCookie)
		
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to get relay management page: %v", err)
		}
		defer resp.Body.Close()
		
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected status OK, got %v", resp.StatusCode)
		}
		
		// Check content type
		contentType := resp.Header.Get("Content-Type")
		if !strings.Contains(contentType, "text/html") {
			t.Errorf("Expected HTML content type, got %s", contentType)
		}
	})
	
	// Test add relay link
	t.Run("TestAddRelayLink", func(t *testing.T) {
		formData := url.Values{}
		formData.Set("url", "https://example.net/stream3")
		
		req, err := http.NewRequest("POST", testServer.URL+"/relay/add", 
			strings.NewReader(formData.Encode()))
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(authCookie)
		
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to add relay link: %v", err)
		}
		defer resp.Body.Close()
		
		// Expect a redirect to relay management page
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("Expected redirect status, got %v", resp.StatusCode)
		}
		
		// Verify link was added
		links := relayManager.GetLinks()
		found := false
		for _, link := range links {
			if link == "https://example.net/stream3" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Link was not added to relay manager")
		}
	})
	
	// Test remove relay link
	t.Run("TestRemoveRelayLink", func(t *testing.T) {
		// Index 0 corresponds to the first test URL
		formData := url.Values{}
		formData.Set("index", "0")
		
		req, err := http.NewRequest("POST", testServer.URL+"/relay/remove", 
			strings.NewReader(formData.Encode()))
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(authCookie)
		
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to remove relay link: %v", err)
		}
		defer resp.Body.Close()
		
		// Expect a redirect to relay management page
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("Expected redirect status, got %v", resp.StatusCode)
		}
		
		// Verify link was removed
		links := relayManager.GetLinks()
		for _, link := range links {
			if link == testURL1 {
				t.Errorf("Link was not removed from relay manager")
			}
		}
	})
	
	// Test toggle relay
	t.Run("TestToggleRelay", func(t *testing.T) {
		// Turn off relay
		formData := url.Values{}
		formData.Set("active", "false")
		
		req, err := http.NewRequest("POST", testServer.URL+"/relay/toggle", 
			strings.NewReader(formData.Encode()))
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(authCookie)
		
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to toggle relay: %v", err)
		}
		defer resp.Body.Close()
		
		// Expect a redirect to relay management page
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("Expected redirect status, got %v", resp.StatusCode)
		}
		
		// Verify relay was turned off
		if relayManager.IsActive() {
			t.Errorf("Relay should be inactive after setting active=false")
		}
		
		// Turn relay back on
		formData.Set("active", "true")
		
		req, err = http.NewRequest("POST", testServer.URL+"/relay/toggle", 
			strings.NewReader(formData.Encode()))
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(authCookie)
		
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to toggle relay: %v", err)
		}
		defer resp.Body.Close()
		
		// Verify relay was turned on
		if !relayManager.IsActive() {
			t.Errorf("Relay should be active after setting active=true")
		}
	})
} 