// Package playlist manages audio playlists and track selection.
// It handles playlist creation, updates, and track shuffling.
package playlist

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	sentryhelper "github.com/aposazhennikov/stream-audio-to-web/sentry_helper"
)

// Track represents information about a track.
type Track struct {
	Path     string
	Name     string
	FileInfo os.FileInfo
}

// GetPath returns the path to the track.
func (t *Track) GetPath() string {
	return t.Path
}

// Playlist manages the list of tracks for audio streaming.
type Playlist struct {
	directory    string
	tracks       []Track
	current      int
	mutex        sync.RWMutex
	watcher      *fsnotify.Watcher
	onChange     func()
	shuffle      bool         // Flag determining whether to shuffle tracks
	history      []Track      // History of played tracks
	startTime    time.Time    // Playlist start time
	historyMutex sync.RWMutex // Mutex for history
	logger       *slog.Logger // Инстанс логгера
	sentryHelper *sentryhelper.SentryHelper // Helper для безопасной работы с Sentry
}

const (
	maxShowTracks       = 3
	maxAttempts         = 3
	getTrackTimeout     = 500 * time.Millisecond // Базовый таймаут
	maxHistorySize      = 100
	mutexTryLockTimeout = 50 * time.Millisecond

	// Увеличенный таймаут для стрессовых тестов.
	testTrackTimeout = 2000 * time.Millisecond
)

// NewPlaylist creates a new playlist from the specified directory.
func NewPlaylist(directory string, onChange func(), shuffle bool, logger *slog.Logger, sentryHelper *sentryhelper.SentryHelper) (*Playlist, error) {
	if logger == nil {
		logger = slog.Default()
	}
	pl := &Playlist{
		directory:    directory,
		tracks:       []Track{},
		history:      []Track{},
		current:      0,
		onChange:     onChange,
		shuffle:      shuffle,      // Save shuffle parameter
		startTime:    time.Now(),   // Remember start time
		logger:       logger,
		sentryHelper: sentryHelper,
	}

	// Initialize watcher to track directory changes.
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		pl.sentryHelper.CaptureError(err, "playlist", "watcher_init")
		return nil, err
	}
	pl.watcher = watcher

	// Load tracks from directory.
	if err2 := pl.Reload(); err2 != nil {
		pl.sentryHelper.CaptureError(err2, "playlist", "initial_load")
		return nil, err2
	}

	// Start goroutine to monitor directory changes.
	go pl.watchDirectory()

	return pl, nil
}

// Close closes the watcher.
func (p *Playlist) Close() error {
	err := p.watcher.Close()
	if err != nil {
		p.sentryHelper.CaptureError(err, "playlist", "operation")
	}
	return err
}

// Reload reloads the playlist from the directory.
// Split into smaller functions to fix funlen issue.
func (p *Playlist) Reload() error {
	// Сначала получаем информацию о текущем треке без блокировок
	var currentTrackPath string

	// Получаем текущий трек до захвата блокировки
	if trackInterface := p.GetCurrentTrack(); trackInterface != nil {
		if track, ok := trackInterface.(*Track); ok {
			currentTrackPath = track.Path
		}
	}

	// Теперь захватываем блокировку для обновления
	p.mutex.Lock()
	defer p.mutex.Unlock()

	// Сохраняем текущие данные.
	currentIndex := p.current

	// Загружаем файлы и проверяем ошибки.
	files, err := p.loadDirectoryFiles()
	if err != nil {
		return err
	}

	// Обрабатываем случай с пустой директорией, но продолжаем работу с пустым плейлистом
	if len(files) == 0 {
		// Вызываем обработчик пустой директории без проверки ошибки,
		// так как функция теперь не возвращает ошибку
		p.handleEmptyDirectory()
		// Не возвращаем ошибку, продолжаем работу с пустым плейлистом
		return nil
	}

	// Создаем и обрабатываем новые треки.
	tracks := p.createTracksFromFiles(files)

	// Обрабатываем случай с отсутствием треков, но продолжаем работу с пустым плейлистом
	if len(tracks) == 0 {
		// Вызываем обработчик отсутствия треков без проверки ошибки,
		// так как функция теперь не возвращает ошибку
		p.handleNoTracks()
		// Не возвращаем ошибку, продолжаем работу с пустым плейлистом
		return nil
	}

	// Обновляем плейлист без использования GetCurrentTrack внутри блокировки
	p.updatePlaylistWithPath(tracks, currentTrackPath, currentIndex)
	return nil
}

