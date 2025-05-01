package audio

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
	"golang.org/x/sync/errgroup"
)

const (
	defaultBufferSize = 65536 // 64KB
	gracePeriodMs     = 200   // 200ms буфер между треками для избежания обрезки начала
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
	
	log.Printf("ДИАГНОСТИКА: Успешно открыт файл: %s (размер: %d байт)", trackPath, fileInfo.Size())

	// Отправляем информацию о текущем треке
	log.Printf("ДИАГНОСТИКА: Обновление информации о текущем треке: %s", trackPath)
	select {
	case s.currentTrackCh <- trackPath:
		log.Printf("ДИАГНОСТИКА: Информация о треке успешно отправлена в канал")
	default:
		// Если канал полон, очищаем его и отправляем новое значение
		log.Printf("ДИАГНОСТИКА: Канал информации о треке полон, очистка...")
		select {
		case <-s.currentTrackCh:
		default:
		}
		s.currentTrackCh <- trackPath
		log.Printf("ДИАГНОСТИКА: Информация о треке отправлена после очистки канала")
	}

	// TODO: Реализовать транскодирование аудио если необходимо
	// Это будет зависеть от формата входного файла и требуемого формата вывода

	// Использование пула буферов для уменьшения нагрузки на GC
	log.Printf("ДИАГНОСТИКА: Получение буфера из пула для чтения файла %s", trackPath)
	buffer := s.bufferPool.Get().([]byte)
	defer s.bufferPool.Put(buffer)

	// Счетчик для отслеживания прогресса
	var bytesRead int64
	
	// Отслеживаем начало воспроизведения
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
		
		// Отправляем данные всем клиентам
		log.Printf("ДИАГНОСТИКА: Отправка %d байт данных клиентам", n)
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

	// Добавляем паузу между треками для избежания обрезки начала
	log.Printf("ДИАГНОСТИКА: Добавление паузы %d мс между треками", gracePeriodMs)
	time.Sleep(gracePeriodMs * time.Millisecond)

	return nil
}

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
		// Создаем копию буфера для нового клиента
		dataCopy := make([]byte, len(s.lastChunk))
		copy(dataCopy, s.lastChunk)
		
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

	// Создаём копию данных для всех клиентов только один раз
	// Это необходимо, потому что буфер может быть переиспользован
	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)
	
	// Сохраняем копию последнего чанка для новых клиентов
	s.lastChunkMutex.Lock()
	s.lastChunk = dataCopy
	s.lastChunkMutex.Unlock()
	
	log.Printf("ДИАГНОСТИКА: Отправка %d байт данных %d клиентам", len(data), clientCount)

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
				case clientChan <- dataCopy: // Используем одну и ту же копию для всех клиентов
					// Данные успешно отправлены
					return nil
				case <-time.After(200 * time.Millisecond): // Уменьшен таймаут с 500 до 200 мс
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
				case client.ch <- dataCopy: // Используем одну и ту же копию для всех клиентов
					// Данные успешно отправлены
				case <-time.After(200 * time.Millisecond): // Уменьшен таймаут
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