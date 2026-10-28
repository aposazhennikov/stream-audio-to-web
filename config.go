package main

import (
	"encoding/json"
	"flag"
	"log/slog"
	"os"
	"strconv"
	"strings"

	sentryhelper "github.com/aposazhennikov/stream-audio-to-web/sentry_helper"
)

// Default configuration.
const (
	defaultPort     = 8000
	defaultAudioDir = "./audio"

	defaultBitrate         = 128
	defaultMaxClients      = 500
	defaultBufferSize      = 65536               // 64KB
	defaultShuffle         = false               // Shuffle tracks is disabled by default
	defaultNormalizeVolume = true                // Volume normalization is enabled by default
	defaultRelayEnabled    = false               // Relay functionality disabled by default
	defaultRelayConfigFile = "./relay_list.json" // Default path for relay configuration
	readTimeoutSec         = 15
	idleTimeoutSec         = 60
	shutdownTimeoutSec     = 10
	defaultRoute           = "/status"
	maxSplitParts          = 2      // Maximum number of parts when splitting configuration strings
	strTrue                = "true" // Строковое значение "true" для проверки
)

// Config describes the application configuration parameters.
type Config struct {
	Port                   int
	AudioDir               string
	DirectoryRoutes        map[string]string
	Bitrate                int
	MaxClients             int
	BufferSize             int
	Shuffle                bool                       // Global shuffle tracks setting
	PerStreamShuffle       map[string]bool            // Per-stream shuffle configuration
	NormalizeVolume        bool                       // Global volume normalization setting
	NormalizeRuntime       string                     // Runtime normalization mode: "auto", "on", "off"
	NormalizeSampleWindows int                        // Number of analysis windows for normalization
	NormalizeSampleMs      int                        // Duration of each analysis window in milliseconds
	EnableRelay            bool                       // Enable relay functionality
	RelayConfigFile        string                     // Path to relay configuration file
	EnableTelegramAlerts   bool                       // Enable telegram alerts functionality
	TelegramAlertsConfig   string                     // Path to telegram alerts configuration file
	TelegramAlertTimezone  string                     // Timezone for telegram alert timestamps
	SentryDSN              string                     // DSN для Sentry
	SentryHelper           *sentryhelper.SentryHelper // Helper для безопасной работы с Sentry
}

// Load configuration from command line flags and environment variables.
func loadConfig() *Config {
	// Parse command line flags.
	config := parseCommandLineFlags()

	// Load configuration from environment variables.
	loadConfigFromEnv(config)

	return config
}

// parseCommandLineFlags парсит флаги командной строки и возвращает начальную конфигурацию.
func parseCommandLineFlags() *Config {
	port := flag.Int("port", defaultPort, "HTTP server port")
	audioDir := flag.String("audio-dir", defaultAudioDir, "Directory with audio files")
	directoryRoutes := flag.String("directory-routes", "", "JSON mapping of route prefixes to directories")
	bitrate := flag.Int("bitrate", defaultBitrate, "Stream bitrate")
	maxClients := flag.Int("max-clients", defaultMaxClients, "Maximum number of clients")
	bufferSize := flag.Int("buffer-size", defaultBufferSize, "Buffer size for audio streaming")
	shuffle := flag.Bool("shuffle", defaultShuffle, "Enable shuffle mode for all streams")
	normalizeVolume := flag.Bool("normalize", defaultNormalizeVolume, "Enable volume normalization")
	normalizeRuntime := flag.String("normalize-runtime", "auto", "Runtime normalization mode (auto, on, off)")
	normalizeSampleWindows := flag.Int("normalize-windows", 10, "Number of analysis windows for normalization")
	normalizeSampleMs := flag.Int("normalize-ms", 1000, "Duration of each analysis window in milliseconds")
	relayEnabled := flag.Bool("relay", defaultRelayEnabled, "Enable relay functionality")
	relayConfigFile := flag.String("relay-config", defaultRelayConfigFile, "Path to relay configuration file")
	sentryDSN := flag.String("sentry-dsn", "", "DSN for Sentry error tracking")

	flag.Parse()

	// Parse directory routes from command line flag.
	directoryRoutesMap := make(map[string]string)
	if *directoryRoutes != "" {
		if err := json.Unmarshal([]byte(*directoryRoutes), &directoryRoutesMap); err != nil {
			slog.Default().Error("Invalid directory-routes JSON", slog.String("error", err.Error()))
		}
	}

	// Create configuration.
	return &Config{
		Port:                   *port,
		AudioDir:               *audioDir,
		DirectoryRoutes:        directoryRoutesMap,
		Bitrate:                *bitrate,
		MaxClients:             *maxClients,
		BufferSize:             *bufferSize,
		Shuffle:                *shuffle,
		PerStreamShuffle:       make(map[string]bool),
		NormalizeVolume:        *normalizeVolume,
		NormalizeRuntime:       *normalizeRuntime,
		NormalizeSampleWindows: *normalizeSampleWindows,
		NormalizeSampleMs:      *normalizeSampleMs,
		EnableRelay:            *relayEnabled,
		RelayConfigFile:        *relayConfigFile,
		SentryDSN:              *sentryDSN,
	}
}