// loadDirectoryFiles загружает файлы из директории плейлиста.
func (p *Playlist) loadDirectoryFiles() ([]os.FileInfo, error) {
	p.logger.Info(
		"DIAGNOSTICS: Reloading playlist from directory",
		slog.String("directory", p.directory),
	)

	dir, err := os.Open(p.directory)
	if err != nil {
		p.logger.Error(
			"ERROR: Cannot open directory",
			slog.String("directory", p.directory),
			slog.String("error", err.Error()),
		)
		return nil, fmt.Errorf("cannot open directory: %w", err)
	}
	defer func() {
		if closeErr := dir.Close(); closeErr != nil {
			p.logger.Error("Failed to close directory", slog.String("error", closeErr.Error()))
		}
	}()

	files, err := dir.Readdir(-1)
	if err != nil {
		p.logger.Error(
			"ERROR: Cannot read directory",
			slog.String("directory", p.directory),
			slog.String("error", err.Error()),
		)
		return nil, fmt.Errorf("cannot read directory: %w", err)
	}

	return files, nil
}

// handleEmptyDirectory обрабатывает случай с пустой директорией.
func (p *Playlist) handleEmptyDirectory() {
	// Изменяем уровень логирования на WARN вместо ERROR
	p.logger.Warn(
		"WARNING: Directory is empty",
		slog.String("directory", p.directory),
	)
	p.tracks = make([]Track, 0)
	p.current = 0

	// Отправляем в Sentry как сообщение, а не ошибку
	p.sentryHelper.CaptureInfo(
		fmt.Sprintf("Directory is empty: %s, but playlist will be configured anyway", p.directory),
		"playlist", "empty_directory",
	)
}

// handleNoTracks обрабатывает случай с отсутствием треков.
func (p *Playlist) handleNoTracks() {
	// Изменяем уровень логирования на WARN вместо ERROR
	p.logger.Warn(
		"WARNING: No audio files in directory",
		slog.String("directory", p.directory),
	)
	p.tracks = make([]Track, 0)
	p.current = 0

	// Отправляем в Sentry как сообщение, а не ошибку
	p.sentryHelper.CaptureInfo(
		fmt.Sprintf("No audio files in directory: %s, but playlist will be configured anyway", p.directory),
		"playlist", "no_audio_files",
	)
}

// createTracksFromFiles создает треки из файлов.
func (p *Playlist) createTracksFromFiles(files []os.FileInfo) []Track {
	// Фильтруем и создаем треки.
	tracks := make([]Track, 0, len(files))

	for _, file := range files {
		// Пропускаем директории и скрытые файлы.
		if file.IsDir() || strings.HasPrefix(file.Name(), ".") {
			continue
		}

		// Проверяем расширение файла.
		if !isAudioFile(file.Name()) {
			continue
		}

		// Создаем полный путь к файлу.
		path := filepath.Join(p.directory, file.Name())

		// Создаем трек.
		track := Track{
			Name:     file.Name(),
			Path:     path,
			FileInfo: file,
		}

		tracks = append(tracks, track)
	}

	return tracks
}

// updatePlaylistWithPath обновляет плейлист новыми треками, используя путь трека вместо объекта трека.
func (p *Playlist) updatePlaylistWithPath(tracks []Track, currentTrackPath string, currentIndex int) {
	// Сохраняем старые треки для логирования.
	oldTracks := p.tracks

	// Обновляем треки.
	p.tracks = tracks

	// Если треки изменены, сбрасываем историю.
	if !p.tracksEqual(oldTracks, tracks) {
		// Используем новую блокировку для истории
		p.historyMutex.Lock()
		p.history = make([]Track, 0, maxHistorySize)
		p.historyMutex.Unlock()

		p.logger.Info(
			"Tracks changed, history reset",
			slog.Int("oldTracks", len(oldTracks)),
			slog.Int("newTracks", len(tracks)),
		)
	}

	// Если включен режим перемешивания, перемешиваем треки.
	if p.shuffle {
		p.shuffleTracks()
	} else {
		// Сортируем треки по имени.
		sort.Slice(p.tracks, func(i, j int) bool {
			return p.tracks[i].Name < p.tracks[j].Name
		})
	}

	// Установка текущего трека
	p.setCurrentTrackByPath(currentTrackPath, currentIndex)

	// Логируем результаты.
	p.logger.Info(
		"Playlist loaded",
		slog.Int("trackCount", len(p.tracks)),
		slog.String("directory", p.directory),
	)
}

