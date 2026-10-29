package http

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	sentry "github.com/getsentry/sentry-go"
	"github.com/gorilla/mux"
)

// trackCurrentTrack tracks current track for specified stream.
func (s *Server) trackCurrentTrack(route string, trackCh <-chan string) {
	for trackPath := range trackCh {
		fileName := filepath.Base(trackPath)
		s.logger.Debug(
			"Current track",
			slog.String("route", route),
			slog.String("fileName", fileName),
			slog.String("path", trackPath),
		)

		if hasUnicodeChars(fileName) {
			sentry.ConfigureScope(func(scope *sentry.Scope) {
				scope.SetContext("track_info", map[string]interface{}{
					"route":         route,
					"track_name":    fileName,
					"track_path":    trackPath,
					"track_dir":     filepath.Dir(trackPath),
					"track_ext":     filepath.Ext(trackPath),
					"track_len":     len(fileName),
					"track_unicode": true,
				})
			})
		}

		s.trackMutex.Lock()
		s.currentTracks[route] = fileName
		s.trackMutex.Unlock()
	}
}

// nowPlayingHandler returns information about current track.
func (s *Server) nowPlayingHandler(w http.ResponseWriter, r *http.Request) {
	route := r.URL.Query().Get("route")

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// Проверка на пустой маршрут.
	if route == "" {
		// Для отсутствующего параметра route возвращаем информацию по всем маршрутам
		// вместо ошибки. Убираем DEBUG лог так как он вызывается каждую секунду.

		// Подготовим карту со всеми текущими треками
		s.trackMutex.RLock()
		allTracks := make(map[string]string)
		for r, track := range s.currentTracks {
			allTracks[r] = track
		}
		s.trackMutex.RUnlock()

		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"tracks": allTracks,
		}); encodeErr != nil {
			s.logger.Error("Failed to encode all tracks info", slog.String("error", encodeErr.Error()))
		}
		return
	}

	s.trackMutex.RLock()
	defer s.trackMutex.RUnlock()

	track, exists := s.currentTracks[route]
	if !exists {
		errorMsg := fmt.Sprintf("Stream %s not found", route)
		s.logger.Error("ERROR: Stream not found", slog.String("route", route))
		s.sentryHelper.CaptureInfo(errorMsg, "http", "handler")
		http.Error(w, errorMsg, http.StatusNotFound)
		return
	}

	s.logger.Info("Track information request", slog.String("route", route), slog.String("track", track))
	s.mutex.RLock()
	playlist, playlistExists := s.playlists[route]
	s.mutex.RUnlock()
	currentTrackInfo := map[string]string{
		"route": route,
		"track": track,
	}
	if playlistExists {
		for range 3 {
			if currentTrack := playlist.GetCurrentTrack(); currentTrack != nil {
				if trackWithPath, ok := currentTrack.(interface{ GetPath() string }); ok {
					currentTrackInfo["path"] = trackWithPath.GetPath()
				}
				break
			}
			time.Sleep(shortSleepMs * time.Millisecond)
		}
	}
	if encodeErr := json.NewEncoder(w).Encode(currentTrackInfo); encodeErr != nil {
		s.logger.Error("Failed to encode track info", slog.String("error", encodeErr.Error()))
		// Обработка ошибки JSON используя логирование.
		s.logNormalizationError(encodeErr, fmt.Sprintf("json encode for route %s", route))
	}
}

