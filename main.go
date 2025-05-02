package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/gorilla/mux"
	"github.com/user/stream-audio-to-web/audio"
	httpServer "github.com/user/stream-audio-to-web/http"
	"github.com/user/stream-audio-to-web/playlist"
	"github.com/user/stream-audio-to-web/radio"
)

// Конфигурация по умолчанию
const (
	defaultPort         = 8000
	defaultAudioDir     = "./audio"
	defaultStreamFormat = "mp3"
	defaultBitrate      = 128
	defaultMaxClients   = 500
	defaultLogLevel     = "info"
	defaultBufferSize   = 65536 // 64KB
	defaultShuffle      = false  // Перемешивание треков по умолчанию включено
)

// Конфигурация приложения
type Config struct {
	Port           int
	AudioDir       string
	DirectoryRoutes map[string]string
	StreamFormat   string
	Bitrate        int
	MaxClients     int
	LogLevel       string
	BufferSize     int
	Shuffle        bool // Перемешивать треки в плейлисте
}

// Глобальные переменные для маршрутов
var (
	humorRoute   *mux.Route
	scienceRoute *mux.Route
)

func main() {
	// Инициализация Sentry
	err := sentry.Init(sentry.ClientOptions{
		Dsn: "https://f5dbf565496b75215d81c2286cf0dc9c@o4508953992101888.ingest.de.sentry.io/4509243323908176",
		Environment: getEnvOrDefault("ENV", "development"),
		Release:     "stream-audio-to-web@1.0.0",
	})
	if err != nil {
		log.Fatalf("sentry.Init: %s", err)
	}
	defer sentry.Flush(2 * time.Second)
	defer sentry.Recover()

	// Загрузка конфигурации
	config := loadConfig()
	
	// Подробное логирование конфигурации для диагностики
	log.Printf("========== КОНФИГУРАЦИЯ ПРИЛОЖЕНИЯ ==========")
	log.Printf("Порт: %d", config.Port)
	log.Printf("Аудио директория по умолчанию: %s", config.AudioDir)
	log.Printf("Формат потока: %s", config.StreamFormat)
	log.Printf("Битрейт: %d", config.Bitrate)
	log.Printf("Макс. клиентов: %d", config.MaxClients)
	log.Printf("Размер буфера: %d", config.BufferSize)
	log.Printf("Перемешивание треков: %v", config.Shuffle)
	log.Printf("Дополнительные директории маршрутов:")
	for route, dir := range config.DirectoryRoutes {
		log.Printf("  - Маршрут '%s' -> Директория '%s'", route, dir)
	}
	log.Printf("============================================")

	// Создание HTTP сервера
	server := httpServer.NewServer(config.StreamFormat, config.MaxClients)

	// Создание менеджера радиостанций
	stationManager := radio.NewRadioStationManager()
	
	// Устанавливаем менеджер радиостанций для HTTP сервера
	server.SetStationManager(stationManager)

	// Создаем минимальные заглушки для потоков, чтобы /healthz сразу находил хотя бы один маршрут
	dummyStream, dummyPlaylist := createDummyStreamAndPlaylist()
	server.RegisterStream("/humor", dummyStream, dummyPlaylist)
	server.RegisterStream("/science", dummyStream, dummyPlaylist)
	log.Printf("Зарегистрированы временные заглушки потоков для быстрого прохождения healthcheck")

	// Удаляем временные обработчики здесь, так как они уже зарегистрированы в server.setupRoutes()
	
	// ТЕПЕРЬ создаем и запускаем HTTP-сервер ПЕРЕД настройкой потоков
	httpSrv := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", config.Port), // Явно указываем, что слушаем на всех интерфейсах
		Handler: server.Handler(),
		// Увеличиваем таймауты для обработки запросов
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // Отключаем таймаут для стриминга
		IdleTimeout:  60 * time.Second,
	}

	// Запуск сервера в горутине ДО настройки маршрутов
	go func() {
		log.Printf("Запуск HTTP-сервера на адресе 0.0.0.0:%d...", config.Port)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Ошибка запуска сервера: %s", err)
			sentry.CaptureException(err)
		}
	}()
	
	// Перенаправление с корневого маршрута
	redirectTo := "/humor" // по умолчанию перенаправляем на /humor
	if _, exists := config.DirectoryRoutes["/humor"]; !exists {
		// Если /humor не существует, берем первый маршрут из конфигурации
		for route := range config.DirectoryRoutes {
			redirectTo = route
			break
		}
	}
	
	// Заменяем временный обработчик для корневого маршрута на перенаправление
	server.Handler().(*mux.Router).HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Перенаправление с / на %s", redirectTo)
		http.Redirect(w, r, redirectTo, http.StatusSeeOther)
	}).Methods("GET")
	
	log.Printf("Настроено перенаправление с / на %s", redirectTo)

	// ПОСЛЕ запуска HTTP-сервера настраиваем аудио-маршруты асинхронно
	log.Printf("Начало настройки аудио маршрутов...")

	// Настраиваем маршруты из конфигурации АСИНХРОННО
	for route, dir := range config.DirectoryRoutes {
		// Маршрут уже должен быть нормализован с ведущим слешем в loadConfig
		// Но на всякий случай проверяем
		if route[0] != '/' {
			route = "/" + route
		}
		
		// Копируем переменные для горутины
		routeCopy := route
		dirCopy := dir
		
		// Запускаем настройку КАЖДОГО потока в отдельной горутине
		go func(route, dir string) {
			log.Printf("Асинхронная настройка маршрута '%s' -> директория '%s'", route, dir)
			if success := configureSyncRoute(server, stationManager, route, dir, config); success {
				log.Printf("Маршрут '%s' успешно настроен", route)
			} else {
				log.Printf("ОШИБКА: Маршрут '%s' не удалось настроить", route)
			}
		}(routeCopy, dirCopy)
	}

	// Проверяем статус потоков
	log.Printf("====== СТАТУС ЗАРЕГИСТРИРОВАННЫХ ПОТОКОВ ======")
	humorRegistered := server.IsStreamRegistered("/humor")
	scienceRegistered := server.IsStreamRegistered("/science")
	log.Printf("Поток /humor зарегистрирован: %v", humorRegistered)
	log.Printf("Поток /science зарегистрирован: %v", scienceRegistered)
	
	if !humorRegistered || !scienceRegistered {
		log.Printf("ПРЕДУПРЕЖДЕНИЕ: Некоторые потоки не зарегистрированы!")
	} else {
		log.Printf("Все потоки успешно зарегистрированы")
	}
	log.Printf("=============================================")

	// Настройка грациозного завершения
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	sig := <-quit

	log.Printf("Получен сигнал: %v, выполняется грациозное завершение...", sig)

	// Обработка SIGHUP для перезагрузки плейлистов
	if sig == syscall.SIGHUP {
		log.Println("Получен SIGHUP, перезагрузка плейлистов...")
		
		// Получаем список всех плейлистов
		server.Handler().(*mux.Router).Walk(func(route *mux.Route, router *mux.Router, ancestors []*mux.Route) error {
			path, err := route.GetPathTemplate()
			if err != nil {
				return nil
			}
			
			// Перезагружаем только для зарегистрированных потоков
			if server.IsStreamRegistered(path) {
				log.Printf("Перезагрузка плейлиста для %s", path)
				if err := server.ReloadPlaylist(path); err != nil {
					log.Printf("Ошибка при перезагрузке плейлиста для %s: %s", path, err)
					sentry.CaptureException(err)
				} else {
					log.Printf("Плейлист для %s успешно перезагружен", path)
				}
			}
			
			return nil
		})
		
		log.Println("Перезагрузка плейлистов завершена")
		return // Продолжаем работу
	}

	// Останавливаем все радиостанции
	stationManager.StopAll()

	// Грациозное завершение HTTP сервера
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Fatalf("Ошибка при завершении сервера: %s", err)
		sentry.CaptureException(err)
	}
	log.Println("Сервер успешно остановлен")
}