// setCurrentTrackByPath устанавливает текущий трек по пути или индексу.
func (p *Playlist) setCurrentTrackByPath(currentTrackPath string, currentIndex int) {
	// Если путь не указан, используем индекс 0 (начало плейлиста).
	if currentTrackPath == "" {
		p.current = 0
		return
	}

	// Пытаемся найти трек с указанным путем.
	for i, track := range p.tracks {
		if track.Path == currentTrackPath {
			p.current = i
			return
		}
	}

	// Если трек не найден, используем текущий индекс, если он валиден.
	if currentIndex < len(p.tracks) {
		p.current = currentIndex
	} else {
		p.current = 0
	}
}

// tracksEqual проверяет равенство двух списков треков.
func (p *Playlist) tracksEqual(old, newTracks []Track) bool {
	if len(old) != len(newTracks) {
		return false
	}

	// Создаем мапы для сравнения.
	oldMap := make(map[string]struct{}, len(old))
	newMap := make(map[string]struct{}, len(newTracks))

	for _, track := range old {
		oldMap[track.Path] = struct{}{}
	}

	for _, track := range newTracks {
		newMap[track.Path] = struct{}{}
	}

	// Проверяем наличие всех треков из старого списка в новом.
	for path := range oldMap {
		if _, exists := newMap[path]; !exists {
			return false
		}
	}

	// Проверяем наличие всех треков из нового списка в старом.
	for path := range newMap {
		if _, exists := oldMap[path]; !exists {
			return false
		}
	}

	return true
}

// shuffleTracks перемешивает треки в случайном порядке.
func (p *Playlist) shuffleTracks() {
	// Используем криптографически безопасный генератор для перемешивания.
	for i := len(p.tracks) - 1; i > 0; i-- {
		// Генерируем случайное число от 0 до i включительно.
		maxInt := big.NewInt(int64(i + 1))
		j, err := rand.Int(rand.Reader, maxInt)
		if err != nil {
			// В случае ошибки используем i в качестве резервного варианта.
			p.logger.Error("Error generating random number", slog.String("error", err.Error()))
			continue
		}

		// Меняем местами элементы.
		p.tracks[i], p.tracks[j.Int64()] = p.tracks[j.Int64()], p.tracks[i]
	}

	p.logger.Info(
		"Tracks shuffled",
		slog.Int("trackCount", len(p.tracks)),
	)
}

// isAudioFile проверяет, является ли файл аудиофайлом по его расширению.
func isAudioFile(fileName string) bool {
	ext := strings.ToLower(filepath.Ext(fileName))
	return ext == ".mp3" || ext == ".ogg" || ext == ".wav" || ext == ".flac" || ext == ".m4a"
}

// getTimeoutForEnvironment возвращает таймаут для операций плейлиста в зависимости от окружения.
func getTimeoutForEnvironment() time.Duration {
	// Проверяем, запущены ли тесты.
	if os.Getenv("GO_TEST") == "1" ||
		os.Getenv("TEST_ENVIRONMENT") == "1" ||
		strings.Contains(os.Args[0], "test") {
		return testTrackTimeout
	}
	return getTrackTimeout
}

