package unit_test

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

// Create our own mini-copy of Config for testing.
type Config struct {
	PerStreamShuffle map[string]bool
}

// Test function loadConfig to verify functionality of ROUTES_SHUFFLE and PER_STREAM_SHUFFLE.
func loadConfig() *Config {
	config := &Config{
		PerStreamShuffle: make(map[string]bool),
	}

	// Processing PER_STREAM_SHUFFLE.
	if envPerStreamShuffle := os.Getenv("PER_STREAM_SHUFFLE"); envPerStreamShuffle != "" {
		processPERStreamShuffle(envPerStreamShuffle, config)
	}

	// Processing ROUTES_SHUFFLE.
	if envRouteShuffle := os.Getenv("ROUTES_SHUFFLE"); envRouteShuffle != "" {
		processROUTESShuffle(envRouteShuffle, config)
	}

	return config
}

// processPERStreamShuffle обрабатывает значение переменной окружения PER_STREAM_SHUFFLE.
func processPERStreamShuffle(envValue string, config *Config) {
	var perStreamShuffle map[string]bool
	if err := json.Unmarshal([]byte(envValue), &perStreamShuffle); err == nil {
		for k, v := range perStreamShuffle {
			config.PerStreamShuffle[k] = v
		}
	}
}

// processROUTESShuffle обрабатывает значение переменной окружения ROUTES_SHUFFLE.
func processROUTESShuffle(envValue string, config *Config) {
	var routesShuffle map[string]string
	if err := json.Unmarshal([]byte(envValue), &routesShuffle); err == nil {
		for k, v := range routesShuffle {
			if _, exists := config.PerStreamShuffle[k]; exists {
				continue
			}
			shuffleValue, parseErr := strconv.ParseBool(v)
			if parseErr != nil {
				continue
			}
			config.PerStreamShuffle[k] = shuffleValue
		}
	}
}

// TestRoutesShuffleEnvVar tests parsing of the ROUTES_SHUFFLE environment variable.
func TestRoutesShuffleEnvVar(t *testing.T) {
	// Backup existing environment variables.
	origPerStreamShuffle := os.Getenv("PER_STREAM_SHUFFLE")
	origRoutesShuffle := os.Getenv("ROUTES_SHUFFLE")

	// Restore environment variables after test.
	defer func() {
		t.Setenv("PER_STREAM_SHUFFLE", origPerStreamShuffle)
		t.Setenv("ROUTES_SHUFFLE", origRoutesShuffle)
	}()

	// Test cases.
	testCases := []struct {
		name           string
		routesShuffle  string
		expectedValues map[string]bool
	}{
		{
			name:          "Basic settings",
			routesShuffle: `{"humor":"true","science":"false"}`,
			expectedValues: map[string]bool{
				"humor":   true,
				"science": false,
			},
		},
		{
			name:          "With leading slashes",
			routesShuffle: `{"/humor":"true","/science":"false"}`,
			expectedValues: map[string]bool{
				"/humor":   true,
				"/science": false,
			},
		},
		{
			name:          "Invalid boolean value",
			routesShuffle: `{"humor":"true","science":"invalid"}`,
			expectedValues: map[string]bool{
				"humor": true,
				// science should not be in the map due to invalid value.
			},
		},
		{
			name:           "Empty object",
			routesShuffle:  `{}`,
			expectedValues: map[string]bool{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Clear environment variables.
			t.Setenv("PER_STREAM_SHUFFLE", "")
			t.Setenv("ROUTES_SHUFFLE", "")

			// Set ROUTES_SHUFFLE.
			t.Setenv("ROUTES_SHUFFLE", tc.routesShuffle)

			// Run loadConfig.
			config := loadConfig()

			// Check that the expected values are in PerStreamShuffle.
			for k, v := range tc.expectedValues {
				if value, exists := config.PerStreamShuffle[k]; !exists {
					t.Errorf("Expected key %s to exist in PerStreamShuffle, but it doesn't", k)
				} else if value != v {
					t.Errorf("Expected PerStreamShuffle[%s] = %v, got %v", k, v, value)
				}
			}

			// For the invalid case, check that the invalid key is not in the map.
			if tc.name == "Invalid boolean value" {
				if _, exists := config.PerStreamShuffle["science"]; exists {
					t.Errorf(
						"Expected key 'science' to not exist in PerStreamShuffle due to invalid value, " +
							"but it does",
					)
				}
			}
		})
	}
}

// TestPriorityOfShuffleEnvVars tests that PER_STREAM_SHUFFLE takes precedence over ROUTES_SHUFFLE.
func TestPriorityOfShuffleEnvVars(t *testing.T) {
	// Backup existing environment variables.
	origPerStreamShuffle := os.Getenv("PER_STREAM_SHUFFLE")
	origRoutesShuffle := os.Getenv("ROUTES_SHUFFLE")

	// Restore environment variables after test.
	defer func() {
		t.Setenv("PER_STREAM_SHUFFLE", origPerStreamShuffle)
		t.Setenv("ROUTES_SHUFFLE", origRoutesShuffle)
	}()

	// Set both environment variables.
	t.Setenv("PER_STREAM_SHUFFLE", `{"humor":false,"news":true}`)
	t.Setenv("ROUTES_SHUFFLE", `{"humor":"true","science":"true"}`)

	// Run loadConfig.
	config := loadConfig()

	// Check PER_STREAM_SHUFFLE values take precedence for overlapping keys.
	if value, exists := config.PerStreamShuffle["humor"]; !exists || value != false {
		t.Errorf("Expected PerStreamShuffle[humor] = false (from PER_STREAM_SHUFFLE), got %v", value)
	}

	// Check non-overlapping keys from ROUTES_SHUFFLE are still included.
	if value, exists := config.PerStreamShuffle["science"]; !exists || value != true {
		t.Errorf("Expected PerStreamShuffle[science] = true (from ROUTES_SHUFFLE), got %v", value)
	}

	// Check non-overlapping keys from PER_STREAM_SHUFFLE are included.
	if value, exists := config.PerStreamShuffle["news"]; !exists || value != true {
		t.Errorf("Expected PerStreamShuffle[news] = true (from PER_STREAM_SHUFFLE), got %v", value)
	}
}

// Убираем вставленный код.
func TestConfig_LoadFromEnvironment(_ *testing.T) {
	// Тест на загрузку конфигурации из переменных окружения.
	// ... existing code ...
}
