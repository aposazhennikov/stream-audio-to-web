package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/aposazhennikov/stream-audio-to-web/audio"
	httpServer "github.com/aposazhennikov/stream-audio-to-web/http"
	"github.com/aposazhennikov/stream-audio-to-web/playlist"
	"github.com/aposazhennikov/stream-audio-to-web/radio"

	"github.com/gorilla/mux"
)

// configureRootRedirection sets up redirection from root route.
func configureRootRedirection(logger *slog.Logger, config *Config, server *httpServer.Server) string {
	// Redirect from root route.
	redirectPath := defaultRoute // redirect to /status by default
	if _, exists := config.DirectoryRoutes["/humor"]; !exists {
		// If /humor doesn't exist, take the first route from configuration.
		for route := range config.DirectoryRoutes {
			redirectPath = route
			break
		}
	}

	// Replace temporary handler for root route with redirection.
	configureRootHandler(logger, server, redirectPath)

	logger.Info("Redirect configured", slog.String("from", "/"), slog.String("to", redirectPath))
	return redirectPath
}

// configureRootHandler configures the handler for the root route.
func configureRootHandler(logger *slog.Logger, server *httpServer.Server, redirectTo string) {
	var routeErr error
	handler := server.Handler()
	router, ok := handler.(*mux.Router)
	if !ok {
		routeErr = errors.New("failed to get mux.Router handler")
		logger.Error("Failed to get router handler", slog.String("error", routeErr.Error()))
		return
	}

	// Получаем обработчик с проверкой ошибки.
	routeHandler := router.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		logger.Info("Redirecting from / to %s (method: %s)", redirectTo, r.Method)
		// For HEAD requests return only headers without redirect.
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, redirectTo, http.StatusSeeOther)
	}).Methods("GET", "HEAD")

	// Проверяем ошибку добавления обработчика.
	if routeHandlerErr := routeHandler.GetError(); routeHandlerErr != nil {
		routeErr = routeHandlerErr
		logger.Error("Failed to register root handler", "error", routeErr)
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureError(routeErr, "main", "route_config")
		}
	}

	if routeErr != nil {
		logger.Error("Error setting up route handler", "error", routeErr)
	}
}

// configureAudioRoutes configures audio routes from the configuration.
func configureAudioRoutes(
	logger *slog.Logger,
	server *httpServer.Server,
	stationManager *radio.StationManager,
	config *Config,
) {
	logger.Info("Starting audio route configuration...")
	logger.Debug("Directory routes found", "count", len(config.DirectoryRoutes))

	// Log all routes for debugging
	for route, dir := range config.DirectoryRoutes {
		logger.Debug("Found route", "route", route, "directory", dir)
	}

	if len(config.DirectoryRoutes) == 0 {
		criticalErr := fmt.Errorf("CRITICAL: No directory routes configured")
		logger.Error("CRITICAL: No directory routes configured! Check DIRECTORY_ROUTES environment variable.")
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureError(criticalErr, "main", "critical")
		}
		return
	}

	// CRITICAL CHECK: Verify normalization configuration only if normalization is enabled
	if config.NormalizeVolume && config.NormalizeRuntime != "off" && config.NormalizeSampleWindows <= 0 {
		criticalErr := fmt.Errorf("CRITICAL: Invalid normalization sample windows: %d (normalization is enabled)", config.NormalizeSampleWindows)
		logger.Error("CRITICAL: Invalid normalization sample windows configuration",
			"windows", config.NormalizeSampleWindows,
			"normalizeVolume", config.NormalizeVolume,
			"normalizeRuntime", config.NormalizeRuntime,
			"expected", "> 0")
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureError(criticalErr, "main", "critical")
		}
		// This is a critical configuration error - don't start audio streams
		return
	}

	logger.Debug("Audio route configuration checks passed",
		"normalizeVolume", config.NormalizeVolume,
		"normalizeRuntime", config.NormalizeRuntime,
		"normalizeWindows", config.NormalizeSampleWindows)

	// Configure routes from configuration ASYNCHRONOUSLY.
	for route, dir := range config.DirectoryRoutes {
		// Route should already be normalized with leading slash in loadConfig.
		// But check just in case.
		if !strings.HasPrefix(route, "/") {
			route = "/" + route
		}

		// Copy variables for goroutine.
		routeCopy := route
		dirCopy := dir

		logger.Debug("Starting goroutine for route", "route", routeCopy, "directory", dirCopy)

		// Start configuring EACH stream in a separate goroutine.
		go func(r, d string) {
			logger.Debug("Asynchronous configuration of route started", slog.String("route", r), slog.String("directory", d))
			if success := configureSyncRoute(logger, server, stationManager, r, d, config); success {
				logger.Debug("Route successfully configured", slog.String("route", r))
			} else {
				criticalErr := fmt.Errorf("CRITICAL: Route configuration failed for %s", r)
				logger.Error("CRITICAL: Route configuration failed", slog.String("route", r))
				if globalSentryHelper != nil {
					globalSentryHelper.CaptureError(criticalErr, "main", "critical")
				}
			}
		}(routeCopy, dirCopy)
	}
}

