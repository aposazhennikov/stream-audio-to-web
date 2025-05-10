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

// mockHTTPServer создает мок HTTP-сервера для тестов релея
type mockHTTPServer struct {
	relayManager *relay.RelayManager
	password     string
	t            *testing.T
}

func newMockHTTPServer(relayManager *relay.RelayManager, password string, t *testing.T) *mockHTTPServer {
	return &mockHTTPServer{
		relayManager: relayManager,
		password:     password,
		t:            t,
	}
}

func (m *mockHTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Проверка аутентификации
	authCookie, err := r.Cookie("status_auth")
	if err != nil || authCookie.Value != m.password {
		m.t.Logf("DEBUG: Аутентификация не прошла: %v", err)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	m.t.Logf("DEBUG: Обработка запроса: %s %s", r.Method, r.URL.Path)

	// Обработка запросов
	switch {
	case r.URL.Path == "/relay-management" && r.Method == http.MethodGet:
		// Отправляем успешный ответ для страницы управления
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("<html><body>Relay Management Page</body></html>"))
		return

	case r.URL.Path == "/relay/add" && r.Method == http.MethodPost:
		// Добавление новой ссылки
		if err := r.ParseForm(); err != nil {
			m.t.Logf("DEBUG: Ошибка парсинга формы: %v", err)
			http.Error(w, "Cannot parse form", http.StatusBadRequest)
			return
		}

		url := r.FormValue("url")
		m.t.Logf("DEBUG: URL из формы: '%s'", url)

		if url == "" {
			m.t.Logf("DEBUG: URL пустой")
			http.Error(w, "URL cannot be empty", http.StatusBadRequest)
			return
		}

		if err := m.relayManager.AddLink(url); err != nil {
			m.t.Logf("DEBUG: Ошибка добавления ссылки: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		m.t.Logf("DEBUG: Ссылка успешно добавлена, делаю редирект")
		w.Header().Set("Location", "/relay-management?success=Relay URL added successfully")
		w.WriteHeader(http.StatusSeeOther)
		return

	case r.URL.Path == "/relay/remove" && r.Method == http.MethodPost:
		// Удаление ссылки
		if err := r.ParseForm(); err != nil {
			m.t.Logf("DEBUG: Ошибка парсинга формы: %v", err)
			http.Error(w, "Cannot parse form", http.StatusBadRequest)
			return
		}

		indexStr := r.FormValue("index")
		m.t.Logf("DEBUG: Индекс из формы: '%s'", indexStr)

		index, err := strtoInt(indexStr)
		if err != nil {
			m.t.Logf("DEBUG: Ошибка преобразования индекса: %v", err)
			http.Error(w, "Invalid index", http.StatusBadRequest)
			return
		}

		if err := m.relayManager.RemoveLink(index); err != nil {
			m.t.Logf("DEBUG: Ошибка удаления ссылки: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		m.t.Logf("DEBUG: Ссылка успешно удалена, делаю редирект")
		w.Header().Set("Location", "/relay-management?success=Relay URL removed successfully")
		w.WriteHeader(http.StatusSeeOther)
		return

	case r.URL.Path == "/relay/toggle" && r.Method == http.MethodPost:
		// Переключение активности релея
		if err := r.ParseForm(); err != nil {
			m.t.Logf("DEBUG: Ошибка парсинга формы: %v", err)
			http.Error(w, "Cannot parse form", http.StatusBadRequest)
			return
		}

		activeStr := r.FormValue("active")
		m.t.Logf("DEBUG: Активность из формы: '%s'", activeStr)

		active, err := strToBool(activeStr)
		if err != nil {
			m.t.Logf("DEBUG: Ошибка преобразования активности: %v", err)
			http.Error(w, "Invalid active value", http.StatusBadRequest)
			return
		}

		m.relayManager.SetActive(active)
		m.t.Logf("DEBUG: Активность успешно изменена на %v, делаю редирект", active)

		status := "enabled"
		if !active {
			status = "disabled"
		}

		w.Header().Set("Location", "/relay-management?success=Relay "+status)
		w.WriteHeader(http.StatusSeeOther)
		return

	default:
		m.t.Logf("DEBUG: Неизвестный путь: %s", r.URL.Path)
		http.Error(w, "Not found", http.StatusNotFound)
	}
}

// Вспомогательные функции
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

func TestRelayHTTPHandlers(t *testing.T) {
	// Create a temporary file for testing
	tempDir := t.TempDir()

	configFile := filepath.Join(tempDir, "relay_list.json")

	// Create a new relay manager
	relayManager := relay.NewRelayManager(configFile, slog.Default())

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

	// Create a test password
	testPassword := "test-password"

	// Create mock HTTP server
	mockServer := newMockHTTPServer(relayManager, testPassword, t)

	// Create a test cookie for authentication
	authCookie := &http.Cookie{
		Name:  "status_auth",
		Value: testPassword,
	}

	// Test relay management page
	t.Run("TestRelayManagementPage", func(t *testing.T) {
		req, errNewReq := http.NewRequest(http.MethodGet, "/relay-management", nil)
		if errNewReq != nil {
			t.Fatalf("Failed to create request: %v", errNewReq)
		}
		req.AddCookie(authCookie)

		// Используем ResponseRecorder вместо реального HTTP-сервера
		recorder := httptest.NewRecorder()
		mockServer.ServeHTTP(recorder, req)

		resp := recorder.Result()
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

		req, errNewReq := http.NewRequest(http.MethodPost, "/relay/add",
			strings.NewReader(formData.Encode()))
		if errNewReq != nil {
			t.Fatalf("Failed to create request: %v", errNewReq)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(authCookie)

		// Используем ResponseRecorder вместо реального HTTP-сервера
		recorder := httptest.NewRecorder()
		mockServer.ServeHTTP(recorder, req)

		resp := recorder.Result()
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

		req, errNewReq := http.NewRequest(http.MethodPost, "/relay/remove",
			strings.NewReader(formData.Encode()))
		if errNewReq != nil {
			t.Fatalf("Failed to create request: %v", errNewReq)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(authCookie)

		// Используем ResponseRecorder вместо реального HTTP-сервера
		recorder := httptest.NewRecorder()
		mockServer.ServeHTTP(recorder, req)

		resp := recorder.Result()
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

		req, errNewReq := http.NewRequest(http.MethodPost, "/relay/toggle",
			strings.NewReader(formData.Encode()))
		if errNewReq != nil {
			t.Fatalf("Failed to create request: %v", errNewReq)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(authCookie)

		// Используем ResponseRecorder вместо реального HTTP-сервера
		recorder := httptest.NewRecorder()
		mockServer.ServeHTTP(recorder, req)

		resp := recorder.Result()
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

		req, errNewReq = http.NewRequest(http.MethodPost, "/relay/toggle",
			strings.NewReader(formData.Encode()))
		if errNewReq != nil {
			t.Fatalf("Failed to create request: %v", errNewReq)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(authCookie)

		// Используем ResponseRecorder вместо реального HTTP-сервера
		recorder = httptest.NewRecorder()
		mockServer.ServeHTTP(recorder, req)

		resp = recorder.Result()
		defer resp.Body.Close()

		// Verify relay was turned on
		if !relayManager.IsActive() {
			t.Errorf("Relay should be active after setting active=true")
		}
	})
}
