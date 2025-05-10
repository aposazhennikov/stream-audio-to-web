// Package playlist manages audio playlists and track selection.
// It handles playlist creation, updates, and track shuffling.
package playlist

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/getsentry/sentry-go"
)

// Supported audio file formats
var supportedExtensions = map[string]bool{
	".mp3": true,
	".aac": true,
	".ogg": true,
}

// Track represents information about a track
type Track struct {
	Path     string
	Name     string
	FileInfo os.FileInfo
}

// GetPath returns the path to the track
func (t *Track) GetPath() string {
	return t.Path
}

// Playlist manages the list of tracks for audio streaming
type Playlist struct {
	directory    string
	tracks       []Track
	current      int
	mutex        sync.RWMutex
	watcher      *fsnotify.Watcher
	onChange     func()
	shuffle      bool         // Flag determining whether to shuffle tracks
	history      []Track      // History of played tracks
	startTime    time.Time    // Playlist start time
	historyMutex sync.RWMutex // Mutex for history
	logger       *slog.Logger // Инстанс логгера
}

const (
	maxShowTracks       = 3
	maxAttempts         = 3
	getTrackTimeout     = 500 * time.Millisecond
	getTrackPause       = 50 * time.Millisecond
	maxHistorySize      = 100
	mutexTryLockTimeout = 50 * time.Millisecond
	shuffleDelayMs      = 10
)

// NewPlaylist creates a new playlist from the specified directory
func NewPlaylist(directory string, onChange func(), shuffle bool, logger *slog.Logger) (*Playlist, error) {
	if logger == nil {
		logger = slog.Default()
	}
	pl := &Playlist{
		directory: directory,
		tracks:    []Track{},
		history:   []Track{},
		current:   0,
		onChange:  onChange,
		shuffle:   shuffle,    // Save shuffle parameter
		startTime: time.Now(), // Remember start time
		logger:    logger,
	}

	// Initialize watcher to track directory changes
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		sentry.CaptureException(err)
		return nil, err
	}
	pl.watcher = watcher

	// Load tracks from directory
	if err2 := pl.Reload(); err2 != nil {
		sentry.CaptureException(err2)
		return nil, err2
	}

	// Start goroutine to monitor directory changes
	go pl.watchDirectory()

	return pl, nil
}

// Close closes the watcher
func (p *Playlist) Close() error {
	err := p.watcher.Close()
	if err != nil {
		sentry.CaptureException(err)
	}
	return err
}

