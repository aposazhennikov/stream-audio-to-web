package audio

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
	"golang.org/x/sync/errgroup"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	defaultBufferSize = 65536 // 64KB
	gracePeriodMs     = 100   // 100ms буфер между треками для избежания обрезки начала (было 200мс)
)

// Streamer управляет стримингом аудио для одного "радио" потока
type Streamer struct {
	bufferPool      *sync.Pool
	bufferSize      int
	clientCounter   int32
	maxClients      int
	quit            chan struct{}
	currentTrackCh  chan string
	clientChannels  map[int]chan []byte
	clientMutex     sync.RWMutex
	transcodeFormat string
	bitrate         int
	lastChunk       []byte       // Последний отправленный кусок аудиоданных
	lastChunkMutex  sync.RWMutex // Мьютекс для защиты lastChunk
}

// NewStreamer создаёт новый аудио стример
func NewStreamer(bufferSize, maxClients int, transcodeFormat string, bitrate int) *Streamer {
	if bufferSize <= 0 {
		bufferSize = defaultBufferSize
	}

	return &Streamer{
		bufferPool: &sync.Pool{
			New: func() interface{} {
				return make([]byte, bufferSize)
			},
		},
		bufferSize:      bufferSize,
		maxClients:      maxClients,
		quit:            make(chan struct{}),
		currentTrackCh:  make(chan string, 1),
		clientChannels:  make(map[int]chan []byte),
		clientMutex:     sync.RWMutex{},
		transcodeFormat: transcodeFormat,
		bitrate:         bitrate,
		lastChunk:       nil,
		lastChunkMutex:  sync.RWMutex{},
	}
}

// Close закрывает стример и все клиентские соединения
func (s *Streamer) Close() {
	close(s.quit)
	s.clientMutex.Lock()
	defer s.clientMutex.Unlock()

	for _, ch := range s.clientChannels {
		close(ch)
	}
}

// GetCurrentTrackChannel возвращает канал с информацией о текущем треке
func (s *Streamer) GetCurrentTrackChannel() <-chan string {
	return s.currentTrackCh
}

// StreamTrack стримит трек всем подключенным клиентам
func (s *Streamer) StreamTrack(trackPath string) error {
	// Проверка на пустой путь
	if trackPath == "" {
		sentryErr := fmt.Errorf("пустой путь к аудиофайлу")
		sentry.CaptureException(sentryErr) // Это ошибка, отправляем в Sentry
		return sentryErr
	}

	log.Printf("ДИАГНОСТИКА: Попытка воспроизведения файла: %s", trackPath)
	
	// Проверка существования файла
	fileInfo, err := os.Stat(trackPath)
	if err != nil {
		log.Printf("ДИАГНОСТИКА: ОШИБКА при проверке файла %s: %v", trackPath, err)
		sentryErr := fmt.Errorf("ошибка проверки файла: %w", err)
		sentry.CaptureException(sentryErr) // Это ошибка, отправляем в Sentry
		return sentryErr
	}
	
	log.Printf("ДИАГНОСТИКА: Файл %s существует, размер: %d байт, права доступа: %v", 
		trackPath, fileInfo.Size(), fileInfo.Mode())
	
	// Проверка, что это не директория
	if fileInfo.IsDir() {
		sentryErr := fmt.Errorf("указанный путь %s является директорией, а не файлом", trackPath)
		sentry.CaptureException(sentryErr) // Это ошибка, отправляем в Sentry
		return sentryErr
	}
	
	// Открываем файл
	log.Printf("ДИАГНОСТИКА: Попытка открыть файл: %s", trackPath)
	file, err := os.Open(trackPath)
	if err != nil {
		log.Printf("ДИАГНОСТИКА: ОШИБКА при открытии файла %s: %v", trackPath, err)
		sentryErr := fmt.Errorf("ошибка открытия файла: %w", err)
		sentry.CaptureException(sentryErr) // Это ошибка, отправляем в Sentry
		return sentryErr
	}
	defer file.Close()
	
	// Инициализация буфера и счетчика
	buffer := s.bufferPool.Get().([]byte)
	defer s.bufferPool.Put(buffer)
	
	// Для отслеживания прогресса
	var bytesRead int64
	
	// Отправляем информацию о текущем треке в канал
	select {
	case s.currentTrackCh <- filepath.Base(trackPath):
		log.Printf("ДИАГНОСТИКА: Обновлена информация о текущем треке: %s", filepath.Base(trackPath))
	default:
		log.Printf("ДИАГНОСТИКА: Не удалось обновить информацию о текущем треке: канал заполнен")
	}
	
	startTime := time.Now()
	log.Printf("ДИАГНОСТИКА: Начало чтения файла %s", trackPath)
	
	for {
		n, err := file.Read(buffer)
		if err == io.EOF {
			log.Printf("ДИАГНОСТИКА: Достигнут конец файла %s", trackPath)
			break
		}
		if err != nil {
			log.Printf("ДИАГНОСТИКА: ОШИБКА при чтении файла %s: %v", trackPath, err)
			sentryErr := fmt.Errorf("ошибка чтения файла: %w", err)
			sentry.CaptureException(sentryErr) // Это ошибка, отправляем в Sentry
			return sentryErr
		}

		bytesRead += int64(n)
		
		// ПЕРЕМЕЩАЕМ: Сохраняем копию последнего чанка для новых клиентов
		// ПЕРЕД отправкой данных всем клиентам
		dataCopy := make([]byte, n)
		copy(dataCopy, buffer[:n])
		
		// Сохраняем копию последнего чанка для новых клиентов
		s.lastChunkMutex.Lock()
		s.lastChunk = dataCopy
		s.lastChunkMutex.Unlock()
		
		// Отправляем данные всем клиентам
		if err := s.broadcastToClients(buffer[:n]); err != nil {
			log.Printf("ДИАГНОСТИКА: ОШИБКА при отправке данных клиентам: %v", err)
			sentry.CaptureException(err) // Это ошибка, отправляем в Sentry
			return err
		}

		// Проверяем сигнал завершения
		select {
		case <-s.quit:
			log.Printf("ДИАГНОСТИКА: Прервано воспроизведение файла %s (прочитано %d байт из %d)", 
				trackPath, bytesRead, fileInfo.Size())
			return nil
		default:
		}
	}
	
	duration := time.Since(startTime)
	log.Printf("ДИАГНОСТИКА: Воспроизведение файла %s завершено (прочитано %d байт за %.2f сек)", 
		trackPath, bytesRead, duration.Seconds())

	// Инкрементируем метрику времени проигрывания для Prometheus
	// Метрика должна быть определена в http/server.go
	if trackSecondsTotal, ok := GetTrackSecondsMetric(); ok {
		routeName := getRouteFromTrackPath(trackPath)
		trackSecondsTotal.WithLabelValues(routeName).Add(duration.Seconds())
		log.Printf("ДИАГНОСТИКА: Метрика trackSecondsTotal увеличена на %.2f сек для %s", 
			duration.Seconds(), routeName)
	}

	// Добавляем паузу между треками для избежания обрезки начала - уменьшена с 200 до 100 мс
	log.Printf("ДИАГНОСТИКА: Добавление паузы %d мс между треками", gracePeriodMs)
	time.Sleep(gracePeriodMs * time.Millisecond)

	return nil
}

