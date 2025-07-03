// Package radio implements the core radio station functionality.
// It manages radio stations, track playback, and stream handling.
package radio

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	sentry "github.com/getsentry/sentry-go"
)

const (
	shortDelayMs            = 50
	emptyTrackWaitSec       = 5
	longWaitSec             = 30
	loopPauseMs             = 100
	trackInterruptTimeoutMs = 500
)

// AudioStreamer interface for playing audio.
type AudioStreamer interface {
	StreamTrack(trackPath string) error
	Close()
	StopCurrentTrack()
	GetClientCount() int
}

// PlaylistManager interface for playlist management.
type PlaylistManager interface {
	GetCurrentTrack() interface{}
	NextTrack() interface{}
	PreviousTrack() interface{}
}

// Station manages a single radio station.
type Station struct {
	streamer     AudioStreamer
	playlist     PlaylistManager
	route        string
	stop         chan struct{}
	restart      chan struct{} // New channel for restarting playback
	currentTrack chan struct{} // Channel for interrupting current track playback
	waitGroup    sync.WaitGroup
	mutex        sync.Mutex // Mutex for synchronizing access to channels
	logger       *slog.Logger
	manualTrackSwitch bool  // НОВЫЙ флаг для отслеживания ручного переключения треков
	switchMutex  sync.RWMutex // НОВЫЙ мьютекс для защиты флага
}

// NewRadioStation creates a new radio station.
func NewRadioStation(route string, streamer AudioStreamer, playlist PlaylistManager, logger *slog.Logger) *Station {
	if logger == nil {
		logger = slog.Default()
	}
	return &Station{
		streamer:          streamer,
		playlist:          playlist,
		route:             route,
		stop:              make(chan struct{}),
		restart:           make(chan struct{}, 1), // Buffered channel for restart
		currentTrack:      make(chan struct{}),    // Channel for interrupting current track
		logger:            logger,
		manualTrackSwitch: false, // НОВЫЙ флаг инициализирован как false
	}
}

// Start launches the radio station.
func (rs *Station) Start() {
	rs.logger.Info("DIAGNOSTICS: Starting radio station...", slog.String("route", rs.route))

	// Create new stop channel.
	rs.mutex.Lock()
	rs.stop = make(chan struct{})
	rs.restart = make(chan struct{}, 1) // Buffered channel to avoid blocking when sending
	rs.mutex.Unlock()

	rs.waitGroup.Add(1)

	// Start main playback loop in a separate goroutine.
	go func() {
		rs.logger.Info("DIAGNOSTICS: Starting streamLoop for station...", slog.String("route", rs.route))
		rs.streamLoop()
	}()

	rs.logger.Info("DIAGNOSTICS: Radio station successfully launched", slog.String("route", rs.route))
}

// Stop stops the radio station.
func (rs *Station) Stop() {
	rs.mutex.Lock()
	close(rs.stop)
	rs.mutex.Unlock()

	rs.waitGroup.Wait()
	rs.logger.Info("Radio station stopped", slog.String("route", rs.route))
}