// Reload reloads the list of tracks from the directory
func (p *Playlist) Reload() error {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	p.logger.Info("DIAGNOSTICS: Starting playlist reload in directory", slog.String("directory", p.directory))

	p.tracks = []Track{}
	p.current = 0

	// Check if directory exists
	if _, err := os.Stat(p.directory); os.IsNotExist(err) {
		errorMsg := fmt.Sprintf("Directory %s does not exist", p.directory)
		p.logger.Info(errorMsg)
		// Don't send to Sentry - this is an informational message
		return nil // Don't consider directory absence an error
	}

	p.logger.Info("Scanning directory", slog.String("directory", p.directory))
	// Don't send to Sentry - this is an informational message

	// Counters for statistics
	var (
		totalFiles       int
		supportedFiles   int
		unsupportedFiles int
		errorFiles       int
	)

	err := filepath.Walk(p.directory, func(path string, info os.FileInfo, err error) error {
		totalFiles++
		if err != nil {
			errorFiles++
			errorMsg := fmt.Sprintf("Error accessing file/directory %s: %v", path, err)
			p.logger.Info(errorMsg)
			sentry.CaptureException(fmt.Errorf("%s: %w", errorMsg, err)) // Send to Sentry as an error
			return nil                                                   // Continue processing even if an individual file is inaccessible
		}

		if !info.IsDir() {
			ext := strings.ToLower(filepath.Ext(path))
			if supportedExtensions[ext] {
				supportedFiles++
				fileName := filepath.Base(path)
				// Don't log every added track

				p.tracks = append(p.tracks, Track{
					Path:     path,
					Name:     fileName,
					FileInfo: info,
				})
			} else {
				unsupportedFiles++
				p.logger.Info("Skipping unsupported file", slog.String("file", filepath.Base(path)), slog.String("extension", ext))
			}
		}
		return nil
	})

	if err != nil {
		sentry.CaptureException(err) // Send to Sentry as an error
		return err
	}

	// Check if there are any tracks
	if len(p.tracks) == 0 {
		errorMsg := fmt.Sprintf("No audio files found in directory %s", p.directory)
		p.logger.Info(errorMsg)

		// Don't send to Sentry - this is an informational message
		return nil // Don't consider absence of tracks an error
	}

	// Log found tracks
	p.logger.Info("Found tracks in directory", slog.Int("tracks", len(p.tracks)), slog.String("directory", p.directory))

	// Show maximum 3 tracks (was 5) to reduce output volume
	trackNames := make([]string, 0, minInt(maxShowTracks, len(p.tracks)))
	for i, track := range p.tracks {
		if i < maxShowTracks {
			p.logger.Info("Track found", slog.Int("position", i+1), slog.String("track", track.Name))
			trackNames = append(trackNames, track.Name)
		} else if i == maxShowTracks {
			p.logger.Info("... and more tracks", slog.Int("remaining", len(p.tracks)-maxShowTracks))
			break
		}
	}

	// Remember the shuffle flag
	needShuffle := p.shuffle

	// Shuffle tracks only if the corresponding flag is enabled
	if needShuffle {
		p.logger.Info("DIAGNOSTICS: Shuffle enabled for playlist", slog.String("directory", p.directory))
	} else {
		p.logger.Info("DIAGNOSTICS: Shuffle disabled for playlist", slog.String("directory", p.directory))
	}

	// Add directory to watcher
	if err2 := p.watcher.Add(p.directory); err2 != nil {
		sentry.CaptureException(err2) // Send to Sentry as an error
		return err2
	}

	// Add current track to history to ensure history has at least one track
	if len(p.tracks) > 0 {
		p.logger.Info("DIAGNOSTICS: Adding initial track to history", slog.String("track", p.tracks[p.current].Name))
		p.addTrackToHistory(p.tracks[p.current])
	}

	// Send statistics only to logs
	p.logger.Info("Playlist loaded", slog.String("directory", p.directory), slog.Int("tracks", len(p.tracks)))

	// If shuffle is needed, start a separate goroutine after mutex unlock
	if needShuffle {
		// Create a separate goroutine for shuffling AFTER mutex is unlocked
		go func() {
			// Small delay to ensure mutex has been unlocked
			time.Sleep(shuffleDelayMs * time.Millisecond)
			p.logger.Info("DIAGNOSTICS: Running shuffle after reload in separate goroutine for", slog.String("directory", p.directory))
			p.Shuffle()
		}()
	}

	return nil
}

// minInt возвращает минимум из двух int
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// maxInt возвращает максимум из двух int
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// GetCurrentTrack returns the current track
func (p *Playlist) GetCurrentTrack() interface{} {
	// Adding retry mechanism
	maxAttempts := maxAttempts

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Use timeout channel for deadlock protection
		c := make(chan interface{}, 1)

		go func() {
			p.mutex.RLock()
			defer p.mutex.RUnlock()

			if len(p.tracks) == 0 {
				c <- nil
				return
			}
			c <- &p.tracks[p.current]
		}()

		// Increase timeout to 500 ms
		select {
		case track := <-c:
			return track
		case <-time.After(getTrackTimeout):
			p.logger.Warn("WARNING: GetCurrentTrack timed out", slog.Int("attempt", attempt), slog.Int("maxAttempts", maxAttempts))
			if attempt == maxAttempts {
				p.logger.Warn("WARNING: All GetCurrentTrack attempts timed out, returning nil")
				return nil
			}
			// Small pause before next attempt
			time.Sleep(getTrackPause)
		}
	}

	// This code should not be reached, but for completeness:
	p.logger.Error("ERROR: Unexpected flow in GetCurrentTrack, returning nil")
	return nil
}

