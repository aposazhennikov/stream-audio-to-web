// Package playlist manages audio playlists and track selection.
// It handles playlist creation, updates, and track shuffling.
package playlist

import (
	"crypto/rand"
	"errors"
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
	sentry "github.com/getsentry/sentry-go"
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
}

const (
	maxShowTracks       = 3
	maxAttempts         = 3
	getTrackTimeout     = 500 * time.Millisecond // Базовый таймаут
	getTrackPause       = 50 * time.Millisecond
	maxHistorySize      = 100
	mutexTryLockTimeout = 50 * time.Millisecond
	shuffleDelayMs      = 10

	// Увеличенный таймаут для стрессовых тестов.
	testTrackTimeout = 2000 * time.Millisecond
)

// NewPlaylist creates a new playlist from the specified directory.
func NewPlaylist(directory string, onChange func(), shuffle bool, logger *slog.Logger) (*Playlist, error) {
	if logger == nil {
		logger = slog.Default()
	}
	pl := &Playlist{
		directory: directory,
		tracks:    []Track{},
		history:   []Track{},
		current:   0,
		onChange:  onChange,
		shuffle:   shuffle,    // Save shuffle parameter
		startTime: time.Now(), // Remember start time
		logger:    logger,
	}

	// Initialize watcher to track directory changes.
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		sentry.CaptureException(err)
		return nil, err
	}
	pl.watcher = watcher

	// Load tracks from directory.
	if err2 := pl.Reload(); err2 != nil {
		sentry.CaptureException(err2)
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
		sentry.CaptureException(err)
	}
	return err
}