// addHealthCheckHandler adds a health check handler.
func addHealthCheckHandler(logger *slog.Logger, server *httpServer.Server) {
	router, ok := server.Handler().(*mux.Router)
	if !ok {
		logger.Error("Failed to get mux.Router handler")
		return
	}

	err := router.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, writeErr := w.Write([]byte("OK")); writeErr != nil {
			logger.Error("Failed to write health check response", slog.String("error", writeErr.Error()))
			return
		}
	}).GetError()

	if err != nil {
		logger.Error("Failed to register healthz handler", slog.String("error", err.Error()))
	}
}

// configureSyncRoute configures one audio stream route synchronously.
func configureSyncRoute(
	logger *slog.Logger,
	server *httpServer.Server,
	stationManager *radio.StationManager,
	route, dir string,
	config *Config,
) bool {
	// Добавляем recovery от panic внутри goroutine
	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("PANIC in configureSyncRoute for route %s: %v", route, r)
			logger.Error("PANIC in route configuration", "route", route, "panic", r)
			if globalSentryHelper != nil {
				globalSentryHelper.CaptureError(err, "main", "operation")
			}
		}
	}()

	logger.Info("Starting synchronous configuration of route", slog.String("route", route), slog.String("directory", dir))

	// Step 1: Check directory exists (consolidated DEBUG logging).
	if !ensureDirectoryExists(logger, dir, route) {
		err := fmt.Errorf("directory check failed for route %s: %s", route, dir)
		logger.Error("Directory check failed", "route", route, "directory", dir)
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureError(err, "main", "operation")
		}
		return false
	}

	// Step 2: Check audio files.
	if !checkAudioFiles(logger, dir, route) {
		err := fmt.Errorf("audio files check failed for route %s: %s", route, dir)
		logger.Error("Audio files check failed", "route", route, "directory", dir)
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureError(err, "main", "operation")
		}
		return false
	}

	// Step 3: Create playlist.
	pl := createPlaylistOrNil(logger, dir, route, config)
	if pl == nil {
		err := fmt.Errorf("playlist creation failed for route %s: %s", route, dir)
		logger.Error("Playlist creation failed", "route", route, "directory", dir)
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureError(err, "main", "operation")
		}
		return false
	}

	// Step 4: Create streamer.
	streamer := createStreamer(logger, config, route)
	if streamer == nil {
		err := fmt.Errorf("streamer creation failed for route %s", route)
		logger.Error("Streamer creation failed", "route", route)
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureError(err, "main", "operation")
		}
		return false
	}

	// Step 5: Add station to manager.
	stationManager.AddStation(route, streamer, pl)

	// Step 6: Register stream on HTTP server.
	server.RegisterStream(route, streamer, pl)

	if !server.IsStreamRegistered(route) {
		logger.Error("CRITICAL ERROR: Stream not registered after all operations", slog.String("route", route))
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureInfo(fmt.Sprintf("Stream %s not registered after all operations", route), "main", "stream_registration")
		}
		return false
	}

	// Step 7: Check/convert bitrate in the background; the station is already playing.
	startBackgroundBitrateCheck(logger, dir, route, config.Bitrate)

	// Check if normalization should be used.
	switch config.NormalizeRuntime {
	case "on":
		streamer.SetVolumeNormalization(true)
		logger.Info(
			"DIAGNOSTICS: Runtime normalization mode 'on' overrides default setting for route",
			slog.String("route", route),
		)
	case "off":
		streamer.SetVolumeNormalization(false)
		logger.Debug("Runtime normalization mode 'off' overrides default setting for route",
			slog.String("route", route))
	case "auto", "":
		logger.Debug("Using default normalization setting for route", slog.String("route", route))
	}

	// Context7: Consolidated route configuration summary for DEBUG.
	logger.Debug("Route configuration completed",
		"route", route,
		"directory", dir,
		"bitrate", config.Bitrate,
		"normalizeVolume", config.NormalizeVolume,
		"normalizeRuntime", config.NormalizeRuntime,
		"shuffle", config.Shuffle)
	return true
}

func ensureDirectoryExists(logger *slog.Logger, dir, route string) bool {
	if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
		logger.Debug("Creating directory for route", slog.String("route", route))
		if mkdirErr := os.MkdirAll(dir, 0750); mkdirErr != nil {
			logger.Error("ERROR: When creating directory",
				"directory", dir,
				"error", mkdirErr,
			)
			if globalSentryHelper != nil {
				globalSentryHelper.CaptureError(fmt.Errorf("error creating directory %s: %w", dir, mkdirErr), "main", "directory_creation")
			}
			return false
		}
	}
	return true
}