// NextTrack moves to the next track and returns it
func (p *Playlist) NextTrack() interface{} {
	// Adding retry mechanism
	maxAttempts := maxAttempts

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Use timeout channel for safe mutex locking
		c := make(chan interface{}, 1)

		go func() {
			// Try to lock mutex in goroutine
			p.mutex.Lock()
			defer p.mutex.Unlock()

			if len(p.tracks) == 0 {
				p.logger.Info("DIAGNOSTICS: NextTrack() called but no tracks available")
				c <- nil
				return
			}

			// Add current track to history before moving to the next
			currentTrack := p.tracks[p.current]
			p.logger.Info("DIAGNOSTICS: Adding current track to history before moving to next", slog.String("track", currentTrack.Name))
			p.addTrackToHistory(currentTrack)

			// Move to the next track
			p.current = (p.current + 1) % len(p.tracks)
			nextTrack := p.tracks[p.current]
			p.logger.Info("DIAGNOSTICS: Moved to next track", slog.String("track", nextTrack.Name), slog.Int("position", p.current))

			// If we reached the end of playlist and shuffle is enabled, reshuffle for the next cycle
			if p.current == 0 && p.shuffle && len(p.tracks) > 1 {
				p.logger.Info("DIAGNOSTICS: Reached end of playlist with shuffle enabled, performing reshuffle directly")
				currentPos := p.current
				currentTrackPath := p.tracks[currentPos].Path
				// Fisher-Yates с crypto/rand
				for i := len(p.tracks) - 1; i > 0; i-- {
					max := big.NewInt(int64(i + 1))
					jBig, err := rand.Int(rand.Reader, max)
					if err != nil {
						p.logger.Info("CRYPTO ERROR: %v, fallback to i", slog.Any("error", err))
						jBig = big.NewInt(int64(i))
					}
					j := int(jBig.Int64())
					p.tracks[i], p.tracks[j] = p.tracks[j], p.tracks[i]
				}
				for i, track := range p.tracks {
					if track.Path == currentTrackPath {
						p.current = i
						break
					}
				}
				p.logger.Info("DIAGNOSTICS: Playlist shuffled in NextTrack, current track at position", slog.Int("position", p.current))
			}

			// Verify history has been updated
			p.historyMutex.RLock()
			historyLength := len(p.history)
			p.historyMutex.RUnlock()
			p.logger.Info("DIAGNOSTICS: History length after NextTrack()", slog.Int("historyLength", historyLength))

			c <- &p.tracks[p.current]
		}()

		// Wait for result with increased timeout
		select {
		case result := <-c:
			return result
		case <-time.After(getTrackTimeout):
			p.logger.Warn("WARNING: NextTrack operation timed out", slog.Int("attempt", attempt), slog.Int("maxAttempts", maxAttempts))
			if attempt == maxAttempts {
				p.logger.Warn("WARNING: All NextTrack attempts timed out, returning nil")
				return nil
			}
			// Small pause before next attempt
			time.Sleep(getTrackPause)
		}
	}

	// This code should not be reached, but for completeness:
	p.logger.Error("ERROR: Unexpected flow in NextTrack, returning nil")
	return nil
}

// PreviousTrack moves to the previous track and returns it
func (p *Playlist) PreviousTrack() interface{} {
	// Adding retry mechanism
	maxAttempts := maxAttempts

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Use timeout channel for safe mutex locking
		c := make(chan interface{}, 1)

		go func() {
			p.mutex.Lock()
			defer p.mutex.Unlock()

			if len(p.tracks) == 0 {
				p.logger.Info("DIAGNOSTICS: PreviousTrack() called but no tracks available")
				c <- nil
				return
			}

			// Add current track to history before moving to the previous
			currentTrack := p.tracks[p.current]
			p.logger.Info("DIAGNOSTICS: Adding current track to history before moving to previous", slog.String("track", currentTrack.Name))
			p.addTrackToHistory(currentTrack)

			// Move to the previous track considering the possibility of going to a negative index
			if p.current == 0 {
				p.current = len(p.tracks) - 1
			} else {
				p.current--
			}

			prevTrack := p.tracks[p.current]
			p.logger.Info("DIAGNOSTICS: Switching to previous track", slog.String("track", prevTrack.Name), slog.Int("position", p.current))

			// Verify history has been updated
			p.historyMutex.RLock()
			historyLength := len(p.history)
			p.historyMutex.RUnlock()
			p.logger.Info("DIAGNOSTICS: History length after PreviousTrack()", slog.Int("historyLength", historyLength))

			c <- &p.tracks[p.current]
		}()

		// Wait for result with increased timeout
		select {
		case result := <-c:
			return result
		case <-time.After(getTrackTimeout):
			p.logger.Warn("WARNING: PreviousTrack operation timed out", slog.Int("attempt", attempt), slog.Int("maxAttempts", maxAttempts))
			if attempt == maxAttempts {
				p.logger.Warn("WARNING: All PreviousTrack attempts timed out, returning nil")
				return nil
			}
			// Small pause before next attempt
			time.Sleep(getTrackPause)
		}
	}

	// This code should not be reached, but for completeness:
	p.logger.Error("ERROR: Unexpected flow in PreviousTrack, returning nil")
	return nil
}

