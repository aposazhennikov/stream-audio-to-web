package http

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	GetHistory() []interface{} // Получение истории треков
	GetStartTime() time.Time   // Получение времени запуска
	PreviousTrack() interface{} // Переключение на предыдущий трек
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
	statusPassword  string // Пароль для доступа к странице /status
	stationManager  interface { RestartPlayback(string) bool } // Интерфейс для перезапуска воспроизведения
}

// NewServer создаёт новый HTTP сервер
func NewServer(streamFormat string, maxClients int) *Server {
	// Получаем пароль для страницы статуса из переменной окружения
	statusPassword := getEnvOrDefault("STATUS_PASSWORD", "1234554321")
	
	server := &Server{
		router:         mux.NewRouter(),
		streams:        make(map[string]StreamHandler),
		playlists:      make(map[string]PlaylistManager),
		streamFormat:   streamFormat,
		maxClients:     maxClients,
		currentTracks:  make(map[string]string),
		statusPassword: statusPassword,
	}

	// Настройка маршрутов
	server.setupRoutes()
	
	// Передаем метрику trackSecondsTotal напрямую в пакет audio
	audio.SetTrackSecondsMetric(trackSecondsTotal)
	log.Printf("ДИАГНОСТИКА: Метрика trackSecondsTotal передана в пакет audio")

	log.Printf("HTTP сервер создан, формат потока: %s, макс. клиентов: %d", streamFormat, maxClients)
	return server
}

