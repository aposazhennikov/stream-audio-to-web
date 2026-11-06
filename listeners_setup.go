package main

import (
	"context"
	"log/slog"
	"os"

	httpServer "github.com/aposazhennikov/stream-audio-to-web/http"
	"github.com/aposazhennikov/stream-audio-to-web/listeners"
)

// setupListenerTracking records who listens: live for the status page, by month for the
// statistics (LISTENING_STATS_DIR, empty keeps nothing on disk). Country and city come from
// the DB-IP database in GEOIP_DIR, downloaded monthly unless GEOIP_DOWNLOAD=false.
func setupListenerTracking(logger *slog.Logger, server *httpServer.Server) {
	ctx := context.Background()
	geoDir := getEnvOrDefault("GEOIP_DIR", "./state/geo")
	geo := listeners.OpenDBIP(ctx, geoDir, os.Getenv("GEOIP_DOWNLOAD") != "false", logger)
	dir := getEnvOrDefault("LISTENING_STATS_DIR", "./state/listening")
	tracker := listeners.New(dir, geo, logger)
	server.SetListenerTracker(tracker)
	go tracker.Run(ctx)
	logger.Info("Listener tracking enabled", slog.String("dir", dir), slog.String("geoDir", geoDir))
}