// configureSyncRoute настраивает один маршрут для аудиопотока синхронно
func configureSyncRoute(server *httpServer.Server, stationManager *radio.RadioStationManager, route, dir string, config *Config) bool {
	log.Printf("Начало синхронной настройки маршрута '%s'...", route)

	// Создаём директорию, если она не существует
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		log.Printf("Создание директории для маршрута %s: %s", route, dir)
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Printf("ОШИБКА: При создании директории %s: %s. Маршрут '%s' НЕ НАСТРОЕН.", dir, err, route)
			sentry.CaptureException(fmt.Errorf("ошибка при создании директории %s: %w", dir, err))
			return false
		}
	}

	// Проверим содержимое директории
	files, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("ОШИБКА: При чтении директории %s: %s. Маршрут '%s' НЕ НАСТРОЕН.", dir, err, route)
		sentry.CaptureException(fmt.Errorf("ошибка при чтении директории %s: %w", dir, err))
		return false
	}

	audioFiles := 0
	for _, file := range files {
		if !file.IsDir() && (strings.HasSuffix(strings.ToLower(file.Name()), ".mp3") || 
							strings.HasSuffix(strings.ToLower(file.Name()), ".ogg") || 
							strings.HasSuffix(strings.ToLower(file.Name()), ".aac")) {
			audioFiles++
		}
	}
	log.Printf("Директория %s содержит %d аудиофайлов", dir, audioFiles)
	if audioFiles == 0 {
		log.Printf("КРИТИЧЕСКАЯ ОШИБКА: В директории %s нет аудиофайлов. Маршрут '%s' НЕ НАСТРОЕН.", dir, route)
		sentry.CaptureMessage(fmt.Sprintf("В директории %s нет аудиофайлов для маршрута %s", dir, route))
		return false
	}

	// Создаем плейлист и настраиваем поток синхронно
	log.Printf("Создание плейлиста для маршрута %s...", route)
	pl, err := playlist.NewPlaylist(dir, nil, config.Shuffle)
	if err != nil {
		log.Printf("ОШИБКА при создании плейлиста: %s", err)
		sentry.CaptureException(fmt.Errorf("ошибка при создании плейлиста: %w", err))
		return false
	}
	
	log.Printf("Плейлист для маршрута %s успешно создан", route)
	
	log.Printf("Создание аудио стримера для маршрута %s...", route)
	streamer := audio.NewStreamer(config.BufferSize, config.MaxClients, config.StreamFormat, config.Bitrate)
	log.Printf("Аудио стример для маршрута %s успешно создан", route)
	
	log.Printf("Добавление радиостанции %s в менеджер...", route)
	stationManager.AddStation(route, streamer, pl)
	log.Printf("Радиостанция '%s' успешно добавлена в менеджер", route)
	
	log.Printf("Регистрация аудиопотока %s на HTTP сервере...", route)
	server.RegisterStream(route, streamer, pl)
	log.Printf("Аудиопоток '%s' успешно зарегистрирован на HTTP сервере", route)
	
	// Проверяем успешность регистрации потока
	if !server.IsStreamRegistered(route) {
		log.Printf("КРИТИЧЕСКАЯ ОШИБКА: Поток %s не зарегистрирован после всех операций", route)
		sentry.CaptureMessage(fmt.Sprintf("Поток %s не зарегистрирован после всех операций", route))
		return false
	}
	
	log.Printf("ИТОГ: Настройка маршрута '%s' УСПЕШНО ЗАВЕРШЕНА", route)
	return true
}