// Вспомогательная функция для получения значения переменной окружения
func getEnvOrDefault(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
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
	
	// Добавляем страницу статуса с проверкой пароля
	s.router.HandleFunc("/status", s.statusLoginHandler).Methods("GET")
	s.router.HandleFunc("/status", s.statusLoginSubmitHandler).Methods("POST")
	s.router.HandleFunc("/status-page", s.statusPageHandler).Methods("GET")
	
	// Добавляем обработчики для переключения треков
	s.router.HandleFunc("/next-track/{route}", s.nextTrackHandler).Methods("POST")
	s.router.HandleFunc("/prev-track/{route}", s.prevTrackHandler).Methods("POST")

	// Добавление статических файлов для веб-интерфейса
	s.router.PathPrefix("/web/").Handler(http.StripPrefix("/web/", http.FileServer(http.Dir("./web"))))
	
	// Настройка обработчика 404
	s.router.NotFoundHandler = http.HandlerFunc(s.notFoundHandler)

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

// isConnectionClosedError проверяет, является ли ошибка результатом закрытия соединения клиентом
func isConnectionClosedError(err error) bool {
	if err == nil {
		return false
	}
	
	errMsg := err.Error()
	return strings.Contains(errMsg, "broken pipe") || 
		   strings.Contains(errMsg, "connection reset by peer") || 
		   strings.Contains(errMsg, "use of closed network connection")
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
					if isConnectionClosedError(err) {
						// Только логируем, но не отправляем в Sentry
						log.Printf("Клиент %d отключился: %s. Всего отправлено: %d байт", 
							clientID, err, totalBytesSent)
					} else {
						// Отправляем в Sentry только необычные ошибки
						log.Printf("Ошибка при отправке данных клиенту %d: %s. Всего отправлено: %d байт", 
							clientID, err, totalBytesSent)
						sentry.CaptureException(fmt.Errorf("ошибка при отправке данных клиенту %d: %w", clientID, err))
					}
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

// statusLoginHandler отображает форму для входа на страницу статуса
func (s *Server) statusLoginHandler(w http.ResponseWriter, r *http.Request) {
	// Проверяем, есть ли cookie с правильной авторизацией
	cookie, err := r.Cookie("status_auth")
	if err == nil && cookie.Value == s.statusPassword {
		// Если пользователь авторизован, перенаправляем на основную страницу
		http.Redirect(w, r, "/status-page", http.StatusFound)
		return
	}

	// HTML форма для ввода пароля
	html := `<!DOCTYPE html>
<html>
<head>
    <title>Авторизация для доступа к статусу</title>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <style>
        body {
            font-family: Arial, sans-serif;
            margin: 0;
            padding: 20px;
            background-color: #f4f4f4;
            color: #333;
            display: flex;
            justify-content: center;
            align-items: center;
            min-height: 100vh;
        }
        .login-container {
            background-color: white;
            border-radius: 5px;
            box-shadow: 0 2px 5px rgba(0,0,0,0.1);
            padding: 20px;
            width: 300px;
        }
        h1 {
            color: #2c3e50;
            font-size: 1.5em;
            margin-bottom: 20px;
            text-align: center;
        }
        input[type="password"] {
            width: 100%;
            padding: 10px;
            margin-bottom: 15px;
            border: 1px solid #ddd;
            border-radius: 3px;
            box-sizing: border-box;
        }
        button {
            width: 100%;
            background-color: #3498db;
            color: white;
            border: none;
            padding: 10px;
            border-radius: 3px;
            cursor: pointer;
        }
        button:hover {
            background-color: #2980b9;
        }
        .error-message {
            color: #e74c3c;
            margin-bottom: 15px;
            text-align: center;
        }
    </style>
</head>
<body>
    <div class="login-container">
        <h1>Доступ к статусу потоков</h1>
        <form method="post" action="/status">
            <input type="password" name="password" placeholder="Введите пароль" required autofocus>
            <button type="submit">Войти</button>
        </form>
    </div>
</body>
</html>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

// statusLoginSubmitHandler обрабатывает отправку формы для входа
func (s *Server) statusLoginSubmitHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	password := r.FormValue("password")
	
	if password == s.statusPassword {
		// Устанавливаем cookie с авторизацией
		cookie := http.Cookie{
			Name:     "status_auth",
			Value:    s.statusPassword,
			Path:     "/",
			HttpOnly: true,
			MaxAge:   3600, // 1 час
		}
		http.SetCookie(w, &cookie)
		
		// Перенаправляем на страницу статуса
		http.Redirect(w, r, "/status-page", http.StatusFound)
		return
	}
	
	// В случае неверного пароля возвращаем страницу логина с сообщением об ошибке
	html := `<!DOCTYPE html>
<html>
<head>
    <title>Авторизация для доступа к статусу</title>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <style>
        body {
            font-family: Arial, sans-serif;
            margin: 0;
            padding: 20px;
            background-color: #f4f4f4;
            color: #333;
            display: flex;
            justify-content: center;
            align-items: center;
            min-height: 100vh;
        }
        .login-container {
            background-color: white;
            border-radius: 5px;
            box-shadow: 0 2px 5px rgba(0,0,0,0.1);
            padding: 20px;
            width: 300px;
        }
        h1 {
            color: #2c3e50;
            font-size: 1.5em;
            margin-bottom: 20px;
            text-align: center;
        }
        input[type="password"] {
            width: 100%;
            padding: 10px;
            margin-bottom: 15px;
            border: 1px solid #ddd;
            border-radius: 3px;
            box-sizing: border-box;
        }
        button {
            width: 100%;
            background-color: #3498db;
            color: white;
            border: none;
            padding: 10px;
            border-radius: 3px;
            cursor: pointer;
        }
        button:hover {
            background-color: #2980b9;
        }
        .error-message {
            color: #e74c3c;
            margin-bottom: 15px;
            text-align: center;
        }
    </style>
</head>
<body>
    <div class="login-container">
        <h1>Доступ к статусу потоков</h1>
        <div class="error-message">Неверный пароль</div>
        <form method="post" action="/status">
            <input type="password" name="password" placeholder="Введите пароль" required autofocus>
            <button type="submit">Войти</button>
        </form>
    </div>
</body>
</html>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

// checkAuth проверяет авторизацию для доступа к странице статуса
func (s *Server) checkAuth(r *http.Request) bool {
	cookie, err := r.Cookie("status_auth")
	return err == nil && cookie.Value == s.statusPassword
}

// statusPageHandler отображает страницу со статусом всех потоков
func (s *Server) statusPageHandler(w http.ResponseWriter, r *http.Request) {
	// Проверяем авторизацию
	if !s.checkAuth(r) {
		http.Redirect(w, r, "/status", http.StatusFound)
		return
	}

	s.mutex.RLock()
	// Копируем потоки и плейлисты
	streams := make(map[string]StreamHandler)
	for k, v := range s.streams {
		streams[k] = v
	}
	
	playlists := make(map[string]PlaylistManager)
	for k, v := range s.playlists {
		playlists[k] = v
	}
	s.mutex.RUnlock()
	
	s.trackMutex.RLock()
	currentTracks := make(map[string]string)
	for k, v := range s.currentTracks {
		currentTracks[k] = v
	}
	s.trackMutex.RUnlock()
	
	// Сортируем ключи маршрутов для стабильного порядка отображения
	var routes []string
	for route := range streams {
		routes = append(routes, route)
	}
	sort.Strings(routes)
	
	// HTML заголовок
	html := `<!DOCTYPE html>
<html>
<head>
    <title>Аудио Стример - Статус</title>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <style>
        body {
            font-family: Arial, sans-serif;
            margin: 0;
            padding: 20px;
            background-color: #f4f4f4;
            color: #333;
        }
        h1 {
            color: #2c3e50;
            border-bottom: 2px solid #3498db;
            padding-bottom: 10px;
        }
        .stream-container {
            background-color: white;
            border-radius: 5px;
            box-shadow: 0 2px 5px rgba(0,0,0,0.1);
            margin-bottom: 20px;
            padding: 15px;
        }
        .stream-header {
            font-size: 1.5em;
            color: #2980b9;
            margin-bottom: 10px;
        }
        .status-info {
            margin-bottom: 10px;
        }
        .track-list {
            display: none;
            margin-top: 10px;
            border: 1px solid #ddd;
            padding: 10px;
            max-height: 300px;
            overflow-y: auto;
        }
        .track-list ul {
            list-style-type: none;
            padding: 0;
        }
        .track-list li {
            padding: 5px 0;
            border-bottom: 1px solid #eee;
        }
        button {
            background-color: #3498db;
            color: white;
            border: none;
            padding: 5px 10px;
            border-radius: 3px;
            cursor: pointer;
            margin-right: 5px;
        }
        button:hover {
            background-color: #2980b9;
        }
        .button-group {
            margin-top: 10px;
            display: flex;
            gap: 5px;
        }
        .error-container {
            background-color: #f8d7da;
            color: #721c24;
            padding: 10px;
            margin-bottom: 20px;
            border-radius: 5px;
        }
    </style>
</head>
<body>
    <h1>Статус Аудио Потоков</h1>
`

	// Для каждого потока в отсортированном порядке
	for _, route := range routes {
		stream := streams[route]
		playlist, exists := playlists[route]
		if !exists {
			continue
		}
		
		currentTrack := "Неизвестно"
		if track, exists := currentTracks[route]; exists {
			currentTrack = track
		}
		
		// Получаем историю треков
		history := playlist.GetHistory()
		historyHtml := "<ul>"
		// История треков в обратном порядке (новые сверху)
		for i := len(history) - 1; i >= 0; i-- {
			// Используем type assertion для получения имени трека
			if track, ok := history[i].(interface{ GetPath() string }); ok {
				trackPath := track.GetPath()
				trackName := filepath.Base(trackPath)
				historyHtml += "<li>" + trackName + "</li>"
			}
		}
		historyHtml += "</ul>"
		
		// Форматируем время запуска
		startTime := playlist.GetStartTime().Format("02.01.2006 15:04:05 MST")
		
		// Получаем ID маршрута для JS функций, удаляя ведущий слеш
		routeID := route[1:]
		
		// HTML для потока
		html += fmt.Sprintf(`
    <div class="stream-container">
        <div class="stream-header"><a href="%s" target="_blank" style="text-decoration:none; color:inherit;">%s</a></div>
        <div class="status-info">Запущено: %s</div>
        <div class="status-info">Текущий трек: %s</div>
        <div class="status-info">Количество слушателей: %d</div>
        <div class="button-group">
            <form method="post" action="/prev-track/%s" style="display: inline;">
                <button type="submit">Переключить назад</button>
            </form>
            <form method="post" action="/next-track/%s" style="display: inline;">
                <button type="submit">Переключить вперед</button>
            </form>
            <button onclick="toggleTrackList('%s')">Показать историю треков</button>
        </div>
        <div id="track-list-%s" class="track-list">
            %s
        </div>
    </div>`, route, route, startTime, currentTrack, stream.GetClientCount(), routeID, routeID, routeID, routeID, historyHtml)
	}
	
	// JavaScript и закрывающие HTML теги
	html += `
    <script>
        function toggleTrackList(id) {
            const trackList = document.getElementById('track-list-' + id);
            const isVisible = trackList.style.display === 'block';
            trackList.style.display = isVisible ? 'none' : 'block';
            
            // Изменяем текст кнопки
            const buttons = trackList.previousElementSibling.querySelectorAll('button');
            const historyButton = buttons[buttons.length - 1];
            historyButton.textContent = isVisible ? 'Показать историю треков' : 'Скрыть историю треков';
        }

        // Функция для автоматического обновления плеера при изменении трека
        const audioPlayers = {};
        const currentTracks = {};

        // Функция для получения текущего трека для каждого маршрута
        function checkCurrentTracks() {
            fetch('/now-playing')
                .then(response => response.json())
                .then(tracks => {
                    Object.keys(tracks).forEach(route => {
                        // Если трек изменился, перезагружаем плеер
                        if (currentTracks[route] !== tracks[route]) {
                            console.log('Трек изменился на маршруте ' + route + ': ' + tracks[route]);
                            currentTracks[route] = tracks[route];
                            
                            // Если у нас открыт этот маршрут, перезагружаем плеер
                            if (window.location.pathname === route) {
                                console.log('Перезагрузка аудиоплеера для ' + route);
                                const audio = document.querySelector('audio');
                                if (audio) {
                                    // Сохраняем текущую громкость
                                    const volume = audio.volume;
                                    // Запоминаем URL для дальнейшего использования
                                    const originalSrc = audio.src;
                                    // Останавливаем текущий плеер
                                    audio.pause();
                                    // Перезагружаем источник с новым временным параметром для обхода кеширования
                                    audio.src = originalSrc + '?t=' + new Date().getTime();
                                    // Восстанавливаем громкость
                                    audio.volume = volume;
                                    // Запускаем воспроизведение
                                    audio.play().catch(e => console.error('Ошибка воспроизведения:', e));
                                }
                            }
                        }
                    });
                })
                .catch(error => console.error('Ошибка при проверке текущего трека:', error));
        }

        // Проверяем текущий трек каждые 2 секунды
        setInterval(checkCurrentTracks, 2000);
        
        // Инициализация при загрузке страницы
        window.addEventListener('DOMContentLoaded', () => {
            // Начальная загрузка информации о треках
            checkCurrentTracks();
        });
    </script>
</body>
</html>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

// nextTrackHandler обрабатывает запрос на переключение трека вперед
func (s *Server) nextTrackHandler(w http.ResponseWriter, r *http.Request) {
	// Проверяем авторизацию
	if !s.checkAuth(r) {
		http.Redirect(w, r, "/status", http.StatusFound)
		return
	}
	
	// Получаем маршрут из URL
	vars := mux.Vars(r)
	route := "/" + vars["route"]
	
	// Блокировка для доступа к плейлисту
	s.mutex.RLock()
	playlist, exists := s.playlists[route]
	s.mutex.RUnlock()
	
	if !exists {
		http.Error(w, "Маршрут не найден", http.StatusNotFound)
		return
	}
	
	// Переключаем трек (игнорируем возвращаемое значение)
	nextTrack := playlist.NextTrack()
	log.Printf("ДИАГНОСТИКА: Ручное переключение на следующий трек для маршрута %s", route)
	
	// Вызываем перезапуск воспроизведения, если менеджер станций доступен
	success := false
	newTrackName := "Неизвестно"
	
	if s.stationManager != nil {
		success = s.stationManager.RestartPlayback(route)
		if success {
			log.Printf("ДИАГНОСТИКА: Перезапуск воспроизведения для маршрута %s выполнен успешно", route)
			
			// Получаем имя нового трека
			if track, ok := nextTrack.(interface{ GetPath() string }); ok {
				newTrackName = filepath.Base(track.GetPath())
				
				// Немедленно обновляем информацию о текущем треке
				s.trackMutex.Lock()
				s.currentTracks[route] = newTrackName
				s.trackMutex.Unlock()
				
				log.Printf("ДИАГНОСТИКА: Немедленно обновлена информация о текущем треке для %s: %s", route, newTrackName)
			}
		} else {
			log.Printf("ОШИБКА: Не удалось перезапустить воспроизведение для маршрута %s", route)
		}
	} else {
		log.Printf("ПРЕДУПРЕЖДЕНИЕ: Менеджер станций не установлен, перезапуск воспроизведения невозможен")
	}
	
	// Определяем, является ли запрос AJAX-запросом
	isAjax := r.Header.Get("X-Requested-With") == "XMLHttpRequest" || r.URL.Query().Get("ajax") == "1"
	
	if isAjax {
		// Отправляем JSON-ответ для AJAX-запросов
		w.Header().Set("Content-Type", "application/json")
		response := map[string]interface{}{
			"success": success,
			"route":   route,
			"track":   newTrackName,
		}
		json.NewEncoder(w).Encode(response)
	} else {
		// Перенаправляем обратно на страницу статуса для обычных запросов
		http.Redirect(w, r, "/status-page", http.StatusFound)
	}
}

// prevTrackHandler обрабатывает запрос на переключение трека назад
func (s *Server) prevTrackHandler(w http.ResponseWriter, r *http.Request) {
	// Проверяем авторизацию
	if !s.checkAuth(r) {
		http.Redirect(w, r, "/status", http.StatusFound)
		return
	}
	
	// Получаем маршрут из URL
	vars := mux.Vars(r)
	route := "/" + vars["route"]
	
	// Блокировка для доступа к плейлисту
	s.mutex.RLock()
	playlist, exists := s.playlists[route]
	s.mutex.RUnlock()
	
	if !exists {
		http.Error(w, "Маршрут не найден", http.StatusNotFound)
		return
	}
	
	// Переключаем трек (игнорируем возвращаемое значение)
	prevTrack := playlist.PreviousTrack()
	log.Printf("ДИАГНОСТИКА: Ручное переключение на предыдущий трек для маршрута %s", route)
	
	// Вызываем перезапуск воспроизведения, если менеджер станций доступен
	success := false
	newTrackName := "Неизвестно"
	
	if s.stationManager != nil {
		success = s.stationManager.RestartPlayback(route)
		if success {
			log.Printf("ДИАГНОСТИКА: Перезапуск воспроизведения для маршрута %s выполнен успешно", route)
			
			// Получаем имя нового трека
			if track, ok := prevTrack.(interface{ GetPath() string }); ok {
				newTrackName = filepath.Base(track.GetPath())
				
				// Немедленно обновляем информацию о текущем треке
				s.trackMutex.Lock()
				s.currentTracks[route] = newTrackName
				s.trackMutex.Unlock()
				
				log.Printf("ДИАГНОСТИКА: Немедленно обновлена информация о текущем треке для %s: %s", route, newTrackName)
			}
		} else {
			log.Printf("ОШИБКА: Не удалось перезапустить воспроизведение для маршрута %s", route)
		}
	} else {
		log.Printf("ПРЕДУПРЕЖДЕНИЕ: Менеджер станций не установлен, перезапуск воспроизведения невозможен")
	}
	
	// Определяем, является ли запрос AJAX-запросом
	isAjax := r.Header.Get("X-Requested-With") == "XMLHttpRequest" || r.URL.Query().Get("ajax") == "1"
	
	if isAjax {
		// Отправляем JSON-ответ для AJAX-запросов
		w.Header().Set("Content-Type", "application/json")
		response := map[string]interface{}{
			"success": success,
			"route":   route,
			"track":   newTrackName,
		}
		json.NewEncoder(w).Encode(response)
	} else {
		// Перенаправляем обратно на страницу статуса для обычных запросов
		http.Redirect(w, r, "/status-page", http.StatusFound)
	}
}

// notFoundHandler обрабатывает запросы к несуществующим маршрутам
func (s *Server) notFoundHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("404 - маршрут не найден: %s", r.URL.Path)
	
	// HTML для страницы 404
	html := `<!DOCTYPE html>
<html>
<head>
    <title>404 - Страница не найдена</title>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <style>
        body {
            font-family: Arial, sans-serif;
            margin: 0;
            padding: 20px;
            background-color: #f4f4f4;
            color: #333;
            text-align: center;
        }
        .error-container {
            background-color: white;
            border-radius: 5px;
            box-shadow: 0 2px 5px rgba(0,0,0,0.1);
            margin: 50px auto;
            padding: 30px;
            max-width: 500px;
        }
        h1 {
            color: #e74c3c;
            font-size: 4em;
            margin: 0;
        }
        p {
            font-size: 1.2em;
            margin: 20px 0;
        }
        .back-link {
            display: inline-block;
            margin-top: 20px;
            background-color: #3498db;
            color: white;
            padding: 10px 20px;
            text-decoration: none;
            border-radius: 3px;
        }
        .back-link:hover {
            background-color: #2980b9;
        }
    </style>
</head>
<body>
    <div class="error-container">
        <h1>404</h1>
        <p>Страница не найдена</p>
        <p>Запрошенный ресурс не существует.</p>
        <a href="/status" class="back-link">Вернуться на главную</a>
    </div>
</body>
</html>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte(html))
}

// SetStationManager устанавливает менеджер радиостанций для перезапуска воспроизведения
func (s *Server) SetStationManager(manager interface { RestartPlayback(string) bool }) {
	s.stationManager = manager
} 