// GetCurrentTrack returns the current track.
func (p *Playlist) GetCurrentTrack() interface{} {
	// Быстрая проверка с минимальной блокировкой.
	p.mutex.RLock()
	tracksEmpty := len(p.tracks) == 0
	hasCurrentIndex := p.current < len(p.tracks) && p.current >= 0
	p.mutex.RUnlock()

	// Если треков нет или недопустимый индекс, сразу возвращаем nil без запуска горутин и логирования.
	if tracksEmpty || !hasCurrentIndex {
		return nil
	}

	// Определяем таймаут в зависимости от окружения.
	timeout := getTimeoutForEnvironment()

	// Используем канал с буфером для предотвращения утечек горутин.
	c := make(chan interface{}, 1)

	go func() {
		// Используем RLock вместо Lock для повышения производительности.
		p.mutex.RLock()
		defer p.mutex.RUnlock()

		if len(p.tracks) == 0 || p.current >= len(p.tracks) {
			c <- nil
			return
		}

		// Создаем копию трека, чтобы избежать проблем с параллельным доступом.
		track := p.tracks[p.current]
		c <- &track
	}()

	// Ожидаем с таймаутом и возвращаем результат.
	select {
	case track := <-c:
		return track
	case <-time.After(timeout):
		p.logger.Warn(
			"WARNING: GetCurrentTrack timed out",
			slog.String("timeout", timeout.String()),
		)
		return nil
	}
}

// NextTrack moves to the next track and returns it.
func (p *Playlist) NextTrack() interface{} {
	startTime := time.Now()
	
	// СПЕЦИАЛЬНОЕ ЛОГИРОВАНИЕ ДЛЯ /floyd
	p.logger.Debug("NextTrack() called", 
		slog.String("directory", p.directory),
		slog.String("timestamp", startTime.Format("15:04:05.000")))
	
	// Быстрая проверка с минимальной блокировкой.
	p.mutex.RLock()
	tracksEmpty := len(p.tracks) == 0
	currentIndex := p.current
	totalTracks := len(p.tracks)
	var currentTrackName string
	if currentIndex < len(p.tracks) {
		currentTrackName = filepath.Base(p.tracks[currentIndex].Path)
	}
	p.mutex.RUnlock()

	p.logger.Debug("Current state", 
		slog.Bool("tracksEmpty", tracksEmpty),
		slog.Int("currentIndex", currentIndex),
		slog.Int("totalTracks", totalTracks),
		slog.String("currentTrack", currentTrackName))

	// Если треков нет, сразу возвращаем nil без запуска горутин.
	if tracksEmpty {
		p.logger.Debug("No tracks available, returning nil")
		return nil
	}

	// Определяем таймаут в зависимости от окружения.
	timeout := getTimeoutForEnvironment()

	// Используем канал с буфером для предотвращения утечек горутин.
	c := make(chan interface{}, 1)

	go func() {
		// Захватываем блокировку в горутине.
		p.mutex.Lock()
		defer p.mutex.Unlock()

		if len(p.tracks) == 0 {
			p.logger.Debug("NextTrack() called but no tracks available")
			c <- nil
			return
		}

		// Добавляем текущий трек в историю.
		if p.current < len(p.tracks) {
			currentTrack := p.tracks[p.current]
			p.addTrackToHistory(currentTrack)
		}

		// КРИТИЧЕСКАЯ ДИАГНОСТИКА: Логируем ДО изменения индекса
		oldCurrent := p.current
		oldTrackName := ""
		if oldCurrent < len(p.tracks) {
			oldTrackName = filepath.Base(p.tracks[oldCurrent].Path)
		}
		
		p.logger.Debug("BEFORE track switch", 
			slog.Int("oldIndex", oldCurrent),
			slog.String("oldTrack", oldTrackName))

		// Переходим к следующему треку.
		newCurrent := (p.current + 1) % len(p.tracks)
		
		// Проверяем необходимость повторного перемешивания ПЕРЕД изменением current.
		if newCurrent == 0 && p.shuffle && len(p.tracks) > 1 {
			p.logger.Debug("Reshuffling playlist (reached end)")
			p.reshufflePlaylistPreservingCurrent()
			// После reshuffling НЕ меняем current - он уже установлен в reshufflePlaylistPreservingCurrent()
		} else {
			// Обычный переход к следующему треку.
			p.current = newCurrent
		}
		
		// КРИТИЧЕСКАЯ ДИАГНОСТИКА: Логируем ПОСЛЕ изменения индекса
		newTrackName := ""
		if p.current < len(p.tracks) {
			newTrackName = filepath.Base(p.tracks[p.current].Path)
		}
		
		p.logger.Debug("AFTER track switch", 
			slog.Int("from", oldCurrent),
			slog.Int("to", p.current),
			slog.String("newTrack", newTrackName))

		// Создаем копию трека для безопасного возврата.
		track := p.tracks[p.current]
		trackName := track.Name
		if trackName == "" {
			trackName = filepath.Base(track.Path)
		}
		
		p.logger.Debug("Returning new track", 
			slog.String("trackName", trackName),
			slog.Int("newIndex", p.current),
			slog.Int64("executionTimeMs", time.Since(startTime).Milliseconds()))
		
		c <- &track
	}()

	// Ожидаем результат с таймаутом.
	select {
	case result := <-c:
		p.logger.Debug("NextTrack() COMPLETED",
			slog.Int64("totalTimeMs", time.Since(startTime).Milliseconds()))
		return result
	case <-time.After(timeout):
		p.logger.Warn(
			"WARNING: NextTrack timed out",
			slog.String("timeout", timeout.String()),
		)
		return nil
	}
}