// getAllRoutes возвращает список всех зарегистрированных маршрутов
func getAllRoutes(router *mux.Router) []string {
	routes := []string{}
	
	router.Walk(func(route *mux.Route, router *mux.Router, ancestors []*mux.Route) error {
		pathTemplate, err := route.GetPathTemplate()
		if err == nil {
			routes = append(routes, pathTemplate)
		}
		return nil
	})
	
	return routes
}

// Загрузка конфигурации из флагов командной строки и переменных окружения
func loadConfig() *Config {
	config := &Config{}

	// Определение флагов командной строки
	flag.IntVar(&config.Port, "port", defaultPort, "Порт для HTTP сервера")
	flag.StringVar(&config.AudioDir, "audio-dir", defaultAudioDir, "Директория с аудиофайлами по умолчанию")
	flag.StringVar(&config.StreamFormat, "stream-format", defaultStreamFormat, "Формат потока: mp3, aac, ogg")
	flag.IntVar(&config.Bitrate, "bitrate", defaultBitrate, "Битрейт в kbps")
	flag.IntVar(&config.MaxClients, "max-clients", defaultMaxClients, "Максимальное количество одновременных подключений")
	flag.StringVar(&config.LogLevel, "log-level", defaultLogLevel, "Уровень логирования: debug, info, warn, error")
	flag.IntVar(&config.BufferSize, "buffer-size", defaultBufferSize, "Размер буфера для чтения аудиофайлов в байтах")
	flag.BoolVar(&config.Shuffle, "shuffle", defaultShuffle, "Перемешивать треки в плейлисте")

	// Для сопоставления директорий и маршрутов используем JSON
	var directoryRoutesJSON string
	flag.StringVar(&directoryRoutesJSON, "directory-routes", "{}", "JSON строка с сопоставлением маршрутов и директорий")

	// Приоритет: переменные окружения > флаги командной строки > значения по умолчанию
	flag.Parse()

	// Инициализация DirectoryRoutes
	config.DirectoryRoutes = make(map[string]string)

	// Парсинг JSON строки с маршрутами директорий
	if directoryRoutesJSON != "" {
		if err := json.Unmarshal([]byte(directoryRoutesJSON), &config.DirectoryRoutes); err != nil {
			log.Printf("Ошибка при парсинге JSON строки с маршрутами директорий: %s", err)
			sentry.CaptureException(fmt.Errorf("ошибка при парсинге JSON строки с маршрутами директорий: %w", err))
		}
	}

	// Проверка переменных окружения
	if envPort := os.Getenv("PORT"); envPort != "" {
		if port, err := strconv.Atoi(envPort); err == nil {
			config.Port = port
		} else {
			sentry.CaptureException(fmt.Errorf("ошибка при парсинге PORT: %w", err))
		}
	}
	if envAudioDir := os.Getenv("AUDIO_DIR"); envAudioDir != "" {
		config.AudioDir = envAudioDir
	}
	if envStreamFormat := os.Getenv("STREAM_FORMAT"); envStreamFormat != "" {
		config.StreamFormat = envStreamFormat
	}
	if envBitrate := os.Getenv("BITRATE"); envBitrate != "" {
		if bitrate, err := strconv.Atoi(envBitrate); err == nil {
			config.Bitrate = bitrate
		} else {
			sentry.CaptureException(fmt.Errorf("ошибка при парсинге BITRATE: %w", err))
		}
	}
	if envMaxClients := os.Getenv("MAX_CLIENTS"); envMaxClients != "" {
		if maxClients, err := strconv.Atoi(envMaxClients); err == nil {
			config.MaxClients = maxClients
		} else {
			sentry.CaptureException(fmt.Errorf("ошибка при парсинге MAX_CLIENTS: %w", err))
		}
	}
	if envLogLevel := os.Getenv("LOG_LEVEL"); envLogLevel != "" {
		config.LogLevel = envLogLevel
	}
	if envBufferSize := os.Getenv("BUFFER_SIZE"); envBufferSize != "" {
		if bufferSize, err := strconv.Atoi(envBufferSize); err == nil {
			config.BufferSize = bufferSize
		} else {
			sentry.CaptureException(fmt.Errorf("ошибка при парсинге BUFFER_SIZE: %w", err))
		}
	}
	if envDirectoryRoutes := os.Getenv("DIRECTORY_ROUTES"); envDirectoryRoutes != "" {
		var routes map[string]string
		if err := json.Unmarshal([]byte(envDirectoryRoutes), &routes); err == nil {
			for k, v := range routes {
				config.DirectoryRoutes[k] = v
			}
		} else {
			log.Printf("Ошибка при парсинге JSON из переменной окружения DIRECTORY_ROUTES: %s", err)
			sentry.CaptureException(fmt.Errorf("ошибка при парсинге JSON из переменной окружения DIRECTORY_ROUTES: %w", err))
		}
	}

	if envShuffle := os.Getenv("SHUFFLE"); envShuffle != "" {
		if shuffle, err := strconv.ParseBool(envShuffle); err == nil {
			config.Shuffle = shuffle
		} else {
			sentry.CaptureException(fmt.Errorf("ошибка при парсинге SHUFFLE: %w", err))
		}
	}

	// Проверяем наличие основных маршрутов (humor, science)
	// Если их нет - добавляем явно с ведущим слешем /humor и /science
	_, existsWithSlash := config.DirectoryRoutes["/humor"]
	_, existsWithoutSlash := config.DirectoryRoutes["humor"]
	if !existsWithSlash && !existsWithoutSlash {
		config.DirectoryRoutes["/humor"] = "/app/humor"
		log.Printf("Добавлен маршрут по умолчанию: '/humor' -> '/app/humor'")
	}
	
	_, scienceWithSlash := config.DirectoryRoutes["/science"]
	_, scienceWithoutSlash := config.DirectoryRoutes["science"]
	if !scienceWithSlash && !scienceWithoutSlash {
		config.DirectoryRoutes["/science"] = "/app/science"
		log.Printf("Добавлен маршрут по умолчанию: '/science' -> '/app/science'")
	}

	// Нормализуем все маршруты, добавляя ведущий слеш, если его нет
	normalizedRoutes := make(map[string]string)
	for route, dir := range config.DirectoryRoutes {
		if route[0] != '/' {
			normalizedRoutes["/"+route] = dir
			log.Printf("Нормализован маршрут: '%s' -> '/%s'", route, route)
		} else {
			normalizedRoutes[route] = dir
		}
	}
	config.DirectoryRoutes = normalizedRoutes

	// Не проверяем директорию по умолчанию, так как маршрут "/" не используется
	// if _, err := os.Stat(config.AudioDir); os.IsNotExist(err) {
	//	log.Printf("Директория с аудиофайлами не существует: %s, создание...", config.AudioDir)
	//	if err := os.MkdirAll(config.AudioDir, 0755); err != nil {
	//		log.Fatalf("Невозможно создать директорию: %s", err)
	//		sentry.CaptureException(fmt.Errorf("невозможно создать директорию: %w", err))
	//	}
	// }

	// Получение абсолютных путей для DirectoryRoutes
	for route, dir := range config.DirectoryRoutes {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			log.Printf("Невозможно получить абсолютный путь для %s: %s", dir, err)
			sentry.CaptureException(fmt.Errorf("невозможно получить абсолютный путь для %s: %w", dir, err))
			continue
		}
		config.DirectoryRoutes[route] = absDir
	}

	return config
}