// RestartPlayback restarts playback of the current track.
// Called when switching tracks via API.
func (rs *Station) RestartPlayback() {
	startTime := time.Now()
	
	// СПЕЦИАЛЬНОЕ ЛОГИРОВАНИЕ ДЛЯ /floyd
	isFloydStream := strings.Contains(rs.route, "floyd")
	
	if isFloydStream {
		rs.logger.Error("FLOYD DEBUG: MANUAL TRACK SWITCH requested - forcing immediate playback restart",
			slog.String("route", rs.route),
			slog.String("timestamp", startTime.Format("15:04:05.000")))
	} else {
		rs.logger.Error("CRITICAL: Track switch requested - forcing immediate playback restart",
			slog.String("route", rs.route),
			slog.String("timestamp", startTime.Format("15:04:05.000")))
	}

	// КРИТИЧЕСКОЕ ИСПРАВЛЕНИЕ: Устанавливаем флаг ручного переключения
	rs.switchMutex.Lock()
	rs.manualTrackSwitch = true
	rs.switchMutex.Unlock()
	
	if isFloydStream {
		rs.logger.Error("FLOYD DEBUG: Manual track switch flag set to TRUE",
			slog.String("route", rs.route))
	}

	rs.mutex.Lock()
	defer rs.mutex.Unlock()

	// CRITICAL: Stop the streamer immediately with enhanced synchronization
	if isFloydStream {
		rs.logger.Error("FLOYD DEBUG: Stopping current track streamer",
			slog.String("route", rs.route))
	} else {
		rs.logger.Error("CRITICAL: Stopping current track streamer",
			slog.String("route", rs.route))
	}
	rs.streamer.StopCurrentTrack()

	// УБИРАЕМ ЗАДЕРЖКУ: Не ждем 100ms, переходим сразу к перезапуску
	if isFloydStream {
		rs.logger.Error("FLOYD DEBUG: Streamer stop signal sent, proceeding with restart immediately",
			slog.String("route", rs.route))
	} else {
		rs.logger.Error("CRITICAL: Streamer stop signal sent, proceeding with restart immediately",
			slog.String("route", rs.route))
	}

	// Interrupt current playback if it's ongoing.
	select {
	case <-rs.currentTrack: // Channel already closed
		// Create new channel for next track.
		rs.currentTrack = make(chan struct{})
		if isFloydStream {
			rs.logger.Error("FLOYD DEBUG: Current track channel was already closed",
				slog.String("route", rs.route))
		} else {
			rs.logger.Error("CRITICAL: Current track channel was already closed",
				slog.String("route", rs.route))
		}
	default:
		// Close channel to interrupt current playback.
		close(rs.currentTrack)
		// Create new channel for next track.
		rs.currentTrack = make(chan struct{})
		if isFloydStream {
			rs.logger.Error("FLOYD DEBUG: Current track channel closed - playback interrupted",
				slog.String("route", rs.route))
		} else {
			rs.logger.Error("CRITICAL: Current track channel closed - playback interrupted",
				slog.String("route", rs.route))
		}
	}

	// Explicitly update the current track in the playlist to ensure it appears in the now-playing.
	// This is critically important for the proper functioning of track switching tests.
	currentTrack := rs.playlist.GetCurrentTrack()
	if isFloydStream {
		rs.logger.Error("FLOYD DEBUG: Current track after manual switching obtained",
			slog.String("route", rs.route),
			slog.Any("track", currentTrack))
	} else {
		rs.logger.Error("CRITICAL: Current track after manual switching obtained",
			slog.String("route", rs.route),
			slog.Any("track", currentTrack))
	}

	// Send restart signal to playback loop.
	select {
	case rs.restart <- struct{}{}: // Send signal if channel is not full
		if isFloydStream {
			rs.logger.Error("FLOYD DEBUG: Restart signal sent for immediate playback",
				slog.String("route", rs.route))
		} else {
			rs.logger.Error("CRITICAL: Restart signal sent for immediate playback",
				slog.String("route", rs.route))
		}
	default:
		// Channel already contains signal, no need to send another.
		if isFloydStream {
			rs.logger.Error("FLOYD DEBUG: Restart signal already queued",
				slog.String("route", rs.route))
		} else {
			rs.logger.Error("CRITICAL: Restart signal already queued",
				slog.String("route", rs.route))
		}
	}
	
	totalTime := time.Since(startTime)
	if isFloydStream {
		rs.logger.Error("FLOYD DEBUG: MANUAL TRACK SWITCH completed",
			slog.String("route", rs.route),
			slog.Int64("totalSwitchTimeMs", totalTime.Milliseconds()))
	} else {
		rs.logger.Error("CRITICAL: Track switch completed",
			slog.String("route", rs.route),
			slog.Int64("totalSwitchTimeMs", totalTime.Milliseconds()))
	}
}