// playbackTimeHandler returns playback time information for all routes or specific route
func (s *Server) playbackTimeHandler(w http.ResponseWriter, r *http.Request) {
	route := r.URL.Query().Get("route")

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	type PlaybackTimeInfo struct {
		Route           string  `json:"route"`
		TrackPath       string  `json:"track_path"`
		TrackName       string  `json:"track_name"`
		StartTime       string  `json:"start_time"`
		ElapsedMs       int64   `json:"elapsed_ms"`
		ElapsedText     string  `json:"elapsed_text"`
		TotalMs         int64   `json:"total_ms"`
		TotalText       string  `json:"total_text"`
		RemainingMs     int64   `json:"remaining_ms"`
		RemainingText   string  `json:"remaining_text"`
		ProgressPercent float64 `json:"progress_percent"`
		IsPlaying       bool    `json:"is_playing"`
	}

	if route == "" {
		// Return info for all routes
		s.mutex.RLock()
		allPlaybackInfo := make(map[string]PlaybackTimeInfo)
		for r, stream := range s.streams {
			trackPath, startTime, elapsed, totalDuration := stream.GetPlaybackInfo()
			trackName := filepath.Base(trackPath)
			if trackName == "." || trackName == "" {
				trackName = "No track"
			}

			elapsedMs := elapsed.Milliseconds()
			elapsedText := formatDuration(elapsed)
			totalMs := totalDuration.Milliseconds()
			totalText := formatDuration(totalDuration)

			remaining := totalDuration - elapsed
			if remaining < 0 {
				remaining = 0
			}
			remainingMs := remaining.Milliseconds()
			remainingText := formatDuration(remaining)

			progressPercent := float64(0)
			if totalDuration > 0 {
				progressPercent = float64(elapsed) / float64(totalDuration) * 100
				if progressPercent > 100 {
					progressPercent = 100
				}
			}

			allPlaybackInfo[r] = PlaybackTimeInfo{
				Route:           r,
				TrackPath:       trackPath,
				TrackName:       trackName,
				StartTime:       startTime.Format("15:04:05"),
				ElapsedMs:       elapsedMs,
				ElapsedText:     elapsedText,
				TotalMs:         totalMs,
				TotalText:       totalText,
				RemainingMs:     remainingMs,
				RemainingText:   remainingText,
				ProgressPercent: progressPercent,
				IsPlaying:       !startTime.IsZero(),
			}
		}
		s.mutex.RUnlock()

		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"playback_times": allPlaybackInfo,
		}); encodeErr != nil {
			s.logger.Error("Failed to encode all playback times", slog.String("error", encodeErr.Error()))
		}
		return
	}

	// Return info for specific route
	s.mutex.RLock()
	stream, exists := s.streams[route]
	s.mutex.RUnlock()

	if !exists {
		errorMsg := fmt.Sprintf("Stream %s not found", route)
		s.logger.Error("ERROR: Stream not found for playback time", slog.String("route", route))
		http.Error(w, errorMsg, http.StatusNotFound)
		return
	}

	trackPath, startTime, elapsed, totalDuration := stream.GetPlaybackInfo()
	trackName := filepath.Base(trackPath)
	if trackName == "." || trackName == "" {
		trackName = "No track"
	}

	elapsedMs := elapsed.Milliseconds()
	elapsedText := formatDuration(elapsed)
	totalMs := totalDuration.Milliseconds()
	totalText := formatDuration(totalDuration)

	remaining := totalDuration - elapsed
	if remaining < 0 {
		remaining = 0
	}
	remainingMs := remaining.Milliseconds()
	remainingText := formatDuration(remaining)

	progressPercent := float64(0)
	if totalDuration > 0 {
		progressPercent = float64(elapsed) / float64(totalDuration) * 100
		if progressPercent > 100 {
			progressPercent = 100
		}
	}

	playbackInfo := PlaybackTimeInfo{
		Route:           route,
		TrackPath:       trackPath,
		TrackName:       trackName,
		StartTime:       startTime.Format("15:04:05"),
		ElapsedMs:       elapsedMs,
		ElapsedText:     elapsedText,
		TotalMs:         totalMs,
		TotalText:       totalText,
		RemainingMs:     remainingMs,
		RemainingText:   remainingText,
		ProgressPercent: progressPercent,
		IsPlaying:       !startTime.IsZero(),
	}

	if encodeErr := json.NewEncoder(w).Encode(playbackInfo); encodeErr != nil {
		s.logger.Error("Failed to encode playback time info", slog.String("error", encodeErr.Error()))
	}
}

// formatDuration formats duration as MM:SS or HH:MM:SS
func formatDuration(d time.Duration) string {
	totalSeconds := int(d.Seconds())
	hours := totalSeconds / 3600
	minutes := (totalSeconds % 3600) / 60
	seconds := totalSeconds % 60

	if hours > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, seconds)
	}
	return fmt.Sprintf("%02d:%02d", minutes, seconds)
}