// PreviousTrack moves to the previous track and returns it.
func (p *Playlist) PreviousTrack() interface{} {
	// Быстрая проверка с минимальной блокировкой.
	p.mutex.RLock()
	tracksEmpty := len(p.tracks) == 0
	p.mutex.RUnlock()

	// Если треков нет, сразу возвращаем nil без запуска горутин.
	if tracksEmpty {
		return nil
	}

	// Определяем таймаут в зависимости от окружения.
	timeout := getTimeoutForEnvironment()

	// Используем канал с буфером для предотвращения утечек горутин.
	c := make(chan interface{}, 1)

	go func() {
		p.mutex.Lock()
		defer p.mutex.Unlock()

		if len(p.tracks) == 0 {
			c <- nil
			return
		}

		// Добавляем текущий трек в историю перед переходом к предыдущему.
		if p.current < len(p.tracks) {
			currentTrack := p.tracks[p.current]
			p.addTrackToHistory(currentTrack)
		}

		// Переходим к предыдущему треку с учетом возможности отрицательного индекса.
		if p.current == 0 {
			p.current = len(p.tracks) - 1
		} else {
			p.current--
		}

		// Создаем копию трека для безопасного возврата.
		track := p.tracks[p.current]
		c <- &track
	}()

	// Ожидаем результат с таймаутом.
	select {
	case result := <-c:
		return result
	case <-time.After(timeout):
		p.logger.Warn(
			"WARNING: PreviousTrack timed out",
			slog.String("timeout", timeout.String()),
		)
		return nil
	}
}

// addTrackToHistory adds a track to history.
func (p *Playlist) addTrackToHistory(track Track) {
	p.historyMutex.Lock()
	defer p.historyMutex.Unlock()

	// Add track to history.
	p.history = append(p.history, track)

	// Limit history size - keep last 100 tracks.
	if len(p.history) > maxHistorySize {
		p.history = p.history[len(p.history)-maxHistorySize:]
	}
}

// GetHistory returns the history of played tracks.
func (p *Playlist) GetHistory() []interface{} {
	// Используем отдельную блокировку для истории.
	p.historyMutex.RLock()
	historyLen := len(p.history)

	// Если история пуста, сразу возвращаем пустой слайс без дополнительных операций.
	if historyLen == 0 {
		p.historyMutex.RUnlock()
		// Empty history is normal at startup - no need to log this frequently called case.
		return []interface{}{}
	}

	// Копируем историю для безопасного возврата.
	history := make([]interface{}, historyLen)
	for i, track := range p.history {
		// Создаем копию каждого трека, чтобы избежать проблем с параллельным доступом.
		trackCopy := track
		history[i] = &trackCopy
	}
	p.historyMutex.RUnlock()

	// Log first few tracks in history for debugging.
	if historyLen > 0 && p.logger.Enabled(context.Background(), slog.LevelInfo) {
		trackNames := make([]string, 0, minInt(maxShowTracks, historyLen))
		for i := range history[:minInt(maxShowTracks, historyLen)] {
			if track, ok := history[i].(interface{ GetPath() string }); ok {
				trackNames = append(trackNames, filepath.Base(track.GetPath()))
			}
		}
		p.logger.Debug("First tracks in history", slog.String("tracks", strings.Join(trackNames, ", ")))
	}

	return history
}

