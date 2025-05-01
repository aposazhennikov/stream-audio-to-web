package http

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/user/stream-audio-to-web/audio"
)

var (
	// Метрики Prometheus
	listenerCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "audio_stream_listener_count",
			Help: "Number of active listeners per stream",
		},
		[]string{"stream"},
	)

	bytesSent = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "audio_stream_bytes_sent_total",
			Help: "Total number of bytes sent to clients",
		},
		[]string{"stream"},
	)
	
	// Счетчик времени проигрывания аудио в секундах
	trackSecondsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "audio_stream_track_seconds_total",
			Help: "Total seconds of audio played",
		},
		[]string{"stream"},
	)
)

func init() {
	// Регистрация метрик Prometheus
	prometheus.MustRegister(listenerCount)
	prometheus.MustRegister(bytesSent)
	prometheus.MustRegister(trackSecondsTotal)
}

// StreamHandler интерфейс для обработки аудиопотока
type StreamHandler interface {
	AddClient() (<-chan []byte, int, error)
	RemoveClient(clientID int)
	GetClientCount() int
	GetCurrentTrackChannel() <-chan string
}

// PlaylistManager интерфейс для управления плейлистом
type PlaylistManager interface {
	Reload() error
	GetCurrentTrack() interface{}
	NextTrack() interface{}
}

// Server представляет HTTP сервер для потоковой передачи аудио
type Server struct {
	router          *mux.Router
	streams         map[string]StreamHandler
	playlists       map[string]PlaylistManager
	streamFormat    string
	maxClients      int
	mutex           sync.RWMutex
	currentTracks   map[string]string
	trackMutex      sync.RWMutex
}

// NewServer создаёт новый HTTP сервер
func NewServer(streamFormat string, maxClients int) *Server {
	server := &Server{
		router:        mux.NewRouter(),
		streams:       make(map[string]StreamHandler),
		playlists:     make(map[string]PlaylistManager),
		streamFormat:  streamFormat,
		maxClients:    maxClients,
		currentTracks: make(map[string]string),
	}

	// Настройка маршрутов
	server.setupRoutes()
	
	// Передаем метрику trackSecondsTotal напрямую в пакет audio
	audio.SetTrackSecondsMetric(trackSecondsTotal)
	log.Printf("ДИАГНОСТИКА: Метрика trackSecondsTotal передана в пакет audio")

	log.Printf("HTTP сервер создан, формат потока: %s, макс. клиентов: %d", streamFormat, maxClients)
	return server
}

// Handler возвращает обработчик HTTP запросов
func (s *Server) Handler() http.Handler {
	return s.router
}

// RegisterStream регистрирует новый аудиопоток
func (s *Server) RegisterStream(route string, stream StreamHandler, playlist PlaylistManager) {
	log.Printf("ДИАГНОСТИКА: Начало регистрации аудиопотока для маршрута '%s'", route)
	
	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Убедимся, что маршрут начинается со слеша
	if route[0] != '/' {
		route = "/" + route
		log.Printf("Поправлен маршрут при регистрации: '%s'", route)
	}

	log.Printf("ДИАГНОСТИКА: Добавление потока в map streams для маршрута '%s'", route)
	s.streams[route] = stream
	log.Printf("ДИАГНОСТИКА: Добавление плейлиста в map playlists для маршрута '%s'", route)
	s.playlists[route] = playlist

	// Проверяем, добавился ли поток
	if _, exists := s.streams[route]; exists {
		log.Printf("ДИАГНОСТИКА: Поток для маршрута '%s' успешно добавлен в map streams", route)
	} else {
		log.Printf("ОШИБКА: Поток для маршрута '%s' не был добавлен в map streams!", route)
	}
	
	// ВАЖНО: Регистрируем обработчик маршрута в роутере
	s.router.HandleFunc(route, s.StreamAudioHandler(route)).Methods("GET")
	log.Printf("ДИАГНОСТИКА: Зарегистрирован HTTP обработчик для маршрута '%s'", route)

	// Запуск горутины для отслеживания текущего трека
	log.Printf("ДИАГНОСТИКА: Запуск горутины для отслеживания текущего трека для маршрута '%s'", route)
	go s.trackCurrentTrack(route, stream.GetCurrentTrackChannel())

	log.Printf("ДИАГНОСТИКА: Аудиопоток для маршрута '%s' успешно зарегистрирован", route)
}

// IsStreamRegistered проверяет, зарегистрирован ли поток с указанным маршрутом
func (s *Server) IsStreamRegistered(route string) bool {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	
	// Проверяем, существует ли поток по указанному маршруту
	_, exists := s.streams[route]
	return exists
}

