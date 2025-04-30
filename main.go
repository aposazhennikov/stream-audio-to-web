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
	log.Printf("Дополнительные директории маршрутов:")
	for route, dir := range config.DirectoryRoutes {
		log.Printf("  - Маршрут '%s' -> Директория '%s'", route, dir)
	}
	log.Printf("============================================")

	// Создание HTTP сервера
	server := httpServer.NewServer(config.StreamFormat, config.MaxClients)

	// Создание менеджера радиостанций
	stationManager := radio.NewRadioStationManager()

	// СНАЧАЛА настраиваем базовые маршруты - healthz и корневой маршрут
	// Добавляем временный обработчик healthz, который всегда возвращает 200 OK
	server.Handler().(*mux.Router).HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Получен запрос healthz от %s (URI: %s)", r.RemoteAddr, r.RequestURI)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Server is starting up"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}).Methods("GET")

	// Добавляем временный обработчик readyz, который всегда возвращает 200 OK
	server.Handler().(*mux.Router).HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Получен запрос readyz от %s (URI: %s)", r.RemoteAddr, r.RequestURI)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Server is starting up"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}).Methods("GET")

	// Добавляем временный обработчик для /humor и /science, чтобы они не возвращали 404
	// Используем пустой хэндлер, а позже заменим его на настоящий
	emptyHumorHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Получен запрос к временному /humor от %s", r.RemoteAddr)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`<!DOCTYPE html>
