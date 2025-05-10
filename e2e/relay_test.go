package e2e_test

import (
	"bytes"
	"errors"
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
	"github.com/user/stream-audio-to-web/slog"
)

// Mock HTTP server that serves fake audio content
type mockAudioServer struct {
	srv *httptest.Server
}

func newMockAudioServer() *mockAudioServer {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		// Simulate audio stream - send "mock audio data" in chunks
		for range [3]struct{}{} {
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
	tempDir := t.TempDir()

	configFile := filepath.Join(tempDir, "relay_list.json")

	// Create a new relay manager
	relayManager := relay.NewRelayManager(configFile, slog.Default())

	// Add test URL
	testStreamURL := mockServer.URL()

	if addTestURLErr := relayManager.AddLink(testStreamURL); addTestURLErr != nil {
		t.Fatalf("Failed to add test URL: %v", addTestURLErr)
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

		requestRelayStream, createRelayStreamRequestErr := http.NewRequest(http.MethodGet, streamURL, nil)
		if createRelayStreamRequestErr != nil {
			t.Fatalf("Failed to create request: %v", createRelayStreamRequestErr)
		}

		client := &http.Client{
			Timeout: 5 * time.Second,
		}

		resp, getRelayStreamErr := client.Do(requestRelayStream)
		if getRelayStreamErr != nil {
			t.Fatalf("Failed to get relay stream: %v", getRelayStreamErr)
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
			_, readStreamDataErr := io.Copy(&buf, resp.Body)
			if readStreamDataErr != nil && !errors.Is(readStreamDataErr, io.EOF) {
				t.Errorf("Error reading stream data: %v", readStreamDataErr)
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

		requestAddRelay, createAddRelayRequestErr := http.NewRequest(http.MethodPost, testServer.URL+"/relay/add",
			strings.NewReader(formData.Encode()))
		if createAddRelayRequestErr != nil {
			t.Fatalf("Failed to create request: %v", createAddRelayRequestErr)
		}
		requestAddRelay.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		requestAddRelay.AddCookie(authCookie)

		resp, addRelayURLErr := http.DefaultClient.Do(requestAddRelay)
		if addRelayURLErr != nil {
			t.Fatalf("Failed to add relay URL: %v", addRelayURLErr)
		}
		defer resp.Body.Close()

		// Verify two URLs are now in the list
		links := relayManager.GetLinks()
		if len(links) != 2 {
			t.Errorf("Expected 2 links after adding, got %d", len(links))
		}

		// Test removing a relay URL
		formData = url.Values{}
		formData.Set("index", "1") // Remove the second URL

		requestRemoveRelay, createRemoveRelayRequestErr := http.NewRequest(http.MethodPost, testServer.URL+"/relay/remove",
			strings.NewReader(formData.Encode()))
		if createRemoveRelayRequestErr != nil {
			t.Fatalf("Failed to create request: %v", createRemoveRelayRequestErr)
		}
		requestRemoveRelay.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		requestRemoveRelay.AddCookie(authCookie)

		resp, removeRelayURLErr := http.DefaultClient.Do(requestRemoveRelay)
		if removeRelayURLErr != nil {
			t.Fatalf("Failed to remove relay URL: %v", removeRelayURLErr)
		}
		defer resp.Body.Close()

		// Verify back to one URL
		links = relayManager.GetLinks()
		if len(links) != 1 {
			t.Errorf("Expected 1 link after removing, got %d", len(links))
		}
	})
}
