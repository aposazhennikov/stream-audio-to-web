package e2e

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// getEnvOrDefault возвращает значение переменной окружения или значение по умолчанию
func getEnvOrDefault(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

func TestHealthzEndpoint(t *testing.T) {
	// Получаем базовый URL из переменной окружения или используем значение по умолчанию
	baseURL := getEnvOrDefault("TEST_SERVER_URL", "http://localhost:8000")
	
	// Отправляем GET-запрос к /healthz
	resp, err := http.Get(fmt.Sprintf("%s/healthz", baseURL))
	if err != nil {
		t.Fatalf("Failed to send request to /healthz: %v", err)
	}
	defer resp.Body.Close()
	
	// Проверяем код ответа
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d, got %d", http.StatusOK, resp.StatusCode)
	}
	
	// Проверяем тело ответа
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}
	
	if string(body) != "OK" {
		t.Errorf("Expected response body 'OK', got '%s'", string(body))
	}
}

func TestReadyzEndpoint(t *testing.T) {
	// Получаем базовый URL из переменной окружения или используем значение по умолчанию
	baseURL := getEnvOrDefault("TEST_SERVER_URL", "http://localhost:8000")
	
	// Отправляем GET-запрос к /readyz
	resp, err := http.Get(fmt.Sprintf("%s/readyz", baseURL))
	if err != nil {
		t.Fatalf("Failed to send request to /readyz: %v", err)
	}
	defer resp.Body.Close()
	
	// Проверяем код ответа
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d, got %d", http.StatusOK, resp.StatusCode)
	}
	
	// Проверяем, что тело ответа содержит "Ready"
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}
	
	if !strings.Contains(string(body), "Ready") {
		t.Errorf("Response body does not contain 'Ready': '%s'", string(body))
	}
}

func TestStreamsEndpoint(t *testing.T) {
	// Получаем базовый URL из переменной окружения или используем значение по умолчанию
	baseURL := getEnvOrDefault("TEST_SERVER_URL", "http://localhost:8000")
	
	// Отправляем GET-запрос к /streams
	resp, err := http.Get(fmt.Sprintf("%s/streams", baseURL))
	if err != nil {
		t.Fatalf("Failed to send request to /streams: %v", err)
	}
	defer resp.Body.Close()
	
	// Проверяем код ответа
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d, got %d", http.StatusOK, resp.StatusCode)
	}
	
	// Проверяем, что тело ответа не пустое и содержит "streams"
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}
	
	if len(body) == 0 {
		t.Errorf("Response body is empty")
	}
	
	if !strings.Contains(string(body), "streams") {
		t.Errorf("Response body does not contain 'streams': '%s'", string(body))
	}
}

func TestAudioStreamEndpoint(t *testing.T) {
	// Получаем базовый URL из переменной окружения или используем значение по умолчанию
	baseURL := getEnvOrDefault("TEST_SERVER_URL", "http://localhost:8000")
	
	// Получаем список маршрутов из /streams
	resp, err := http.Get(fmt.Sprintf("%s/streams", baseURL))
	if err != nil {
		t.Fatalf("Failed to send request to /streams: %v", err)
	}
	
	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}
	
	// Простая проверка - если не удалось получить маршруты из /streams, 
	// используем маршрут по умолчанию
	testRoute := "/humor"
	if strings.Contains(string(body), "/science") {
		testRoute = "/science"
	}
	
	// Отправляем HEAD-запрос к аудио-стриму
	req, err := http.NewRequest("HEAD", fmt.Sprintf("%s%s", baseURL, testRoute), nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	
	client := &http.Client{
		Timeout: 5 * time.Second,
	}
	
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("Failed to send HEAD request to %s: %v", testRoute, err)
	}
	defer resp.Body.Close()
	
	// Проверяем код ответа
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d for %s, got %d", http.StatusOK, testRoute, resp.StatusCode)
	}
	
	// Проверяем заголовок Content-Type
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "audio/") {
		t.Errorf("Expected Content-Type to contain 'audio/', got '%s'", contentType)
	}
} 