// streamLoop main track playback loop.
func (rs *Station) streamLoop() {
	defer rs.waitGroup.Done()

	isFloydStream := strings.Contains(rs.route, "floyd")
	
	if isFloydStream {
		rs.logger.Error("FLOYD DEBUG: Main playback loop started", slog.String("route", rs.route))
	} else {
		rs.logger.Info("DIAGNOSTICS: Main playback loop started for station...", slog.String("route", rs.route))
	}

	consecutiveEmptyTracks := 0
	maxEmptyAttempts := 5 // Maximum number of attempts to check empty playlist
	var isRestartRequested bool
	loopCounter := 0

	for {
		loopCounter++
		
		if isFloydStream {
			rs.logger.Error("FLOYD DEBUG: Stream loop iteration", 
				slog.String("route", rs.route),
				slog.Int("iteration", loopCounter),
				slog.Bool("isRestartRequested", isRestartRequested))
		}

		// Check stop and restart signals before starting new cycle.
		if rs.checkStopSignal() {
			if isFloydStream {
				rs.logger.Error("FLOYD DEBUG: Stop signal received, exiting stream loop", 
					slog.String("route", rs.route))
			}
			return
		}

		isRestartRequested = rs.checkRestartSignal(isRestartRequested)

		// Get current track.
		if isFloydStream {
			rs.logger.Error("FLOYD DEBUG: Getting current track", slog.String("route", rs.route))
		} else {
			rs.logger.Info("DIAGNOSTICS: Getting current track for station...", slog.String("route", rs.route))
		}
		track := rs.playlist.GetCurrentTrack()

		if track == nil {
			// Обрабатываем случай с отсутствием трека.
			if isFloydStream {
				rs.logger.Error("FLOYD DEBUG: No track available", slog.String("route", rs.route))
			}
			consecutiveEmptyTracks = rs.handleNoTrack(consecutiveEmptyTracks, maxEmptyAttempts)
			continue
		}

		// Reset empty attempt counter if track is found.
		consecutiveEmptyTracks = 0

		// Получаем трек и проверяем его валидность.
		trackPath := rs.validateTrack(track, isRestartRequested)
		if trackPath == "" {
			if isFloydStream {
				rs.logger.Error("FLOYD DEBUG: Track validation failed", slog.String("route", rs.route))
			}
			continue
		}

		if isFloydStream {
			rs.logger.Error("FLOYD DEBUG: About to play track", 
				slog.String("route", rs.route),
				slog.String("trackPath", trackPath),
				slog.Bool("isRestartRequested", isRestartRequested))
		}

		// Проигрываем трек и обрабатываем результат.
		isRestartRequested = rs.playAndProcessTrack(trackPath, isRestartRequested)

		if isFloydStream {
			rs.logger.Error("FLOYD DEBUG: Track playback completed", 
				slog.String("route", rs.route),
				slog.Bool("isRestartRequested", isRestartRequested))
		}

		// Small pause before next iteration to prevent CPU racing.
		time.Sleep(loopPauseMs * time.Millisecond)
	}
}

// checkStopSignal проверяет сигнал остановки.
func (rs *Station) checkStopSignal() bool {
	select {
	case <-rs.stop:
		isFloydStream := strings.Contains(rs.route, "floyd")
		if isFloydStream {
			rs.logger.Error("FLOYD DEBUG: Stop signal received - stopping radio station", 
				slog.String("route", rs.route))
		} else {
			rs.logger.Info("Stopping radio station", slog.String("route", rs.route))
		}
		return true
	default:
		return false
	}
}

// checkRestartSignal проверяет сигнал рестарта.
func (rs *Station) checkRestartSignal(currentState bool) bool {
	select {
	case <-rs.restart:
		isFloydStream := strings.Contains(rs.route, "floyd")
		if isFloydStream {
			rs.logger.Error("FLOYD DEBUG: Restart signal processed for station", slog.String("route", rs.route))
		} else {
			rs.logger.Info("DIAGNOSTICS: Restart signal processed for station", slog.String("route", rs.route))
		}
		
		// КРИТИЧЕСКОЕ ИСПРАВЛЕНИЕ: При получении restart сигнала сбрасываем флаг ручного переключения
		rs.switchMutex.Lock()
		oldFlag := rs.manualTrackSwitch
		rs.manualTrackSwitch = false
		rs.switchMutex.Unlock()
		
		if isFloydStream && oldFlag {
			rs.logger.Error("FLOYD DEBUG: Manual track switch flag reset during restart signal processing",
				slog.String("route", rs.route))
		}
		
		return true
	default:
		return currentState
	}
}