// TrackInfo returns track information for specified stream.
func (s *Server) TrackInfo(route string, track string) (map[string]interface{}, error) {
	s.logger.Info("Track information request", slog.String("route", route), slog.String("track", track))

	s.trackMutex.RLock()
	defer s.trackMutex.RUnlock()

	trackInfo := map[string]interface{}{
		"route": route,
		"track": track,
	}

	s.mutex.RLock()
	playlist, exists := s.playlists[route]
	s.mutex.RUnlock()

	if exists {
		currentTrack := playlist.GetCurrentTrack()
		if currentTrack != nil {
			if trackWithPath, ok := currentTrack.(interface{ GetPath() string }); ok {
				trackInfo["path"] = trackWithPath.GetPath()
			}
		}
	}

	return trackInfo, nil
}

// logNormalizationError обрабатывает ошибки нормализации звука.
// Использует DEBUG для обычных EOF ошибок, которые возникают при нормальной работе.
func (s *Server) logNormalizationError(err error, filePath string) {
	// Обрабатываем только критические ошибки для Sentry.
	if err != nil {
		// Проверяем, содержит ли ошибка строку "EOF", которая обычно не является критической.
		if strings.Contains(err.Error(), "EOF") {
			// Для EOF-ошибок используем уровень DEBUG, чтобы не спамить логи
			s.logger.Debug("Non-critical normalization error",
				slog.String("error", err.Error()),
				slog.String("route", filepath.Base(filepath.Dir(filePath))))
		} else {
			// Для более серьезных ошибок и логируем, и отправляем в Sentry.
			errMsg := fmt.Sprintf("ERROR during audio normalization error=%q", err.Error())
			s.logger.Error(errMsg)
			s.sentryHelper.CaptureInfo(errMsg, "http", "handler")
		}
	}
}

// trackHandler returns information about the current track for a specific route.
func (s *Server) trackHandler(w http.ResponseWriter, r *http.Request) {
	route, routeExists := mux.Vars(r)["route"]

	// Проверка на пустой маршрут.
	if !routeExists || route == "" {
		errorMsg := "Route parameter is required"
		s.logger.Error("ERROR: Empty route parameter in trackHandler")
		http.Error(w, errorMsg, http.StatusBadRequest)
		return
	}

	// Retrieve current track from the playlist.
	s.mutex.RLock()
	playlist, exists := s.playlists[route]
	s.mutex.RUnlock()

	if !exists {
		s.logger.Error("ERROR: Stream not found", slog.String("route", route))
		http.Error(w, fmt.Sprintf("Stream %s not found", route), http.StatusNotFound)
		return
	}

	// Try to get current track with retries.
	const (
		maxAttempts = 3
		retryDelay  = 50 * time.Millisecond
	)

	var currentTrack interface{}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		currentTrack = playlist.GetCurrentTrack()
		if currentTrack != nil {
			break
		}
		s.logger.Warn("WARNING: GetCurrentTrack timed out",
			slog.Int("attempt", attempt),
			slog.Int("maxAttempts", maxAttempts))
		time.Sleep(retryDelay)
	}

	if currentTrack == nil {
		s.logger.Warn("WARNING: All GetCurrentTrack attempts timed out, returning nil")
		http.Error(w, "Current track information not available", http.StatusServiceUnavailable)
		return
	}

	// Handle different types of tracks.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	switch track := currentTrack.(type) {
	case string:
		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"track": track,
		}); encodeErr != nil {
			s.logger.Error("Failed to encode track", slog.String("error", encodeErr.Error()))
		}
	case interface{ GetNormalizedPath() string }:
		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"track": track.GetNormalizedPath(),
		}); encodeErr != nil {
			s.logger.Error("Failed to encode track", slog.String("error", encodeErr.Error()))
		}
	default:
		if encodeErr := json.NewEncoder(w).Encode(map[string]interface{}{
			"track": fmt.Sprintf("%v", track),
		}); encodeErr != nil {
			s.logger.Error("Failed to encode track", slog.String("error", encodeErr.Error()))
		}
	}
}