// getRouteFromTrackPath пытается извлечь имя маршрута из пути к файлу
func getRouteFromTrackPath(trackPath string) string {
	// Извлекаем путь к директории
	dir := filepath.Dir(trackPath)
	
	// Получаем последний компонент пути, который обычно соответствует имени маршрута
	route := filepath.Base(dir)
	
	// Добавляем ведущий слеш, если его нет
	if !strings.HasPrefix(route, "/") {
		route = "/" + route
	}
	
	return route
}

// Для обмена метриками между пакетами

var (
	trackSecondsMetric *prometheus.CounterVec
	metricMutex        sync.RWMutex
)

// SetTrackSecondsMetric устанавливает метрику извне
func SetTrackSecondsMetric(metric *prometheus.CounterVec) {
	metricMutex.Lock()
	defer metricMutex.Unlock()
	
	trackSecondsMetric = metric
	log.Printf("ДИАГНОСТИКА: SetTrackSecondsMetric сохранила указатель на метрику")
}

// GetTrackSecondsMetric возвращает метрику, если она установлена
func GetTrackSecondsMetric() (*prometheus.CounterVec, bool) {
	metricMutex.RLock()
	defer metricMutex.RUnlock()
	
	if trackSecondsMetric == nil {
		return nil, false
	}
	
	return trackSecondsMetric, true
}

// Для отслеживания логов при отправке данных
var (
	lastClientCount int
	lastLogTime     time.Time
)

