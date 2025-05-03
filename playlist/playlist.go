package playlist

import (
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/getsentry/sentry-go"
)

// Supported audio file formats
var supportedExtensions = map[string]bool{
	".mp3": true,
	".aac": true,
	".ogg": true,
}

// Track represents information about a track
type Track struct {
	Path     string
	Name     string
	FileInfo os.FileInfo
}

// GetPath returns the path to the track
func (t *Track) GetPath() string {
	return t.Path
}

// Playlist manages the list of tracks for audio streaming
type Playlist struct {
	directory      string
	tracks         []Track
	current        int
	mutex          sync.RWMutex
	watcher        *fsnotify.Watcher
	onChange       func()
	shuffle        bool  // Flag determining whether to shuffle tracks
	history        []Track // History of played tracks
	startTime      time.Time // Playlist start time
	historyMutex   sync.RWMutex // Mutex for history
}

// NewPlaylist creates a new playlist from the specified directory
func NewPlaylist(directory string, onChange func(), shuffle bool) (*Playlist, error) {
	pl := &Playlist{
		directory: directory,
		tracks:    []Track{},
		history:   []Track{},
		current:   0,
		onChange:  onChange,
		shuffle:   shuffle, // Save shuffle parameter
		startTime: time.Now(), // Remember start time
	}

	// Initialize watcher to track directory changes
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		sentry.CaptureException(err)
		return nil, err
	}
	pl.watcher = watcher

	// Load tracks from directory
	if err := pl.Reload(); err != nil {
		sentry.CaptureException(err)
		return nil, err
	}

	// Start goroutine to monitor directory changes
	go pl.watchDirectory()

	return pl, nil
}

// Close closes the watcher
func (p *Playlist) Close() error {
	err := p.watcher.Close()
	if err != nil {
		sentry.CaptureException(err)
	}
	return err
}

// Reload reloads the list of tracks from the directory
func (p *Playlist) Reload() error {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	
	log.Printf("DIAGNOSTICS: Starting playlist reload in directory %s", p.directory)

	p.tracks = []Track{}
	p.current = 0

	// Check if directory exists
	if _, err := os.Stat(p.directory); os.IsNotExist(err) {
		errorMsg := fmt.Sprintf("Directory %s does not exist", p.directory)
		log.Printf("%s", errorMsg)
		// Don't send to Sentry - this is an informational message
		return nil // Don't consider directory absence an error
	}

	log.Printf("Scanning directory %s for audio files", p.directory)
	// Don't send to Sentry - this is an informational message

	// Counters for statistics
	var (
		totalFiles      int
		supportedFiles  int
		unsupportedFiles int
		errorFiles      int
	)

	err := filepath.Walk(p.directory, func(path string, info os.FileInfo, err error) error {
		totalFiles++
		if err != nil {
			errorFiles++
			errorMsg := fmt.Sprintf("Error accessing file/directory %s: %v", path, err)
			log.Printf("%s", errorMsg)
			sentry.CaptureException(fmt.Errorf("%s: %w", errorMsg, err)) // Send to Sentry as an error
			return nil // Continue processing even if an individual file is inaccessible
		}
		
		if !info.IsDir() {
			ext := strings.ToLower(filepath.Ext(path))
			if supportedExtensions[ext] {
				supportedFiles++
				fileName := filepath.Base(path)
				// Don't log every added track
				
				p.tracks = append(p.tracks, Track{
					Path:     path,
					Name:     fileName,
					FileInfo: info,
				})
			} else {
				unsupportedFiles++
				log.Printf("Skipping unsupported file: %s (extension: %s)", filepath.Base(path), ext)
			}
		}
		return nil
	})

	if err != nil {
		sentry.CaptureException(err) // Send to Sentry as an error
		return err
	}

	// Check if there are any tracks
	if len(p.tracks) == 0 {
		errorMsg := fmt.Sprintf("No audio files found in directory %s", p.directory)
		log.Printf("%s", errorMsg)
		
		// Don't send to Sentry - this is an informational message
		return nil // Don't consider absence of tracks an error
	}

	// Log found tracks
	log.Printf("Found %d tracks in %s:", len(p.tracks), p.directory)
	
	// Show maximum 3 tracks (was 5) to reduce output volume
	trackNames := make([]string, 0, min(3, len(p.tracks)))
	for i, track := range p.tracks {
		if i < 3 { // Show only first 3 tracks
			log.Printf("  %d. %s", i+1, track.Name)
			trackNames = append(trackNames, track.Name)
		} else if i == 3 {
			log.Printf("  ... and %d more tracks", len(p.tracks)-3)
			break
		}
	}

	// Запоминаем флаг перемешивания
	needShuffle := p.shuffle
	
	// Shuffle tracks only if the corresponding flag is enabled
	if needShuffle {
		log.Printf("DIAGNOSTICS: Shuffle enabled for playlist %s", p.directory)
	} else {
		log.Printf("DIAGNOSTICS: Shuffle disabled for playlist %s", p.directory)
	}

	// Add directory to watcher
	if err := p.watcher.Add(p.directory); err != nil {
		sentry.CaptureException(err) // Send to Sentry as an error
		return err
	}

	// Add current track to history to ensure history has at least one track
	if len(p.tracks) > 0 {
		log.Printf("DIAGNOSTICS: Adding initial track %s to history", p.tracks[p.current].Name)
		p.addTrackToHistory(p.tracks[p.current])
	}

	// Send statistics only to logs
	log.Printf("Playlist loaded: %s, tracks: %d", p.directory, len(p.tracks))
	
	// Если нужно перемешать, то запускаем отдельную горутину после разблокировки мьютекса
	if needShuffle {
		// Создаем отдельную горутину для перемешивания ПОСЛЕ разблокировки мьютекса
		go func() {
			// Небольшая задержка, чтобы мьютекс успел разблокироваться
			time.Sleep(10 * time.Millisecond)
			log.Printf("DIAGNOSTICS: Running shuffle after reload in separate goroutine for %s", p.directory)
			p.Shuffle()
		}()
	}
	
	return nil
}

