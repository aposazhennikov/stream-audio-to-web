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
	StopCurrentTrack()
}

// PlaylistManager интерфейс для управления плейлистом
type PlaylistManager interface {
	GetCurrentTrack() interface{}
	NextTrack() interface{}
	PreviousTrack() interface{}
}

// RadioStation управляет одной радиостанцией
type RadioStation struct {
	streamer     AudioStreamer
	playlist     PlaylistManager
	route        string
	stop         chan struct{}
	restart      chan struct{} // Новый канал для перезапуска воспроизведения
	currentTrack chan struct{} // Канал для прерывания воспроизведения текущего трека
	waitGroup    sync.WaitGroup
	mutex        sync.Mutex // Мьютекс для синхронизации доступа к каналам
}

// NewRadioStation создаёт новую радиостанцию
func NewRadioStation(route string, streamer AudioStreamer, playlist PlaylistManager) *RadioStation {
	return &RadioStation{
		streamer:     streamer,
		playlist:     playlist,
		route:        route,
		stop:         make(chan struct{}),
		restart:      make(chan struct{}, 1), // Буферизованный канал для restart
		currentTrack: make(chan struct{}),    // Канал для прерывания текущего трека
	}
}

// Start запускает радиостанцию
func (rs *RadioStation) Start() {
	log.Printf("ДИАГНОСТИКА: Начало запуска радиостанции %s...", rs.route)
	
	// Создаем новый канал остановки
	rs.mutex.Lock()
	rs.stop = make(chan struct{})
	rs.restart = make(chan struct{}, 1) // Буферизованный канал, чтобы избежать блокировки при отправке
	rs.mutex.Unlock()
	
	rs.waitGroup.Add(1)
	
	// Запускаем основной цикл воспроизведения в отдельной горутине
	go func() {
		log.Printf("ДИАГНОСТИКА: Запуск streamLoop для станции %s", rs.route)
		rs.streamLoop()
	}()
	
	log.Printf("ДИАГНОСТИКА: Радиостанция %s успешно запущена", rs.route)
}

// Stop останавливает радиостанцию
func (rs *RadioStation) Stop() {
	rs.mutex.Lock()
	close(rs.stop)
	rs.mutex.Unlock()
	
	rs.waitGroup.Wait()
	log.Printf("Остановлена радиостанция: %s", rs.route)
}

// RestartPlayback запускает воспроизведение текущего трека заново
// Вызывается при переключении треков через API
func (rs *RadioStation) RestartPlayback() {
	rs.mutex.Lock()
	defer rs.mutex.Unlock()
	
	// Немедленно остановить стример (прерывает текущее воспроизведение)
	rs.streamer.StopCurrentTrack()
	
	// Добавляем короткую задержку для завершения операций со стримером
	time.Sleep(50 * time.Millisecond)
	
	// Прерываем текущее воспроизведение, если оно идет
	select {
	case <-rs.currentTrack: // Канал уже закрыт
		// Создаем новый канал для следующего трека
		rs.currentTrack = make(chan struct{})
	default:
		// Закрываем канал, чтобы прервать текущее воспроизведение
		close(rs.currentTrack)
		// Создаем новый канал для следующего трека
		rs.currentTrack = make(chan struct{})
	}
	
	// Отправляем сигнал перезапуска в цикл воспроизведения
	select {
	case rs.restart <- struct{}{}: // Отправляем сигнал, если канал не заполнен
		log.Printf("ДИАГНОСТИКА: Отправлен сигнал перезапуска воспроизведения для станции %s", rs.route)
	default:
		// Канал уже содержит сигнал, не нужно отправлять еще один
		log.Printf("ДИАГНОСТИКА: Сигнал перезапуска уже в очереди для станции %s", rs.route)
	}
}

