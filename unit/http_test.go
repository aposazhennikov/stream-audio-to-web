package unit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	httpServer "github.com/user/stream-audio-to-web/http"
)

// Mock implementation for StreamHandler
type mockStreamHandler struct {
	clientCount int
	trackChan   chan string
}

func (m *mockStreamHandler) AddClient() (<-chan []byte, int, error) {
	ch := make(chan []byte, 1)
	m.clientCount++
	return ch, 1, nil
}

func (m *mockStreamHandler) RemoveClient(clientID int) {
	m.clientCount--
}

func (m *mockStreamHandler) GetClientCount() int {
	return m.clientCount
}

func (m *mockStreamHandler) GetCurrentTrackChannel() <-chan string {
	return m.trackChan
}

// Mock implementation for PlaylistManager
type mockPlaylistManager struct {
	currentTrack string
	history      []interface{}
	startTime    time.Time
}

func (m *mockPlaylistManager) Reload() error {
	return nil
}

func (m *mockPlaylistManager) GetCurrentTrack() interface{} {
	return m.currentTrack
}

func (m *mockPlaylistManager) NextTrack() interface{} {
	m.currentTrack = "next_track.mp3"
	m.history = append(m.history, m.currentTrack)
	return m.currentTrack
}

func (m *mockPlaylistManager) GetHistory() []interface{} {
	return m.history
}

func (m *mockPlaylistManager) GetStartTime() time.Time {
	return m.startTime
}

func (m *mockPlaylistManager) PreviousTrack() interface{} {
	if len(m.history) > 1 {
		m.currentTrack = m.history[len(m.history)-2].(string)
	}
	return m.currentTrack
}

// Shuffle implements PlaylistManager.Shuffle method
func (m *mockPlaylistManager) Shuffle() {
	// Mock implementation, doesn't need to actually shuffle anything
}

func TestHealthzEndpoint(t *testing.T) {
	// Create HTTP server
	server := httpServer.NewServer("mp3", 10)

	// Create test HTTP request
	req, err := http.NewRequest("GET", "/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Create ResponseRecorder to record the response
	rr := httptest.NewRecorder()

	// Process the request
	server.Handler().ServeHTTP(rr, req)

	// Check response code
	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, http.StatusOK)
	}

	// Check response body
	expected := "OK"
	if rr.Body.String() != expected {
		t.Errorf("handler returned unexpected body: got %v want %v",
			rr.Body.String(), expected)
	}
}

func TestReadyzEndpoint(t *testing.T) {
	// Create HTTP server
	server := httpServer.NewServer("mp3", 10)

	// Create test HTTP request
	req, err := http.NewRequest("GET", "/readyz", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Create ResponseRecorder to record the response
	rr := httptest.NewRecorder()

	// Process the request
	server.Handler().ServeHTTP(rr, req)

	// Check response code
	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, http.StatusOK)
	}
}

func TestStreamRegistration(t *testing.T) {
	// Create HTTP server
	server := httpServer.NewServer("mp3", 10)

	// Create mocks for stream and playlist
	mockStream := &mockStreamHandler{
		clientCount: 0,
		trackChan:   make(chan string),
	}

	mockPlaylist := &mockPlaylistManager{
		currentTrack: "test.mp3",
		history:      []interface{}{"test.mp3"},
		startTime:    time.Now(),
	}

	// Register stream
	server.RegisterStream("/test", mockStream, mockPlaylist)

	// Check that stream is registered
	if !server.IsStreamRegistered("/test") {
		t.Errorf("Stream not registered properly")
	}
}

// AddTestSetShuffleMode tests the SetShuffleMode handler
func TestSetShuffleMode(t *testing.T) {
	// Create HTTP server
	server := httpServer.NewServer("mp3", 10)

	// We cannot directly call SetStatusPassword,
	// so we will rely on the default password ("1234554321")
	// server.(*httpServer.TestableServer).SetStatusPassword("testpassword")

	// Create mocks for stream and playlist
	mockStream := &mockStreamHandler{
		clientCount: 0,
		trackChan:   make(chan string),
	}

	mockPlaylist := &mockPlaylistManager{
		currentTrack: "test.mp3",
		history:      []interface{}{"test.mp3"},
		startTime:    time.Now(),
	}

	// Register stream
	server.RegisterStream("/test", mockStream, mockPlaylist)

	// Use default password for tests
	defaultPassword := "1234554321"

	// Test cases
	testCases := []struct {
		name           string
		path           string
		method         string
		cookieAuth     bool
		expectedStatus int
	}{
		{
			name:           "SetShuffleOnWithAuth",
			path:           "/set-shuffle/test/on",
			method:         "POST",
			cookieAuth:     true,
			expectedStatus: http.StatusSeeOther, // Redirect to status page
		},
		{
			name:           "SetShuffleOffWithAuth",
			path:           "/set-shuffle/test/off",
			method:         "POST",
			cookieAuth:     true,
			expectedStatus: http.StatusSeeOther, // Redirect to status page
		},
		{
			name:           "SetShuffleOnWithoutAuth",
			path:           "/set-shuffle/test/on",
			method:         "POST",
			cookieAuth:     false,
			expectedStatus: http.StatusSeeOther, // Redirect to login page
		},
		{
			name:           "SetShuffleOffWithoutAuth",
			path:           "/set-shuffle/test/off",
			method:         "POST",
			cookieAuth:     false,
			expectedStatus: http.StatusSeeOther, // Redirect to login page
		},
		{
			name:           "SetShuffleInvalidMode",
			path:           "/set-shuffle/test/invalid",
			method:         "POST",
			cookieAuth:     true,
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "SetShuffleNonExistentRoute",
			path:           "/set-shuffle/nonexistent/on",
			method:         "POST",
			cookieAuth:     true,
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create test HTTP request
			req, err := http.NewRequest(tc.method, tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}

			// Add authentication cookie if needed
			if tc.cookieAuth {
				req.AddCookie(&http.Cookie{
					Name:  "status_auth",
					Value: defaultPassword,
				})
			}

			// Create ResponseRecorder to record the response
			rr := httptest.NewRecorder()

			// Process the request
			server.Handler().ServeHTTP(rr, req)

			// Check response code
			if status := rr.Code; status != tc.expectedStatus {
				t.Errorf("handler returned wrong status code: got %v want %v",
					status, tc.expectedStatus)
			}
		})
	}
} 