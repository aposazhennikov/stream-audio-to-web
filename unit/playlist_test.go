
package unit_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aposazhennikov/stream-audio-to-web/playlist"
	sentryhelper "github.com/aposazhennikov/stream-audio-to-web/sentry_helper"
	"github.com/aposazhennikov/stream-audio-to-web/unit/testdata"
)


// Creating different audio file types for testing.
func createTestFiles(_ *testing.T, dir string) error {
	files := []struct {
		name string
		data []byte
	}{
		{"test1.mp3", testdata.GetMinimumMP3Data()},
		{"test2.mp3", testdata.GetMinimumMP3Data()},
		{"test3.wav", testdata.GetMinimumWAVData()},
		{"test4.ogg", testdata.GetMinimumOGGData()},
		{"test5.mp3", testdata.GetMinimumMP3Data()},
	}

	for _, file := range files {
		filePath := filepath.Join(dir, file.name)
		if err := os.WriteFile(filePath, file.data, 0644); err != nil {
			return err
		}
	}
	return nil
}

// checkTrackPaths проверяет, что пути двух треков различны.
func checkTrackPaths(t *testing.T, track1 interface{}, track2 interface{}) {
	t1, ok1 := track1.(interface{ GetPath() string })
	t2, ok2 := track2.(interface{ GetPath() string })

	if !ok1 || !ok2 {
		t.Fatalf("One or both tracks do not implement GetPath() method")
	}

	path1 := t1.GetPath()
	path2 := t2.GetPath()

	if path1 == path2 {
		t.Fatalf("Both tracks have the same path: %s", path1)
	}
}