// trackCurrentTrack отслеживает текущий трек для указанного потока
func (s *Server) trackCurrentTrack(route string, trackCh <-chan string) {
	for trackPath := range trackCh {
		// Извлекаем только имя файла без пути
		fileName := filepath.Base(trackPath)
		
		// Обрабатываем специфические символы в имени файла (логирование)
		log.Printf("Текущий трек для %s: %s (путь: %s)", route, fileName, trackPath)
		
		// Сохраняем расширенную информацию в Sentry только для проблемных имен файлов с unicode
		if hasUnicodeChars(fileName) {
			sentry.ConfigureScope(func(scope *sentry.Scope) {
				scope.SetContext("track_info", map[string]interface{}{
					"route":         route,
					"track_name":    fileName,
					"track_path":    trackPath,
					"track_dir":     filepath.Dir(trackPath),
					"track_ext":     filepath.Ext(trackPath),
					"track_len":     len(fileName),
					"track_unicode": true,
				})
			})
		}
		
		s.trackMutex.Lock()
		s.currentTracks[route] = fileName
		s.trackMutex.Unlock()
	}
}

// hasUnicodeChars проверяет наличие не-ASCII символов в строке
func hasUnicodeChars(s string) bool {
	for _, r := range s {
		if r > 127 {
			return true
		}
	}
	return false
}

// setupRoutes настраивает маршруты HTTP сервера
func (s *Server) setupRoutes() {
	// Эндпоинты мониторинга и здоровья
	s.router.HandleFunc("/healthz", s.healthzHandler).Methods("GET")
	s.router.HandleFunc("/readyz", s.readyzHandler).Methods("GET")
	s.router.Handle("/metrics", promhttp.Handler()).Methods("GET")

	// API для управления плейлистами
	s.router.HandleFunc("/streams", s.streamsHandler).Methods("GET")
	s.router.HandleFunc("/reload-playlist", s.reloadPlaylistHandler).Methods("POST")
	s.router.HandleFunc("/now-playing", s.nowPlayingHandler).Methods("GET")

	// Добавление статических файлов для веб-интерфейса
	s.router.PathPrefix("/web/").Handler(http.StripPrefix("/web/", http.FileServer(http.Dir("./web"))))

	log.Printf("HTTP маршруты настроены")
}

// healthzHandler возвращает 200 OK, если сервер работает
func (s *Server) healthzHandler(w http.ResponseWriter, r *http.Request) {
	// Логирование запроса healthz
	log.Printf("Получен запрос healthz от %s (URI: %s)", r.RemoteAddr, r.RequestURI)
	
	// Проверка наличия зарегистрированных потоков
	s.mutex.RLock()
	streamsCount := len(s.streams)
	streamsList := make([]string, 0, streamsCount)
	for route := range s.streams {
		streamsList = append(streamsList, route)
	}
	s.mutex.RUnlock()
	
	// Логирование статуса
	if streamsCount == 0 {
		log.Printf("ПРЕДУПРЕЖДЕНИЕ: Нет зарегистрированных потоков, но сервер работает")
	} else {
		log.Printf("Статус healthz: %d потоков зарегистрировано. Маршруты: %v", streamsCount, streamsList)
	}
	
	// Всегда возвращаем успешный ответ, так как потоки могут настраиваться асинхронно
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
	
	// Принудительно отправляем ответ
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	
	// Дополнительное логирование успешного ответа
	log.Printf("Отправлен успешный ответ healthz клиенту %s", r.RemoteAddr)
}

// readyzHandler проверяет готовность к работе
func (s *Server) readyzHandler(w http.ResponseWriter, r *http.Request) {
	// Логирование запроса readyz
	log.Printf("Получен запрос readyz от %s (URI: %s)", r.RemoteAddr, r.RequestURI)
	
	// Добавляем заголовки для предотвращения кеширования
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	
	// Проверка, есть ли хотя бы один поток
	s.mutex.RLock()
	streamsCount := len(s.streams)
	streamsList := make([]string, 0, streamsCount)
	for route := range s.streams {
		streamsList = append(streamsList, route)
	}
	s.mutex.RUnlock()
	
	// Логирование статуса 
	log.Printf("Статус readyz: %d потоков. Маршруты: %v", streamsCount, streamsList)

	// Всегда возвращаем OK для readyz, чтобы избежать перезапусков контейнера
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(fmt.Sprintf("Ready - %d streams registered", streamsCount)))
	
	// Отправка данных немедленно
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	
	// Дополнительное логирование успешного ответа
	log.Printf("Отправлен успешный ответ readyz клиенту %s", r.RemoteAddr)
}