// streamLoop основной цикл воспроизведения треков
func (rs *RadioStation) streamLoop() {
	defer rs.waitGroup.Done()
	
	log.Printf("ДИАГНОСТИКА: Запущен основной цикл воспроизведения для станции %s", rs.route)

	consecutiveEmptyTracks := 0
	maxEmptyAttempts := 5 // Максимальное количество попыток проверки пустого плейлиста
	var isRestartRequested bool

	for {
		// Проверяем сигналы остановки и перезапуска перед началом нового цикла
		select {
		case <-rs.stop:
			log.Printf("Остановка радиостанции %s", rs.route)
			return
		case <-rs.restart:
			log.Printf("ДИАГНОСТИКА: Обработан сигнал перезапуска для станции %s", rs.route)
			isRestartRequested = true
		default:
			// Нет сигналов, продолжаем обычное выполнение
		}

		// Получаем текущий трек
		log.Printf("ДИАГНОСТИКА: Получение текущего трека для станции %s", rs.route)
		track := rs.playlist.GetCurrentTrack()
		
		if track == nil {
			consecutiveEmptyTracks++
			log.Printf("ДИАГНОСТИКА: Нет трека для станции %s (попытка %d/%d)", rs.route, consecutiveEmptyTracks, maxEmptyAttempts)
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
		log.Printf("ДИАГНОСТИКА: Трек найден для станции %s", rs.route)

		// Стриминг текущего трека
		trackPath := getTrackPath(track)
		log.Printf("ДИАГНОСТИКА: Получен путь к треку для станции %s: %s", rs.route, trackPath)
		
		if trackPath == "" {
			log.Printf("Невозможно получить путь к треку для станции %s, переход к следующему", rs.route)
			sentry.CaptureMessage(fmt.Sprintf("Невозможно получить путь к треку для станции %s", rs.route)) // Это ошибка, отправляем в Sentry
			rs.playlist.NextTrack()
			continue
		}
		
		// Создаем локальную копию канала для текущего трека
		rs.mutex.Lock()
		currentTrackCh := rs.currentTrack
		rs.mutex.Unlock()

		// Запускаем воспроизведение трека в отдельной горутине, 
		// чтобы иметь возможность прервать его при получении сигнала
		trackFinished := make(chan error, 1)
		go func() {
			log.Printf("ДИАГНОСТИКА: Начало воспроизведения трека %s на станции %s", trackPath, rs.route)
			err := rs.streamer.StreamTrack(trackPath)
			trackFinished <- err
		}()

		// Ожидаем или завершения трека, или сигнала прерывания
		select {
		case <-rs.stop:
			// Станция остановлена, выходим из цикла
			log.Printf("Остановка радиостанции %s во время воспроизведения", rs.route)
			return

		case <-currentTrackCh:
			// Трек прерван сигналом RestartPlayback
			log.Printf("ДИАГНОСТИКА: Воспроизведение трека %s на станции %s прервано вручную", trackPath, rs.route)
			// Подождем для уверенности, что горутина воспроизведения завершила работу
			select {
			case <-trackFinished:
				log.Printf("ДИАГНОСТИКА: Горутина воспроизведения трека %s успешно завершена после прерывания", trackPath)
			case <-time.After(500 * time.Millisecond):
				log.Printf("ДИАГНОСТИКА: Тайм-аут ожидания завершения горутины воспроизведения трека %s", trackPath)
			}
			
			// Проверяем, был ли запрос на перезапуск
			if isRestartRequested {
				log.Printf("ДИАГНОСТИКА: Обнаружен запрос на перезапуск для станции %s, сбрасываем флаг", rs.route)
				isRestartRequested = false
			}

		case err := <-trackFinished:
			// Трек завершен естественным образом или произошла ошибка
			if err != nil {
				log.Printf("Ошибка при воспроизведении трека %s: %s", trackPath, err)
				sentry.CaptureException(fmt.Errorf("ошибка при воспроизведении трека %s: %w", trackPath, err))
				// При ошибке пропускаем трек и переходим к следующему
				rs.playlist.NextTrack()
			} else {
				log.Printf("ДИАГНОСТИКА: Завершено воспроизведение трека %s на станции %s", trackPath, rs.route)
				// Переход к следующему треку только если не было запроса на перезапуск
				if !isRestartRequested {
					log.Printf("ДИАГНОСТИКА: Переход к следующему треку для станции %s", rs.route)
					rs.playlist.NextTrack()
				} else {
					log.Printf("ДИАГНОСТИКА: Обнаружен запрос на перезапуск для станции %s, сбрасываем флаг", rs.route)
					isRestartRequested = false
				}
			}
		}

		// Небольшая пауза перед следующей итерацией для предотвращения CPU-гонки
		time.Sleep(100 * time.Millisecond)
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

	log.Printf("ДИАГНОСТИКА: Начало добавления радиостанции %s в менеджер...", route)

	if _, exists := rm.stations[route]; exists {
		// Если станция уже существует, останавливаем её перед заменой
		log.Printf("Радиостанция %s уже существует, останавливаем её перед заменой", route)
		rm.stations[route].Stop()
		log.Printf("Существующая радиостанция %s остановлена", route)
	}

	// Создаём новую станцию
	log.Printf("ДИАГНОСТИКА: Создание новой радиостанции %s...", route)
	station := NewRadioStation(route, streamer, playlist)
	rm.stations[route] = station

	// Запускаем станцию асинхронно, чтобы не блокировать основной поток
	log.Printf("ДИАГНОСТИКА: Запуск радиостанции %s в отдельной горутине...", route)
	go func() {
		log.Printf("ДИАГНОСТИКА: Начинаем запуск станции %s внутри горутины", route)
		station.Start()
		log.Printf("ДИАГНОСТИКА: Горутина радиостанции %s успешно запущена", route)
	}()

	log.Printf("ДИАГНОСТИКА: Радиостанция %s добавлена в менеджер", route)
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

// GetStation возвращает радиостанцию по маршруту
func (rm *RadioStationManager) GetStation(route string) *RadioStation {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	if station, exists := rm.stations[route]; exists {
		return station
	}
	return nil
}

// RestartPlayback перезапускает воспроизведение для указанного маршрута
func (rm *RadioStationManager) RestartPlayback(route string) bool {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	if station, exists := rm.stations[route]; exists {
		station.RestartPlayback()
		return true
	}
	return false
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