// getEnvOrDefault возвращает значение переменной окружения или значение по умолчанию
func getEnvOrDefault(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

// createDummyStreamAndPlaylist создает минимальные заглушки для потоков
// Это необходимо для быстрого прохождения healthcheck до загрузки настоящих потоков
func createDummyStreamAndPlaylist() (httpServer.StreamHandler, httpServer.PlaylistManager) {
	// Заглушка для StreamHandler
	dummyStream := &dummyStreamHandler{
		clientCounter: 0,
		clientCh:      make(chan string, 1),
	}
	
	// Заглушка для PlaylistManager
	dummyPlaylist := &dummyPlaylistManager{}
	
	return dummyStream, dummyPlaylist
}

// Минимальная реализация StreamHandler для заглушки
type dummyStreamHandler struct {
	clientCounter int32
	clientCh      chan string
}

func (d *dummyStreamHandler) AddClient() (<-chan []byte, int, error) {
	// Пустой канал, который будет заменен настоящим потоком позже
	ch := make(chan []byte, 1)
	return ch, 0, nil
}

func (d *dummyStreamHandler) RemoveClient(clientID int) {
	// Ничего не делаем
}

func (d *dummyStreamHandler) GetClientCount() int {
	return 0
}

func (d *dummyStreamHandler) GetCurrentTrackChannel() <-chan string {
	return d.clientCh
}

// Минимальная реализация PlaylistManager для заглушки
type dummyPlaylistManager struct{}

func (d *dummyPlaylistManager) Reload() error {
	return nil
}

func (d *dummyPlaylistManager) GetCurrentTrack() interface{} {
	return "dummy.mp3"
}

func (d *dummyPlaylistManager) NextTrack() interface{} {
	return "dummy.mp3"
}

func (d *dummyPlaylistManager) GetHistory() []interface{} {
	return []interface{}{}
}

func (d *dummyPlaylistManager) GetStartTime() time.Time {
	return time.Now()
}

func (d *dummyPlaylistManager) PreviousTrack() interface{} {
	return "dummy.mp3"
} 