// handleNoTrack обрабатывает ситуацию с отсутствием трека.
func (rs *Station) handleNoTrack(consecutiveEmptyTracks, maxEmptyAttempts int) int {
	consecutiveEmptyTracks++
	rs.logger.Info(
		"DIAGNOSTICS: No track for station",
		slog.String("route", rs.route),
		slog.String("action", "waiting"),
	)

	if consecutiveEmptyTracks <= maxEmptyAttempts {
		rs.logger.Info(
			"No tracks in playlist for",
			slog.String("route", rs.route),
			slog.Int("attempt", consecutiveEmptyTracks),
			slog.Int("maxAttempts", maxEmptyAttempts),
			slog.Int("waitSeconds", emptyTrackWaitSec),
		)
		// Wait and try again.
		time.Sleep(emptyTrackWaitSec * time.Second)
		return consecutiveEmptyTracks
	}

	// If after several attempts playlist is still empty, switch to long wait mode.
	rs.logger.Info("Playlist is empty. Switching to wait mode...", slog.String("route", rs.route))

	// Wait longer between checks to save resources.
	time.Sleep(longWaitSec * time.Second)

	// Reset counter for new series of checks.
	return 0
}

// validateTrack проверяет трек на валидность.
func (rs *Station) validateTrack(track interface{}, isRestartRequested bool) string {
	rs.logger.Info("DIAGNOSTICS: Track found for station", slog.String("route", rs.route))

	// Streaming current track.
	trackPath := getTrackPath(track)
	rs.logger.Info(
		"DIAGNOSTICS: Track path obtained for station",
		slog.String("route", rs.route),
		slog.String("trackPath", trackPath),
	)

	if trackPath == "" {
		rs.logger.Error("Unable to get track path for station", slog.String("route", rs.route))
		sentry.CaptureMessage(
			fmt.Sprintf("Unable to get track path for station %s", rs.route),
		)
		
		// Only call NextTrack if this is not a manual restart.
		if !isRestartRequested {
			rs.logger.Info("DIAGNOSTICS: Moving to next track due to invalid path", slog.String("route", rs.route))
			rs.playlist.NextTrack()
		} else {
			rs.logger.Info("DIAGNOSTICS: Restart request detected, not calling NextTrack for invalid path", slog.String("route", rs.route))
		}
		return ""
	}

	return trackPath
}

// playAndProcessTrack запускает проигрывание трека и обрабатывает его результат.
func (rs *Station) playAndProcessTrack(trackPath string, isRestartRequested bool) bool {
	isFloydStream := strings.Contains(rs.route, "floyd")
	
	if isFloydStream {
		rs.logger.Error("FLOYD DEBUG: Starting track playback processing", 
			slog.String("route", rs.route),
			slog.String("trackPath", trackPath),
			slog.Bool("isRestartRequested", isRestartRequested))
	}

	// Create local copy of channel for current track.
	rs.mutex.Lock()
	currentTrackCh := rs.currentTrack
	rs.mutex.Unlock()

	if isFloydStream {
		rs.logger.Error("FLOYD DEBUG: Current track channel acquired", 
			slog.String("route", rs.route))
	}

	// Start track playback in separate goroutine.
	trackFinished := make(chan error, 1)
	go rs.startTrackPlayback(trackPath, trackFinished)

	if isFloydStream {
		rs.logger.Error("FLOYD DEBUG: Track playback goroutine started", 
			slog.String("route", rs.route))
	}

	// Wait for either track completion or interrupt signal.
	result := rs.waitForPlaybackResult(currentTrackCh, trackFinished, trackPath, isRestartRequested)
	
	if isFloydStream {
		rs.logger.Error("FLOYD DEBUG: Track playback processing completed", 
			slog.String("route", rs.route),
			slog.Bool("result", result))
	}
	
	return result
}