// min returns the minimum of two numbers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// GetCurrentTrack returns the current track
func (p *Playlist) GetCurrentTrack() interface{} {
	// Добавляем механизм повторных попыток
	maxAttempts := 3
	
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Используем канал с таймаутом для защиты от блокировок
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
		
		// Увеличиваем таймаут до 500 мс
		select {
		case track := <-c:
			return track
		case <-time.After(500 * time.Millisecond):
			log.Printf("WARNING: GetCurrentTrack timed out (attempt %d/%d), retrying...", attempt, maxAttempts)
			if attempt == maxAttempts {
				log.Printf("WARNING: All GetCurrentTrack attempts timed out, returning nil")
				return nil
			}
			// Небольшая пауза перед следующей попыткой
			time.Sleep(50 * time.Millisecond)
		}
	}
	
	// Этот код не должен достигаться, но для полноты:
	log.Printf("ERROR: Unexpected flow in GetCurrentTrack, returning nil")
	return nil
}

// NextTrack moves to the next track and returns it
func (p *Playlist) NextTrack() interface{} {
	// Добавляем механизм повторных попыток
	maxAttempts := 3
	
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Используем канал с таймаутом для безопасной блокировки мьютекса
		c := make(chan interface{}, 1)
		
		go func() {
			// Пытаемся заблокировать мьютекс в горутине
			p.mutex.Lock()
			defer p.mutex.Unlock()

			if len(p.tracks) == 0 {
				log.Printf("DIAGNOSTICS: NextTrack() called but no tracks available")
				c <- nil
				return
			}

			// Add current track to history before moving to the next
			currentTrack := p.tracks[p.current]
			log.Printf("DIAGNOSTICS: Adding current track %s to history before moving to next", currentTrack.Name)
			p.addTrackToHistory(currentTrack)

			// Move to the next track
			p.current = (p.current + 1) % len(p.tracks)
			nextTrack := p.tracks[p.current]
			log.Printf("DIAGNOSTICS: Moved to next track: %s (position: %d)", nextTrack.Name, p.current)
			
			// If we reached the end of playlist and shuffle is enabled, reshuffle for the next cycle
			if p.current == 0 && p.shuffle {
				log.Printf("DIAGNOSTICS: Reached end of playlist with shuffle enabled, performing reshuffle directly")
				// Вместо вызова отдельной функции, перемешиваем прямо здесь, когда мьютекс уже заблокирован
				if len(p.tracks) > 1 {
					// Создаем генератор случайных чисел с новым seed
					r := rand.New(rand.NewSource(time.Now().UnixNano()))
					
					// Запоминаем текущий трек
					currentPos := p.current
					currentTrackPath := p.tracks[currentPos].Path
					
					// Перемешиваем треки (алгоритм Fisher-Yates)
					for i := len(p.tracks) - 1; i > 0; i-- {
						j := r.Intn(i + 1)
						p.tracks[i], p.tracks[j] = p.tracks[j], p.tracks[i]
					}
					
					// Находим новую позицию текущего трека
					for i, track := range p.tracks {
						if track.Path == currentTrackPath {
							p.current = i
							break
						}
					}
					
					log.Printf("DIAGNOSTICS: Playlist shuffled in NextTrack, current track at position %d", p.current)
				}
			}
			
			// Verify history has been updated
			p.historyMutex.RLock()
			historyLength := len(p.history)
			p.historyMutex.RUnlock()
			log.Printf("DIAGNOSTICS: History length after NextTrack(): %d", historyLength)
			
			c <- &p.tracks[p.current]
		}()
		
		// Ждем результат с увеличенным таймаутом
		select {
		case result := <-c:
			return result
		case <-time.After(500 * time.Millisecond):
			log.Printf("WARNING: NextTrack operation timed out (attempt %d/%d), retrying...", attempt, maxAttempts)
			if attempt == maxAttempts {
				log.Printf("WARNING: All NextTrack attempts timed out, returning nil")
				return nil
			}
			// Небольшая пауза перед следующей попыткой
			time.Sleep(50 * time.Millisecond)
		}
	}
	
	// Этот код не должен достигаться, но для полноты:
	log.Printf("ERROR: Unexpected flow in NextTrack, returning nil")
	return nil
}

