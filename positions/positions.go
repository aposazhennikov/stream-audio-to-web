// Package positions remembers which track every station is on, so that after a restart
// (the server restarts every night) a station continues from the same track instead of
// starting its playlist from the first one.
package positions

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// Store keeps the current track file name of every station in a JSON file.
// A Store with an empty path remembers nothing.
type Store struct {
	path   string
	mu     sync.Mutex
	tracks map[string]string // route -> track file name
	logger *slog.Logger
}

// Load opens the store at path, reading what was saved before. A missing or unreadable
// file gives an empty store.
func Load(path string, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Store{path: path, tracks: make(map[string]string), logger: logger}
	if path == "" {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("Cannot read station positions", slog.String("file", path), slog.String("error", err.Error()))
		}
		return s
	}
	if err := json.Unmarshal(data, &s.tracks); err != nil {
		logger.Warn("Cannot parse station positions", slog.String("file", path), slog.String("error", err.Error()))
		s.tracks = make(map[string]string)
	}
	return s
}

// Track returns the track file name saved for route, or "".
func (s *Store) Track(route string) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tracks[route]
}

// Save records that route now plays track (a file name) and writes the file.
func (s *Store) Save(route, track string) {
	if s == nil || s.path == "" || track == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tracks[route] == track {
		return
	}
	s.tracks[route] = track
	data, err := json.MarshalIndent(s.tracks, "", " ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(s.path), 0o755)
	}
	if err == nil {
		// Temporary file and rename: a crash never leaves a half-written file.
		tmp := s.path + ".tmp"
		if err = os.WriteFile(tmp, data, 0o644); err == nil {
			err = os.Rename(tmp, s.path)
		}
	}
	if err != nil {
		s.logger.Warn("Cannot save station positions", slog.String("file", s.path), slog.String("error", err.Error()))
	}
}
