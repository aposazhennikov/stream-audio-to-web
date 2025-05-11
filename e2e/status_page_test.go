package e2e_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestStatusPageContent(t *testing.T) {
	// Get base URL from environment variable or use default value
	baseURL := getEnvOrDefault("TEST_SERVER_URL", "http://localhost:8000")
	password := getEnvOrDefault("STATUS_PASSWORD", "1234554321")

	// Create HTTP client with cookie support
	jar, createCookieJarErr := cookiejar.New(nil)
	if createCookieJarErr != nil {
		t.Fatalf("Failed to create cookie jar: %v", createCookieJarErr)
	}

	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			return nil // allow redirects
		},
		Timeout: 10 * time.Second,
	}

	// Authenticate for access to status page
	form := url.Values{}
	form.Add("password", password)

	_, authStatusPageErr := client.PostForm(fmt.Sprintf("%s/status", baseURL), form)
	if authStatusPageErr != nil {
		t.Fatalf("Failed to authenticate: %v", authStatusPageErr)
	}

	// Get status page
	statusResp, getStatusPageErr := client.Get(fmt.Sprintf("%s/status-page", baseURL))
	if getStatusPageErr != nil {
		t.Fatalf("Failed to get status page: %v", getStatusPageErr)
	}
	defer statusResp.Body.Close()

	// Check response code
	if statusResp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d for /status-page, got %d", http.StatusOK, statusResp.StatusCode)
	}

	// Get HTML page content
	body, readStatusPageBodyErr := io.ReadAll(statusResp.Body)
	if readStatusPageBodyErr != nil {
		t.Fatalf("Failed to read status page body: %v", readStatusPageBodyErr)
	}

	htmlContent := string(body)

	// Check for main page elements
	requiredElements := []string{
		"<h1>Audio Streams Status</h1>",       // Page title
		"class=\"status-info listeners\"",     // Listeners information
		"class=\"status-info current-track\"", // Current track information
		"class=\"status-info start-time\"",    // Start time information
		"class=\"button-group\"",              // Control buttons
		"class=\"prev-track\"",                // Previous track button
		"class=\"next-track\"",                // Next track button
		"class=\"show-history\"",              // History button
	}

	// Check for each element on the page
	for _, element := range requiredElements {
		if !strings.Contains(htmlContent, element) {
			t.Errorf("Status page doesn't contain required element: %s", element)
		}
	}

	// Check time format (expected format like "02.01.2006 15:04:05")
	timePattern := "Started: "
	if !strings.Contains(htmlContent, timePattern) {
		t.Errorf("Status page doesn't contain start time information with pattern '%s'", timePattern)
	}

	// Check listeners count display
	listenersPattern := "Listeners count: <span class=\"listeners-count"
	if !strings.Contains(htmlContent, listenersPattern) {
		t.Errorf("Status page doesn't contain listeners count information with pattern '%s'", listenersPattern)
	}

	t.Logf("Status page contains all required elements")
}

func TestAuthenticationFailure(t *testing.T) {
	// Get base URL from environment variable or use default value
	baseURL := getEnvOrDefault("TEST_SERVER_URL", "http://localhost:8000")

	// 1. Check access to status-page without authentication
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // don't follow redirects
		},
		Timeout: 5 * time.Second,
	}

	statusResp, getStatusPageNoAuthErr := client.Get(fmt.Sprintf("%s/status-page", baseURL))
	if getStatusPageNoAuthErr != nil {
		t.Fatalf("Failed to get status page without auth: %v", getStatusPageNoAuthErr)
	}
	defer statusResp.Body.Close()

	// Expect redirect to login page
	if statusResp.StatusCode != http.StatusFound {
		t.Errorf("Expected status code %d for unauthenticated /status-page access, got %d",
			http.StatusFound, statusResp.StatusCode)
	}

	// Check redirect URL
	location := statusResp.Header.Get("Location")
	if location != "/status" {
		t.Errorf("Expected redirect to /status, got %s", location)
	}

	t.Logf("Unauthenticated access to /status-page correctly redirects to /status")

	// 2. Check authentication with wrong password
	jar, createCookieJarErr := cookiejar.New(nil)
	if createCookieJarErr != nil {
		t.Fatalf("Failed to create cookie jar: %v", createCookieJarErr)
	}
	clientWithCookies := &http.Client{
		Jar: jar,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			return nil // follow redirects
		},
		Timeout: 5 * time.Second,
	}

	// Submit form with wrong password
	form := url.Values{}
	form.Add("password", "wrong_password")

	loginResp, submitLoginFormErr := clientWithCookies.PostForm(fmt.Sprintf("%s/status", baseURL), form)
	if submitLoginFormErr != nil {
		t.Fatalf("Failed to submit login form: %v", submitLoginFormErr)
	}
	defer loginResp.Body.Close()

	// Check that wrong password doesn't pass authentication
	// (returns to login page or contains error message)
	body, readLoginRespBodyErr := io.ReadAll(loginResp.Body)
	if readLoginRespBodyErr != nil {
		t.Fatalf("Failed to read login response body: %v", readLoginRespBodyErr)
	}

	htmlContent := string(body)

	// Check for error message or that we stayed on login page
	isLoginPage := strings.Contains(htmlContent, "<form") &&
		strings.Contains(htmlContent, "method=\"post\"") &&
		strings.Contains(htmlContent, "action=\"/status\"")

	if !isLoginPage {
		t.Errorf("Expected to stay on login page after wrong password, but got different page")
	}

	// Check that after wrong password there's no access to protected page
	statusPageResp, getStatusPageAfterWrongPwdErr := clientWithCookies.Get(fmt.Sprintf("%s/status-page", baseURL))
	if getStatusPageAfterWrongPwdErr != nil {
		t.Fatalf("Failed to get status page after wrong password: %v", getStatusPageAfterWrongPwdErr)
	}
	defer statusPageResp.Body.Close()

	// If access is denied, should be redirected to login page
	if statusPageResp.Request.URL.Path != "/status" && statusPageResp.StatusCode != http.StatusFound {
		t.Errorf("Expected access to be denied after wrong password authentication")
	}

	t.Logf("Authentication correctly denies access with wrong password")
}
