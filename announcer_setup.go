package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/aposazhennikov/stream-audio-to-web/announce"
	httpServer "github.com/aposazhennikov/stream-audio-to-web/http"
)

// setupAnnouncer enables spoken announcements when a speech service is configured
// (ANNOUNCE_TTS_URL) and starts preparing the clips in the background.
func setupAnnouncer(logger *slog.Logger, config *Config, server *httpServer.Server) {
	// The stations file also labels the status page, with or without announcements.
	stationsFile := getEnvOrDefault("ANNOUNCE_STATIONS_FILE", "./announce_stations.json")
	stations, err := announce.LoadStations(stationsFile)
	if err != nil {
		logger.Warn("Cannot read the stations file, stations are not named",
			slog.String("file", stationsFile), slog.String("error", err.Error()))
		stations = map[string]announce.Station{}
	}
	server.SetStationLabels(stations)

	ttsURL := os.Getenv("ANNOUNCE_TTS_URL")
	if ttsURL == "" {
		logger.Info("Announcements disabled: ANNOUNCE_TTS_URL is not set")
		return
	}
	var userAgents []string
	for _, ua := range strings.Split(getEnvOrDefault("ANNOUNCE_USER_AGENTS", "WR-230SF"), ",") {
		if ua = strings.TrimSpace(ua); ua != "" {
			userAgents = append(userAgents, ua)
		}
	}
	announcer := announce.New(announce.Config{
		TTSURL:     ttsURL,
		CacheDir:   getEnvOrDefault("ANNOUNCE_CACHE_DIR", "./state/voice"),
		Stations:   stations,
		UserAgents: userAgents,
		Logger:     logger,
	})
	server.SetAnnouncer(announcer)

	buttons := map[string]bool{}
	if route := normalizeRouteEnv("REMOTE_NEXT_ROUTE"); route != "" {
		buttons[route] = true
	}
	if route := normalizeRouteEnv("REMOTE_PREV_ROUTE"); route != "" {
		buttons[route] = false
	}
	go announcer.Prepare(context.Background(),
		func() map[string][]string { return audioFilesByRoute(config) },
		config.Positions.All, buttons)
	logger.Info("Announcements enabled", slog.String("tts", ttsURL), slog.Int("stations", len(stations)),
		slog.Any("userAgents", userAgents))
}

func normalizeRouteEnv(name string) string {
	route := strings.TrimSpace(os.Getenv(name))
	if route != "" && !strings.HasPrefix(route, "/") {
		route = "/" + route
	}
	return route
}

// audioFilesByRoute lists the audio file paths of every station directory.
func audioFilesByRoute(config *Config) map[string][]string {
	result := make(map[string][]string, len(config.DirectoryRoutes))
	for route, dir := range config.DirectoryRoutes {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() && isAudioFileName(e.Name()) {
				result[route] = append(result[route], filepath.Join(dir, e.Name()))
			}
		}
	}
	return result
}