// loadConfigFromEnv загружает конфигурацию из переменных окружения.
func loadConfigFromEnv(config *Config) {
	// Разделим функцию на более простые части
	loadGenericConfig(config)
	loadNormalizationConfig(config)
	loadRelayConfig(config)
	loadTelegramConfig(config)
	loadStreamConfig(config)

	// Load directory routes from environment.
	loadDirectoryRoutesFromEnv(config, slog.Default())

	// Load per-stream shuffle settings from environment.
	loadShuffleSettingsFromEnv(config)
}

// loadGenericConfig загружает общие параметры конфигурации из переменных окружения.
func loadGenericConfig(config *Config) {
	// Override config with environment variables if provided
	if portStr := os.Getenv("PORT"); portStr != "" {
		if port, err := strconv.Atoi(portStr); err == nil {
			config.Port = port
		}
	}

	if bufferSizeStr := os.Getenv("BUFFER_SIZE"); bufferSizeStr != "" {
		if bufferSize, err := strconv.Atoi(bufferSizeStr); err == nil {
			config.BufferSize = bufferSize
		}
	}

	if shuffleStr := os.Getenv("SHUFFLE"); shuffleStr != "" {
		config.Shuffle = strings.ToLower(shuffleStr) == strTrue
	}

	if sentryDSN := os.Getenv("SENTRY_DSN"); sentryDSN != "" {
		config.SentryDSN = sentryDSN
	}
}

// loadNormalizationConfig загружает параметры нормализации из переменных окружения.
func loadNormalizationConfig(config *Config) {
	if normalizeVolumeStr := os.Getenv("NORMALIZE_VOLUME"); normalizeVolumeStr != "" {
		config.NormalizeVolume = strings.ToLower(normalizeVolumeStr) == strTrue
	}

	if normalizeRuntime := os.Getenv("NORMALIZE_RUNTIME"); normalizeRuntime != "" {
		config.NormalizeRuntime = normalizeRuntime
	}

	if normalizeSampleWindowsStr := os.Getenv("NORMALIZE_SAMPLE_WINDOWS"); normalizeSampleWindowsStr != "" {
		if normalizeSampleWindows, err := strconv.Atoi(normalizeSampleWindowsStr); err == nil {
			config.NormalizeSampleWindows = normalizeSampleWindows
		}
	}

	if normalizeSampleMsStr := os.Getenv("NORMALIZE_SAMPLE_MS"); normalizeSampleMsStr != "" {
		if normalizeSampleMs, err := strconv.Atoi(normalizeSampleMsStr); err == nil {
			config.NormalizeSampleMs = normalizeSampleMs
		}
	}
}

// loadRelayConfig загружает параметры реле из переменных окружения.
func loadRelayConfig(config *Config) {
	// Проверяем переменную RELAY для включения/выключения relay функциональности
	if relayEnabledStr := os.Getenv("RELAY"); relayEnabledStr != "" {
		config.EnableRelay = strings.ToLower(relayEnabledStr) == strTrue
	}

	// Для обратной совместимости также проверяем старое имя переменной RELAY_ENABLED
	if relayEnabledStr := os.Getenv("RELAY_ENABLED"); relayEnabledStr != "" && os.Getenv("RELAY") == "" {
		config.EnableRelay = strings.ToLower(relayEnabledStr) == strTrue
	}

	if relayConfigFile := os.Getenv("RELAY_CONFIG_FILE"); relayConfigFile != "" {
		config.RelayConfigFile = relayConfigFile
	}
}

// loadTelegramConfig загружает параметры телеграм-алертов из переменных окружения.
func loadTelegramConfig(config *Config) {
	// Проверяем переменную TG_ALERT для включения/выключения telegram alerts функциональности
	if telegramAlertsEnabledStr := os.Getenv("TG_ALERT"); telegramAlertsEnabledStr != "" {
		config.EnableTelegramAlerts = strings.ToLower(telegramAlertsEnabledStr) == strTrue
	}

	// Для обратной совместимости также проверяем старое имя переменной TG_ALERTS
	if telegramAlertsEnabledStr := os.Getenv("TG_ALERTS"); telegramAlertsEnabledStr != "" && os.Getenv("TG_ALERT") == "" {
		config.EnableTelegramAlerts = strings.ToLower(telegramAlertsEnabledStr) == strTrue
	}

	// Путь к файлу конфигурации телеграм-алертов
	if telegramAlertsConfigFile := os.Getenv("TG_ALERT_CONFIG_FILE"); telegramAlertsConfigFile != "" {
		config.TelegramAlertsConfig = telegramAlertsConfigFile
	} else {
		// Значение по умолчанию
		config.TelegramAlertsConfig = "./telegram_alerts.json"
	}

	// Timezone для отметок времени в telegram алертах
	if telegramTimezone := os.Getenv("TG_ALERT_TIMEZONE"); telegramTimezone != "" {
		config.TelegramAlertTimezone = telegramTimezone
	} else {
		// Значение по умолчанию
		config.TelegramAlertTimezone = "Europe/Moscow"
	}
}