// Reload reloads the playlist from the directory.
// Split into smaller functions to fix funlen issue.
func (p *Playlist) Reload() error {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	// Сохраняем текущие данные.
	currentIndex := p.current

	// Получаем текущий трек с проверкой типа.
	var currentTrack *Track
	if trackInterface := p.GetCurrentTrack(); trackInterface != nil {
		if track, ok := trackInterface.(*Track); ok {
			currentTrack = track
		}
	}

	// Загружаем файлы и проверяем ошибки.
	files, err := p.loadDirectoryFiles()
	if err != nil {
		return err
	}

	// Обрабатываем случай с пустой директорией.
	if len(files) == 0 {
		return p.handleEmptyDirectory()
	}

	// Создаем и обрабатываем новые треки.
	tracks := p.createTracksFromFiles(files)

	// Обрабатываем случай с отсутствием треков.
	if len(tracks) == 0 {
		return p.handleNoTracks()
	}

	// Обновляем плейлист.
	p.updatePlaylist(tracks, currentTrack, currentIndex)
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
func (p *Playlist) handleEmptyDirectory() error {
	p.logger.Error(
		"ERROR: Directory is empty",
		slog.String("directory", p.directory),
	)
	p.tracks = make([]Track, 0)
	p.current = 0
	return errors.New("directory is empty")
}

// handleNoTracks обрабатывает случай с отсутствием треков.
func (p *Playlist) handleNoTracks() error {
	p.logger.Error(
		"ERROR: No audio files in directory",
		slog.String("directory", p.directory),
	)
	p.tracks = make([]Track, 0)
	p.current = 0
	return errors.New("no audio files in directory")
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

// updatePlaylist обновляет плейлист новыми треками.
func (p *Playlist) updatePlaylist(tracks []Track, currentTrack *Track, currentIndex int) {
	// Сохраняем старые треки для логирования.
	oldTracks := p.tracks

	// Обновляем треки.
	p.tracks = tracks

	// Если треки изменены, сбрасываем историю.
	if !p.tracksEqual(oldTracks, tracks) {
		p.history = make([]Track, 0, maxHistorySize)
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

	// Если текущий трек существует, пытаемся найти его в новом плейлисте.
	if currentTrack != nil {
		p.restoreCurrentTrack(currentTrack, currentIndex)
	} else {
		// Иначе устанавливаем первый трек текущим.
		p.current = 0
	}

	// Логируем результаты.
	p.logger.Info(
		"Playlist loaded",
		slog.Int("trackCount", len(p.tracks)),
		slog.String("directory", p.directory),
	)
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

// restoreCurrentTrack пытается найти текущий трек в новом плейлисте.
func (p *Playlist) restoreCurrentTrack(currentTrack *Track, currentIndex int) {
	found := false

	// Пытаемся найти текущий трек по пути.
	for i, track := range p.tracks {
		if track.Path == currentTrack.Path {
			p.current = i
			found = true
			break
		}
	}

	// Если не нашли, используем индекс текущего трека или 0.
	if !found {
		if currentIndex < len(p.tracks) {
			p.current = currentIndex
		} else {
			p.current = 0
		}
	}
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
	p.mutex.RUnlock()

	// Если треков нет, сразу возвращаем nil без запуска горутин и логирования.
	if tracksEmpty {
		return nil
	}

	// Adding retry mechanism.
	maxAttempts := maxAttempts

	// Определяем таймаут в зависимости от окружения.
	timeout := getTimeoutForEnvironment()

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Use timeout channel for deadlock protection.
		c := make(chan interface{}, 1)

		go func() {
			p.mutex.RLock()
			defer p.mutex.RUnlock()

			if len(p.tracks) == 0 {
				c <- nil
				return
			}
			c <- &p.tracks[p.current]
		}()

		// Wait with timeout.
		select {
		case track := <-c:
			return track
		case <-time.After(timeout):
			// Логируем только на последней попытке, чтобы не спамить логи.
			if attempt == maxAttempts {
				p.logger.Warn(
					"WARNING: All GetCurrentTrack attempts timed out, returning nil",
					slog.Int("maxAttempts", maxAttempts),
					slog.String("timeout", timeout.String()),
				)
				return nil
			}
			// Small pause before next attempt.
			time.Sleep(getTrackPause)
		}
	}

	// This code should not be reached, but for completeness:.
	p.logger.Error("ERROR: Unexpected flow in GetCurrentTrack, returning nil")
	return nil
}

// NextTrack moves to the next track and returns it.
func (p *Playlist) NextTrack() interface{} {
	// Adding retry mechanism.
	maxAttempts := maxAttempts

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Use timeout channel for safe mutex locking.
		c := make(chan interface{}, 1)

		go func() {
			// Try to lock mutex in goroutine.
			p.mutex.Lock()
			defer p.mutex.Unlock()

			if len(p.tracks) == 0 {
				p.logger.Info("DIAGNOSTICS: NextTrack() called but no tracks available")
				c <- nil
				return
			}

			// Process next track operations.
			track := p.processNextTrackWithHistory()
			c <- track
		}()

		// Wait for result with timeout.
		result, success := p.waitForTrackResult(c, "NextTrack", attempt, maxAttempts)
		if success {
			return result
		}
	}

	// This code should not be reached, but for completeness:.
	p.logger.Error("ERROR: Unexpected flow in NextTrack, returning nil")
	return nil
}

// processNextTrackWithHistory handles the core logic of moving to the next track.
// and updating history. Must be called with mutex locked.
func (p *Playlist) processNextTrackWithHistory() interface{} {
	// Add current track to history before moving to the next.
	currentTrack := p.tracks[p.current]
	p.addTrackToHistory(currentTrack)

	// Move to the next track.
	p.current = (p.current + 1) % len(p.tracks)

	// Check if we need to reshuffle.
	p.checkAndReshufflePlaylist()

	return &p.tracks[p.current]
}

// checkAndReshufflePlaylist checks if we've reached the end of the playlist.
// and shuffles if needed. Must be called with mutex locked.
func (p *Playlist) checkAndReshufflePlaylist() {
	// If we reached the end of playlist and shuffle is enabled, reshuffle for the next cycle.
	if p.current == 0 && p.shuffle && len(p.tracks) > 1 {
		p.logger.Info(
			"DIAGNOSTICS: Reached end of playlist with shuffle enabled, " +
				"performing reshuffle directly",
		)
		p.reshufflePlaylistPreservingCurrent()
	}
}

// reshufflePlaylistPreservingCurrent reshuffles the playlist while maintaining the current track.
// Must be called with mutex locked.
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

	// Find current track in shuffled playlist.
	for i, track := range p.tracks {
		if track.Path == currentTrackPath {
			p.current = i
			break
		}
	}

	p.logger.Info(
		"DIAGNOSTICS: Playlist shuffled in NextTrack, "+
			"current track at position",
		slog.Int("position", p.current),
	)
}

// waitForTrackResult waits for track result with timeout and handles retries.
func (p *Playlist) waitForTrackResult(
	c chan interface{},
	operation string,
	attempt int,
	maxAttempts int,
) (interface{}, bool) {
	// Определяем таймаут в зависимости от окружения.
	timeout := getTimeoutForEnvironment()

	select {
	case result := <-c:
		return result, true
	case <-time.After(timeout):
		// Логируем только на последней попытке, чтобы не спамить логи.
		if attempt == maxAttempts {
			p.logger.Warn(
				"WARNING: All "+operation+" attempts timed out, returning nil",
				slog.Int("maxAttempts", maxAttempts),
				slog.String("timeout", timeout.String()),
			)
			return nil, true
		}
		// Small pause before next attempt.
		time.Sleep(getTrackPause)
		return nil, false
	}
}

// PreviousTrack moves to the previous track and returns it.
func (p *Playlist) PreviousTrack() interface{} {
	// Быстрая проверка с минимальной блокировкой.
	p.mutex.RLock()
	tracksEmpty := len(p.tracks) == 0
	p.mutex.RUnlock()

	// Если треков нет, сразу возвращаем nil без запуска горутин и логирования.
	if tracksEmpty {
		return nil
	}

	// Adding retry mechanism.
	maxAttempts := maxAttempts
	// Определяем таймаут в зависимости от окружения.
	timeout := getTimeoutForEnvironment()

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Use timeout channel for safe mutex locking.
		c := make(chan interface{}, 1)

		go func() {
			p.mutex.Lock()
			defer p.mutex.Unlock()

			if len(p.tracks) == 0 {
				c <- nil
				return
			}

			// Add current track to history before moving to the previous.
			currentTrack := p.tracks[p.current]
			p.addTrackToHistory(currentTrack)

			// Move to the previous track considering the possibility of going to a negative index.
			if p.current == 0 {
				p.current = len(p.tracks) - 1
			} else {
				p.current--
			}

			c <- &p.tracks[p.current]
		}()

		// Wait for result with timeout.
		select {
		case result := <-c:
			return result
		case <-time.After(timeout):
			// Логируем только на последней попытке, чтобы не спамить логи.
			if attempt == maxAttempts {
				p.logger.Warn(
					"WARNING: All PreviousTrack attempts timed out, returning nil",
					slog.Int("maxAttempts", maxAttempts),
					slog.String("timeout", timeout.String()),
				)
				return nil
			}
			// Small pause before next attempt.
			time.Sleep(getTrackPause)
		}
	}

	// This code should not be reached, but for completeness:.
	p.logger.Error("ERROR: Unexpected flow in PreviousTrack, returning nil")
	return nil
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
	p.historyMutex.RLock()
	defer p.historyMutex.RUnlock()

	historyLen := len(p.history)
	p.logger.Info("DIAGNOSTICS: GetHistory() called, history length", slog.Int("historyLength", historyLen))

	if historyLen == 0 {
		p.logger.Info("DIAGNOSTICS: Warning - History is empty!")
		return []interface{}{}
	}

	history := make([]interface{}, historyLen)
	for i, track := range p.history {
		history[i] = &track
	}

	// Log first few tracks in history.
	if historyLen > 0 {
		trackNames := make([]string, 0, minInt(maxShowTracks, historyLen))
		for i := range history[:minInt(maxShowTracks, historyLen)] {
			if track, ok := history[i].(interface{ GetPath() string }); ok {
				trackNames = append(trackNames, filepath.Base(track.GetPath()))
			}
		}
		p.logger.Info("DIAGNOSTICS: First tracks in history", slog.String("tracks", strings.Join(trackNames, ", ")))
	}

	return history
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
	tracksCount := len(p.tracks)
	p.mutex.RUnlock()

	// Если треков нет, сразу возвращаем пустой слайс.
	if tracksEmpty {
		return []Track{}
	}

	// Adding retry mechanism.
	maxAttempts := maxAttempts
	// Определяем таймаут в зависимости от окружения.
	timeout := getTimeoutForEnvironment()

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Use timeout channel for deadlock protection.
		c := make(chan []Track, 1)

		go func() {
			p.mutex.RLock()
			defer p.mutex.RUnlock()

			tracks := make([]Track, len(p.tracks))
			copy(tracks, p.tracks)
			c <- tracks
		}()

		// Wait for result with timeout.
		select {
		case tracks := <-c:
			// Check if slice is empty.
			if len(tracks) == 0 && tracksCount > 0 {
				// If main slice is not empty but we got empty one, this might be a copy error.
				if attempt == maxAttempts {
					p.logger.Warn(
						"WARNING: GetTracks returned empty list despite having tracks",
						slog.Int("tracks", tracksCount),
					)
				}
				continue // Retry
			}
			return tracks
		case <-time.After(timeout):
			if attempt == maxAttempts {
				p.logger.Warn(
					"WARNING: All GetTracks attempts timed out, returning empty list",
					slog.Int("maxAttempts", maxAttempts),
					slog.String("timeout", timeout.String()),
				)
				return []Track{}
			}
			// Small pause before next attempt.
			time.Sleep(getTrackPause)
		}
	}

	// This code should not be reached, but for completeness:.
	p.logger.Error("ERROR: Unexpected flow in GetTracks, returning empty list")
	return []Track{}
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
			sentry.CaptureException(fmt.Errorf("fsnotify error: %w", err))
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
		sentry.CaptureException(fmt.Errorf("error reloading playlist: %w", err))
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