// AddClient добавляет нового клиента и возвращает канал для получения данных
func (s *Streamer) AddClient() (<-chan []byte, int, error) {
	// Проверяем, не превышено ли максимальное количество клиентов
	if s.maxClients > 0 && atomic.LoadInt32(&s.clientCounter) >= int32(s.maxClients) {
		err := fmt.Errorf("превышено максимальное количество клиентов (%d)", s.maxClients)
		sentry.CaptureException(err) // Это ошибка, отправляем в Sentry
		return nil, 0, err
	}

	s.clientMutex.Lock()
	defer s.clientMutex.Unlock()

	// Увеличиваем счётчик клиентов
	clientID := int(atomic.AddInt32(&s.clientCounter, 1))
	
	// Создаём канал для клиента с буфером
	// Буферизованный канал нужен для предотвращения блокировки 
	// при медленных клиентах
	clientChannel := make(chan []byte, 32) // Увеличен размер буфера с 10 до 32
	s.clientChannels[clientID] = clientChannel

	log.Printf("Клиент %d подключен. Всего клиентов: %d", clientID, atomic.LoadInt32(&s.clientCounter))

	// ВАЖНО: Если у нас есть последний буфер данных, отправляем его сразу новому клиенту
	// чтобы он не ждал следующего чтения из файла
	s.lastChunkMutex.RLock()
	if s.lastChunk != nil {
		// Используем append для создания новой копии для клиента - одна аллокация вместо двух
		dataCopy := append([]byte(nil), s.lastChunk...)
		
		log.Printf("ДИАГНОСТИКА: Отправка последнего буфера (%d байт) новому клиенту %d", len(dataCopy), clientID)
		
		// Отправляем данные в канал нового клиента
		select {
		case clientChannel <- dataCopy:
			log.Printf("ДИАГНОСТИКА: Последний буфер успешно отправлен клиенту %d", clientID)
		default:
			log.Printf("ДИАГНОСТИКА: Невозможно отправить последний буфер клиенту %d, канал заполнен", clientID)
		}
	} else {
		log.Printf("ДИАГНОСТИКА: Нет последнего буфера для отправки клиенту %d", clientID)
	}
	s.lastChunkMutex.RUnlock()

	return clientChannel, clientID, nil
}

// RemoveClient удаляет клиента
func (s *Streamer) RemoveClient(clientID int) {
	s.clientMutex.Lock()
	defer s.clientMutex.Unlock()

	if channel, exists := s.clientChannels[clientID]; exists {
		close(channel)
		delete(s.clientChannels, clientID)
		atomic.AddInt32(&s.clientCounter, -1)
		log.Printf("Клиент %d отключен. Всего клиентов: %d", clientID, atomic.LoadInt32(&s.clientCounter))
	}
}

// GetClientCount возвращает текущее количество клиентов
func (s *Streamer) GetClientCount() int {
	return int(atomic.LoadInt32(&s.clientCounter))
}

// broadcastToClients отправляет данные всем подключенным клиентам
func (s *Streamer) broadcastToClients(data []byte) error {
	s.clientMutex.RLock()
	defer s.clientMutex.RUnlock()

	// Если нет клиентов, просто возвращаемся
	clientCount := len(s.clientChannels)
	if clientCount == 0 {
		return nil
	}

	// Сократим число диагностических сообщений - выводим только когда меняется количество клиентов
	// или раз в 10 секунд
	if clientCount > 0 {
		// Выводим сообщение только при изменении количества клиентов 
		// или не чаще раза в 10 секунд
		if clientCount != lastClientCount || time.Since(lastLogTime) > 10*time.Second {
			log.Printf("ДИАГНОСТИКА: Отправка %d байт данных %d клиентам", len(data), clientCount)
			lastClientCount = clientCount
			lastLogTime = time.Now()
		}
	}

	// Если клиентов очень много, используем пакетный подход
	const batchSize = 50
	
	// Для небольшого количества клиентов используем прямую отправку
	if clientCount <= batchSize {
		var g errgroup.Group
		
		// Отправляем данные каждому клиенту в отдельной горутине
		for clientID, clientChan := range s.clientChannels {
			clientID := clientID
			clientChan := clientChan
			
			g.Go(func() error {
				select {
				case clientChan <- data: // Используем оригинальный буфер, а не копию
					// Данные успешно отправлены
					return nil
				case <-time.After(200 * time.Millisecond):
					// Тайм-аут при отправке данных
					log.Printf("Тайм-аут при отправке данных клиенту %d, отключаем...", clientID)
					s.RemoveClient(clientID)
					return nil
				}
			})
		}

		return g.Wait()
	}
	
	// Для большого количества клиентов используем пакетную обработку
	// Создаем список всех клиентов
	clients := make([]struct {
		id  int
		ch  chan []byte
	}, 0, clientCount)
	
	for id, ch := range s.clientChannels {
		clients = append(clients, struct {
			id  int
			ch  chan []byte
		}{id, ch})
	}
	
	// Обрабатываем клиентов пакетами
	var wg sync.WaitGroup
	for i := 0; i < clientCount; i += batchSize {
		end := i + batchSize
		if end > clientCount {
			end = clientCount
		}
		
		wg.Add(1)
		go func(batch []struct {
			id  int
			ch  chan []byte
		}) {
			defer wg.Done()
			
			for _, client := range batch {
				select {
				case client.ch <- data: // Используем оригинальный буфер, а не копию
					// Данные успешно отправлены
				case <-time.After(200 * time.Millisecond):
					// Тайм-аут при отправке данных
					log.Printf("Тайм-аут при отправке данных клиенту %d, отключаем...", client.id)
					s.RemoveClient(client.id)
				}
			}
		}(clients[i:end])
	}
	
	wg.Wait()
	return nil
} 