// PreviousTrack moves to the previous track and returns it
func (p *Playlist) PreviousTrack() interface{} {
	// Добавляем механизм повторных попыток
	maxAttempts := 3
	
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Используем канал с таймаутом для безопасной блокировки мьютекса
		c := make(chan interface{}, 1)
		
		go func() {
			p.mutex.Lock()
			defer p.mutex.Unlock()

			if len(p.tracks) == 0 {
				log.Printf("DIAGNOSTICS: PreviousTrack() called but no tracks available")
				c <- nil
				return
			}

			// Add current track to history before moving to the previous
			currentTrack := p.tracks[p.current]
			log.Printf("DIAGNOSTICS: Adding current track %s to history before moving to previous", currentTrack.Name)
			p.addTrackToHistory(currentTrack)

			// Move to the previous track considering the possibility of going to a negative index
			if p.current == 0 {
				p.current = len(p.tracks) - 1
			} else {
				p.current--
			}
			
			prevTrack := p.tracks[p.current]
			log.Printf("DIAGNOSTICS: Switching to previous track: %s (position: %d)", prevTrack.Name, p.current)
			
			// Verify history has been updated
			p.historyMutex.RLock()
			historyLength := len(p.history)
			p.historyMutex.RUnlock()
			log.Printf("DIAGNOSTICS: History length after PreviousTrack(): %d", historyLength)
			
			c <- &p.tracks[p.current]
		}()
		
		// Ждем результат с увеличенным таймаутом
		select {
		case result := <-c:
			return result
		case <-time.After(500 * time.Millisecond):
			log.Printf("WARNING: PreviousTrack operation timed out (attempt %d/%d), retrying...", attempt, maxAttempts)
			if attempt == maxAttempts {
				log.Printf("WARNING: All PreviousTrack attempts timed out, returning nil")
				return nil
			}
			// Небольшая пауза перед следующей попыткой
			time.Sleep(50 * time.Millisecond)
		}
	}
	
	// Этот код не должен достигаться, но для полноты:
	log.Printf("ERROR: Unexpected flow in PreviousTrack, returning nil")
	return nil
}

