// Package radio implements the core radio station functionality.
// It manages radio stations, track playback, and stream handling.
package radio

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	sentryhelper "github.com/aposazhennikov/stream-audio-to-web/sentry_helper"
)

const (
	emptyTrackWaitSec       = 5
	longWaitSec             = 30
	loopPauseMs             = 100
	trackInterruptTimeoutMs = 500
	errorPauseSec           = 1 // Пауза после ошибки воспроизведения, чтобы битые файлы не крутили цикл вхолостую.
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
//
// Playback rules:
//   - a track that played to its end moves the playlist to the next track;
//   - a manual switch (RestartPlayback) has already moved the playlist, so the interrupted
//     track must not move it again;
//   - the next track starts only after the previous StreamTrack call has returned,
//     so two tracks never write into the same stream.
type Station struct {
	streamer     AudioStreamer
	playlist     PlaylistManager
	route        string
	stop         chan struct{}
	stopOnce     sync.Once
	restart      chan struct{} // Будит цикл, если он ждёт треков в пустом плейлисте
	currentTrack chan struct{} // Закрывается, чтобы прервать текущий трек
	waitGroup    sync.WaitGroup
	mutex        sync.Mutex // Защищает currentTrack
	logger       *slog.Logger
	sentryHelper *sentryhelper.SentryHelper // Helper для безопасной работы с Sentry.
	// switchGeneration растёт при каждом ручном переключении. Если за время трека он изменился,
	// трек прервали вручную и плейлист двигать не нужно.
	switchGeneration atomic.Uint64
	// consecutiveErrors считает ошибки воспроизведения подряд; используется только циклом станции.
	consecutiveErrors int
}

// NewRadioStation creates a new radio station.
func NewRadioStation(route string, streamer AudioStreamer, playlist PlaylistManager, logger *slog.Logger, sentryHelper *sentryhelper.SentryHelper) *Station {
	if logger == nil {
		logger = slog.Default()
	}
	return &Station{
		streamer:     streamer,
		playlist:     playlist,
		route:        route,
		stop:         make(chan struct{}),
		restart:      make(chan struct{}, 1),
		currentTrack: make(chan struct{}),
		logger:       logger,
		sentryHelper: sentryHelper,
	}
}

// Start launches the radio station.
func (rs *Station) Start() {
	rs.logger.Debug("Starting radio station...", slog.String("route", rs.route))

	rs.waitGroup.Add(1)
	go rs.streamLoop()

	rs.logger.Debug("Radio station successfully launched", slog.String("route", rs.route))
}

// Stop stops the radio station and waits until its playback loop has exited.
func (rs *Station) Stop() {
	rs.stopOnce.Do(func() {
		close(rs.stop)
	})
	// Прерываем текущий трек: иначе StreamTrack продолжит писать в поток уже остановленной станции.
	rs.streamer.StopCurrentTrack()

	rs.waitGroup.Wait()
	rs.logger.Info("Radio station stopped", slog.String("route", rs.route))
}

// RestartPlayback restarts playback from the current playlist position.
// Called after the playlist was switched via API (next/previous track, reload).
func (rs *Station) RestartPlayback() {
	startTime := time.Now()

	// Сначала отмечаем переключение: прерванный трек увидит новое поколение и не сдвинет плейлист.
	rs.switchGeneration.Add(1)

	rs.mutex.Lock()
	defer rs.mutex.Unlock()

	rs.streamer.StopCurrentTrack()

	// Прерываем ожидание текущего трека в цикле станции.
	close(rs.currentTrack)
	rs.currentTrack = make(chan struct{})

	// Будим цикл, если он ждёт треков.
	select {
	case rs.restart <- struct{}{}:
	default:
	}

	rs.logger.Debug("Manual track switch completed",
		slog.String("route", rs.route),
		slog.Int64("totalSwitchTimeMs", time.Since(startTime).Milliseconds()))
}

// streamLoop main track playback loop.
func (rs *Station) streamLoop() {
	defer rs.waitGroup.Done()

	rs.logger.Debug("Main playback loop started", slog.String("route", rs.route))

	consecutiveEmptyTracks := 0
	maxEmptyAttempts := 5 // Maximum number of attempts to check empty playlist

	for {
		if rs.isStopped() {
			rs.logger.Debug("Stop signal received, exiting stream loop", slog.String("route", rs.route))
			return
		}

		// Сигнал рестарта нужен только чтобы разбудить ожидание пустого плейлиста.
		select {
		case <-rs.restart:
		default:
		}

		track := rs.playlist.GetCurrentTrack()
		if track == nil {
			consecutiveEmptyTracks = rs.handleNoTrack(consecutiveEmptyTracks, maxEmptyAttempts)
			continue
		}
		consecutiveEmptyTracks = 0

		trackPath := getTrackPath(track)
		if trackPath == "" {
			rs.logger.Error("Unable to get track path for station", slog.String("route", rs.route))
			rs.sentryHelper.CaptureInfo(
				fmt.Sprintf("Unable to get track path for station %s", rs.route),
				"radio", "track_path",
			)
			rs.playlist.NextTrack()
			rs.pause(errorPauseSec * time.Second)
			continue
		}

		if !rs.playAndProcessTrack(trackPath) {
			return
		}

		// Small pause before next iteration to prevent CPU racing.
		rs.pause(loopPauseMs * time.Millisecond)
	}
}

// isStopped reports whether the station was stopped.
func (rs *Station) isStopped() bool {
	select {
	case <-rs.stop:
		return true
	default:
		return false
	}
}

// pause sleeps for d or until the station is stopped or restarted.
func (rs *Station) pause(d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-rs.stop:
	case <-rs.restart:
	}
}