// startTrackPlayback запускает проигрывание трека.
func (rs *Station) startTrackPlayback(trackPath string, resultCh chan<- error) {
	isFloydStream := strings.Contains(rs.route, "floyd")
	
	if isFloydStream {
		rs.logger.Error("FLOYD DEBUG: Starting playback of track in goroutine",
			slog.String("trackPath", trackPath),
			slog.String("route", rs.route))
	} else {
		rs.logger.Info(
			"DIAGNOSTICS: Starting playback of track",
			slog.String("trackPath", trackPath),
			slog.String("route", rs.route),
		)
	}
	
	err := rs.streamer.StreamTrack(trackPath)
	
	if isFloydStream {
		rs.logger.Error("FLOYD DEBUG: Streamer returned from StreamTrack",
			slog.String("trackPath", trackPath),
			slog.String("route", rs.route),
			slog.String("error", fmt.Sprintf("%v", err)))
	}
	
	resultCh <- err
}

// waitForPlaybackResult ожидает результата проигрывания трека.
func (rs *Station) waitForPlaybackResult(
	currentTrackCh chan struct{},
	trackFinished chan error,
	trackPath string,
	isRestartRequested bool,
) bool {
	isFloydStream := strings.Contains(rs.route, "floyd")
	
	if isFloydStream {
		rs.logger.Error("FLOYD DEBUG: Waiting for playback result", 
			slog.String("route", rs.route),
			slog.String("trackPath", trackPath))
	}

	select {
	case <-rs.stop:
		// Station stopped, exit loop.
		if isFloydStream {
			rs.logger.Error("FLOYD DEBUG: Stop signal received during playback", 
				slog.String("route", rs.route))
		} else {
			rs.logger.Info("Stopping radio station during playback", slog.String("route", rs.route))
		}
		return isRestartRequested // Возвращаем текущее значение, хотя оно не будет использовано

	case <-currentTrackCh:
		// Track interrupted by RestartPlayback signal.
		if isFloydStream {
			rs.logger.Error("FLOYD DEBUG: Track interrupted by RestartPlayback signal", 
				slog.String("route", rs.route))
		}
		return rs.handleTrackInterruption(trackFinished, trackPath, isRestartRequested)

	case err := <-trackFinished:
		// Track completed naturally or an error occurred.
		if isFloydStream {
			rs.logger.Error("FLOYD DEBUG: Track playback finished", 
				slog.String("route", rs.route),
				slog.String("error", fmt.Sprintf("%v", err)))
		}
		return rs.handleTrackCompletion(err, trackPath, isRestartRequested)
	}
}

// handleTrackInterruption обрабатывает прерывание трека.
func (rs *Station) handleTrackInterruption(trackFinished chan error, trackPath string, isRestartRequested bool) bool {
	rs.logger.Info(
		"DIAGNOSTICS: Playback of track manually interrupted",
		slog.String("trackPath", trackPath),
		slog.String("route", rs.route),
	)

	// Wait to make sure playback goroutine has finished.
	select {
	case <-trackFinished:
		rs.logger.Info(
			"DIAGNOSTICS: Track playback goroutine successfully completed after interruption",
			slog.String("trackPath", trackPath),
		)
	case <-time.After(trackInterruptTimeoutMs * time.Millisecond):
		rs.logger.Info(
			"DIAGNOSTICS: Timeout waiting for track playback goroutine to complete",
			slog.String("trackPath", trackPath),
		)
	}

	// Check if restart was requested.
	if isRestartRequested {
		rs.logger.Info(
			"DIAGNOSTICS: Restart request detected for station, resetting flag",
			slog.String("route", rs.route),
		)
		return false // Сбрасываем флаг
	}

	return isRestartRequested
}