func checkAudioFiles(logger *slog.Logger, dir, route string) bool {
	logger.Debug("Checking audio files in directory",
		slog.String("directory", dir), slog.String("route", route))

	files, readErr := os.ReadDir(dir)
	if readErr != nil {
		logger.Error("ERROR: When reading directory",
			"directory", dir,
			"error", readErr,
		)
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureError(fmt.Errorf("error reading directory %s: %w", dir, readErr), "main", "directory_read")
		}
		return false
	}

	// Выводим список всех файлов для диагностики
	logger.Debug("List of files in directory", slog.String("directory", dir))
	for i, file := range files {
		logger.Debug("File found",
			slog.Int("index", i),
			slog.String("name", file.Name()),
			slog.Bool("isDir", file.IsDir()),
			slog.String("directory", dir))
	}

	audioFiles := 0
	for _, file := range files {
		fileName := file.Name()
		lowerFileName := strings.ToLower(fileName)

		// Проверяем расширения файлов, учитывая возможные варианты регистра (.MP3, .Mp3 и т.д.)
		isMP3 := strings.HasSuffix(lowerFileName, ".mp3")
		isOGG := strings.HasSuffix(lowerFileName, ".ogg")
		isAAC := strings.HasSuffix(lowerFileName, ".aac")
		isWAV := strings.HasSuffix(lowerFileName, ".wav")   // Добавим поддержку WAV
		isFLAC := strings.HasSuffix(lowerFileName, ".flac") // Добавим поддержку FLAC

		// Проверка на наличие точки в начале имени файла (скрытый файл в Unix)
		isHidden := strings.HasPrefix(fileName, ".")

		// Детальное логирование проверки файла
		logger.Debug("Checking file",
			slog.String("fileName", fileName),
			slog.String("lowerFileName", lowerFileName),
			slog.Bool("isDir", file.IsDir()),
			slog.Bool("isHidden", isHidden),
			slog.Bool("isMP3", isMP3),
			slog.Bool("isOGG", isOGG),
			slog.Bool("isAAC", isAAC),
			slog.Bool("isWAV", isWAV),
			slog.Bool("isFLAC", isFLAC))

		// Считаем аудиофайлы (не директории, не скрытые файлы, с поддержкой аудиоформатов)
		if !file.IsDir() && !isHidden && (isMP3 || isOGG || isAAC || isWAV || isFLAC) {
			audioFiles++
			// Context7: Individual file counting is too verbose - using summary log instead.
		}
	}

	logger.Debug("Directory contains", slog.Int("audio_files", audioFiles), slog.String("directory", dir))

	// Вместо ошибки только выводим предупреждение, если нет аудиофайлов, но разрешаем продолжить работу
	if audioFiles == 0 {
		logger.Warn("WARNING: No audio files in directory, stream will be empty until files are added",
			slog.String("directory", dir),
			slog.String("route", route))

		// Только записываем сообщение в Sentry, но не считаем это ошибкой
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureInfo(fmt.Sprintf("No audio files in directory %s, but stream will be configured anyway", dir), "main", "empty_directory")
		}

		// Возвращаем true, чтобы разрешить конфигурацию маршрута даже без файлов
		return true
	}
	return true
}

func createPlaylistOrNil(logger *slog.Logger, dir, route string, config *Config) httpServer.PlaylistManager {
	shuffleSetting := config.Shuffle
	if specificShuffle, exists := config.PerStreamShuffle[route]; exists {
		shuffleSetting = specificShuffle
		logger.Debug("Using specific shuffle setting for route", slog.String("route", route))
	} else {
		logger.Debug("Using global shuffle setting for route", slog.String("route", route))
	}
	pl, playlistErr := playlist.NewPlaylist(dir, nil, shuffleSetting, logger, config.SentryHelper)
	if playlistErr != nil {
		logger.Error("ERROR creating playlist", "error", playlistErr)
		config.SentryHelper.CaptureError(fmt.Errorf("error creating playlist: %w", playlistErr), "main", "playlist_creation")
		return nil
	}
	logger.Debug("Playlist for route successfully created", slog.String("route", route))
	return pl
}

func createStreamer(logger *slog.Logger, config *Config, route string) *audio.Streamer {
	logger.Debug("Creating audio streamer for route", slog.String("route", route))
	streamer := audio.NewStreamer(config.BufferSize, config.MaxClients, config.Bitrate, logger, config.SentryHelper)
	streamer.SetVolumeNormalization(config.NormalizeVolume)
	logger.Debug("Audio streamer for route successfully created", slog.String("route", route))
	return streamer
}