// addTrackToHistory adds a track to history
func (p *Playlist) addTrackToHistory(track Track) {
	p.historyMutex.Lock()
	defer p.historyMutex.Unlock()
	
	// Add track to history
	p.history = append(p.history, track)
	
	// Limit history size - keep last 100 tracks
	const maxHistorySize = 100
	if len(p.history) > maxHistorySize {
		p.history = p.history[len(p.history)-maxHistorySize:]
	}
}

// GetHistory returns the history of played tracks
func (p *Playlist) GetHistory() []interface{} {
	p.historyMutex.RLock()
	defer p.historyMutex.RUnlock()
	
	historyLen := len(p.history)
	log.Printf("DIAGNOSTICS: GetHistory() called, history length: %d", historyLen)
	
	if historyLen == 0 {
		log.Printf("DIAGNOSTICS: Warning - History is empty!")
		return []interface{}{}
	}
	
	history := make([]interface{}, historyLen)
	for i, track := range p.history {
		history[i] = &track
	}
	
	// Log first few tracks in history
	if historyLen > 0 {
		trackNames := make([]string, 0, min(3, historyLen))
		for i := 0; i < min(3, historyLen); i++ {
			if track, ok := history[i].(interface{ GetPath() string }); ok {
				trackNames = append(trackNames, filepath.Base(track.GetPath()))
			}
		}
		log.Printf("DIAGNOSTICS: First tracks in history: %v", trackNames)
	}
	
	return history
}

// GetStartTime returns the playlist start time
func (p *Playlist) GetStartTime() time.Time {
	return p.startTime
}

// Shuffle randomizes the track list
func (p *Playlist) Shuffle() {
	callerID := fmt.Sprintf("%p", p) // Уникальный ID для экземпляра плейлиста
	log.Printf("SHUFFLE-%s: Starting playlist shuffle for directory %s...", callerID, p.directory)
	
	// Создаем безопасную копию треков для перемешивания без длительной блокировки
	var tracksToShuffle []Track
	var originalTrackPath string
	
	// Пытаемся заблокировать мьютекс не более чем на 50 мс для копирования треков
	lockDone := make(chan bool, 1)
	go func() {
		if !p.mutex.TryLock() {
			// Не смогли получить мьютекс мгновенно, пробуем с таймаутом
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-timer.C:
				lockDone <- false
				return
			default:
				p.mutex.Lock()
				timer.Stop()
			}
		}
		// Успешно заблокировали, делаем быстрое копирование
		defer p.mutex.Unlock()
		
		// Если треков мало или нет, то нечего перемешивать
		if len(p.tracks) <= 1 {
			lockDone <- false
			return
		}
		
		// Сохраняем текущий трек
		if p.current < len(p.tracks) {
			originalTrackPath = p.tracks[p.current].Path
		}
		
		// Делаем быструю копию треков
		tracksToShuffle = make([]Track, len(p.tracks))
		copy(tracksToShuffle, p.tracks)
		
		// Сигнализируем, что копирование завершено
		lockDone <- true
	}()
	
	// Ждем результата блокировки
	if !<-lockDone {
		log.Printf("SHUFFLE-%s: Could not acquire mutex or not enough tracks, skipping shuffle", callerID)
		return
	}
	
	// Здесь мы уже разблокировали мьютекс и у нас есть локальная копия треков
	log.Printf("SHUFFLE-%s: Shuffling %d tracks (copy made successfully)", callerID, len(tracksToShuffle))
	
	// Перемешиваем копию треков
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := len(tracksToShuffle) - 1; i > 0; i-- {
		j := r.Intn(i + 1)
		tracksToShuffle[i], tracksToShuffle[j] = tracksToShuffle[j], tracksToShuffle[i]
	}
	
	// Находим позицию оригинального трека в перемешанном массиве
	newPosition := 0
	if originalTrackPath != "" {
		for i, track := range tracksToShuffle {
			if track.Path == originalTrackPath {
				newPosition = i
				break
			}
		}
	}
	
	// Пытаемся заблокировать мьютекс еще раз для применения изменений
	applyDone := make(chan bool, 1)
	go func() {
		if !p.mutex.TryLock() {
			// Не смогли получить мьютекс мгновенно, пробуем с таймаутом
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-timer.C:
				applyDone <- false
				return
			default:
				p.mutex.Lock()
				timer.Stop()
			}
		}
		// Успешно заблокировали, применяем изменения
		defer p.mutex.Unlock()
		
		// Применяем перемешанные треки
		p.tracks = tracksToShuffle
		
		// Восстанавливаем позицию текущего трека
		if originalTrackPath != "" {
			p.current = newPosition
			log.Printf("SHUFFLE-%s: Restored current track position to %d", callerID, newPosition)
		}
		
		// Сигнализируем об успешном применении
		applyDone <- true
	}()
	
	// Проверяем результат применения изменений
	if <-applyDone {
		log.Printf("SHUFFLE-%s: Playlist shuffled successfully", callerID)
	} else {
		log.Printf("SHUFFLE-%s: Could not acquire mutex to apply changes", callerID)
	}
}

