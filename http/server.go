package http

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"sync"

	"github.com/getsentry/sentry-go"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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
)

func init() {
	// Регистрация метрик Prometheus
	prometheus.MustRegister(listenerCount)
	prometheus.MustRegister(bytesSent)
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

	sentry.CaptureMessage(fmt.Sprintf("HTTP сервер создан, формат потока: %s, макс. клиентов: %d", streamFormat, maxClients))
	return server
}

// Handler возвращает обработчик HTTP запросов
func (s *Server) Handler() http.Handler {
	return s.router
}

// RegisterStream регистрирует новый аудиопоток
func (s *Server) RegisterStream(route string, stream StreamHandler, playlist PlaylistManager) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.streams[route] = stream
	s.playlists[route] = playlist

	// Запуск горутины для отслеживания текущего трека
	go s.trackCurrentTrack(route, stream.GetCurrentTrackChannel())

	sentry.CaptureMessage(fmt.Sprintf("Зарегистрирован аудиопоток: %s", route))
}

// trackCurrentTrack отслеживает текущий трек для указанного потока
func (s *Server) trackCurrentTrack(route string, trackCh <-chan string) {
	for trackPath := range trackCh {
		s.trackMutex.Lock()
		s.currentTracks[route] = filepath.Base(trackPath)
		s.trackMutex.Unlock()
		sentry.CaptureMessage(fmt.Sprintf("Текущий трек для %s: %s", route, filepath.Base(trackPath)))
	}
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

	sentry.CaptureMessage("HTTP маршруты настроены")
}

// healthzHandler возвращает 200 OK, если сервер работает
func (s *Server) healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// readyzHandler проверяет готовность к работе
func (s *Server) readyzHandler(w http.ResponseWriter, r *http.Request) {
	// TODO: Добавить проверки дискового пространства, памяти и доступности директории с плейлистом
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Ready"))
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
			sentry.CaptureMessage(errorMsg)
			http.Error(w, errorMsg, http.StatusNotFound)
			return
		}

		if err := playlist.Reload(); err != nil {
			errorMsg := fmt.Sprintf("Ошибка перезагрузки плейлиста: %s", err)
			sentry.CaptureException(fmt.Errorf("ошибка перезагрузки плейлиста для %s: %w", route, err))
			http.Error(w, errorMsg, http.StatusInternalServerError)
			return
		}

		sentry.CaptureMessage(fmt.Sprintf("Плейлист для потока %s перезагружен", route))
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

		sentry.CaptureMessage("Все плейлисты перезагружены")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Все плейлисты перезагружены"))
	}
}

// nowPlayingHandler возвращает информацию о текущем треке
func (s *Server) nowPlayingHandler(w http.ResponseWriter, r *http.Request) {
	route := r.URL.Query().Get("route")

	s.trackMutex.RLock()
	defer s.trackMutex.RUnlock()

	if route != "" {
		// Информация о конкретном потоке
		if track, exists := s.currentTracks[route]; exists {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"route": route,
				"track": track,
			})
		} else {
			errorMsg := fmt.Sprintf("Поток %s не найден", route)
			sentry.CaptureMessage(errorMsg)
			http.Error(w, errorMsg, http.StatusNotFound)
		}
	} else {
		// Информация о всех потоках
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.currentTracks)
	}
}

// StreamAudioHandler создаёт HTTP обработчик для стриминга аудио
func (s *Server) StreamAudioHandler(route string) http.HandlerFunc {
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

	return func(w http.ResponseWriter, r *http.Request) {
		s.mutex.RLock()
		stream, exists := s.streams[route]
		s.mutex.RUnlock()

		if !exists {
			errorMsg := fmt.Sprintf("Поток %s не найден", route)
			sentry.CaptureMessage(errorMsg)
			http.Error(w, errorMsg, http.StatusNotFound)
			return
		}

		// Настройка заголовков для стриминга
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		// Флаги для контроля состояния соединения
		flusher, ok := w.(http.Flusher)
		if !ok {
			errorMsg := "Streaming not supported"
			sentry.CaptureMessage(errorMsg)
			http.Error(w, errorMsg, http.StatusInternalServerError)
			return
		}

		// Получаем канал для данных и ID клиента
		clientCh, clientID, err := stream.AddClient()
		if err != nil {
			sentry.CaptureException(err)
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer stream.RemoveClient(clientID)

		// Обновляем метрики
		listenerCount.WithLabelValues(route).Inc()
		defer listenerCount.WithLabelValues(route).Dec()

		// Логирование подключения клиента
		remoteAddr := r.RemoteAddr
		sentry.CaptureMessage(fmt.Sprintf("Клиент подключен к потоку %s: %s (ID: %d)", route, remoteAddr, clientID))

		// Проверяем закрытие соединения
		clientClosed := r.Context().Done()

		// Отправляем данные клиенту
		for {
			select {
			case <-clientClosed:
				// Клиент отключился
				sentry.CaptureMessage(fmt.Sprintf("Клиент отключился от потока %s: %s (ID: %d)", route, remoteAddr, clientID))
				return
			case data, ok := <-clientCh:
				if !ok {
					// Канал закрыт
					sentry.CaptureMessage(fmt.Sprintf("Канал закрыт для клиента %s (ID: %d)", remoteAddr, clientID))
					return
				}
				
				// Отправка данных клиенту
				_, err := w.Write(data)
				if err != nil {
					log.Printf("Ошибка при отправке данных клиенту %d: %s", clientID, err)
					sentry.CaptureException(fmt.Errorf("ошибка при отправке данных клиенту %d: %w", clientID, err))
					return
				}
				
				// Обновляем метрики
				bytesSent.WithLabelValues(route).Add(float64(len(data)))
				
				// Отправляем данные немедленно
				flusher.Flush()
			}
		}
	}
} 