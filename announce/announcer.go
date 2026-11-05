// Package announce builds spoken announcements for a blind listener: which station he is on,
// what record plays and how much of it is left. The phrases are synthesized by a separate
// speech service (tts/) and cached on disk; an announcement is put together from cached clips,
// so it is ready the moment the receiver connects.
package announce

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Station is how a station is announced: its preset number on the receiver and its name.
type Station struct {
	Preset int    `json:"preset"`
	Name   string `json:"name"`
}

// Config sets up an Announcer.
type Config struct {
	TTSURL       string             // speech service, e.g. http://tts:5002; empty disables announcements
	CacheDir     string             // where synthesized clips are kept
	Stations     map[string]Station // route -> station
	UserAgents   []string           // receivers that get announcements (substring of User-Agent)
	MaxMinutes   int                // longest record the minute clips are prepared for
	SpeakTimeout time.Duration      // limit for synthesizing a clip that is not cached yet
	Logger       *slog.Logger
}

// Announcer builds announcements from cached speech clips.
type Announcer struct {
	cfg    Config
	client *http.Client
	synth  sync.Mutex   // one synthesis at a time: the speech service has one CPU
	demand atomic.Int32 // listeners waiting for a clip: the background preparation gives way
}

// LoadStations reads the stations file: {"/monosov8": {"preset": 32, "name": "Целительство"}, ...}.
func LoadStations(path string) (map[string]Station, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	stations := map[string]Station{}
	if err := json.Unmarshal(data, &stations); err != nil {
		return nil, fmt.Errorf("stations file %s: %w", path, err)
	}
	return stations, nil
}

// New returns an Announcer, or nil if no speech service is configured.
func New(cfg Config) *Announcer {
	if cfg.TTSURL == "" {
		return nil
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxMinutes <= 0 {
		cfg.MaxMinutes = 240
	}
	if cfg.SpeakTimeout <= 0 {
		// A long phrase takes several seconds on one CPU. A listener waiting for an
		// announcement is limited by the caller's context, not by this.
		cfg.SpeakTimeout = 30 * time.Second
	}
	return &Announcer{cfg: cfg, client: &http.Client{}}
}

// Wants reports whether this client gets announcements: one of the configured receivers, or
// a request with ?voice=1 (to try it from a browser or a player).
func (a *Announcer) Wants(r *http.Request) bool {
	if a == nil {
		return false
	}
	if r.URL.Query().Get("voice") == "1" {
		return true
	}
	ua := r.UserAgent()
	for _, want := range a.cfg.UserAgents {
		if want != "" && strings.Contains(ua, want) {
			return true
		}
	}
	return false
}

// StationName returns the announced name of a station ("" if it has none).
func (a *Announcer) StationName(route string) string {
	if a == nil {
		return ""
	}
	return a.cfg.Stations[route].Name
}

// ForStation is said when the receiver connects to a station: the station, the record and
// the time. Clips that are not ready are left out; nothing ready gives nil.
func (a *Announcer) ForStation(ctx context.Context, route, trackPath string, elapsed, total time.Duration) []byte {
	if a == nil {
		return nil
	}
	var parts []string
	if st, ok := a.cfg.Stations[route]; ok {
		parts = append(parts, StationIntro(st.Preset, st.Name))
	}
	parts = append(parts, NowPlaying(filepath.Base(trackPath)))
	parts = append(parts, timeClips(elapsed, total)...)
	return a.join(ctx, parts)
}

// ForSwitch is said after a button switched the station: the new record and its length.
func (a *Announcer) ForSwitch(ctx context.Context, trackPath string, total time.Duration) []byte {
	if a == nil {
		return nil
	}
	parts := []string{NowPlaying(filepath.Base(trackPath))}
	parts = append(parts, timeClips(0, total)...)
	return a.join(ctx, parts)
}

// ForButton is said when the receiver arrives on a button.
func (a *Announcer) ForButton(ctx context.Context, next bool, stationRoute string) []byte {
	if a == nil {
		return nil
	}
	name := a.StationName(stationRoute)
	if name == "" {
		name = strings.TrimPrefix(stationRoute, "/")
	}
	return a.join(ctx, []string{ButtonPhrase(next, name)})
}

// timeClips splits the time phrase into clips that can be prepared in advance: "Прошло
// двенадцать минут," and "осталось двадцать восемь минут." (a few hundred clips instead of
// every combination).
func timeClips(elapsed, total time.Duration) []string {
	phrase := TimePhrase(elapsed, total)
	if i := strings.Index(phrase, ", осталось"); i > 0 {
		return []string{phrase[:i+1], phrase[i+2:]}
	}
	if i := strings.Index(phrase, ", осталась"); i > 0 {
		return []string{phrase[:i+1], phrase[i+2:]}
	}
	if phrase == "" {
		return nil
	}
	return []string{phrase}
}

// join synthesizes (or takes from the cache) every phrase and joins the MP3 clips.
func (a *Announcer) join(ctx context.Context, phrases []string) []byte {
	var out bytes.Buffer
	for _, p := range phrases {
		if p == "" {
			continue
		}
		clip, err := a.Speak(ctx, p)
		if err != nil {
			a.cfg.Logger.Warn("Announcement clip not ready", slog.String("text", p), slog.String("error", err.Error()))
			continue
		}
		out.Write(clip)
	}
	if out.Len() == 0 {
		return nil
	}
	return out.Bytes()
}

func (a *Announcer) clipPath(text string) string {
	sum := sha1.Sum([]byte("ruslan-medium\n" + text))
	return filepath.Join(a.cfg.CacheDir, hex.EncodeToString(sum[:])+".mp3")
}

// Cached returns the clip for text if it is already synthesized.
func (a *Announcer) Cached(text string) ([]byte, bool) {
	path := a.clipPath(text)
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now) // used clips are kept by the cache cleanup
	return data, true
}