// streamsHandler возвращает информацию о всех доступных потоках
func (s *Server) streamsHandler(w http.ResponseWriter, r *http.Request) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	s.trackMutex.RLock()
	defer s.trackMutex.RUnlock()

	type streamInfo struct {
		Route        string `json:"route"`
		Listeners    int    `json:"listeners"`
		CurrentTrack string `json:"current_track"`
	}

	streams := make([]streamInfo, 0, len(s.streams))
	for route, stream := range s.streams {
		info := streamInfo{
			Route:        route,
			Listeners:    stream.GetClientCount(),
			CurrentTrack: s.currentTracks[route],
		}
		streams = append(streams, info)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"streams": streams,
	})
}

// reloadPlaylistHandler перезагружает плейлист
func (s *Server) reloadPlaylistHandler(w http.ResponseWriter, r *http.Request) {
	route := r.URL.Query().Get("route")

	if route != "" {
		// Перезагрузка конкретного плейлиста
		s.mutex.RLock()
		playlist, exists := s.playlists[route]
		s.mutex.RUnlock()

		if !exists {
			errorMsg := fmt.Sprintf("Поток %s не найден", route)
			log.Printf("ОШИБКА: %s", errorMsg)
			sentry.CaptureMessage(errorMsg) // Сохраняем, так как это ошибка
			http.Error(w, errorMsg, http.StatusNotFound)
			return
		}

		if err := playlist.Reload(); err != nil {
			errorMsg := fmt.Sprintf("Ошибка перезагрузки плейлиста: %s", err)
			sentry.CaptureException(fmt.Errorf("ошибка перезагрузки плейлиста для %s: %w", route, err))
			http.Error(w, errorMsg, http.StatusInternalServerError)
			return
		}

		log.Printf("Плейлист для потока %s перезагружен", route)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(fmt.Sprintf("Плейлист для потока %s перезагружен", route)))
	} else {
		// Перезагрузка всех плейлистов
		s.mutex.RLock()
		playlists := make([]PlaylistManager, 0, len(s.playlists))
		for _, playlist := range s.playlists {
			playlists = append(playlists, playlist)
		}
		s.mutex.RUnlock()

		for _, playlist := range playlists {
			if err := playlist.Reload(); err != nil {
				errorMsg := fmt.Sprintf("Ошибка перезагрузки плейлиста: %s", err)
				sentry.CaptureException(fmt.Errorf("ошибка перезагрузки плейлиста: %w", err))
				http.Error(w, errorMsg, http.StatusInternalServerError)
				return
			}
		}

		log.Printf("Все плейлисты перезагружены")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Все плейлисты перезагружены"))
	}
}

// nowPlayingHandler возвращает информацию о текущем треке
func (s *Server) nowPlayingHandler(w http.ResponseWriter, r *http.Request) {
	route := r.URL.Query().Get("route")

	s.trackMutex.RLock()
	defer s.trackMutex.RUnlock()

	// Устанавливаем правильные заголовки для Unicode
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if route != "" {
		// Информация о конкретном потоке
		if track, exists := s.currentTracks[route]; exists {
			// Только логгирование, не отправляем в Sentry
			log.Printf("Запрос информации о треке для %s: %s", route, track)
			
			// Отправляем JSON-ответ
			json.NewEncoder(w).Encode(map[string]string{
				"route": route,
				"track": track,
			})
		} else {
			errorMsg := fmt.Sprintf("Поток %s не найден", route)
			log.Printf("ОШИБКА: %s", errorMsg)
			sentry.CaptureMessage(errorMsg) // Сохраняем, так как это ошибка
			http.Error(w, errorMsg, http.StatusNotFound)
		}
	} else {
		// Информация о всех потоках
		// Только логгирование, не отправляем в Sentry
		log.Printf("Запрос информации о всех текущих треках")
		
		// Отправляем JSON-ответ
		json.NewEncoder(w).Encode(s.currentTracks)
	}
}

