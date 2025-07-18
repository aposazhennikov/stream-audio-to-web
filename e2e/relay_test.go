package e2e_test

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	httpServer "github.com/aposazhennikov/stream-audio-to-web/http"
	"github.com/aposazhennikov/stream-audio-to-web/relay"
)

// Mock HTTP server that serves fake audio content.
type mockAudioServer struct {
	srv *httptest.Server
}

func newMockAudioServer() *mockAudioServer {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		// Simulate audio stream - send "mock audio data" in chunks.
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
	// Подготовка тестового окружения.
	mockServer, testServer, relayManager, authCookie := setupRelayEndToEndTest(t)
	defer cleanupTestEnvironment(mockServer, testServer)

	// Тестирование стриминга релея.
	t.Run("TestRelayStream", func(t *testing.T) {
		testRelayStream(t, testServer)
	})

	// Тестирование операций управления релеем.
	t.Run("TestRelayManagement", func(t *testing.T) {
		testRelayManagement(t, testServer, relayManager, authCookie)
	})
}

// setupRelayEndToEndTest подготавливает тестовое окружение для конечного тестирования релея.
func setupRelayEndToEndTest(t *testing.T) (*mockAudioServer, *httptest.Server, *relay.Manager, *http.Cookie) {
	// Создаем мок аудио сервера, который будет релеиться.
	mockServer := newMockAudioServer()

	// Создаем временный файл для тестирования.
	tempDir := t.TempDir()
	configFile := filepath.Join(tempDir, "relay_list.json")

	// Создаем директорию шаблонов и partials.
	templatesDir := filepath.Join(tempDir, "templates")
	partialsDir := filepath.Join(templatesDir, "partials")
	if mkdirErr := os.MkdirAll(partialsDir, 0755); mkdirErr != nil {
		t.Fatalf("Failed to create templates/partials directory: %v", mkdirErr)
	}

	// Копируем шаблон 404.html в директорию шаблонов.
	template404Path := filepath.Join("../templates", "404.html")
	template404Content, readTemplate404Err := os.ReadFile(template404Path)
	if readTemplate404Err != nil {
		t.Logf("Could not read template file: %v", readTemplate404Err)
		// Создаем пустой файл шаблона.
		template404Content = []byte("<html><body>404 Not Found</body></html>")
	}

	outputTemplate404Path := filepath.Join(templatesDir, "404.html")
	if writeTemplate404Err := os.WriteFile(outputTemplate404Path, template404Content, 0644); writeTemplate404Err != nil {
		t.Fatalf("Failed to write template file: %v", writeTemplate404Err)
	}

	// Копируем шаблон partials/head.html.
	templateHeadPath := filepath.Join("../templates/partials", "head.html")
	templateHeadContent, readTemplateHeadErr := os.ReadFile(templateHeadPath)
	if readTemplateHeadErr != nil {
		t.Logf("Could not read head template file: %v", readTemplateHeadErr)
		// Создаем пустой файл шаблона.
		templateHeadContent = []byte("<head><title>Test</title></head>")
	}

	outputTemplateHeadPath := filepath.Join(partialsDir, "head.html")
	if writeTemplateHeadErr := os.WriteFile(outputTemplateHeadPath, templateHeadContent, 0644); writeTemplateHeadErr != nil {
		t.Fatalf("Failed to write head template file: %v", writeTemplateHeadErr)
	}

	// Устанавливаем текущую директорию на tempDir, чтобы шаблоны были доступны.
	oldWd, _ := os.Getwd()
	if chdirErr := os.Chdir(tempDir); chdirErr != nil {
		t.Fatalf("Failed to change directory: %v", chdirErr)
	}
	// После тестов вернуться в исходную директорию.
	t.Cleanup(func() {
		os.Chdir(oldWd)
	})

	// Создаем новый менеджер релеев.
	relayManager := relay.NewRelayManager(configFile, slog.Default())

	// Добавляем тестовый URL.
	testStreamURL := mockServer.URL()
	if addTestURLErr := relayManager.AddLink(testStreamURL); addTestURLErr != nil {
		t.Fatalf("Failed to add test URL: %v", addTestURLErr)
	}

	// Активируем релей.
	relayManager.SetActive(true)

	// Создаем новый HTTP сервер.
	server := httpServer.NewServer("mp3", 10)

	// Устанавливаем менеджер релеев.
	server.SetRelayManager(relayManager)

	// Создаем тестовый сервер.
	testServer := httptest.NewServer(server.Handler())

	// Получаем пароль статуса из окружения или используем значение по умолчанию.
	testPassword := getTestPassword()
	server.SetStatusPassword(testPassword)

	authCookie := &http.Cookie{
		Name:  "status_auth",
		Value: testPassword,
	}

	return mockServer, testServer, relayManager, authCookie
}