// GetTracks returns a copy of the track list
func (p *Playlist) GetTracks() []Track {
	// Добавляем механизм повторных попыток
	maxAttempts := 3
	
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Используем канал с таймаутом для защиты от блокировок
		c := make(chan []Track, 1)
		
		go func() {
			p.mutex.RLock()
			defer p.mutex.RUnlock()

			tracks := make([]Track, len(p.tracks))
			copy(tracks, p.tracks)
			c <- tracks
		}()
		
		// Увеличиваем таймаут до 500 мс, поскольку 50 мс было слишком мало
		select {
		case tracks := <-c:
			// Проверяем, не пустой ли слайс
			if len(tracks) == 0 && len(p.tracks) > 0 {
				// Если основной слайс не пуст, но мы получили пустой, это может быть ошибка копирования
				log.Printf("WARNING: GetTracks returned empty list despite having %d tracks, retrying...", len(p.tracks))
				continue // Повторяем попытку
			}
			return tracks
		case <-time.After(500 * time.Millisecond):
			log.Printf("WARNING: GetTracks timed out (attempt %d/%d), retrying...", attempt, maxAttempts)
			if attempt == maxAttempts {
				log.Printf("WARNING: All GetTracks attempts timed out, returning empty list")
				return []Track{}
			}
			// Небольшая пауза перед следующей попыткой
			time.Sleep(50 * time.Millisecond)
		}
	}
	
	// Этот код не должен достигаться, но для полноты:
	log.Printf("ERROR: Unexpected flow in GetTracks, returning empty list")
	return []Track{}
}

// watchDirectory monitors changes in the playlist directory
func (p *Playlist) watchDirectory() {
	for {
		select {
		case event, ok := <-p.watcher.Events:
			if !ok {
				return
			}

			ext := strings.ToLower(filepath.Ext(event.Name))
			if !supportedExtensions[ext] {
				continue
			}

			if event.Op&(fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0 {
				log.Printf("Change detected in playlist: %s", event.Name)
				
				// Reload playlist
				if err := p.Reload(); err != nil {
					log.Printf("Error reloading playlist: %s", err)
					sentry.CaptureException(fmt.Errorf("error reloading playlist: %w", err))
					continue
				}

				// Call callback when playlist changes
				if p.onChange != nil {
					p.onChange()
				}
			}

		case err, ok := <-p.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("fsnotify error: %s", err)
			sentry.CaptureException(fmt.Errorf("fsnotify error: %w", err))
		}
	}
} 