// StreamAudioHandler создаёт HTTP обработчик для стриминга аудио
func (s *Server) StreamAudioHandler(route string) http.HandlerFunc {
	log.Printf("Создание обработчика аудиопотока для маршрута %s", route)
	
	contentType := ""
	switch s.streamFormat {
	case "mp3":
		contentType = "audio/mpeg"
	case "aac":
		contentType = "audio/aac"
	case "ogg":
		contentType = "audio/ogg"
	default:
		contentType = "audio/mpeg"
	}
	
	log.Printf("Формат аудиопотока для маршрута %s: %s (MIME: %s)", route, s.streamFormat, contentType)

	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Получен запрос на аудиопоток %s от %s", route, r.RemoteAddr)
		
		s.mutex.RLock()
		stream, exists := s.streams[route]
		s.mutex.RUnlock()

		if !exists {
			errorMsg := fmt.Sprintf("Поток %s не найден", route)
			log.Printf("ОШИБКА: %s", errorMsg)
			sentry.CaptureMessage(errorMsg) // Сохраняем, так как это ошибка
			http.Error(w, errorMsg, http.StatusNotFound)
			return
		}
		
		log.Printf("Поток %s найден, настройка заголовков для стриминга", route)

		// Настройка заголовков для стриминга - ВАЖНО!
		// 1. Content-Type должен соответствовать формату аудиопотока
		w.Header().Set("Content-Type", contentType)
		// 2. Отключаем кеширование для live-стрима
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		// 3. Защита от угадывания типа контента
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// НЕ устанавливаем Transfer-Encoding: chunked, Go сделает это сам
		// НЕ устанавливаем Connection: keep-alive, это базовое поведение HTTP/1.1
		
		// ВАЖНО: НЕ устанавливать Content-Length для стримов, иначе браузер будет ждать точное количество байт

		// Проверка поддержки Flusher
		flusher, ok := w.(http.Flusher)
		if !ok {
			errorMsg := "Streaming not supported"
			log.Printf("ОШИБКА: %s", errorMsg)
			sentry.CaptureMessage(errorMsg) // Сохраняем, так как это ошибка
			http.Error(w, errorMsg, http.StatusInternalServerError)
			return
		}
		
		// Немедленно сбрасываем заголовки - КРИТИЧЕСКИ ВАЖНО!
		// Это сигнализирует браузеру, что начинается поток
		flusher.Flush()
		
		log.Printf("Добавление клиента к потоку %s...", route)

		// Получаем канал для данных и ID клиента
		clientCh, clientID, err := stream.AddClient()
		if err != nil {
			log.Printf("Ошибка при добавлении клиента к потоку %s: %s", route, err)
			sentry.CaptureException(err) // Сохраняем, так как это ошибка
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer stream.RemoveClient(clientID)

		// Обновляем метрики ТОЛЬКО ПОСЛЕ успешного подключения и отправки заголовков
		listenerCount.WithLabelValues(route).Inc()
		defer listenerCount.WithLabelValues(route).Dec()

		// Логирование подключения клиента
		remoteAddr := r.RemoteAddr
		log.Printf("Клиент подключен к потоку %s: %s (ID: %d)", route, remoteAddr, clientID)

		// Проверяем закрытие соединения
		clientClosed := r.Context().Done()
		
		log.Printf("Начало отправки данных клиенту %d для потока %s", clientID, route)

		// Счетчик для отслеживания переданных данных
		var totalBytesSent int64
		var lastLogTime time.Time = time.Now()
		const logEveryBytes = 1024 * 1024 // Логировать каждый переданный мегабайт
		
		// Отправляем данные клиенту
		for {
			select {
			case <-clientClosed:
				// Клиент отключился
				log.Printf("Клиент отключился от потока %s: %s (ID: %d). Всего отправлено: %d байт", 
					route, remoteAddr, clientID, totalBytesSent)
				return
			case data, ok := <-clientCh:
				if !ok {
					// Канал закрыт
					log.Printf("Канал закрыт для клиента %s (ID: %d). Всего отправлено: %d байт", 
						remoteAddr, clientID, totalBytesSent)
					return
				}
				
				// Отправка данных клиенту
				n, err := w.Write(data)
				if err != nil {
					log.Printf("Ошибка при отправке данных клиенту %d: %s. Всего отправлено: %d байт", 
						clientID, err, totalBytesSent)
					sentry.CaptureException(fmt.Errorf("ошибка при отправке данных клиенту %d: %w", clientID, err))
					return
				}
				
				// Обновляем счетчик и метрики
				totalBytesSent += int64(n)
				bytesSent.WithLabelValues(route).Add(float64(n))
				
				// Периодически логируем количество отправленных данных
				if totalBytesSent >= logEveryBytes && time.Since(lastLogTime) > 5*time.Second {
					log.Printf("Клиенту %d (IP: %s) отправлено %d Мб данных", 
						clientID, remoteAddr, totalBytesSent/1024/1024)
					lastLogTime = time.Now()
				}
				
				// ОБЯЗАТЕЛЬНО вызываем Flush после КАЖДОЙ отправки данных!
				// Это гарантирует, что данные немедленно отправляются клиенту
				flusher.Flush()
			}
		}
	}
}

// ReloadPlaylist перезагружает плейлист для указанного маршрута
func (s *Server) ReloadPlaylist(route string) error {
	s.mutex.RLock()
	playlist, exists := s.playlists[route]
	s.mutex.RUnlock()

	if !exists {
		return fmt.Errorf("плейлист для маршрута %s не найден", route)
	}

	return playlist.Reload()
} 