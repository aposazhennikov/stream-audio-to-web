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

func TestTrackSwitchingEndpoints(t *testing.T) {
	// Получаем базовый URL из переменной окружения или используем значение по умолчанию
	baseURL := getEnvOrDefault("TEST_SERVER_URL", "http://localhost:8000")
	password := getEnvOrDefault("STATUS_PASSWORD", "1234554321")
	
	// Получаем информацию о доступных стримах
	streamsResp, err := http.Get(fmt.Sprintf("%s/streams", baseURL))
	if err != nil {
		t.Fatalf("Failed to get streams info: %v", err)
	}
	defer streamsResp.Body.Close()
	
	// Декодируем JSON ответ
	var streamsData map[string]interface{}
	if err := json.NewDecoder(streamsResp.Body).Decode(&streamsData); err != nil {
		t.Fatalf("Failed to decode streams response: %v", err)
		return
	}
	
	// Находим первый доступный маршрут
	streams, ok := streamsData["streams"].([]interface{})
	if !ok || len(streams) == 0 {
		t.Skip("No streams available for testing")
		return
	}
	
	// Берем первый доступный стрим для теста
	var routeName string
	if stream, ok := streams[0].(map[string]interface{}); ok {
		if route, ok := stream["route"].(string); ok {
			routeName = route
			// Удаляем ведущий слеш, если есть
			if routeName[0] == '/' {
				routeName = routeName[1:]
			}
		}
	}
	
	if routeName == "" {
		t.Skip("Could not determine stream route for testing")
		return
	}
	
	t.Logf("Using stream route: %s for track switching tests", routeName)
	
	// Создаем клиент с поддержкой cookies для аутентификации
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		Timeout: 10 * time.Second,
	}
	
	// Аутентифицируемся
	form := url.Values{}
	form.Add("password", password)
	
	_, err = client.PostForm(fmt.Sprintf("%s/status", baseURL), form)
	if err != nil {
		t.Fatalf("Failed to authenticate: %v", err)
	}
	
	// Тест 1: Проверка API переключения треков вперед
	testNextTrackAPI(t, client, baseURL, routeName)
	
	// Тест 2: Проверка API переключения треков назад
	testPrevTrackAPI(t, client, baseURL, routeName)
}

func testNextTrackAPI(t *testing.T, client *http.Client, baseURL, routeName string) {
	// Получаем информацию о текущем треке
	initialTrackInfo := getCurrentTrackInfo(t, client, baseURL, routeName)
	initialTrackName := initialTrackInfo["track"].(string)
	
	t.Logf("Initial track: %s", initialTrackName)
	
	// Вызываем API для переключения на следующий трек (используем AJAX=1 для получения JSON)
	nextTrackURL := fmt.Sprintf("%s/next-track/%s?ajax=1", baseURL, routeName)
	resp, err := client.Post(nextTrackURL, "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("Failed to call next track API: %v", err)
	}
	defer resp.Body.Close()
	
	// Проверяем код ответа
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d for next-track API, got %d", http.StatusOK, resp.StatusCode)
		body, _ := ioutil.ReadAll(resp.Body)
		t.Logf("Response body: %s", string(body))
		return
	}
	
	// Проверяем, что тип контента JSON
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "application/json") {
		t.Errorf("Expected Content-Type to be application/json, got %s", contentType)
	}
	
	// Декодируем JSON ответ
	var responseData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&responseData); err != nil {
		t.Errorf("Failed to decode JSON response: %v", err)
		return
	}
	
	// Проверяем, что в ответе есть данные об успешном переключении
	success, ok := responseData["success"].(bool)
	if !ok || !success {
		t.Errorf("Next track API did not report success")
	}
	
	// Даем время серверу на переключение
	time.Sleep(500 * time.Millisecond)
	
	// Получаем информацию о новом текущем треке
	newTrackInfo := getCurrentTrackInfo(t, client, baseURL, routeName)
	newTrackName := newTrackInfo["track"].(string)
	
	t.Logf("New track after next: %s", newTrackName)
	
	// Проверяем, что трек действительно изменился
	if newTrackName == initialTrackName {
		t.Errorf("Track did not change after calling next-track API")
	}
}

func testPrevTrackAPI(t *testing.T, client *http.Client, baseURL, routeName string) {
	// Получаем информацию о текущем треке
	initialTrackInfo := getCurrentTrackInfo(t, client, baseURL, routeName)
	initialTrackName := initialTrackInfo["track"].(string)
	
	t.Logf("Initial track before prev: %s", initialTrackName)
	
	// Вызываем API для переключения на предыдущий трек
	prevTrackURL := fmt.Sprintf("%s/prev-track/%s?ajax=1", baseURL, routeName)
	resp, err := client.Post(prevTrackURL, "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("Failed to call previous track API: %v", err)
	}
	defer resp.Body.Close()
	
	// Проверяем код ответа
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d for prev-track API, got %d", http.StatusOK, resp.StatusCode)
		return
	}
	
	// Даем время серверу на переключение
	time.Sleep(500 * time.Millisecond)
	
	// Получаем информацию о новом текущем треке
	newTrackInfo := getCurrentTrackInfo(t, client, baseURL, routeName)
	newTrackName := newTrackInfo["track"].(string)
	
	t.Logf("New track after prev: %s", newTrackName)
	
	// Проверяем, что трек действительно изменился
	if newTrackName == initialTrackName {
		t.Errorf("Track did not change after calling prev-track API")
	}
}

// Вспомогательная функция для получения информации о текущем треке
func getCurrentTrackInfo(t *testing.T, client *http.Client, baseURL, routeName string) map[string]interface{} {
	trackInfoURL := fmt.Sprintf("%s/now-playing?route=/%s", baseURL, routeName)
	resp, err := client.Get(trackInfoURL)
	if err != nil {
		t.Fatalf("Failed to get current track info: %v", err)
	}
	defer resp.Body.Close()
	
	var trackInfo map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&trackInfo); err != nil {
		t.Fatalf("Failed to decode track info response: %v", err)
	}
	
	return trackInfo
} 