// handleNoTrack обрабатывает ситуацию с отсутствием трека.
func (rs *Station) handleNoTrack(consecutiveEmptyTracks, maxEmptyAttempts int) int {
	consecutiveEmptyTracks++

	if consecutiveEmptyTracks <= maxEmptyAttempts {
		rs.logger.Info(
			"No tracks in playlist for",
			slog.String("route", rs.route),
			slog.Int("attempt", consecutiveEmptyTracks),
			slog.Int("maxAttempts", maxEmptyAttempts),
			slog.Int("waitSeconds", emptyTrackWaitSec),
		)
		rs.pause(emptyTrackWaitSec * time.Second)
		return consecutiveEmptyTracks
	}

	// If after several attempts playlist is still empty, switch to long wait mode.
	rs.logger.Info("Playlist is empty. Switching to wait mode...", slog.String("route", rs.route))
	rs.pause(longWaitSec * time.Second)

	// Reset counter for new series of checks.
	return 0
}

// playAndProcessTrack plays one track and moves the playlist when the track ends by itself.
// Returns false when the station was stopped.
func (rs *Station) playAndProcessTrack(trackPath string) bool {
	rs.mutex.Lock()
	currentTrackCh := rs.currentTrack
	rs.mutex.Unlock()
	generation := rs.switchGeneration.Load()

	rs.logger.Info("Starting track playback",
		slog.String("route", rs.route),
		slog.String("trackPath", trackPath))

	trackFinished := make(chan error, 1)
	go func() {
		trackFinished <- rs.streamer.StreamTrack(trackPath)
	}()

	var err error
	select {
	case <-rs.stop:
		rs.waitForPlaybackGoroutine(trackFinished, trackPath)
		return false
	case <-currentTrackCh:
		rs.logger.Info("Playback of track manually interrupted",
			slog.String("trackPath", trackPath),
			slog.String("route", rs.route))
		rs.waitForPlaybackGoroutine(trackFinished, trackPath)
		return !rs.isStopped()
	case err = <-trackFinished:
	}

	if err != nil {
		rs.consecutiveErrors++
		rs.logger.Error("Error playing track",
			slog.String("route", rs.route),
			slog.String("trackPath", trackPath),
			slog.Int("consecutiveErrors", rs.consecutiveErrors),
			slog.String("error", err.Error()))
		rs.sentryHelper.CaptureError(fmt.Errorf("error playing track %s: %w", trackPath, err), "radio", "track_playback")
	} else {
		rs.consecutiveErrors = 0
	}

	switch {
	case rs.isStopped():
		return false
	case rs.switchGeneration.Load() != generation:
		// Трек прервали ручным переключением: плейлист уже стоит на нужном треке.
		rs.logger.Debug("Track ended by manual switch, playlist not advanced",
			slog.String("route", rs.route),
			slog.String("trackPath", trackPath))
	default:
		rs.logger.Debug("Track ended, moving to next track",
			slog.String("route", rs.route),
			slog.String("trackPath", trackPath))
		rs.playlist.NextTrack()
		// Один битый файл пропускаем сразу; если ошибки идут подряд, не крутим цикл вхолостую.
		if rs.consecutiveErrors > 1 {
			rs.pause(errorPauseSec * time.Second)
		}
	}
	return true
}

// waitForPlaybackGoroutine ждёт завершения горутины проигрывания и повторяет остановку,
// если она не завершилась за trackInterruptTimeoutMs (например, стартовала уже после StopCurrentTrack).
// Следующий трек нельзя запускать, пока предыдущий StreamTrack не вернулся:
// иначе два трека пишут в один поток.
func (rs *Station) waitForPlaybackGoroutine(trackFinished chan error, trackPath string) {
	for attempt := 1; ; attempt++ {
		select {
		case <-trackFinished:
			return
		case <-time.After(trackInterruptTimeoutMs * time.Millisecond):
			rs.logger.Warn(
				"Track playback goroutine still running after interruption, stopping it again",
				slog.String("trackPath", trackPath),
				slog.String("route", rs.route),
				slog.Int("attempt", attempt),
			)
			rs.streamer.StopCurrentTrack()
		}
	}
}

// StationManager manages multiple radio stations.
type StationManager struct {
	stations     map[string]*Station
	mutex        sync.RWMutex
	logger       *slog.Logger
	sentryHelper *sentryhelper.SentryHelper // Helper для безопасной работы с Sentry.
}

// NewRadioStationManager creates a new radio station manager.
func NewRadioStationManager(logger *slog.Logger, sentryHelper *sentryhelper.SentryHelper) *StationManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &StationManager{
		stations:     make(map[string]*Station),
		logger:       logger,
		sentryHelper: sentryHelper,
	}
}

// AddStation adds a new radio station.
func (rm *StationManager) AddStation(route string, streamer AudioStreamer, playlist PlaylistManager) {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	rm.logger.Debug("Starting to add radio station to manager...", slog.String("route", route))

	if _, exists := rm.stations[route]; exists {
		// If station already exists, stop it before replacing.
		rm.logger.Info("Radio station already exists, stopping it before replacement", slog.String("route", route))
		rm.stations[route].Stop()
		rm.logger.Info("Existing radio station stopped", slog.String("route", route))
	}

	// Create new station.
	rm.logger.Debug("Creating new radio station...", slog.String("route", route))
	station := NewRadioStation(route, streamer, playlist, rm.logger, rm.sentryHelper)
	rm.stations[route] = station

	// Start only launches the playback goroutine, so it does not block.
	station.Start()

	rm.logger.Debug("Radio station added to manager", slog.String("route", route))
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
	// TODO: Add Sentry logging when sentryHelper is available
	return ""
}
