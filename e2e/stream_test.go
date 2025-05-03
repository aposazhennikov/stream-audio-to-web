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

// getEnvOrDefault returns the value of environment variable or the default value
func getEnvOrDefault(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

func TestHealthzEndpoint(t *testing.T) {
	// Get base URL from environment variable or use default value
	baseURL := getEnvOrDefault("TEST_SERVER_URL", "http://localhost:8000")
	
	// Send GET request to /healthz
	resp, err := http.Get(fmt.Sprintf("%s/healthz", baseURL))
	if err != nil {
		t.Fatalf("Failed to send request to /healthz: %v", err)
	}
	defer resp.Body.Close()
	
	// Check response code
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d, got %d", http.StatusOK, resp.StatusCode)
	}
	
	// Check response body
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}
	
	if string(body) != "OK" {
		t.Errorf("Expected response body 'OK', got '%s'", string(body))
	}
}

func TestReadyzEndpoint(t *testing.T) {
	// Get base URL from environment variable or use default value
	baseURL := getEnvOrDefault("TEST_SERVER_URL", "http://localhost:8000")
	
	// Send GET request to /readyz
	resp, err := http.Get(fmt.Sprintf("%s/readyz", baseURL))
	if err != nil {
		t.Fatalf("Failed to send request to /readyz: %v", err)
	}
	defer resp.Body.Close()
	
	// Check response code
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d, got %d", http.StatusOK, resp.StatusCode)
	}
	
	// Check that response body contains "Ready"
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}
	
	if !strings.Contains(string(body), "Ready") {
		t.Errorf("Response body does not contain 'Ready': '%s'", string(body))
	}
}

func TestStreamsEndpoint(t *testing.T) {
	// Get base URL from environment variable or use default value
	baseURL := getEnvOrDefault("TEST_SERVER_URL", "http://localhost:8000")
	
	// Send GET request to /streams
	resp, err := http.Get(fmt.Sprintf("%s/streams", baseURL))
	if err != nil {
		t.Fatalf("Failed to send request to /streams: %v", err)
	}
	defer resp.Body.Close()
	
	// Check response code
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d, got %d", http.StatusOK, resp.StatusCode)
	}
	
	// Check that response body is not empty and contains "streams"
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
	// Get base URL from environment variable or use default value
	baseURL := getEnvOrDefault("TEST_SERVER_URL", "http://localhost:8000")
	
	// Get list of routes from /streams
	resp, err := http.Get(fmt.Sprintf("%s/streams", baseURL))
	if err != nil {
		t.Fatalf("Failed to send request to /streams: %v", err)
	}
	
	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}
	
	// Simple check - if routes couldn't be obtained from /streams,
	// use default route
	testRoute := "/humor"
	if strings.Contains(string(body), "/science") {
		testRoute = "/science"
	}
	
	// Send HEAD request to audio stream
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
	
	// Check response code
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status code %d for %s, got %d", http.StatusOK, testRoute, resp.StatusCode)
	}
	
	// Check Content-Type header
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "audio/") {
		t.Errorf("Expected Content-Type to contain 'audio/', got '%s'", contentType)
	}
} 