// getTestPassword возвращает пароль для тестирования.
func getTestPassword() string {
	testPassword := os.Getenv("STATUS_PASSWORD")
	if testPassword == "" {
		testPassword = "test-password"
	}
	return testPassword
}

// cleanupTestEnvironment очищает тестовое окружение.
func cleanupTestEnvironment(mockServer *mockAudioServer, testServer *httptest.Server) {
	mockServer.Close()
	testServer.Close()
}

// testRelayStream тестирует эндпоинт потоковой передачи релея.
func testRelayStream(t *testing.T, testServer *httptest.Server) {
	// Индекс потока 0 соответствует нашему мок потоку.
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

	validateRelayStreamResponse(t, resp)

	// Чтение данных потока (с таймаутом).
	readRelayStreamData(t, resp)
}

// validateRelayStreamResponse проверяет ответ потоковой передачи релея.
func validateRelayStreamResponse(t *testing.T, resp *http.Response) {
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status OK, got %v", resp.StatusCode)
	}

	// Проверяем тип контента.
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "audio") {
		t.Errorf("Expected audio content type, got %s", contentType)
	}
}

// readRelayStreamData читает данные потока релея.
func readRelayStreamData(t *testing.T, resp *http.Response) {
	dataReceived := make(chan bool, 1)
	var buf bytes.Buffer

	go func() {
		_, readStreamDataErr := io.Copy(&buf, resp.Body)
		if readStreamDataErr != nil && !errors.Is(readStreamDataErr, io.EOF) {
			t.Errorf("Error reading stream data: %v", readStreamDataErr)
		}
		dataReceived <- true
	}()

	// Ждем данные или таймаут.
	select {
	case <-dataReceived:
		// Успех - мы получили некоторые данные.
		data := buf.String()
		if !strings.Contains(data, "mock audio data") {
			t.Errorf("Did not receive expected stream data. Got: %s", data)
		}
	case <-time.After(2 * time.Second):
		t.Error("Timeout waiting for stream data")
	}
}

// testRelayManagement тестирует операции управления релеем.
func testRelayManagement(
	t *testing.T,
	testServer *httptest.Server,
	relayManager *relay.Manager,
	authCookie *http.Cookie,
) {
	// Тестируем добавление нового URL релея.
	testAddRelayURL(t, testServer, relayManager, authCookie)

	// Тестируем удаление URL релея.
	testRemoveRelayURL(t, testServer, relayManager, authCookie)
}

// testAddRelayURL тестирует добавление URL релея.
func testAddRelayURL(t *testing.T, testServer *httptest.Server, relayManager *relay.Manager, authCookie *http.Cookie) {
	formData := url.Values{}
	formData.Set("url", "https://example.com/stream2")

	requestAddRelay, createAddRelayRequestErr := http.NewRequest(
		http.MethodPost,
		testServer.URL+"/relay/add",
		strings.NewReader(formData.Encode()),
	)
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

	// Проверяем, что теперь в списке два URL.
	links := relayManager.GetLinks()
	if len(links) != 2 {
		t.Errorf("Expected 2 links after adding, got %d", len(links))
	}
}

// testRemoveRelayURL тестирует удаление URL релея.
func testRemoveRelayURL(
	t *testing.T,
	testServer *httptest.Server,
	relayManager *relay.Manager,
	authCookie *http.Cookie,
) {
	formData := url.Values{}
	formData.Set("index", "1") // Удаляем второй URL

	requestRemoveRelay, createRemoveRelayRequestErr := http.NewRequest(
		http.MethodPost,
		testServer.URL+"/relay/remove",
		strings.NewReader(formData.Encode()),
	)
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

	// Проверяем, что снова только один URL.
	links := relayManager.GetLinks()
	if len(links) != 1 {
		t.Errorf("Expected 1 link after removing, got %d", len(links))
	}
}
