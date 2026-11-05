package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	httpServer "github.com/aposazhennikov/stream-audio-to-web/http"
	"github.com/aposazhennikov/stream-audio-to-web/positions"
	"github.com/aposazhennikov/stream-audio-to-web/radio"
	sentryhelper "github.com/aposazhennikov/stream-audio-to-web/sentry_helper"
	"github.com/aposazhennikov/stream-audio-to-web/unit/testdata"
)

// The HTTP server registers global metrics, so a test binary can create it only once.
var (
	testServerOnce sync.Once
	testServer     *httpServer.Server
	testLogger     = slog.New(slog.NewTextHandler(io.Discard, nil))
	testSentry     = sentryhelper.NewSentryHelper(false, testLogger)
)

func sharedTestServer() *httpServer.Server {
	testServerOnce.Do(func() { testServer = httpServer.NewServer(10, testLogger, testSentry) })
	return testServer
}

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

	logger, sentry := testLogger, testSentry
	server := sharedTestServer()
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

// TestStationContinuesFromSavedTrack: after a restart a station continues from the track it
// was playing (saved in the positions file), not from the first one, and keeps saving.
func TestStationContinuesFromSavedTrack(t *testing.T) {
	tools := t.TempDir()
	writeFakeTool(t, tools, "ffprobe", "echo 128000\n") // already 128 kbps: no conversion
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))

	audioDir := t.TempDir()
	track := make([]byte, 160000) // ten seconds at 128 kbps
	copy(track, testdata.GetMinimumMP3Data())
	for _, name := range []string{"01 a.mp3", "02 b.mp3", "03 c.mp3"} {
		if err := os.WriteFile(filepath.Join(audioDir, name), track, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stateFile := filepath.Join(t.TempDir(), "positions.json")
	if err := os.WriteFile(stateFile, []byte(`{"/resume": "02 b.mp3"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	server := sharedTestServer()
	store := positions.Load(stateFile, testLogger)
	server.SetPositionStore(store)
	defer server.SetPositionStore(nil)
	stations := radio.NewRadioStationManager(testLogger, testSentry)
	defer stations.StopAll()
	server.SetStationManager(stations)
	config := &Config{
		Bitrate:          128,
		MaxClients:       10,
		BufferSize:       8192,
		PerStreamShuffle: map[string]bool{},
		NormalizeRuntime: "off",
		SentryHelper:     testSentry,
		Positions:        store,
	}
	if !configureSyncRoute(testLogger, server, stations, "/resume", audioDir, config) {
		t.Fatal("route configuration failed")
	}

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	deadline := time.Now().Add(5 * time.Second)
	var playing string
	for time.Now().Before(deadline) {
		resp, err := http.Get(ts.URL + "/now-playing?route=/resume")
		if err == nil {
			var info struct {
				Track string `json:"track"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&info)
			resp.Body.Close()
			if playing = info.Track; playing != "" {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if playing != "02 b.mp3" {
		t.Fatalf("station plays %q after the restart, want the saved 02 b.mp3", playing)
	}

	// A manual switch is saved too.
	if err := server.SwitchTrack("/resume", "next"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && positions.Load(stateFile, testLogger).Track("/resume") != "03 c.mp3" {
		time.Sleep(50 * time.Millisecond)
	}
	if got := positions.Load(stateFile, testLogger).Track("/resume"); got != "03 c.mp3" {
		t.Fatalf("saved position after a switch = %q, want 03 c.mp3", got)
	}
}
