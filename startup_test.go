package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	httpServer "github.com/aposazhennikov/stream-audio-to-web/http"
	"github.com/aposazhennikov/stream-audio-to-web/radio"
	sentryhelper "github.com/aposazhennikov/stream-audio-to-web/sentry_helper"
	"github.com/aposazhennikov/stream-audio-to-web/unit/testdata"
)

// writeFakeTool puts an executable shell script named name into dir.
func writeFakeTool(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestStationStartsBeforeBitrateCheck: a slow bitrate check (ffprobe on every file, ~1 min on
// the server) must not delay the station. The route is registered at once, and the check,
// including a conversion, still runs in the background.
func TestStationStartsBeforeBitrateCheck(t *testing.T) {
	tools := t.TempDir()
	audioDir := t.TempDir()
	marker := filepath.Join(tools, "converted")

	// ffprobe takes 3 s and reports 64 kbps, so the file needs conversion to 128 kbps.
	writeFakeTool(t, tools, "ffprobe", "sleep 3\necho 64000\n")
	// ffmpeg -y -i <in> ... <out>: copy the input to the output and leave a marker.
	writeFakeTool(t, tools, "ffmpeg", "for last; do :; done\ncp \"$3\" \"$last\"\ntouch "+marker+"\n")
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := os.WriteFile(filepath.Join(audioDir, "01.mp3"), testdata.GetMinimumMP3Data(), 0o644); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sentry := sentryhelper.NewSentryHelper(false, logger)
	server := httpServer.NewServer(10, logger, sentry)
	stations := radio.NewRadioStationManager(logger, sentry)
	defer stations.StopAll()
	config := &Config{
		Bitrate:          128,
		MaxClients:       10,
		BufferSize:       8192,
		PerStreamShuffle: map[string]bool{},
		NormalizeRuntime: "off",
		SentryHelper:     sentry,
	}

	start := time.Now()
	if !configureSyncRoute(logger, server, stations, "/fast", audioDir, config) {
		t.Fatal("route configuration failed")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("route configuration took %v, the bitrate check must not block it", elapsed)
	}
	if !server.IsStreamRegistered("/fast") {
		t.Fatal("stream is not registered right after configuration")
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background bitrate conversion did not run")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Let the check release its slot before the temporary directories are removed.
	for len(bitrateCheckSlots) > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
}