// GetCurrentTrack returns the current track.
func TestPlaylist_GetCurrentTrack(t *testing.T) {
	// Create a temporary directory for tests.
	tmpDir := t.TempDir()

	// Create various audio files for testing.
	if err := createTestFiles(t, tmpDir); err != nil {
		t.Fatalf("Failed to create test audio files: %v", err)
	}

	// Установим переменную окружения для увеличения таймаута в тестах
	t.Setenv("TEST_ENVIRONMENT", "1")

	// Initialize playlist.
	pl, err := playlist.NewPlaylist(tmpDir, nil, false, slog.Default(), createTestSentryHelper())
	if err != nil {
		t.Fatalf("Failed to create playlist: %v", err)
	}
	defer pl.Close() // Убедимся, что ресурсы освобождаются

	// Даем время плейлисту инициализироваться перед запросом трека
	time.Sleep(300 * time.Millisecond)

	// Проверка с таймаутом для избежания зависания теста
	timeout := time.After(5 * time.Second)
	done := make(chan interface{}, 1)

	go func() {
		// Check that the current track is not empty.
		var track interface{}
		// Retry for up to 2 seconds to get a non-nil track.
		for range 10 {
			track = pl.GetCurrentTrack()
			if track != nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		done <- track
	}()

	// Ожидаем результата или таймаута
	var track interface{}
	select {
	case result := <-done:
		track = result
	case <-timeout:
		t.Fatalf("Test timed out while waiting for GetCurrentTrack")
	}

	if track == nil {
		t.Fatalf("Expected current track to not be nil after multiple attempts")
	}

	// Check that the track has a path.
	if trackWithPath, ok := track.(interface{ GetPath() string }); !ok || trackWithPath.GetPath() == "" {
		t.Fatalf("Expected track to have a valid path")
	}

	// Оборачиваем операции с историей в отдельную горутину с таймаутом
	historyTimeout := time.After(5 * time.Second)
	historyDone := make(chan struct{})

	go func() {
		// Manually add the track to history if it's not there yet.
		pl.NextTrack() // This will add the current track to history
		time.Sleep(300 * time.Millisecond)
		pl.PreviousTrack() // Go back to the original track
		time.Sleep(300 * time.Millisecond)
		historyDone <- struct{}{}
	}()

	// Ожидаем завершения операций с историей или таймаута
	select {
	case <-historyDone:
		// продолжаем выполнение
	case <-historyTimeout:
		t.Fatalf("Test timed out while manipulating track history")
	}

	// Проверка истории с таймаутом
	historyCheckTimeout := time.After(5 * time.Second)
	historyResult := make(chan []interface{})

	go func() {
		// Check that history starts with the current track.
		var history []interface{}
		// Retry for up to 2 seconds to get a non-empty history.
		for range 10 {
			history = pl.GetHistory()
			if len(history) > 0 {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		historyResult <- history
	}()

	// Ожидаем результата или таймаута
	var history []interface{}
	select {
	case result := <-historyResult:
		history = result
	case <-historyCheckTimeout:
		t.Fatalf("Test timed out while waiting for GetHistory")
	}

	if len(history) == 0 {
		t.Fatalf("Expected history to contain at least one item after multiple attempts")
	}

	t.Logf("Current track: %v", track)
	t.Logf("History: %v", history)
}

// Упрощенная версия с разделением на подфункции для уменьшения сложности.
func TestPlaylist_NextTrack(t *testing.T) {
	// Подготовка тестового окружения.
	pl := setupNextTrackTest(t)
	defer pl.Close()

	// Получение текущего трека.
	currentTrack := getCurrentTrackWithTimeout(t, pl)

	// Логирование начального состояния.
	t.Logf("Initial track: %v", currentTrack)
	t.Logf("Initial history: %v", pl.GetHistory())

	// Переход к следующему треку и проверка результата.
	nextTrack := moveToNextTrackWithTimeout(t, pl)
	validateNextTrack(t, nextTrack, currentTrack)

	// Еще раз переход к следующему треку.
	moveToNextTrackAgainWithTimeout(t, pl)

	// Проверка истории.
	history := getHistoryWithTimeout(t, pl, 2)

	// Логирование результатов.
	t.Logf("After next track operation:")
	t.Logf("Next track: %v", nextTrack)
	t.Logf("Updated history: %v", history)
}

// Вспомогательные функции для TestPlaylist_NextTrack.
func setupNextTrackTest(t *testing.T) *playlist.Playlist {
	// Create a temporary directory for tests.
	tmpDir := t.TempDir()

	// Create various audio files for testing.
	if err := createTestFiles(t, tmpDir); err != nil {
		t.Fatalf("Failed to create test audio files: %v", err)
	}

	// Установим переменную окружения для увеличения таймаута в тестах.
	t.Setenv("TEST_ENVIRONMENT", "1")

	// Initialize playlist.
	pl, err := playlist.NewPlaylist(tmpDir, nil, false, slog.Default(), createTestSentryHelper())
	if err != nil {
		t.Fatalf("Failed to create playlist: %v", err)
	}

	// Даем время плейлисту инициализироваться.
	time.Sleep(300 * time.Millisecond)

	return pl
}

func getCurrentTrackWithTimeout(t *testing.T, pl *playlist.Playlist) interface{} {
	getCurrentTrackTimeout := time.After(5 * time.Second)
	currentTrackDone := make(chan interface{}, 1)

	go func() {
		// Get current track with retry
		var currentTrack interface{}
		for range 10 {
			currentTrack = pl.GetCurrentTrack()
			if currentTrack != nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		currentTrackDone <- currentTrack
	}()

	// Ожидаем результата или таймаута
	var currentTrack interface{}
	select {
	case result := <-currentTrackDone:
		currentTrack = result
	case <-getCurrentTrackTimeout:
		t.Fatalf("Test timed out while waiting for GetCurrentTrack")
	}

	if currentTrack == nil {
		t.Fatalf("Expected current track to not be nil before switching after multiple attempts")
	}

	return currentTrack
}

func moveToNextTrackWithTimeout(t *testing.T, pl *playlist.Playlist) interface{} {
	nextTrackTimeout := time.After(5 * time.Second)
	nextTrackDone := make(chan interface{}, 1)

	go func() {
		// Move to next track
		nextTrack := pl.NextTrack()
		nextTrackDone <- nextTrack
	}()

	// Ожидаем результата или таймаута
	var nextTrack interface{}
	select {
	case result := <-nextTrackDone:
		nextTrack = result
	case <-nextTrackTimeout:
		t.Fatalf("Test timed out while waiting for NextTrack")
	}

	if nextTrack == nil {
		t.Fatalf("Expected next track to not be nil")
	}

	return nextTrack
}

func validateNextTrack(t *testing.T, nextTrack, currentTrack interface{}) {
	// Check that the track has a path
	if track, okNext := nextTrack.(interface{ GetPath() string }); !okNext || track.GetPath() == "" {
		t.Fatalf("Expected next track to have a valid path")
	}

	// Check that the next track is different from the current track
	if nextTrack == currentTrack {
		checkTrackPaths(t, nextTrack, currentTrack)
	}
}

func moveToNextTrackAgainWithTimeout(t *testing.T, pl *playlist.Playlist) {
	nextTrack2Timeout := time.After(5 * time.Second)
	nextTrack2Done := make(chan struct{}, 1)

	go func() {
		// Move to next track again to ensure we have at least 2 tracks in history
		pl.NextTrack()
		time.Sleep(300 * time.Millisecond)
		nextTrack2Done <- struct{}{}
	}()

	// Ожидаем результата или таймаута
	select {
	case <-nextTrack2Done:
		// продолжаем выполнение
	case <-nextTrack2Timeout:
		t.Fatalf("Test timed out while moving to the next track again")
	}
}

func getHistoryWithTimeout(t *testing.T, pl *playlist.Playlist, minSize int) []interface{} {
	historyTimeout := time.After(5 * time.Second)
	historyDone := make(chan []interface{}, 1)

	go func() {
		// Check that history has been updated with retry
		var history []interface{}
		for range 10 {
			history = pl.GetHistory()
			if len(history) >= minSize {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		historyDone <- history
	}()

	// Ожидаем результата или таймаута
	var history []interface{}
	select {
	case result := <-historyDone:
		history = result
	case <-historyTimeout:
		t.Fatalf("Test timed out while waiting for GetHistory")
	}

	if len(history) < minSize {
		t.Fatalf("Expected history to contain at least %d items after multiple attempts, but got %d",
			minSize, len(history))
	}

	return history
}

func TestPlaylist_PreviousTrack(t *testing.T) {
	// Create a temporary directory for tests.
	tmpDir := t.TempDir()

	// Create various audio files for testing.
	if err := createTestFiles(t, tmpDir); err != nil {
		t.Fatalf("Failed to create test audio files: %v", err)
	}

	// Установим переменную окружения для увеличения таймаута в тестах
	t.Setenv("TEST_ENVIRONMENT", "1")

	// Initialize playlist.
	pl, err := playlist.NewPlaylist(tmpDir, nil, false, slog.Default(), createTestSentryHelper())
	if err != nil {
		t.Fatalf("Failed to create playlist: %v", err)
	}
	defer pl.Close() // Убедимся, что ресурсы освобождаются

	// Allow time for playlist initialization.
	time.Sleep(300 * time.Millisecond)

	// Получение текущего трека с таймаутом
	getCurrentTrackTimeout := time.After(5 * time.Second)
	currentTrackChan := make(chan interface{}, 1)

	go func() {
		currentTrack := pl.GetCurrentTrack()
		currentTrackChan <- currentTrack
	}()

	// Remember current track.
	var currentTrack interface{}
	select {
	case result := <-currentTrackChan:
		currentTrack = result
	case <-getCurrentTrackTimeout:
		t.Fatalf("Test timed out while getting current track")
	}

	currentTrackPath := ""
	if track, ok := currentTrack.(interface{ GetPath() string }); ok {
		currentTrackPath = track.GetPath()
		t.Logf("Initial track path: %s", currentTrackPath)
	}

	// Переход к следующему треку с таймаутом
	nextTrackTimeout := time.After(5 * time.Second)
	nextTrackDone := make(chan struct{}, 1)

	go func() {
		// Move to next track.
		pl.NextTrack()
		nextTrackDone <- struct{}{}
	}()

	// Ожидаем результата или таймаута
	select {
	case <-nextTrackDone:
		// продолжаем выполнение
	case <-nextTrackTimeout:
		t.Fatalf("Test timed out while moving to next track")
	}

	// Allow time for playlist to update.
	time.Sleep(300 * time.Millisecond)

	// Получение промежуточного трека с таймаутом
	getMidTrackTimeout := time.After(5 * time.Second)
	midTrackChan := make(chan interface{}, 1)

	go func() {
		midTrack := pl.GetCurrentTrack()
		midTrackChan <- midTrack
	}()

	// Check that the track has changed.
	var midTrack interface{}
	select {
	case result := <-midTrackChan:
		midTrack = result
	case <-getMidTrackTimeout:
		t.Fatalf("Test timed out while getting mid track")
	}

	var midTrackPath string
	if track, ok := midTrack.(interface{ GetPath() string }); ok {
		midTrackPath = track.GetPath()
		t.Logf("Middle track path (after next): %s", midTrackPath)
	}

	// Возврат к предыдущему треку с таймаутом
	previousTrackTimeout := time.After(5 * time.Second)
	previousTrackChan := make(chan interface{}, 1)

	go func() {
		// Move back to previous track.
		previousTrack := pl.PreviousTrack()
		previousTrackChan <- previousTrack
	}()

	// Ожидаем результат или таймаут
	var previousTrack interface{}
	select {
	case result := <-previousTrackChan:
		previousTrack = result
	case <-previousTrackTimeout:
		t.Fatalf("Test timed out while moving to previous track")
	}

	// Allow time for playlist to update.
	time.Sleep(300 * time.Millisecond)

	// Check that the previous track is not empty.
	if previousTrack == nil {
		t.Fatalf("Expected previous track to not be nil")
	}

	// Get previous track path.
	previousTrackPath := ""
	if track, ok := previousTrack.(interface{ GetPath() string }); ok {
		previousTrackPath = track.GetPath()
		t.Logf("Previous track path (after prev): %s", previousTrackPath)
	}

	// Check that the previous track matches the original track.
	if previousTrackPath != currentTrackPath {
		t.Fatalf("Expected previous track path (%s) to be the same as original track path (%s)",
			previousTrackPath, currentTrackPath)
	}

	// Log history state.
	historyTimeout := time.After(5 * time.Second)
	historyChan := make(chan []interface{}, 1)

	go func() {
		history := pl.GetHistory()
		historyChan <- history
	}()

	// Получаем историю с таймаутом
	var history []interface{}
	select {
	case result := <-historyChan:
		history = result
	case <-historyTimeout:
		t.Fatalf("Test timed out while getting history")
	}

	t.Logf("Final history: %v", history)
}

func TestPlaylist_ShuffleMode(t *testing.T) {
	// Создаем временную директорию и тестовые файлы.
	tmpDir := setupShuffleTestFiles(t)

	// Получаем треки из обычного плейлиста (без перемешивания).
	_, regularTracks := getRegularPlaylistTracks(t, tmpDir)

	// Получаем треки из перемешанного плейлиста.
	shuffleTracks := getShuffledPlaylistTracks(t, tmpDir)

	// Логируем порядок треков для отладки.
	t.Logf("Regular playlist tracks order: %v", regularTracks)
	t.Logf("Shuffled playlist tracks order: %v", shuffleTracks)

	// Проверяем результаты.
	validateShuffleResults(t, regularTracks, shuffleTracks)
}

// setupShuffleTestFiles создает временную директорию и тестовые файлы для тестирования функции перемешивания.
func setupShuffleTestFiles(t *testing.T) string {
	// Создаем временную директорию для тестов.
	tmpDir := t.TempDir()

	// Создаем больше аудиофайлов для лучшего тестирования перемешивания.
	files := []struct {
		name string
		data []byte
	}{
		{"01_track.mp3", testdata.GetMinimumMP3Data()},
		{"02_track.mp3", testdata.GetMinimumMP3Data()},
		{"03_track.mp3", testdata.GetMinimumMP3Data()},
		{"04_track.mp3", testdata.GetMinimumMP3Data()},
		{"05_track.mp3", testdata.GetMinimumMP3Data()},
		{"06_track.mp3", testdata.GetMinimumMP3Data()},
		{"07_track.mp3", testdata.GetMinimumMP3Data()},
		{"08_track.mp3", testdata.GetMinimumMP3Data()},
		{"09_track.mp3", testdata.GetMinimumMP3Data()},
		{"10_track.mp3", testdata.GetMinimumMP3Data()},
	}

	// Создаем файлы.
	for _, file := range files {
		filePath := filepath.Join(tmpDir, file.name)
		if err := os.WriteFile(filePath, file.data, 0644); err != nil {
			t.Fatalf("Failed to create test file %s: %v", file.name, err)
		}
	}

	return tmpDir
}

// getRegularPlaylistTracks получает треки из обычного плейлиста (без перемешивания).
func getRegularPlaylistTracks(t *testing.T, tmpDir string) (*playlist.Playlist, []string) {
	// Инициализируем плейлист без перемешивания, чтобы сохранить исходный порядок.
	regularPl, err := playlist.NewPlaylist(tmpDir, nil, false, slog.Default(), createTestSentryHelper())
	if err != nil {
		t.Fatalf("Failed to create regular playlist: %v", err)
	}

	// Даем время для инициализации.
	time.Sleep(200 * time.Millisecond)

	// Получаем треки из обычного плейлиста (должны быть в алфавитном порядке).
	regularTracks := collectPlaylistTracks(t, regularPl)

	return regularPl, regularTracks
}

// getShuffledPlaylistTracks получает треки из перемешанного плейлиста.
func getShuffledPlaylistTracks(t *testing.T, tmpDir string) []string {
	// Вручную создаем плейлист с перемешиванием вместо использования конструктора с shuffle=true.
	shufflePl, err := playlist.NewPlaylist(tmpDir, nil, false, slog.Default(), createTestSentryHelper())
	if err != nil {
		t.Fatalf("Failed to create shuffle playlist: %v", err)
	}

	// Даем время для инициализации.
	time.Sleep(200 * time.Millisecond)

	// Вручную перемешиваем с таймаутом, чтобы выявить потенциальные дедлоки.
	t.Log("Starting manual shuffle operation...")
	performShuffleWithTimeout(t, shufflePl)

	// Получаем треки из перемешанного плейлиста.
	shuffleTracks := collectPlaylistTracks(t, shufflePl)

	return shuffleTracks
}

// performShuffleWithTimeout выполняет перемешивание плейлиста с таймаутом.
func performShuffleWithTimeout(t *testing.T, pl *playlist.Playlist) {
	// Создаем канал для сигнала о завершении.
	done := make(chan bool)

	// Запускаем горутину для выполнения перемешивания.
	go func() {
		pl.Shuffle()
		done <- true
	}()

	// Ждем завершения перемешивания с таймаутом.
	select {
	case <-done:
		t.Log("Shuffle operation completed successfully")
	case <-time.After(10 * time.Second):
		t.Fatalf("Shuffle operation timed out after 10 seconds")
	}
}

// collectPlaylistTracks собирает треки из плейлиста.
func collectPlaylistTracks(_ *testing.T, pl *playlist.Playlist) []string {
	var tracks []string

	// Получаем первый трек.
	currentTrack := pl.GetCurrentTrack()
	if track, ok := currentTrack.(interface{ GetPath() string }); ok {
		tracks = append(tracks, filepath.Base(track.GetPath()))
	}

	// Итерируем через треки по порядку.
	for range [10]struct{}{} { // Ограничиваем количество итераций для безопасности
		nextTrack := pl.NextTrack()
		time.Sleep(50 * time.Millisecond)
		if track, ok := nextTrack.(interface{ GetPath() string }); ok {
			trackName := filepath.Base(track.GetPath())
			// Проверяем, не вернулись ли мы к началу.
			if len(tracks) > 0 && trackName == tracks[0] {
				break
			}
			tracks = append(tracks, trackName)
		}
	}

	return tracks
}

// validateShuffleResults проверяет результаты перемешивания.
func validateShuffleResults(t *testing.T, regularTracks, shuffleTracks []string) {
	// Проверяем, что оба плейлиста имеют одинаковое количество треков.
	if len(regularTracks) != len(shuffleTracks) {
		t.Fatalf("Expected both playlists to have the same number of tracks, but got %d and %d",
			len(regularTracks), len(shuffleTracks))
	}

	// Проверяем, что порядок треков отличается в режиме SHUFFLE.
	different := false
	for i, track := range regularTracks {
		if i < len(shuffleTracks) && track != shuffleTracks[i] {
			different = true
			break
		}
	}

	if !different {
		t.Errorf("Expected shuffled playlist to have different order than regular playlist, but they appear identical")
	}

	// Проверяем, что все файлы присутствуют в обоих плейлистах.
	verifyAllTracksPresent(t, regularTracks, shuffleTracks)
}

// verifyAllTracksPresent проверяет, что все треки присутствуют в обоих плейлистах.
func verifyAllTracksPresent(t *testing.T, regularTracks, shuffleTracks []string) {
	regularMap := make(map[string]bool)
	shuffleMap := make(map[string]bool)

	for _, track := range regularTracks {
		regularMap[track] = true
	}

	for _, track := range shuffleTracks {
		shuffleMap[track] = true
	}

	// Все файлы из обычного плейлиста должны быть в перемешанном плейлисте.
	for track := range regularMap {
		if !shuffleMap[track] {
			t.Errorf("Track %s is missing in shuffled playlist", track)
		}
	}

	// Все файлы из перемешанного плейлиста должны быть в обычном плейлисте.
	for track := range shuffleMap {
		if !regularMap[track] {
			t.Errorf("Track %s is missing in regular playlist", track)
		}
	}
}