// handleTrackCompletion обрабатывает завершение проигрывания трека.
func (rs *Station) handleTrackCompletion(err error, trackPath string, isRestartRequested bool) bool {
	// СПЕЦИАЛЬНОЕ ЛОГИРОВАНИЕ ДЛЯ /floyd
	isFloydStream := strings.Contains(rs.route, "floyd")
	
	// КРИТИЧЕСКОЕ ИСПРАВЛЕНИЕ: Проверяем флаг ручного переключения
	rs.switchMutex.RLock()
	isManualSwitch := rs.manualTrackSwitch
	rs.switchMutex.RUnlock()
	
	if isFloydStream {
		rs.logger.Error("FLOYD DEBUG: Track completion handler called",
			slog.String("trackPath", trackPath),
			slog.Bool("isRestartRequested", isRestartRequested),
			slog.Bool("isManualSwitch", isManualSwitch))
	}
	
	if err != nil {
		if isFloydStream {
			rs.logger.Error("FLOYD DEBUG: Error playing track",
				slog.String("trackPath", trackPath),
				slog.String("error", err.Error()),
				slog.Bool("isRestartRequested", isRestartRequested),
				slog.Bool("isManualSwitch", isManualSwitch))
		} else {
			rs.logger.Error("Error playing track",
				slog.String("trackPath", trackPath),
				slog.String("error", err.Error()))
		}
		sentry.CaptureException(fmt.Errorf("error playing track %s: %w", trackPath, err))
		
		// On error, skip track and move to next only if not a manual restart and not manual switch.
		if !isRestartRequested && !isManualSwitch {
			if isFloydStream {
				rs.logger.Error("FLOYD DEBUG: Moving to next track due to error", slog.String("route", rs.route))
			} else {
				rs.logger.Info("DIAGNOSTICS: Moving to next track due to error", slog.String("route", rs.route))
			}
			nextTrack := rs.playlist.NextTrack()
			if isFloydStream {
				rs.logger.Error("FLOYD DEBUG: NextTrack() returned after error", 
					slog.Any("nextTrack", nextTrack))
			}
		} else {
			if isFloydStream {
				rs.logger.Error("FLOYD DEBUG: Skipping NextTrack due to restart or manual switch", 
					slog.String("route", rs.route),
					slog.Bool("isRestartRequested", isRestartRequested),
					slog.Bool("isManualSwitch", isManualSwitch))
			} else {
				rs.logger.Info("DIAGNOSTICS: Restart request detected, not calling NextTrack for error", slog.String("route", rs.route))
			}
			
			// КРИТИЧЕСКОЕ ИСПРАВЛЕНИЕ: Сбрасываем флаг ручного переключения
			if isManualSwitch {
				rs.switchMutex.Lock()
				rs.manualTrackSwitch = false
				rs.switchMutex.Unlock()
				if isFloydStream {
					rs.logger.Error("FLOYD DEBUG: Manual track switch flag reset to FALSE",
						slog.String("route", rs.route))
				}
			}
			
			return false // Сбрасываем флаг
		}
	} else {
		if isFloydStream {
			rs.logger.Error("FLOYD DEBUG: Completed playback of track",
				slog.String("trackPath", trackPath),
				slog.String("route", rs.route),
				slog.Bool("isRestartRequested", isRestartRequested),
				slog.Bool("isManualSwitch", isManualSwitch))
		} else {
			rs.logger.Info("DIAGNOSTICS: Completed playback of track",
				slog.String("trackPath", trackPath),
				slog.String("route", rs.route))
		}
		// Move to next track only if no restart request and no manual switch.
		if !isRestartRequested && !isManualSwitch {
			if isFloydStream {
				rs.logger.Error("FLOYD DEBUG: Moving to next track for station", slog.String("route", rs.route))
			} else {
				rs.logger.Info("DIAGNOSTICS: Moving to next track for station", slog.String("route", rs.route))
			}
			nextTrack := rs.playlist.NextTrack()
			if isFloydStream {
				rs.logger.Error("FLOYD DEBUG: NextTrack() returned after natural completion", 
					slog.Any("nextTrack", nextTrack))
			}
		} else {
			if isFloydStream {
				rs.logger.Error("FLOYD DEBUG: Skipping NextTrack due to restart or manual switch",
					slog.String("route", rs.route),
					slog.Bool("isRestartRequested", isRestartRequested),
					slog.Bool("isManualSwitch", isManualSwitch))
			} else {
				rs.logger.Info("DIAGNOSTICS: Restart request detected for station, resetting flag",
					slog.String("route", rs.route))
			}
			
			// КРИТИЧЕСКОЕ ИСПРАВЛЕНИЕ: Сбрасываем флаг ручного переключения
			if isManualSwitch {
				rs.switchMutex.Lock()
				rs.manualTrackSwitch = false
				rs.switchMutex.Unlock()
				if isFloydStream {
					rs.logger.Error("FLOYD DEBUG: Manual track switch flag reset to FALSE",
						slog.String("route", rs.route))
				}
			}
			
			return false // Сбрасываем флаг
		}
	}

	// КРИТИЧЕСКОЕ ИСПРАВЛЕНИЕ: Всегда сбрасываем флаг ручного переключения в конце
	if isManualSwitch {
		rs.switchMutex.Lock()
		rs.manualTrackSwitch = false
		rs.switchMutex.Unlock()
		if isFloydStream {
			rs.logger.Error("FLOYD DEBUG: Manual track switch flag reset to FALSE at end",
				slog.String("route", rs.route))
		}
	}

	return isRestartRequested
}