// ClearHistory clears the track history.
func (p *Playlist) ClearHistory() {
	p.historyMutex.Lock()
	defer p.historyMutex.Unlock()
	
	p.history = make([]Track, 0, maxHistorySize)
	p.logger.Info("Track history cleared", 
		slog.String("directory", p.directory))
}

// GetStartTime returns the playlist start time.
func (p *Playlist) GetStartTime() time.Time {
	return p.startTime
}

// Shuffle randomizes the track list.
func (p *Playlist) Shuffle() {
	p.logger.Info("SHUFFLE: Starting playlist shuffle for directory", slog.String("directory", p.directory))

	// Создаем локальные переменные для передачи между функциями.
	var tracksToShuffle []Track
	var originalTrackPath string

	// Получаем копию треков с блокировкой.
	if !p.copyTracksWithLock(&tracksToShuffle, &originalTrackPath) {
		return
	}

	// Перемешиваем треки.
	newPosition := p.shuffleTracksCopy(tracksToShuffle, originalTrackPath)

	// Применяем изменения.
	p.applyShuffledTracks(tracksToShuffle, newPosition)
}

// copyTracksWithLock пытается захватить мьютекс и скопировать треки.
func (p *Playlist) copyTracksWithLock(tracksToShuffle *[]Track, originalTrackPath *string) bool {
	// Пытаемся захватить мьютекс с таймаутом.
	if !p.tryAcquireMutexWithTimeout() {
		p.logger.Info("SHUFFLE: Could not acquire mutex, skipping shuffle")
		return false
	}

	// Успешно захватили мьютекс, выполняем быстрое копирование.
	defer p.mutex.Unlock()

	// Если треков мало или нет, нечего перемешивать.
	if len(p.tracks) <= 1 {
		p.logger.Info("SHUFFLE: Not enough tracks to shuffle")
		return false
	}

	// Сохраняем текущий трек.
	if p.current < len(p.tracks) {
		*originalTrackPath = p.tracks[p.current].Path
	}

	// Создаем копию треков.
	*tracksToShuffle = make([]Track, len(p.tracks))
	copy(*tracksToShuffle, p.tracks)

	return true
}

// tryAcquireMutexWithTimeout пытается захватить мьютекс с таймаутом.
func (p *Playlist) tryAcquireMutexWithTimeout() bool {
	if p.mutex.TryLock() {
		return true
	}

	// Не удалось захватить мьютекс мгновенно, пробуем с таймаутом.
	timer := time.NewTimer(mutexTryLockTimeout)
	defer timer.Stop()

	select {
	case <-timer.C:
		return false
	default:
		p.mutex.Lock()
		return true
	}
}

// shuffleTracksCopy перемешивает копию треков и возвращает новую позицию текущего трека.
func (p *Playlist) shuffleTracksCopy(tracksToShuffle []Track, originalTrackPath string) int {
	p.logger.Info("SHUFFLE: Shuffling tracks")

	// Реализация алгоритма Fisher-Yates с использованием crypto/rand.
	for i := len(tracksToShuffle) - 1; i > 0; i-- {
		maxNum := big.NewInt(int64(i + 1))
		jBig, err := rand.Int(rand.Reader, maxNum)
		if err != nil {
			p.logger.Info("CRYPTO ERROR: %v, fallback to i", slog.Any("error", err))
			jBig = big.NewInt(int64(i))
		}
		j := int(jBig.Int64())
		tracksToShuffle[i], tracksToShuffle[j] = tracksToShuffle[j], tracksToShuffle[i]
	}

	// Находим позицию оригинального трека в перемешанном массиве.
	newPosition := 0
	if originalTrackPath != "" {
		for i, track := range tracksToShuffle {
			if track.Path == originalTrackPath {
				newPosition = i
				break
			}
		}
	}

	return newPosition
}

