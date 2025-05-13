package unit_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/user/stream-audio-to-web/relay"
	"github.com/user/stream-audio-to-web/slog"
)

// mockHTTPServer creates a mock HTTP server for relay tests.
type mockHTTPServer struct {
	relayManager   *relay.Manager
	statusPassword string
	t              *testing.T
}

func newMockHTTPServer(relayManager *relay.Manager, password string, t *testing.T) *mockHTTPServer {
	return &mockHTTPServer{
		relayManager:   relayManager,
		statusPassword: password,
		t:              t,
	}
}

func (m *mockHTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Authentication check.
	if !m.authenticate(w, r) {
		return
	}

	m.t.Logf("DEBUG: Processing request: %s %s", r.Method, r.URL.Path)

	// Маршрутизация запросов.
	switch {
	case r.URL.Path == "/relay-management" && r.Method == http.MethodGet:
		m.handleManagementPage(w, r)
	case r.URL.Path == "/relay/add" && r.Method == http.MethodPost:
		m.handleAddLink(w, r)
	case r.URL.Path == "/relay/remove" && r.Method == http.MethodPost:
		m.handleRemoveLink(w, r)
	case r.URL.Path == "/relay/toggle" && r.Method == http.MethodPost:
		m.handleToggleRelay(w, r)
	default:
		m.t.Logf("DEBUG: Unknown path: %s", r.URL.Path)
		http.Error(w, "Not found", http.StatusNotFound)
	}
}

// authenticate проверяет аутентификацию запроса.
func (m *mockHTTPServer) authenticate(w http.ResponseWriter, r *http.Request) bool {
	authCookie, err := r.Cookie("status_auth")
	if err != nil || authCookie.Value != m.statusPassword {
		m.t.Logf("DEBUG: Authentication failed: %v", err)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

// handleManagementPage обрабатывает запрос страницы управления.
func (m *mockHTTPServer) handleManagementPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("<html><body>Relay Management Page</body></html>"))
}

// handleAddLink обрабатывает запрос на добавление ссылки.
func (m *mockHTTPServer) handleAddLink(w http.ResponseWriter, r *http.Request) {
	if !m.parseForm(w, r) {
		return
	}

	url := r.FormValue("url")
	m.t.Logf("DEBUG: URL from form: '%s'", url)

	if url == "" {
		m.t.Logf("DEBUG: URL is empty")
		http.Error(w, "URL cannot be empty", http.StatusBadRequest)
		return
	}

	if addErr := m.relayManager.AddLink(url); addErr != nil {
		m.t.Logf("DEBUG: Error adding link: %v", addErr)
		http.Error(w, addErr.Error(), http.StatusBadRequest)
		return
	}

	m.t.Logf("DEBUG: Link successfully added, making redirect")
	m.redirectToManagement(w, "Relay URL added successfully")
}

// handleRemoveLink обрабатывает запрос на удаление ссылки.
func (m *mockHTTPServer) handleRemoveLink(w http.ResponseWriter, r *http.Request) {
	if !m.parseForm(w, r) {
		return
	}

	indexStr := r.FormValue("index")
	m.t.Logf("DEBUG: Index from form: '%s'", indexStr)

	index, parseIntErr := strtoInt(indexStr)
	if parseIntErr != nil {
		m.t.Logf("DEBUG: Error converting index: %v", parseIntErr)
		http.Error(w, "Invalid index", http.StatusBadRequest)
		return
	}

	if removeErr := m.relayManager.RemoveLink(index); removeErr != nil {
		m.t.Logf("DEBUG: Error removing link: %v", removeErr)
		http.Error(w, removeErr.Error(), http.StatusBadRequest)
		return
	}

	m.t.Logf("DEBUG: Link successfully removed, making redirect")
	m.redirectToManagement(w, "Relay URL removed successfully")
}

// handleToggleRelay обрабатывает запрос на переключение relay.
func (m *mockHTTPServer) handleToggleRelay(w http.ResponseWriter, r *http.Request) {
	if !m.parseForm(w, r) {
		return
	}

	activeStr := r.FormValue("active")
	m.t.Logf("DEBUG: Activity from form: '%s'", activeStr)

	active, parseBoolErr := strToBool(activeStr)
	if parseBoolErr != nil {
		m.t.Logf("DEBUG: Error converting activity: %v", parseBoolErr)
		http.Error(w, "Invalid active value", http.StatusBadRequest)
		return
	}

	m.relayManager.SetActive(active)
	m.t.Logf("DEBUG: Activity successfully changed to %v, making redirect", active)

	status := "enabled"
	if !active {
		status = "disabled"
	}

	m.redirectToManagement(w, "Relay "+status)
}

// parseForm разбирает форму запроса.
func (m *mockHTTPServer) parseForm(w http.ResponseWriter, r *http.Request) bool {
	if parseErr := r.ParseForm(); parseErr != nil {
		m.t.Logf("DEBUG: Form parsing error: %v", parseErr)
		http.Error(w, "Cannot parse form", http.StatusBadRequest)
		return false
	}
	return true
}

// redirectToManagement выполняет перенаправление на страницу управления.
func (m *mockHTTPServer) redirectToManagement(w http.ResponseWriter, message string) {
	w.Header().Set("Location", "/relay-management?success="+message)
	w.WriteHeader(http.StatusSeeOther)
}

// Helper functions.
func strtoInt(s string) (int, error) {
	var result int
	_, err := fmt.Sscanf(s, "%d", &result)
	return result, err
}

func strToBool(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean value: %s", s)
	}
}