<html lang="ru">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Аудиопоток юмора</title>
    <style>
        body { font-family: Arial, sans-serif; text-align: center; margin-top: 50px; }
        h1 { color: #333; }
        .loader { border: 5px solid #f3f3f3; border-top: 5px solid #3498db; border-radius: 50%; width: 50px; height: 50px; animation: spin 2s linear infinite; margin: 20px auto; }
        @keyframes spin { 0% { transform: rotate(0deg); } 100% { transform: rotate(360deg); } }
    </style>
</head>
<body>
    <h1>Аудиопоток юмора загружается...</h1>
    <div class="loader"></div>
    <p>Пожалуйста, подождите несколько секунд и обновите страницу.</p>
    <p><small>Если страница не обновляется автоматически, проверьте наличие аудиофайлов в директории.</small></p>
    <script>
        setTimeout(function() { location.reload(); }, 5000);
    </script>
</body>
</html>`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})

	emptyScienceHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Получен запрос к временному /science от %s", r.RemoteAddr)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`<!DOCTYPE html>
<html lang="ru">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Аудиопоток науки</title>
    <style>
        body { font-family: Arial, sans-serif; text-align: center; margin-top: 50px; }
        h1 { color: #333; }
        .loader { border: 5px solid #f3f3f3; border-top: 5px solid #3498db; border-radius: 50%; width: 50px; height: 50px; animation: spin 2s linear infinite; margin: 20px auto; }
        @keyframes spin { 0% { transform: rotate(0deg); } 100% { transform: rotate(360deg); } }
    </style>
</head>
<body>
    <h1>Аудиопоток науки загружается...</h1>
    <div class="loader"></div>
    <p>Пожалуйста, подождите несколько секунд и обновите страницу.</p>
    <p><small>Если страница не обновляется автоматически, проверьте наличие аудиофайлов в директории.</small></p>
    <script>
        setTimeout(function() { location.reload(); }, 5000);
    </script>
</body>
</html>`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})

	// Временная функция, которая будет использоваться для проверки и перенаправления на реальный обработчик
	humorRedirectHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Вызван перенаправляющий обработчик для /humor от %s", r.RemoteAddr)
		
		// Проверяем, есть ли зарегистрированный поток
		if server.IsStreamRegistered("/humor") {
			// Если поток зарегистрирован, используем его обработчик
			audioHandler := server.StreamAudioHandler("/humor")
			audioHandler(w, r)
			return
		}
		
		// Иначе показываем временную страницу
		emptyHumorHandler.ServeHTTP(w, r)
	})
	
	scienceRedirectHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Вызван перенаправляющий обработчик для /science от %s", r.RemoteAddr)
		
		// Проверяем, есть ли зарегистрированный поток
		if server.IsStreamRegistered("/science") {
			// Если поток зарегистрирован, используем его обработчик
			audioHandler := server.StreamAudioHandler("/science")
			audioHandler(w, r)
			return
		}
		
		// Иначе показываем временную страницу
		emptyScienceHandler.ServeHTTP(w, r)
	})

	// Регистрируем перенаправляющие обработчики
	humorRoute = server.Handler().(*mux.Router).Path("/humor").Methods("GET").Handler(humorRedirectHandler)
	scienceRoute = server.Handler().(*mux.Router).Path("/science").Methods("GET").Handler(scienceRedirectHandler)

	// Добавляем временный обработчик для корневого маршрута
	server.Handler().(*mux.Router).HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Получен запрос / от %s", r.RemoteAddr)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`<!DOCTYPE html>
<html lang="ru">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Аудио-стример</title>
    <style>
        body { 
            font-family: Arial, sans-serif; 
            text-align: center; 
            margin-top: 50px;
            background-color: #f5f5f5;
        }
        .container {
            max-width: 800px;
            margin: 0 auto;
            padding: 20px;
            background-color: white;
            border-radius: 8px;
            box-shadow: 0 2px 10px rgba(0,0,0,0.1);
        }
        h1 { color: #2c3e50; }
        .loader { 
            border: 5px solid #f3f3f3; 
            border-top: 5px solid #3498db; 
            border-radius: 50%; 
            width: 50px; 
            height: 50px; 
            animation: spin 2s linear infinite; 
            margin: 20px auto; 
        }
        @keyframes spin { 
            0% { transform: rotate(0deg); } 
            100% { transform: rotate(360deg); } 
        }
        .links {
            display: flex;
            justify-content: center;
            gap: 20px;
            margin-top: 30px;
        }
        .link-button {
            display: inline-block;
            padding: 10px 20px;
            background-color: #3498db;
            color: white;
            text-decoration: none;
            border-radius: 5px;
            transition: background-color 0.3s;
        }
        .link-button:hover {
            background-color: #2980b9;
        }
    </style>
</head>
<body>
    <div class="container">
        <h1>Аудио-стример загружается...</h1>
        <div class="loader"></div>
        <p>Пожалуйста, подождите несколько секунд, пока сервер настраивает аудиопотоки.</p>
        <div class="links">
            <a href="/humor" class="link-button">Юмор</a>
            <a href="/science" class="link-button">Наука</a>
        </div>
    </div>
</body>
</html>`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}).Methods("GET")

	// ТЕПЕРЬ создаем и запускаем HTTP-сервер с временными маршрутами
	httpSrv := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", config.Port), // Явно указываем, что слушаем на всех интерфейсах
		Handler: server.Handler(),
		// Увеличиваем таймауты для обработки запросов
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
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

	// Ждем небольшую паузу, чтобы сервер успел запуститься
	time.Sleep(500 * time.Millisecond)

	// Проверяем, что сервер слушает порт
	log.Printf("Проверка доступности HTTP-сервера...")
	
	// Попытки проверки с небольшими задержками
	serverStarted := false
	for i := 0; i < 5; i++ {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/healthz", config.Port))
		if err == nil {
			resp.Body.Close()
			log.Printf("HTTP-сервер успешно запущен и отвечает на запросы!")
			serverStarted = true
			break
		}
		log.Printf("Попытка %d: HTTP-сервер еще не готов: %s", i+1, err)
		time.Sleep(500 * time.Millisecond)
	}

	if !serverStarted {
		log.Printf("ПРЕДУПРЕЖДЕНИЕ: HTTP-сервер не отвечает на запросы healthz после 5 попыток")
	}

	// ПОСЛЕ запуска HTTP-сервера настраиваем аудио-маршруты
	log.Printf("Начало настройки аудио маршрутов...")
	
	// Перенаправление с корневого маршрута
	redirectTo := "/humor" // по умолчанию перенаправляем на /humor
	if _, exists := config.DirectoryRoutes["/humor"]; !exists {
		// Если /humor не существует, берем первый маршрут из конфигурации
		for route := range config.DirectoryRoutes {
			redirectTo = route
			break
		}
	}

	// Настраиваем маршруты из конфигурации
	for route, dir := range config.DirectoryRoutes {
		// Маршрут уже должен быть нормализован с ведущим слешем в loadConfig
		// Но на всякий случай проверяем
		if route[0] != '/' {
			route = "/" + route
		}
		
		log.Printf("Настройка маршрута '%s' -> директория '%s'", route, dir)
		configureRoute(server, stationManager, route, dir, config)
		log.Printf("Маршрут '%s' настроен успешно", route)
	}
	
	// Заменяем временный обработчик для корневого маршрута на перенаправление
	server.Handler().(*mux.Router).HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Перенаправление с / на %s", redirectTo)
		// Проверяем, готов ли поток к которому хотим перенаправить
		if !server.IsStreamRegistered(redirectTo) {
			// Если целевой поток еще не готов, выводим страницу с ссылками
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`<!DOCTYPE html>
<html lang="ru">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Аудио-стример</title>
    <style>
        body { 
            font-family: Arial, sans-serif; 
            text-align: center; 
            margin-top: 50px;
            background-color: #f5f5f5;
        }
        .container {
            max-width: 800px;
            margin: 0 auto;
            padding: 20px;
            background-color: white;
            border-radius: 8px;
            box-shadow: 0 2px 10px rgba(0,0,0,0.1);
        }
        h1 { color: #2c3e50; }
        p { margin-bottom: 20px; }
        .links {
            display: flex;
            justify-content: center;
            gap: 20px;
            margin-top: 30px;
        }
        .link-button {
            display: inline-block;
            padding: 10px 20px;
            background-color: #3498db;
            color: white;
            text-decoration: none;
            border-radius: 5px;
            transition: background-color 0.3s;
        }
        .link-button:hover {
            background-color: #2980b9;
        }
    </style>
</head>
<body>
    <div class="container">
        <h1>Аудио-стример</h1>
        <p>Выберите один из доступных потоков:</p>
        <div class="links">
            <a href="/humor" class="link-button">Юмор</a>
            <a href="/science" class="link-button">Наука</a>
        </div>
    </div>
</body>
</html>`))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		
		// Если поток готов, выполняем перенаправление
		http.Redirect(w, r, redirectTo, http.StatusSeeOther)
	}).Methods("GET")
	
	log.Printf("Настроено перенаправление с / на %s", redirectTo)
	log.Printf("Аудио маршруты настроены успешно")

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
		// Маршрут уже должен быть нормализован с ведущим слешем в loadConfig
		// Но на всякий случай проверяем
		if route[0] != '/' {
			route = "/" + route
		}
		
		log.Printf("Настройка маршрута '%s' -> директория '%s'", route, dir)
		configureRoute(server, stationManager, route, dir, config)
		log.Printf("Маршрут '%s' настроен успешно", route)
	}
	
	// Добавляем перенаправление с корневого маршрута на /humor, если он существует, или первый доступный маршрут
	redirectTo := "/humor" // по умолчанию перенаправляем на /humor
	if _, exists := config.DirectoryRoutes["/humor"]; !exists {
		// Если /humor не существует, берем первый маршрут из конфигурации
		for route := range config.DirectoryRoutes {
			redirectTo = route
			break
		}
	}
	
	// Добавляем обработчик для корневого маршрута
	server.Handler().(*mux.Router).HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Перенаправление с / на %s", redirectTo)
		http.Redirect(w, r, redirectTo, http.StatusSeeOther)
	}).Methods("GET")
	
	log.Printf("Настроено перенаправление с / на %s", redirectTo)
}

// configureRoute настраивает один маршрут для аудиопотока
func configureRoute(server *httpServer.Server, stationManager *radio.RadioStationManager, route, dir string, config *Config) {
	log.Printf("Начало настройки маршрута '%s'...", route)
	
	// Создаём директорию, если она не существует
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		log.Printf("Создание директории для маршрута %s: %s", route, dir)
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Printf("Ошибка при создании директории %s: %s", dir, err)
			sentry.CaptureException(fmt.Errorf("ошибка при создании директории %s: %w", dir, err))
			return
		}
	}

	// Проверим содержимое директории
	files, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("Ошибка при чтении директории %s: %s", dir, err)
	} else {
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
			log.Printf("ПРЕДУПРЕЖДЕНИЕ: В директории %s нет аудиофайлов. Аудиопоток может не работать.", dir)
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
	log.Printf("Радиостанция '%s' добавлена в менеджер", route)

	// Регистрируем аудиопоток на HTTP сервере
	server.RegisterStream(route, streamer, pl)
	log.Printf("Аудиопоток '%s' зарегистрирован на HTTP сервере", route)

	// Для маршрутов /humor и /science мы не регистрируем новые обработчики,
	// так как они обрабатываются через перенаправляющие обработчики
	if route != "/humor" && route != "/science" {
		// Для других маршрутов регистрируем обработчики напрямую
		audioHandler := server.StreamAudioHandler(route)
		server.Handler().(*mux.Router).HandleFunc(route, audioHandler).Methods("GET")
		log.Printf("Зарегистрирован новый обработчик для маршрута '%s'", route)
	}
	
	// Проверяем, что маршрут действительно был зарегистрирован
	routes := getAllRoutes(server.Handler().(*mux.Router))
	routeExists := false
	for _, r := range routes {
		if r == route {
			routeExists = true
			break
		}
	}
	
	if routeExists {
		log.Printf("Маршрут '%s' успешно зарегистрирован в HTTP сервере", route)
	} else {
		log.Printf("ВНИМАНИЕ: Маршрут '%s' может отсутствовать в списке маршрутов, но поток зарегистрирован!", route)
	}
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