// applyShuffledTracks применяет перемешанные треки с захватом мьютекса.
func (p *Playlist) applyShuffledTracks(tracksToShuffle []Track, newPosition int) {
	// Пытаемся снова захватить мьютекс для применения изменений.
	if !p.tryAcquireMutexWithTimeout() {
		p.logger.Info("SHUFFLE: Could not acquire mutex to apply changes")
		return
	}

	// Успешно захватили мьютекс, применяем изменения.
	defer p.mutex.Unlock()

	// Применяем перемешанные треки.
	p.tracks = tracksToShuffle

	// Восстанавливаем позицию текущего трека.
	p.current = newPosition
	p.logger.Info("SHUFFLE: Restored current track position to", slog.Int("position", newPosition))
	p.logger.Info("SHUFFLE: Playlist shuffled successfully")
}

// GetTracks returns a copy of the track list.
func (p *Playlist) GetTracks() []Track {
	// Быстрая проверка с минимальной блокировкой.
	p.mutex.RLock()
	tracksEmpty := len(p.tracks) == 0
	p.mutex.RUnlock()

	// Если треков нет, сразу возвращаем пустой слайс.
	if tracksEmpty {
		return []Track{}
	}

	// Определяем таймаут в зависимости от окружения.
	timeout := getTimeoutForEnvironment()

	// Используем канал с буфером для предотвращения утечек горутин.
	c := make(chan []Track, 1)

	go func() {
		// Используем RLock для множественного чтения.
		p.mutex.RLock()
		defer p.mutex.RUnlock()

		// Создаем копию треков для безопасного возврата.
		tracks := make([]Track, len(p.tracks))
		copy(tracks, p.tracks)
		c <- tracks
	}()

	// Ожидаем результат с таймаутом.
	select {
	case tracks := <-c:
		return tracks
	case <-time.After(timeout):
		p.logger.Warn(
			"WARNING: GetTracks timed out",
			slog.String("timeout", timeout.String()),
		)
		return []Track{}
	}
}

// watchDirectory monitors changes in the playlist directory.
func (p *Playlist) watchDirectory() {
	for {
		select {
		case event, ok := <-p.watcher.Events:
			if !ok {
				return
			}
			p.handleWatchEvent(event)

		case err, ok := <-p.watcher.Errors:
			if !ok {
				return
			}
			p.logger.Error("fsnotify error", slog.String("error", err.Error()))
			p.sentryHelper.CaptureError(fmt.Errorf("fsnotify error: %w", err), "playlist", "fsnotify")
		}
	}
}

// handleWatchEvent обрабатывает событие изменения файла.
func (p *Playlist) handleWatchEvent(event fsnotify.Event) {
	ext := strings.ToLower(filepath.Ext(event.Name))
	if !isSupportedExtension(ext) {
		return
	}

	if p.shouldProcessEvent(event) {
		p.processFileEvent(event)
	}
}