// Speak returns the clip for text, synthesizing and caching it if needed. A listener waiting
// for an announcement goes before the background preparation.
func (a *Announcer) Speak(ctx context.Context, text string) ([]byte, error) {
	if data, ok := a.Cached(text); ok {
		return data, nil
	}
	a.demand.Add(1)
	defer a.demand.Add(-1)
	return a.speak(ctx, text)
}

// prepare synthesizes a clip for the background preparation, after any waiting listener.
func (a *Announcer) prepare(ctx context.Context, text string) ([]byte, error) {
	for a.demand.Load() > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return a.speak(ctx, text)
}

func (a *Announcer) speak(ctx context.Context, text string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.SpeakTimeout)
	defer cancel()
	a.synth.Lock()
	defer a.synth.Unlock()
	if data, ok := a.Cached(text); ok { // synthesized while we waited
		return data, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.cfg.TTSURL, "/")+"/speak", strings.NewReader(text))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK || len(data) == 0 {
		return nil, fmt.Errorf("speech service: %s %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if err := os.MkdirAll(a.cfg.CacheDir, 0o755); err == nil {
		path := a.clipPath(text)
		tmp := path + ".tmp"
		if os.WriteFile(tmp, data, 0o644) == nil {
			_ = os.Rename(tmp, path)
		}
	}
	return data, nil
}

// Prepare synthesizes, in the background and one by one, every phrase an announcement can
// need: station intros and button phrases, the minute clips, and the titles of all records
// (tracks returns route -> file names and is asked again on every pass, so new files are
// picked up). Clips unused for two weeks are removed. Runs until ctx is done.
// playing returns the track file name each station plays now; their titles are made first.
func (a *Announcer) Prepare(ctx context.Context, tracks func() map[string][]string, playing func() map[string]string, buttons map[string]bool) {
	if a == nil {
		return
	}
	for {
		start := time.Now()
		made := a.preparePass(ctx, tracks, playing, buttons)
		a.cleanup(14 * 24 * time.Hour)
		if made > 0 {
			a.cfg.Logger.Info("Announcement clips prepared", slog.Int("new", made), slog.Duration("took", time.Since(start)))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Minute):
		}
	}
}

func (a *Announcer) preparePass(ctx context.Context, tracks func() map[string][]string, playing func() map[string]string, buttons map[string]bool) int {
	var phrases []string
	for route, st := range a.cfg.Stations {
		phrases = append(phrases, StationIntro(st.Preset, st.Name))
		for button, next := range buttons {
			if route != button {
				phrases = append(phrases, ButtonPhrase(next, st.Name))
			}
		}
	}
	// What plays now is what the listener will hear about first.
	if playing != nil {
		for _, name := range playing() {
			phrases = append(phrases, NowPlaying(name))
		}
	}
	// Minute clips for usual lengths first, titles next, long records last.
	const usual = 90
	minuteClips := func(from, to int) {
		for m := from; m <= to; m++ {
			total := time.Duration(m) * time.Minute
			phrases = append(phrases, timeClips(0, total)...)
			phrases = append(phrases, timeClips(total, total+2*time.Minute)[0], timeClips(time.Minute, total+time.Minute)[1])
		}
	}
	minuteClips(1, usual)
	for _, names := range tracks() {
		for _, name := range names {
			phrases = append(phrases, NowPlaying(name))
		}
	}
	minuteClips(usual+1, a.cfg.MaxMinutes)

	made := 0
	for _, p := range phrases {
		if ctx.Err() != nil {
			return made
		}
		if p == "" {
			continue
		}
		if _, ok := a.Cached(p); ok {
			continue
		}
		if _, err := a.prepare(ctx, p); err != nil {
			a.cfg.Logger.Warn("Cannot prepare announcement clip", slog.String("text", p), slog.String("error", err.Error()))
			select { // the speech service may be starting; try again shortly
			case <-ctx.Done():
				return made
			case <-time.After(5 * time.Second):
			}
			continue
		}
		made++
	}
	return made
}

// cleanup removes clips not used for maxAge (titles of deleted news videos and the like).
func (a *Announcer) cleanup(maxAge time.Duration) {
	entries, err := os.ReadDir(a.cfg.CacheDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && !e.IsDir() && time.Since(info.ModTime()) > maxAge {
			_ = os.Remove(filepath.Join(a.cfg.CacheDir, e.Name()))
		}
	}
}