// setupRelayTest создает и настраивает окружение для тестирования relay.
func setupRelayTest(t *testing.T) (*relay.Manager, *mockHTTPServer, *http.Cookie) {
	// Create a temporary file for testing.
	tempDir := t.TempDir()
	configFile := filepath.Join(tempDir, "relay_list.json")

	// Create a new relay manager.
	relayManager := relay.NewRelayManager(configFile, slog.Default())

	// Add test URLs.
	testURL1 := "http://example.com/stream1"
	testURL2 := "https://example.org/stream2"

	if err := relayManager.AddLink(testURL1); err != nil {
		t.Fatalf("Failed to add test URL1: %v", err)
	}
	if err := relayManager.AddLink(testURL2); err != nil {
		t.Fatalf("Failed to add test URL2: %v", err)
	}

	// Activate relay.
	relayManager.SetActive(true)

	// Create a test password.
	testPassword := "test-password"

	// Create mock HTTP server.
	mockServer := newMockHTTPServer(relayManager, testPassword, t)

	// Create a test cookie for authentication.
	authCookie := &http.Cookie{
		Name:  "status_auth",
		Value: testPassword,
	}

	return relayManager, mockServer, authCookie
}

// createRequest создает HTTP запрос с необходимыми параметрами для тестов.
func createRequest(t *testing.T, method, path string, formData url.Values, authCookie *http.Cookie) *http.Request {
	var body *strings.Reader
	if formData != nil {
		body = strings.NewReader(formData.Encode())
	}

	req, err := http.NewRequest(method, path, body)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	if formData != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	req.AddCookie(authCookie)
	return req
}

// executeRequest выполняет HTTP запрос и возвращает ответ.
func executeRequest(t *testing.T, server *mockHTTPServer, req *http.Request) *http.Response {
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)

	resp := recorder.Result()
	t.Cleanup(func() {
		resp.Body.Close()
	})

	return resp
}

// TestRelayHTTPHandlers проверяет HTTP обработчики для relay функциональности.
func TestRelayHTTPHandlers(t *testing.T) {
	relayManager, mockServer, authCookie := setupRelayTest(t)

	// Тестируем страницу управления relay.
	t.Run("TestRelayManagementPage", func(t *testing.T) {
		testRelayManagementPage(t, mockServer, authCookie)
	})

	// Тестируем добавление relay ссылки.
	t.Run("TestAddRelayLink", func(t *testing.T) {
		testAddRelayLink(t, mockServer, relayManager, authCookie)
	})

	// Тестируем удаление relay ссылки.
	t.Run("TestRemoveRelayLink", func(t *testing.T) {
		testRemoveRelayLink(t, mockServer, relayManager, authCookie)
	})

	// Тестируем переключение relay.
	t.Run("TestToggleRelay", func(t *testing.T) {
		testToggleRelay(t, mockServer, relayManager, authCookie)
	})
}

// testRelayManagementPage тестирует страницу управления relay.
func testRelayManagementPage(t *testing.T, server *mockHTTPServer, authCookie *http.Cookie) {
	req := createRequest(t, http.MethodGet, "/relay-management", nil, authCookie)
	resp := executeRequest(t, server, req)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status OK, got %v", resp.StatusCode)
	}

	// Check content type.
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/html") {
		t.Errorf("Expected HTML content type, got %s", contentType)
	}
}

// testAddRelayLink тестирует добавление relay ссылки.
func testAddRelayLink(t *testing.T, server *mockHTTPServer, relayManager *relay.Manager, authCookie *http.Cookie) {
	formData := url.Values{}
	formData.Set("url", "https://example.net/stream3")

	req := createRequest(t, http.MethodPost, "/relay/add", formData, authCookie)
	resp := executeRequest(t, server, req)

	// Expect a redirect to relay management page.
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("Expected redirect status, got %v", resp.StatusCode)
	}

	// Verify link was added.
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
}

// testRemoveRelayLink тестирует удаление relay ссылки.
func testRemoveRelayLink(t *testing.T, server *mockHTTPServer, relayManager *relay.Manager, authCookie *http.Cookie) {
	// Index 0 corresponds to the first test URL.
	formData := url.Values{}
	formData.Set("index", "0")

	req := createRequest(t, http.MethodPost, "/relay/remove", formData, authCookie)
	resp := executeRequest(t, server, req)

	// Expect a redirect to relay management page.
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("Expected redirect status, got %v", resp.StatusCode)
	}

	// Verify link was removed.
	links := relayManager.GetLinks()
	for _, link := range links {
		if link == "http://example.com/stream1" {
			t.Errorf("Link was not removed from relay manager")
		}
	}
}

// testToggleRelay тестирует переключение relay.
func testToggleRelay(t *testing.T, server *mockHTTPServer, relayManager *relay.Manager, authCookie *http.Cookie) {
	// Тестируем выключение relay.
	testRelayToggleToState(t, server, relayManager, authCookie, false)

	// Тестируем включение relay.
	testRelayToggleToState(t, server, relayManager, authCookie, true)
}

// testRelayToggleToState тестирует переключение relay в указанное состояние.
func testRelayToggleToState(
	t *testing.T,
	server *mockHTTPServer,
	relayManager *relay.Manager,
	authCookie *http.Cookie,
	active bool,
) {
	activeStr := "false"
	if active {
		activeStr = "true"
	}

	formData := url.Values{}
	formData.Set("active", activeStr)

	req := createRequest(t, http.MethodPost, "/relay/toggle", formData, authCookie)
	resp := executeRequest(t, server, req)

	// Expect a redirect to relay management page.
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("Expected redirect status, got %v", resp.StatusCode)
	}

	// Verify relay state was changed properly.
	if relayManager.IsActive() != active {
		t.Errorf("Relay should be %v after setting active=%s", active, activeStr)
	}
}
