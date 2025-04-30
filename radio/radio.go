package radio

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
)

// AudioStreamer интерфейс для воспроизведения аудио
type AudioStreamer interface {
	StreamTrack(trackPath string) error
	Close()
}

// PlaylistManager интерфейс для управления плейлистом
type PlaylistManager interface {
	GetCurrentTrack() interface{}
	NextTrack() interface{}
}

// RadioStation управляет одной радиостанцией
type RadioStation struct {
	streamer  AudioStreamer
	playlist  PlaylistManager
	route     string
	stop      chan struct{}
	waitGroup sync.WaitGroup
}

// NewRadioStation создаёт новую радиостанцию
func NewRadioStation(route string, streamer AudioStreamer, playlist PlaylistManager) *RadioStation {
	return &RadioStation{
		streamer: streamer,
		playlist: playlist,
		route:    route,
		stop:     make(chan struct{}),
	}
}

// Start запускает радиостанцию
func (rs *RadioStation) Start() {
	rs.waitGroup.Add(1)
	go rs.streamLoop()
	sentry.CaptureMessage(fmt.Sprintf("Запущена радиостанция: %s", rs.route))
}

// Stop останавливает радиостанцию
func (rs *RadioStation) Stop() {
	close(rs.stop)
	rs.waitGroup.Wait()
	sentry.CaptureMessage(fmt.Sprintf("Остановлена радиостанция: %s", rs.route))
}

// streamLoop основной цикл воспроизведения треков
func (rs *RadioStation) streamLoop() {
	defer rs.waitGroup.Done()

	consecutiveEmptyTracks := 0
	maxEmptyAttempts := 5 // Максимальное количество попыток проверки пустого плейлиста

	for {
		select {
		case <-rs.stop:
			log.Printf("Остановка радиостанции %s", rs.route)
			return
		default:
			// Получаем текущий трек
			track := rs.playlist.GetCurrentTrack()
			
			if track == nil {
				consecutiveEmptyTracks++
				if consecutiveEmptyTracks <= maxEmptyAttempts {
					log.Printf("Нет треков в плейлисте для %s (попытка %d/%d), ожидание 5 секунд...", 
						rs.route, consecutiveEmptyTracks, maxEmptyAttempts)
					sentry.CaptureMessage(fmt.Sprintf("Нет треков в плейлисте для %s, ожидание 5 секунд...", rs.route))
					
					// Подождем и попробуем снова
					time.Sleep(5 * time.Second)
					continue
				} else {
					// Если после нескольких попыток плейлист всё ещё пуст, переходим в режим длительного ожидания
					log.Printf("Плейлист %s пуст. Переход в режим ожидания...", rs.route)
					sentry.CaptureMessage(fmt.Sprintf("Плейлист %s пуст. Переход в режим ожидания...", rs.route))
					
					// Ждём дольше между проверками, чтобы не тратить ресурсы
					time.Sleep(30 * time.Second)
					
					// Сбрасываем счетчик для новой серии проверок
					consecutiveEmptyTracks = 0
					continue
				}
			}
			
			// Сбрасываем счетчик пустых попыток, если трек найден
			consecutiveEmptyTracks = 0

			// Стриминг текущего трека
			trackPath := getTrackPath(track)
			if trackPath == "" {
				log.Printf("Невозможно получить путь к треку для станции %s, переход к следующему", rs.route)
				sentry.CaptureMessage(fmt.Sprintf("Невозможно получить путь к треку для станции %s", rs.route))
				rs.playlist.NextTrack()
				continue
			}
			
			log.Printf("Воспроизведение трека %s на станции %s", trackPath, rs.route)
			sentry.CaptureMessage(fmt.Sprintf("Воспроизведение трека %s на станции %s", trackPath, rs.route))
			
			err := rs.streamer.StreamTrack(trackPath)
			if err != nil {
				log.Printf("Ошибка при воспроизведении трека %s: %s", trackPath, err)
				sentry.CaptureException(fmt.Errorf("ошибка при воспроизведении трека %s: %w", trackPath, err))
				// При ошибке пропускаем трек и переходим к следующему
				rs.playlist.NextTrack()
				continue
			}

			// Переход к следующему треку
			rs.playlist.NextTrack()
		}
	}
}

// RadioStationManager управляет несколькими радиостанциями
type RadioStationManager struct {
	stations map[string]*RadioStation
	mutex    sync.RWMutex
}

// NewRadioStationManager создаёт новый менеджер радиостанций
func NewRadioStationManager() *RadioStationManager {
	return &RadioStationManager{
		stations: make(map[string]*RadioStation),
	}
}

// AddStation добавляет новую радиостанцию
func (rm *RadioStationManager) AddStation(route string, streamer AudioStreamer, playlist PlaylistManager) {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	if _, exists := rm.stations[route]; exists {
		// Если станция уже существует, останавливаем её перед заменой
		rm.stations[route].Stop()
	}

	// Создаём новую станцию
	station := NewRadioStation(route, streamer, playlist)
	rm.stations[route] = station

	// Запускаем станцию
	station.Start()
	log.Printf("Радиостанция %s запущена", route)
	sentry.CaptureMessage(fmt.Sprintf("Радиостанция %s добавлена и запущена", route))
}

// RemoveStation удаляет радиостанцию
func (rm *RadioStationManager) RemoveStation(route string) {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	if station, exists := rm.stations[route]; exists {
		station.Stop()
		delete(rm.stations, route)
		log.Printf("Радиостанция %s остановлена и удалена", route)
		sentry.CaptureMessage(fmt.Sprintf("Радиостанция %s остановлена и удалена", route))
	}
}

// StopAll останавливает все радиостанции
func (rm *RadioStationManager) StopAll() {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	for route, station := range rm.stations {
		station.Stop()
		log.Printf("Радиостанция %s остановлена", route)
	}
	sentry.CaptureMessage("Все радиостанции остановлены")
}

// getTrackPath извлекает путь к треку из интерфейса
func getTrackPath(track interface{}) string {
	// Распаковка интерфейса зависит от конкретной реализации Track
	// Здесь предполагается, что track имеет поле Path
	if t, ok := track.(interface{ GetPath() string }); ok {
		return t.GetPath()
	}
	if t, ok := track.(map[string]string); ok {
		return t["path"]
	}
	if t, ok := track.(struct{ Path string }); ok {
		return t.Path
	}
	
	// Если не удалось распаковать, пробуем преобразовать в строку
	if s, ok := track.(string); ok {
		return s
	}
	
	log.Printf("Неизвестный тип трека: %T", track)
	sentry.CaptureMessage(fmt.Sprintf("Неизвестный тип трека: %T", track))
	return ""
} 