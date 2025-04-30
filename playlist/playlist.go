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

// Поддерживаемые форматы аудиофайлов
var supportedExtensions = map[string]bool{
	".mp3": true,
	".aac": true,
	".ogg": true,
}

// Track представляет информацию о треке
type Track struct {
	Path     string
	Name     string
	FileInfo os.FileInfo
}

// GetPath возвращает путь к треку
func (t *Track) GetPath() string {
	return t.Path
}

// Playlist управляет списком треков для аудиопотока
type Playlist struct {
	directory string
	tracks    []Track
	current   int
	mutex     sync.RWMutex
	watcher   *fsnotify.Watcher
	onChange  func()
}

// NewPlaylist создаёт новый плейлист из указанной директории
func NewPlaylist(directory string, onChange func()) (*Playlist, error) {
	pl := &Playlist{
		directory: directory,
		tracks:    []Track{},
		current:   0,
		onChange:  onChange,
	}

	// Инициализация watcher для отслеживания изменений в директории
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		sentry.CaptureException(err)
		return nil, err
	}
	pl.watcher = watcher

	// Загрузка треков из директории
	if err := pl.Reload(); err != nil {
		sentry.CaptureException(err)
		return nil, err
	}

	// Запуск горутины для отслеживания изменений в директории
	go pl.watchDirectory()

	return pl, nil
}

// Close закрывает watcher
func (p *Playlist) Close() error {
	err := p.watcher.Close()
	if err != nil {
		sentry.CaptureException(err)
	}
	return err
}

// Reload перезагружает список треков из директории
func (p *Playlist) Reload() error {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	p.tracks = []Track{}
	p.current = 0

	// Проверка существования директории
	if _, err := os.Stat(p.directory); os.IsNotExist(err) {
		errorMsg := fmt.Sprintf("Директория %s не существует", p.directory)
		log.Printf("%s", errorMsg)
		// Не отправляем в Sentry - это информационное сообщение
		return nil // Не считаем отсутствие директории ошибкой
	}

	log.Printf("Сканирование директории %s на наличие аудиофайлов", p.directory)
	// Не отправляем в Sentry - это информационное сообщение

	// Счетчики для статистики
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
			errorMsg := fmt.Sprintf("Ошибка при доступе к файлу/директории %s: %v", path, err)
			log.Printf("%s", errorMsg)
			sentry.CaptureException(fmt.Errorf("%s: %w", errorMsg, err)) // Отправляем в Sentry как ошибку
			return nil // Продолжаем обработку, даже если отдельный файл недоступен
		}
		
		if !info.IsDir() {
			ext := strings.ToLower(filepath.Ext(path))
			if supportedExtensions[ext] {
				supportedFiles++
				fileName := filepath.Base(path)
				log.Printf("Добавление трека: %s", fileName)
				
				p.tracks = append(p.tracks, Track{
					Path:     path,
					Name:     fileName,
					FileInfo: info,
				})
			} else {
				unsupportedFiles++
				log.Printf("Пропуск неподдерживаемого файла: %s (расширение: %s)", filepath.Base(path), ext)
			}
		}
		return nil
	})

	if err != nil {
		sentry.CaptureException(err) // Отправляем в Sentry как ошибку
		return err
	}

	// Проверка на наличие треков
	if len(p.tracks) == 0 {
		errorMsg := fmt.Sprintf("В директории %s не найдено аудиофайлов", p.directory)
		log.Printf("%s", errorMsg)
		
		// Не отправляем в Sentry - это информационное сообщение
		return nil // Не считаем отсутствие треков ошибкой
	}

	// Логирование найденных треков
	log.Printf("Найдено %d треков в %s:", len(p.tracks), p.directory)
	
	trackNames := make([]string, 0, min(10, len(p.tracks)))
	for i, track := range p.tracks {
		if i < 10 || len(p.tracks) < 20 { // Показываем первые 10 треков или все если их меньше 20
			log.Printf("  %d. %s", i+1, track.Name)
			if i < 10 {
				trackNames = append(trackNames, track.Name)
			}
		} else if i == 10 && len(p.tracks) >= 20 {
			log.Printf("  ... и ещё %d треков", len(p.tracks)-10)
			break
		}
	}

	// Перемешивание треков
	p.Shuffle()

	// Добавление директории в watcher
	if err := p.watcher.Add(p.directory); err != nil {
		sentry.CaptureException(err) // Отправляем в Sentry как ошибку
		return err
	}

	// Отправляем статистику только в логи
	log.Printf("Плейлист загружен: %s, треков: %d", p.directory, len(p.tracks))
	
	return nil
}

// min возвращает минимальное из двух чисел
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// GetCurrentTrack возвращает текущий трек
func (p *Playlist) GetCurrentTrack() interface{} {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	if len(p.tracks) == 0 {
		return nil
	}
	return &p.tracks[p.current]
}

// NextTrack переходит к следующему треку и возвращает его
func (p *Playlist) NextTrack() interface{} {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if len(p.tracks) == 0 {
		return nil
	}

	p.current = (p.current + 1) % len(p.tracks)
	return &p.tracks[p.current]
}

// Shuffle перемешивает список треков
func (p *Playlist) Shuffle() {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if len(p.tracks) <= 1 {
		return
	}

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	r.Shuffle(len(p.tracks), func(i, j int) {
		p.tracks[i], p.tracks[j] = p.tracks[j], p.tracks[i]
	})

	log.Printf("Плейлист перемешан: %s, треков: %d", p.directory, len(p.tracks))
}

// GetTracks возвращает копию списка треков
func (p *Playlist) GetTracks() []Track {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	tracks := make([]Track, len(p.tracks))
	copy(tracks, p.tracks)
	return tracks
}

// watchDirectory отслеживает изменения в директории с плейлистом
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
				log.Printf("Обнаружено изменение в плейлисте: %s", event.Name)
				
				// Перезагрузка плейлиста
				if err := p.Reload(); err != nil {
					log.Printf("Ошибка при перезагрузке плейлиста: %s", err)
					sentry.CaptureException(fmt.Errorf("ошибка при перезагрузке плейлиста: %w", err))
					continue
				}

				// Вызов колбэка при изменении плейлиста
				if p.onChange != nil {
					p.onChange()
				}
			}

		case err, ok := <-p.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("Ошибка fsnotify: %s", err)
			sentry.CaptureException(fmt.Errorf("ошибка fsnotify: %w", err))
		}
	}
} 