package e2e

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestStatusPageContent(t *testing.T) {
	// Получаем базовый URL из переменной окружения или используем значение по умолчанию
	baseURL := getEnvOrDefault("TEST_SERVER_URL", "http://localhost:8000")
	password := getEnvOrDefault("STATUS_PASSWORD", "1234554321")
	
	// Создаем HTTP-клиент с поддержкой cookie
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("Failed to create cookie jar: %v", err)
	}
	
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return nil // разрешаем перенаправления
		},
		Timeout: 10 * time.Second,
	}
	
	// Аутентифицируемся для доступа к странице статуса
	form := url.Values{}
	form.Add("password", password)
	
	_, err = client.PostForm(fmt.Sprintf("%s/status", baseURL), form)
	if err != nil {
		t.Fatalf("Failed to authenticate: %v", err)
	}
	
	// Получаем страницу статуса
	statusResp, err := client.Get(fmt.Sprintf("%s/status-page", baseURL))
	if err != nil {
		t.Fatalf("Failed to get status page: %v", err)
	}
	defer statusResp.Body.Close()
	
	// Проверяем код ответа
	if statusResp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d for /status-page, got %d", http.StatusOK, statusResp.StatusCode)
	}
	
	// Получаем HTML-код страницы
	body, err := ioutil.ReadAll(statusResp.Body)
	if err != nil {
		t.Fatalf("Failed to read status page body: %v", err)
	}
	
	htmlContent := string(body)
	
	// Проверяем наличие основных элементов страницы
	requiredElements := []string{
		"<h1>Audio Streams Status</h1>", // Заголовок страницы
		"class=\"status-info listeners\"", // Информация о слушателях
		"class=\"status-info current-track\"", // Информация о текущем треке
		"class=\"status-info start-time\"", // Информация о времени начала
		"class=\"button-group\"", // Кнопки управления
		"class=\"prev-track\"", // Кнопка предыдущего трека
		"class=\"next-track\"", // Кнопка следующего трека
		"class=\"show-history\"", // Кнопка истории
	}
	
	// Проверяем наличие каждого элемента на странице
	for _, element := range requiredElements {
		if !strings.Contains(htmlContent, element) {
			t.Errorf("Status page doesn't contain required element: %s", element)
		}
	}
	
	// Проверяем формат времени (ожидается формат типа "02.01.2006 15:04:05")
	timePattern := "Started: "
	if !strings.Contains(htmlContent, timePattern) {
		t.Errorf("Status page doesn't contain start time information with pattern '%s'", timePattern)
	}
	
	// Проверяем отображение количества слушателей
	listenersPattern := "Listeners count: <span class=\"listeners-count"
	if !strings.Contains(htmlContent, listenersPattern) {
		t.Errorf("Status page doesn't contain listeners count information with pattern '%s'", listenersPattern)
	}
	
	t.Logf("Status page contains all required elements")
}

func TestAuthenticationFailure(t *testing.T) {
	// Получаем базовый URL из переменной окружения или используем значение по умолчанию
	baseURL := getEnvOrDefault("TEST_SERVER_URL", "http://localhost:8000")
	
	// 1. Проверяем доступ к status-page без аутентификации
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // не следуем за перенаправлениями
		},
		Timeout: 5 * time.Second,
	}
	
	statusResp, err := client.Get(fmt.Sprintf("%s/status-page", baseURL))
	if err != nil {
		t.Fatalf("Failed to get status page without auth: %v", err)
	}
	defer statusResp.Body.Close()
	
	// Ожидаем перенаправление на страницу входа
	if statusResp.StatusCode != http.StatusFound {
		t.Errorf("Expected status code %d for unauthenticated /status-page access, got %d", 
			http.StatusFound, statusResp.StatusCode)
	}
	
	// Проверяем URL перенаправления
	location := statusResp.Header.Get("Location")
	if location != "/status" {
		t.Errorf("Expected redirect to /status, got %s", location)
	}
	
	t.Logf("Unauthenticated access to /status-page correctly redirects to /status")
	
	// 2. Проверяем аутентификацию с неверным паролем
	jar, _ := cookiejar.New(nil)
	clientWithCookies := &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return nil // следуем за перенаправлениями
		},
		Timeout: 5 * time.Second,
	}
	
	// Отправляем форму с неверным паролем
	form := url.Values{}
	form.Add("password", "wrong_password")
	
	loginResp, err := clientWithCookies.PostForm(fmt.Sprintf("%s/status", baseURL), form)
	if err != nil {
		t.Fatalf("Failed to submit login form: %v", err)
	}
	defer loginResp.Body.Close()
	
	// Проверяем, что неверный пароль не проходит аутентификацию
	// (возвращается обратно на страницу входа или содержит сообщение об ошибке)
	body, err := ioutil.ReadAll(loginResp.Body)
	if err != nil {
		t.Fatalf("Failed to read login response body: %v", err)
	}
	
	htmlContent := string(body)
	
	// Проверяем наличие сообщения об ошибке или то, что мы остались на странице входа
	isLoginPage := strings.Contains(htmlContent, "<form") && 
		strings.Contains(htmlContent, "method=\"post\"") && 
		strings.Contains(htmlContent, "action=\"/status\"")
	
	if !isLoginPage {
		t.Errorf("Expected to stay on login page after wrong password, but got different page")
	}
	
	// Проверяем, что после неверного пароля нет доступа к защищенной странице
	statusPageResp, err := clientWithCookies.Get(fmt.Sprintf("%s/status-page", baseURL))
	if err != nil {
		t.Fatalf("Failed to get status page after wrong password: %v", err)
	}
	defer statusPageResp.Body.Close()
	
	// Если доступ запрещен, должно быть перенаправление на страницу входа
	if statusPageResp.Request.URL.Path != "/status" && statusPageResp.StatusCode != http.StatusFound {
		t.Errorf("Expected access to be denied after wrong password authentication")
	}
	
	t.Logf("Authentication correctly denies access with wrong password")
} 