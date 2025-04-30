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
}

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
	log.Printf("Дополнительные директории маршрутов:")
	for route, dir := range config.DirectoryRoutes {
		log.Printf("  - Маршрут '%s' -> Директория '%s'", route, dir)
	}
	log.Printf("============================================")

	// Создание HTTP сервера
	server := httpServer.NewServer(config.StreamFormat, config.MaxClients)

	// Создание менеджера радиостанций
	stationManager := radio.NewRadioStationManager()

	// Настройка маршрутов для аудиопотоков
	configureAudioRoutes(server, stationManager, config)

	// Создание HTTP сервера
	httpSrv := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", config.Port), // Явно указываем, что слушаем на всех интерфейсах
		Handler: server.Handler(),
		// Увеличиваем таймауты для обработки запросов
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Запуск сервера в горутине
	go func() {
		log.Printf("Сервер запущен и слушает на 0.0.0.0:%d", config.Port)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Ошибка запуска сервера: %s", err)
			sentry.CaptureException(err)
		}
	}()

	// Отдельная горутина для проверки доступности сервера
	go func() {
		// Ждем некоторое время, чтобы сервер успел запуститься
		time.Sleep(3 * time.Second)
		
		// Проверяем доступность сервера
		for i := 0; i < 3; i++ {
			resp, err := http.Get(fmt.Sprintf("http://localhost:%d/healthz", config.Port))
			if err != nil {
				log.Printf("Ошибка при проверке доступности сервера (попытка %d/3): %s", i+1, err)
				time.Sleep(2 * time.Second)
				continue
			}
			
			if resp.StatusCode == http.StatusOK {
				log.Printf("Сервер успешно ответил на запрос healthz")
				resp.Body.Close()
				break
			}
			
			log.Printf("Сервер вернул неожиданный статус: %d", resp.StatusCode)
			resp.Body.Close()
			time.Sleep(2 * time.Second)
		}
	}()

	// Настройка грациозного завершения
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	sig := <-quit

	log.Printf("Получен сигнал: %v, выполняется грациозное завершение...", sig)

	// Обработка SIGHUP для перезагрузки плейлистов
	if sig == syscall.SIGHUP {
		log.Println("Получен SIGHUP, перезагрузка плейлистов...")
		// TODO: Реализовать перезагрузку плейлистов без остановки сервера
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

// configureAudioRoutes настраивает маршруты для аудиопотоков
func configureAudioRoutes(server *httpServer.Server, stationManager *radio.RadioStationManager, config *Config) {
	// Не настраиваем маршрут по умолчанию
	// configureRoute(server, stationManager, "/", config.AudioDir, config)

	// Настраиваем маршруты из конфигурации
	for route, dir := range config.DirectoryRoutes {
		// Нормализуем путь маршрута
		if route[0] != '/' {
			route = "/" + route
		}
		configureRoute(server, stationManager, route, dir, config)
	}
	
	// Добавляем перенаправление с корневого маршрута на /humor, если он существует, или первый доступный маршрут
	redirectTo := "/humor" // по умолчанию перенаправляем на /humor
	if _, exists := config.DirectoryRoutes["humor"]; !exists {
		// Если /humor не существует, берем первый маршрут из конфигурации
		for route := range config.DirectoryRoutes {
			redirectTo = "/" + route
			if redirectTo[0] != '/' {
				redirectTo = "/" + redirectTo
			}
			break
		}
	}
	
	// Добавляем обработчик для корневого маршрута
	server.Handler().(*mux.Router).HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Перенаправление с / на %s", redirectTo)
		http.Redirect(w, r, redirectTo, http.StatusSeeOther)
	}).Methods("GET")
}

// configureRoute настраивает один маршрут для аудиопотока
func configureRoute(server *httpServer.Server, stationManager *radio.RadioStationManager, route, dir string, config *Config) {
	// Создаём директорию, если она не существует
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		log.Printf("Создание директории для маршрута %s: %s", route, dir)
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Printf("Ошибка при создании директории %s: %s", dir, err)
			sentry.CaptureException(fmt.Errorf("ошибка при создании директории %s: %w", dir, err))
			return
		}
	}

	// Создаём менеджер плейлиста
	pl, err := playlist.NewPlaylist(dir, nil)
	if err != nil {
		log.Printf("Ошибка при создании плейлиста для маршрута %s: %s", route, err)
		sentry.CaptureException(fmt.Errorf("ошибка при создании плейлиста для маршрута %s: %w", route, err))
		return
	}

	// Создаём аудио стример
	streamer := audio.NewStreamer(config.BufferSize, config.MaxClients, config.StreamFormat, config.Bitrate)

	// Добавляем радиостанцию
	stationManager.AddStation(route, streamer, pl)

	// Регистрируем аудиопоток на HTTP сервере
	server.RegisterStream(route, streamer, pl)

	// Регистрируем обработчик для маршрута
	server.Handler().(*mux.Router).HandleFunc(route, server.StreamAudioHandler(route)).Methods("GET")
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

	// Проверяем наличие основных маршрутов (humor, science)
	// Если их нет - добавляем явно
	if _, exists := config.DirectoryRoutes["humor"]; !exists {
		config.DirectoryRoutes["humor"] = "/app/humor"
		log.Printf("Добавлен маршрут по умолчанию: 'humor' -> '/app/humor'")
	}
	
	if _, exists := config.DirectoryRoutes["science"]; !exists {
		config.DirectoryRoutes["science"] = "/app/science"
		log.Printf("Добавлен маршрут по умолчанию: 'science' -> '/app/science'")
	}

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