package e2e

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	httpServer "github.com/user/stream-audio-to-web/http"
	"github.com/user/stream-audio-to-web/relay"
)

// Mock HTTP server that serves fake audio content
type mockAudioServer struct {
	srv *httptest.Server
}

func newMockAudioServer() *mockAudioServer {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		// Simulate audio stream - send "mock audio data" in chunks
		for i := 0; i < 3; i++ {
			w.Write([]byte("mock audio data"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(100 * time.Millisecond)
		}
	})

	return &mockAudioServer{
		srv: httptest.NewServer(handler),
	}
}

func (m *mockAudioServer) Close() {
	m.srv.Close()
}

func (m *mockAudioServer) URL() string {
	return m.srv.URL
}

func TestRelayEndToEnd(t *testing.T) {
	// Create a mock audio server that will be relayed
	mockServer := newMockAudioServer()
	defer mockServer.Close()

	// Create a temporary file for testing
	tempDir, err := os.MkdirTemp("", "relay-e2e-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	configFile := filepath.Join(tempDir, "relay_list.json")

	// Create a new relay manager
	relayManager := relay.NewRelayManager(configFile)
	
	// Add test URL
	testStreamURL := mockServer.URL()
	
	if err := relayManager.AddLink(testStreamURL); err != nil {
		t.Fatalf("Failed to add test URL: %v", err)
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

	// Get the status password from environment or use default
	testPassword := os.Getenv("STATUS_PASSWORD")
	if testPassword == "" {
		testPassword = "test-password"
	}
	server.SetStatusPassword(testPassword)
	
	authCookie := &http.Cookie{
		Name:  "status_auth",
		Value: testPassword,
	}

	// Test the relay stream endpoint
	t.Run("TestRelayStream", func(t *testing.T) {
		// Stream index 0 corresponds to our mock stream
		streamURL := testServer.URL + "/relay/stream/0"
		
		req, err := http.NewRequest("GET", streamURL, nil)
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}
		
		client := &http.Client{
			Timeout: 5 * time.Second,
		}
		
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("Failed to get relay stream: %v", err)
		}
		defer resp.Body.Close()
		
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected status OK, got %v", resp.StatusCode)
		}
		
		// Check content type
		contentType := resp.Header.Get("Content-Type")
		if !strings.Contains(contentType, "audio") {
			t.Errorf("Expected audio content type, got %s", contentType)
		}
		
		// Read stream data (with timeout)
		dataReceived := make(chan bool, 1)
		var buf bytes.Buffer
		
		go func() {
			_, err := io.Copy(&buf, resp.Body)
			if err != nil && err != io.EOF {
				t.Errorf("Error reading stream data: %v", err)
			}
			dataReceived <- true
		}()
		
		// Wait for data or timeout
		select {
		case <-dataReceived:
			// Success - we got some data
			data := buf.String()
			if !strings.Contains(data, "mock audio data") {
				t.Errorf("Did not receive expected stream data. Got: %s", data)
			}
		case <-time.After(2 * time.Second):
			t.Error("Timeout waiting for stream data")
		}
	})
	
	// Test management operations
	t.Run("TestRelayManagement", func(t *testing.T) {
		// Test adding a new relay URL
		formData := url.Values{}
		formData.Set("url", "https://example.com/stream2")
		
		req, err := http.NewRequest("POST", testServer.URL+"/relay/add", 
			strings.NewReader(formData.Encode()))
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(authCookie)
		
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to add relay URL: %v", err)
		}
		resp.Body.Close()
		
		// Verify two URLs are now in the list
		links := relayManager.GetLinks()
		if len(links) != 2 {
			t.Errorf("Expected 2 links after adding, got %d", len(links))
		}
		
		// Test removing a relay URL
		formData = url.Values{}
		formData.Set("index", "1") // Remove the second URL
		
		req, err = http.NewRequest("POST", testServer.URL+"/relay/remove", 
			strings.NewReader(formData.Encode()))
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(authCookie)
		
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to remove relay URL: %v", err)
		}
		resp.Body.Close()
		
		// Verify back to one URL
		links = relayManager.GetLinks()
		if len(links) != 1 {
			t.Errorf("Expected 1 link after removing, got %d", len(links))
		}
	})
} 