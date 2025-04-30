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
	log.Printf("Начало запуска радиостанции %s...", rs.route)
	
	// Создаем новый канал остановки
	rs.stop = make(chan struct{})
	
	rs.waitGroup.Add(1)
	
	// Запускаем основной цикл воспроизведения в отдельной горутине
	go rs.streamLoop()
	
	log.Printf("Радиостанция %s успешно запущена", rs.route)
}

// Stop останавливает радиостанцию
func (rs *RadioStation) Stop() {
	close(rs.stop)
	rs.waitGroup.Wait()
	log.Printf("Остановлена радиостанция: %s", rs.route)
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
					// Не отправляем в Sentry - это информационное сообщение
					
					// Подождем и попробуем снова
					time.Sleep(5 * time.Second)
					continue
				} else {
					// Если после нескольких попыток плейлист всё ещё пуст, переходим в режим длительного ожидания
					log.Printf("Плейлист %s пуст. Переход в режим ожидания...", rs.route)
					// Не отправляем в Sentry - это информационное сообщение
					
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
				sentry.CaptureMessage(fmt.Sprintf("Невозможно получить путь к треку для станции %s", rs.route)) // Это ошибка, отправляем в Sentry
				rs.playlist.NextTrack()
				continue
			}
			
			log.Printf("Воспроизведение трека %s на станции %s", trackPath, rs.route)
			// Не отправляем в Sentry - это информационное сообщение
			
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

	log.Printf("Начало добавления радиостанции %s в менеджер...", route)

	if _, exists := rm.stations[route]; exists {
		// Если станция уже существует, останавливаем её перед заменой
		log.Printf("Радиостанция %s уже существует, останавливаем её перед заменой", route)
		rm.stations[route].Stop()
		log.Printf("Существующая радиостанция %s остановлена", route)
	}

	// Создаём новую станцию
	log.Printf("Создание новой радиостанции %s...", route)
	station := NewRadioStation(route, streamer, playlist)
	rm.stations[route] = station

	// Запускаем станцию асинхронно, чтобы не блокировать основной поток
	log.Printf("Запуск радиостанции %s в отдельной горутине...", route)
	go func() {
		station.Start()
		log.Printf("Горутина радиостанции %s успешно запущена", route)
	}()

	log.Printf("Радиостанция %s запущена", route)
}

// RemoveStation удаляет радиостанцию
func (rm *RadioStationManager) RemoveStation(route string) {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	if station, exists := rm.stations[route]; exists {
		station.Stop()
		delete(rm.stations, route)
		log.Printf("Радиостанция %s остановлена и удалена", route)
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
	log.Printf("Все радиостанции остановлены")
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
	sentry.CaptureMessage(fmt.Sprintf("Неизвестный тип трека: %T", track)) // Это ошибка, отправляем в Sentry
	return ""
} 