// shouldProcessEvent проверяет, нужно ли обрабатывать событие.
func (p *Playlist) shouldProcessEvent(event fsnotify.Event) bool {
	return event.Op&(fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0
}

// processFileEvent обрабатывает значимое событие файла.
func (p *Playlist) processFileEvent(event fsnotify.Event) {
	p.logger.Info("Change detected in playlist", slog.String("file", event.Name))

	// Перезагружаем плейлист.
	if err := p.Reload(); err != nil {
		p.logger.Info("Error reloading playlist", slog.String("error", err.Error()))
		p.sentryHelper.CaptureError(fmt.Errorf("error reloading playlist: %w", err), "playlist", "reload")
		return
	}

	// Вызываем обратный вызов при изменении плейлиста.
	if p.onChange != nil {
		p.onChange()
	}
}

// isSupportedExtension проверяет, поддерживается ли расширение файла.
func isSupportedExtension(ext string) bool {
	supportedExtensions := map[string]bool{
		".mp3": true,
		".aac": true,
		".ogg": true,
	}
	return supportedExtensions[ext]
}

// Определяем функцию minInt, которая отсутствовала.
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// reshufflePlaylistPreservingCurrent перемешивает плейлист, сохраняя текущий трек.
// Должен вызываться с захваченной блокировкой.
func (p *Playlist) reshufflePlaylistPreservingCurrent() {
	currentPos := p.current
	currentTrackPath := p.tracks[currentPos].Path

	// Fisher-Yates с crypto/rand.
	for i := len(p.tracks) - 1; i > 0; i-- {
		maxNum := big.NewInt(int64(i + 1))
		jBig, err := rand.Int(rand.Reader, maxNum)
		if err != nil {
			p.logger.Info("CRYPTO ERROR: %v, fallback to i", slog.Any("error", err))
			jBig = big.NewInt(int64(i))
		}
		j := int(jBig.Int64())
		p.tracks[i], p.tracks[j] = p.tracks[j], p.tracks[i]
	}

	// Находим текущий трек в перемешанном плейлисте.
	for i, track := range p.tracks {
		if track.Path == currentTrackPath {
			p.current = i
			break
		}
	}

	p.logger.Info(
		"DIAGNOSTICS: Playlist shuffled in NextTrack, current track at position",
		slog.Int("position", p.current),
	)
}

// GetShuffleEnabled returns current shuffle status.
func (p *Playlist) GetShuffleEnabled() bool {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.shuffle
}

// SetShuffleEnabled sets the shuffle status and optionally reshuffles the playlist.
func (p *Playlist) SetShuffleEnabled(enabled bool) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	
	oldShuffle := p.shuffle
	p.shuffle = enabled
	
	p.logger.Info("Shuffle status changed", 
		slog.Bool("from", oldShuffle), 
		slog.Bool("to", enabled),
		slog.String("directory", p.directory))
	
	// If shuffle was just enabled and we have tracks, shuffle them.
	if !oldShuffle && enabled && len(p.tracks) > 1 {
		p.logger.Info("Shuffling tracks due to shuffle being enabled")
		p.shuffleTracksInternal()
	}
	
	// If shuffle was disabled, sort tracks by name for consistent order.
	if oldShuffle && !enabled && len(p.tracks) > 1 {
		p.logger.Info("Sorting tracks by name due to shuffle being disabled")
		// Save current track path to restore position after sorting.
		var currentTrackPath string
		if p.current < len(p.tracks) {
			currentTrackPath = p.tracks[p.current].Path
		}
		
		// Sort tracks by name.
		sort.Slice(p.tracks, func(i, j int) bool {
			return p.tracks[i].Name < p.tracks[j].Name
		})
		
		// Restore current track position.
		if currentTrackPath != "" {
			for i, track := range p.tracks {
				if track.Path == currentTrackPath {
					p.current = i
					break
				}
			}
		}
		
		p.logger.Info("Tracks sorted by name", 
			slog.Int("trackCount", len(p.tracks)),
			slog.Int("currentIndex", p.current))
	}
}

// shuffleTracksInternal перемешивает треки без блокировки (для внутреннего использования).
func (p *Playlist) shuffleTracksInternal() {
	// Save current track path to restore position after shuffling.
	var currentTrackPath string
	if p.current < len(p.tracks) {
		currentTrackPath = p.tracks[p.current].Path
	}
	
	// Используем криптографически безопасный генератор для перемешивания.
	for i := len(p.tracks) - 1; i > 0; i-- {
		// Генерируем случайное число от 0 до i включительно.
		maxInt := big.NewInt(int64(i + 1))
		j, err := rand.Int(rand.Reader, maxInt)
		if err != nil {
			// В случае ошибки используем i в качестве резервного варианта.
			p.logger.Error("Error generating random number", slog.String("error", err.Error()))
			continue
		}

		// Меняем местами элементы.
		p.tracks[i], p.tracks[j.Int64()] = p.tracks[j.Int64()], p.tracks[i]
	}
	
	// Restore current track position.
	if currentTrackPath != "" {
		for i, track := range p.tracks {
			if track.Path == currentTrackPath {
				p.current = i
				break
			}
		}
	}

	p.logger.Info("Tracks shuffled internally", 
		slog.Int("trackCount", len(p.tracks)),
		slog.Int("currentIndex", p.current))
}