// StationManager manages multiple radio stations.
type StationManager struct {
	stations map[string]*Station
	mutex    sync.RWMutex
	logger   *slog.Logger
}

// NewRadioStationManager creates a new radio station manager.
func NewRadioStationManager(logger *slog.Logger) *StationManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &StationManager{
		stations: make(map[string]*Station),
		logger:   logger,
	}
}

// AddStation adds a new radio station.
func (rm *StationManager) AddStation(route string, streamer AudioStreamer, playlist PlaylistManager) {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	rm.logger.Info("DIAGNOSTICS: Starting to add radio station to manager...", slog.String("route", route))

	if _, exists := rm.stations[route]; exists {
		// If station already exists, stop it before replacing.
		rm.logger.Info("Radio station already exists, stopping it before replacement", slog.String("route", route))
		rm.stations[route].Stop()
		rm.logger.Info("Existing radio station stopped", slog.String("route", route))
	}

	// Create new station.
	rm.logger.Info("DIAGNOSTICS: Creating new radio station...", slog.String("route", route))
	station := NewRadioStation(route, streamer, playlist, rm.logger)
	rm.stations[route] = station

	// Launch station asynchronously to avoid blocking main thread.
	rm.logger.Info("DIAGNOSTICS: Starting radio station in separate goroutine...", slog.String("route", route))
	go func() {
		rm.logger.Info("DIAGNOSTICS: Beginning to start station inside goroutine", slog.String("route", route))
		station.Start()
		rm.logger.Info("DIAGNOSTICS: Radio station goroutine successfully started", slog.String("route", route))
	}()

	rm.logger.Info("DIAGNOSTICS: Radio station added to manager", slog.String("route", route))
}

// RemoveStation removes a radio station.
func (rm *StationManager) RemoveStation(route string) {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	if station, exists := rm.stations[route]; exists {
		station.Stop()
		delete(rm.stations, route)
		rm.logger.Info("Radio station stopped and removed", slog.String("route", route))
	}
}

// StopAll stops all radio stations.
func (rm *StationManager) StopAll() {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	for route, station := range rm.stations {
		station.Stop()
		rm.logger.Info("Radio station stopped", slog.String("route", route))
	}
	rm.logger.Info("All radio stations stopped")
}

// GetStation returns a radio station by route.
func (rm *StationManager) GetStation(route string) *Station {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	if station, exists := rm.stations[route]; exists {
		return station
	}
	return nil
}

// RestartPlayback restarts playback for specified route.
func (rm *StationManager) RestartPlayback(route string) bool {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	if station, exists := rm.stations[route]; exists {
		station.RestartPlayback()
		return true
	}
	return false
}

// getTrackPath extracts track path from interface.
func getTrackPath(track interface{}) string {
	// Interface unpacking depends on specific Track implementation.
	// Here it's assumed that track has a Path field.
	if t, ok := track.(interface{ GetPath() string }); ok {
		return t.GetPath()
	}
	if t, ok := track.(map[string]string); ok {
		return t["path"]
	}
	if t, ok := track.(struct{ Path string }); ok {
		return t.Path
	}

	// If unpacking failed, try to convert to string.
	if s, ok := track.(string); ok {
		return s
	}

	slog.Default().Error("Unknown track type", slog.Any("track", track))
	sentry.CaptureMessage(fmt.Sprintf("Unknown track type: %T", track)) // This is an error, send to Sentry
	return ""
}