// loadStreamConfig загружает параметры потока из переменных окружения.
func loadStreamConfig(config *Config) {
	if bitrateStr := os.Getenv("BITRATE"); bitrateStr != "" {
		if bitrate, err := strconv.Atoi(bitrateStr); err == nil {
			config.Bitrate = bitrate
		}
	}

	if maxClientsStr := os.Getenv("MAX_CLIENTS"); maxClientsStr != "" {
		if maxClients, err := strconv.Atoi(maxClientsStr); err == nil {
			config.MaxClients = maxClients
		}
	}
}

// loadDirectoryRoutesFromEnv загружает маршруты директорий из переменных окружения.
func loadDirectoryRoutesFromEnv(config *Config, logger *slog.Logger) {
	dirRoutes := getEnvOrDefault("DIRECTORY_ROUTES", "")
	if dirRoutes == "" {
		// No directory routes specified.
		return
	}

	// Попробуем сначала обработать формат JSON
	var jsonRoutes map[string]string
	if err := json.Unmarshal([]byte(dirRoutes), &jsonRoutes); err == nil {
		logger.Info("Обнаружен JSON формат в DIRECTORY_ROUTES")

		// Перебираем маршруты из JSON
		for route, path := range jsonRoutes {
			// Нормализуем маршрут (добавляем слэш в начало, если его нет)
			if !strings.HasPrefix(route, "/") {
				logger.Debug("Normalized route", slog.String("from", route), slog.String("to", "/"+route))
				route = "/" + route
			}

			// Проверяем существование директории
			if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
				logger.Warn("Directory does not exist", slog.String("path", path), slog.String("route", route))
				continue
			}

			// Ключ - маршрут, значение - путь к директории
			config.DirectoryRoutes[route] = path
			logger.Debug("Added directory route from JSON", slog.String("route", route), slog.String("path", path))
		}

		logger.Debug("Directory routes configured from JSON", slog.Int("count", len(config.DirectoryRoutes)))
		return
	}

	// Если JSON не сработал, пробуем старый формат
	logger.Info("Пробуем старый формат DIRECTORY_ROUTES с разделителями")

	// Process each directory route.
	routes := strings.Split(dirRoutes, ";")
	for _, route := range routes {
		parts := strings.Split(route, ":")
		if len(parts) != maxSplitParts {
			logger.Warn("Invalid directory route format", slog.String("route", route))
			continue
		}

		url := parts[0]  // Маршрут
		path := parts[1] // Путь к директории

		if !strings.HasPrefix(url, "/") {
			url = "/" + url
		}

		// Check if directory exists.
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			logger.Warn("Directory does not exist", slog.String("path", path), slog.String("route", url))
			continue
		}

		// Add to configuration.
		config.DirectoryRoutes[url] = path

		logger.Debug("Added directory route", slog.String("url", url), slog.String("path", path))
	}

	logger.Debug("Directory routes configured", slog.Int("count", len(config.DirectoryRoutes)))
}

// loadShuffleSettingsFromEnv загружает настройки перемешивания из переменных окружения.
func loadShuffleSettingsFromEnv(config *Config) {
	shuffleSettings := getEnvOrDefault("SHUFFLE_SETTINGS", "")
	parseShuffleSettings(config, shuffleSettings)
}

func parseShuffleSettings(config *Config, shuffleSettings string) {
	if shuffleSettings == "" {
		return
	}
	settings := strings.Split(shuffleSettings, ",")
	for _, setting := range settings {
		parts := strings.SplitN(setting, ":", maxSplitParts)
		if len(parts) == maxSplitParts {
			routePath := strings.TrimSpace(parts[0])
			shuffleValue := strings.TrimSpace(parts[1])
			// Ensure route starts with slash.
			if routePath[0] != '/' {
				routePath = "/" + routePath
			}
			// Parse shuffle value.
			switch shuffleValue {
			case strTrue:
				config.PerStreamShuffle[routePath] = true
			case "false":
				config.PerStreamShuffle[routePath] = false
			}
		}
	}
}

// getEnvOrDefault returns environment variable value or default value.
func getEnvOrDefault(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}