// addTrackToHistory adds a track to history
func (p *Playlist) addTrackToHistory(track Track) {
	p.historyMutex.Lock()
	defer p.historyMutex.Unlock()

	// Add track to history
	p.history = append(p.history, track)

	// Limit history size - keep last 100 tracks
	if len(p.history) > maxHistorySize {
		p.history = p.history[len(p.history)-maxHistorySize:]
	}
}

// GetHistory returns the history of played tracks
func (p *Playlist) GetHistory() []interface{} {
	p.historyMutex.RLock()
	defer p.historyMutex.RUnlock()

	historyLen := len(p.history)
	p.logger.Info("DIAGNOSTICS: GetHistory() called, history length", slog.Int("historyLength", historyLen))

	if historyLen == 0 {
		p.logger.Info("DIAGNOSTICS: Warning - History is empty!")
		return []interface{}{}
	}

	history := make([]interface{}, historyLen)
	for i, track := range p.history {
		history[i] = &track
	}

	// Log first few tracks in history
	if historyLen > 0 {
		trackNames := make([]string, 0, minInt(maxShowTracks, historyLen))
		for i := range history[:minInt(maxShowTracks, historyLen)] {
			if track, ok := history[i].(interface{ GetPath() string }); ok {
				trackNames = append(trackNames, filepath.Base(track.GetPath()))
			}
		}
		p.logger.Info("DIAGNOSTICS: First tracks in history", slog.String("tracks", strings.Join(trackNames, ", ")))
	}

	return history
}

// GetStartTime returns the playlist start time
func (p *Playlist) GetStartTime() time.Time {
	return p.startTime
}

// Shuffle randomizes the track list
func (p *Playlist) Shuffle() {
	p.logger.Info("SHUFFLE: Starting playlist shuffle for directory", slog.String("directory", p.directory))

	// Create a safe copy of tracks for shuffling without long mutex blocking
	var tracksToShuffle []Track
	var originalTrackPath string

	// Try to lock mutex for no more than 50 ms to copy tracks
	lockDone := make(chan bool, 1)
	go func() {
		if !p.mutex.TryLock() {
			// Could not acquire mutex instantly, try with timeout
			timer := time.NewTimer(mutexTryLockTimeout)
			select {
			case <-timer.C:
				lockDone <- false
				return
			default:
				p.mutex.Lock()
				timer.Stop()
			}
		}
		// Successfully locked, do fast copying
		defer p.mutex.Unlock()

		// If there are few or no tracks, nothing to shuffle
		if len(p.tracks) <= 1 {
			lockDone <- false
			return
		}

		// Save current track
		if p.current < len(p.tracks) {
			originalTrackPath = p.tracks[p.current].Path
		}

		// Make a quick copy of tracks
		tracksToShuffle = make([]Track, len(p.tracks))
		copy(tracksToShuffle, p.tracks)

		// Signal that copying is complete
		lockDone <- true
	}()

	// Wait for the locking result
	if !<-lockDone {
		p.logger.Info("SHUFFLE: Could not acquire mutex or not enough tracks, skipping shuffle")
		return
	}

	// Here we've already unlocked the mutex and have a local copy of tracks
	p.logger.Info("SHUFFLE: Shuffling tracks")

	// Fisher-Yates с crypto/rand
	for i := len(tracksToShuffle) - 1; i > 0; i-- {
		max := big.NewInt(int64(i + 1))
		jBig, err := rand.Int(rand.Reader, max)
		if err != nil {
			p.logger.Info("CRYPTO ERROR: %v, fallback to i", slog.Any("error", err))
			jBig = big.NewInt(int64(i))
		}
		j := int(jBig.Int64())
		tracksToShuffle[i], tracksToShuffle[j] = tracksToShuffle[j], tracksToShuffle[i]
	}

	// Find position of original track in shuffled array
	newPosition := 0
	if originalTrackPath != "" {
		for i, track := range tracksToShuffle {
			if track.Path == originalTrackPath {
				newPosition = i
				break
			}
		}
	}

	// Try to lock mutex again to apply changes
	applyDone := make(chan bool, 1)
	go func() {
		if !p.mutex.TryLock() {
			// Could not acquire mutex instantly, try with timeout
			timer := time.NewTimer(mutexTryLockTimeout)
			select {
			case <-timer.C:
				applyDone <- false
				return
			default:
				p.mutex.Lock()
				timer.Stop()
			}
		}
		// Successfully locked, apply changes
		defer p.mutex.Unlock()

		// Apply shuffled tracks
		p.tracks = tracksToShuffle

		// Restore current track position
		if originalTrackPath != "" {
			p.current = newPosition
			p.logger.Info("SHUFFLE: Restored current track position to", slog.Int("position", newPosition))
		}

		// Signal successful application
		applyDone <- true
	}()

	// Check result of applying changes
	if <-applyDone {
		p.logger.Info("SHUFFLE: Playlist shuffled successfully")
	} else {
		p.logger.Info("SHUFFLE: Could not acquire mutex to apply changes")
	}
}

// GetTracks returns a copy of the track list
func (p *Playlist) GetTracks() []Track {
	// Adding retry mechanism
	maxAttempts := maxAttempts

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Use timeout channel for deadlock protection
		c := make(chan []Track, 1)

		go func() {
			p.mutex.RLock()
			defer p.mutex.RUnlock()

			tracks := make([]Track, len(p.tracks))
			copy(tracks, p.tracks)
			c <- tracks
		}()

		// Increase timeout to 500 ms, since 50 ms was too small
		select {
		case tracks := <-c:
			// Check if slice is empty
			if len(tracks) == 0 && len(p.tracks) > 0 {
				// If main slice is not empty but we got empty one, this might be a copy error
				p.logger.Warn("WARNING: GetTracks returned empty list despite having tracks", slog.Int("tracks", len(p.tracks)))
				continue // Retry
			}
			return tracks
		case <-time.After(getTrackTimeout):
			p.logger.Warn("WARNING: GetTracks timed out", slog.Int("attempt", attempt), slog.Int("maxAttempts", maxAttempts))
			if attempt == maxAttempts {
				p.logger.Warn("WARNING: All GetTracks attempts timed out, returning empty list")
				return []Track{}
			}
			// Small pause before next attempt
			time.Sleep(getTrackPause)
		}
	}

	// This code should not be reached, but for completeness:
	p.logger.Error("ERROR: Unexpected flow in GetTracks, returning empty list")
	return []Track{}
}

// watchDirectory monitors changes in the playlist directory
func (p *Playlist) watchDirectory() {
	for {
		select {
		case event, ok := <-p.watcher.Events:
			if !ok {
				return
			}

			ext := strings.ToLower(filepath.Ext(event.Name))
			if !supportedExtensions[ext] {
				continue
			}

			if event.Op&(fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0 {
				p.logger.Info("Change detected in playlist", slog.String("file", event.Name))

				// Reload playlist
				if err := p.Reload(); err != nil {
					p.logger.Info("Error reloading playlist", slog.String("error", err.Error()))
					sentry.CaptureException(fmt.Errorf("error reloading playlist: %w", err))
					continue
				}

				// Call callback when playlist changes
				if p.onChange != nil {
					p.onChange()
				}
			}

		case err, ok := <-p.watcher.Errors:
			if !ok {
				return
			}
			p.logger.Error("fsnotify error", slog.String("error", err.Error()))
			sentry.CaptureException(fmt.Errorf("fsnotify error: %w", err))
		}
	}
}
