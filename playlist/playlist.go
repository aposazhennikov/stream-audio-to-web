package playlist

import (
	"fmt"
	"log"
	"math/rand"
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
	directory      string
	tracks         []Track
	current        int
	mutex          sync.RWMutex
	watcher        *fsnotify.Watcher
	onChange       func()
	shuffle        bool  // Flag determining whether to shuffle tracks
	history        []Track // History of played tracks
	startTime      time.Time // Playlist start time
	historyMutex   sync.RWMutex // Mutex for history
}

// NewPlaylist creates a new playlist from the specified directory
func NewPlaylist(directory string, onChange func(), shuffle bool) (*Playlist, error) {
	pl := &Playlist{
		directory: directory,
		tracks:    []Track{},
		history:   []Track{},
		current:   0,
		onChange:  onChange,
		shuffle:   shuffle, // Save shuffle parameter
		startTime: time.Now(), // Remember start time
	}

	// Initialize watcher to track directory changes
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		sentry.CaptureException(err)
		return nil, err
	}
	pl.watcher = watcher

	// Load tracks from directory
	if err := pl.Reload(); err != nil {
		sentry.CaptureException(err)
		return nil, err
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
	
	log.Printf("DIAGNOSTICS: Starting playlist reload in directory %s", p.directory)

	p.tracks = []Track{}
	p.current = 0

	// Check if directory exists
	if _, err := os.Stat(p.directory); os.IsNotExist(err) {
		errorMsg := fmt.Sprintf("Directory %s does not exist", p.directory)
		log.Printf("%s", errorMsg)
		// Don't send to Sentry - this is an informational message
		return nil // Don't consider directory absence an error
	}

	log.Printf("Scanning directory %s for audio files", p.directory)
	// Don't send to Sentry - this is an informational message

	// Counters for statistics
	var (
		totalFiles      int
		supportedFiles  int
		unsupportedFiles int
		errorFiles      int
	)

	err := filepath.Walk(p.directory, func(path string, info os.FileInfo, err error) error {
		totalFiles++
		if err != nil {
			errorFiles++
			errorMsg := fmt.Sprintf("Error accessing file/directory %s: %v", path, err)
			log.Printf("%s", errorMsg)
			sentry.CaptureException(fmt.Errorf("%s: %w", errorMsg, err)) // Send to Sentry as an error
			return nil // Continue processing even if an individual file is inaccessible
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
				log.Printf("Skipping unsupported file: %s (extension: %s)", filepath.Base(path), ext)
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
		log.Printf("%s", errorMsg)
		
		// Don't send to Sentry - this is an informational message
		return nil // Don't consider absence of tracks an error
	}

	// Log found tracks
	log.Printf("Found %d tracks in %s:", len(p.tracks), p.directory)
	
	// Show maximum 3 tracks (was 5) to reduce output volume
	trackNames := make([]string, 0, min(3, len(p.tracks)))
	for i, track := range p.tracks {
		if i < 3 { // Show only first 3 tracks
			log.Printf("  %d. %s", i+1, track.Name)
			trackNames = append(trackNames, track.Name)
		} else if i == 3 {
			log.Printf("  ... and %d more tracks", len(p.tracks)-3)
			break
		}
	}

	// Shuffle tracks only if the corresponding flag is enabled
	if p.shuffle {
		log.Printf("DIAGNOSTICS: Shuffle enabled for playlist %s", p.directory)
		p.Shuffle()
	} else {
		log.Printf("DIAGNOSTICS: Shuffle disabled for playlist %s", p.directory)
	}

	// Add directory to watcher
	if err := p.watcher.Add(p.directory); err != nil {
		sentry.CaptureException(err) // Send to Sentry as an error
		return err
	}

	// Send statistics only to logs
	log.Printf("Playlist loaded: %s, tracks: %d", p.directory, len(p.tracks))
	
	return nil
}

// min returns the minimum of two numbers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// GetCurrentTrack returns the current track
func (p *Playlist) GetCurrentTrack() interface{} {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	if len(p.tracks) == 0 {
		return nil
	}
	return &p.tracks[p.current]
}

// NextTrack moves to the next track and returns it
func (p *Playlist) NextTrack() interface{} {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if len(p.tracks) == 0 {
		return nil
	}

	// Add current track to history before moving to the next
	currentTrack := p.tracks[p.current]
	p.addTrackToHistory(currentTrack)

	// Move to the next track
	p.current = (p.current + 1) % len(p.tracks)
	
	// If we reached the end of playlist and shuffle is enabled, reshuffle for the next cycle
	if p.current == 0 && p.shuffle {
		log.Printf("DIAGNOSTICS: Reached end of playlist, will reshuffle for next cycle")
		go p.reshuffleAtEnd() // Launch reshuffling in a separate goroutine
	}
	
	return &p.tracks[p.current]
}

// reshuffleAtEnd reshuffles the playlist after a small delay
// to avoid issues with current playback
func (p *Playlist) reshuffleAtEnd() {
	// Small delay to allow current track to start playing
	time.Sleep(10 * time.Millisecond) // Reduced from 500ms to 10ms for tests
	
	log.Printf("DIAGNOSTICS: reshuffleAtEnd() called, attempting to shuffle playlist")
	
	// Using a timeout to prevent deadlocks
	done := make(chan bool)
	
	// Start shuffle in a goroutine with a timeout
	go func() {
		log.Printf("DIAGNOSTICS: reshuffleAtEnd() goroutine starting Shuffle()")
		p.Shuffle()
		done <- true
	}()
	
	// Wait for shuffle with timeout
	select {
	case <-done:
		log.Printf("DIAGNOSTICS: reshuffleAtEnd() shuffle completed successfully")
	case <-time.After(5 * time.Second):
		log.Printf("ERROR: reshuffleAtEnd() shuffle operation timed out after 5 seconds")
	}
}

// PreviousTrack moves to the previous track and returns it
func (p *Playlist) PreviousTrack() interface{} {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if len(p.tracks) == 0 {
		return nil
	}

	// Add current track to history before moving to the previous
	currentTrack := p.tracks[p.current]
	p.addTrackToHistory(currentTrack)

	// Move to the previous track considering the possibility of going to a negative index
	if p.current == 0 {
		p.current = len(p.tracks) - 1
	} else {
		p.current--
	}
	
	log.Printf("DIAGNOSTICS: Switching to previous track: %s", p.tracks[p.current].Name)
	return &p.tracks[p.current]
}

// addTrackToHistory adds a track to history
func (p *Playlist) addTrackToHistory(track Track) {
	p.historyMutex.Lock()
	defer p.historyMutex.Unlock()
	
	// Add track to history
	p.history = append(p.history, track)
	
	// Limit history size - keep last 100 tracks
	const maxHistorySize = 100
	if len(p.history) > maxHistorySize {
		p.history = p.history[len(p.history)-maxHistorySize:]
	}
}

// GetHistory returns the history of played tracks
func (p *Playlist) GetHistory() []interface{} {
	p.historyMutex.RLock()
	defer p.historyMutex.RUnlock()
	
	history := make([]interface{}, len(p.history))
	for i, track := range p.history {
		history[i] = &track
	}
	return history
}

// GetStartTime returns the playlist start time
func (p *Playlist) GetStartTime() time.Time {
	return p.startTime
}

// Shuffle randomizes the track list
func (p *Playlist) Shuffle() {
	log.Printf("DIAGNOSTICS: Starting playlist shuffle for directory %s...", p.directory)
	
	log.Printf("DIAGNOSTICS: Trying to acquire mutex lock for shuffle operation...")
	p.mutex.Lock()
	log.Printf("DIAGNOSTICS: Mutex lock acquired for shuffle operation")
	defer func() {
		log.Printf("DIAGNOSTICS: Releasing mutex lock after shuffle operation")
		p.mutex.Unlock()
	}()

	if len(p.tracks) <= 1 {
		log.Printf("DIAGNOSTICS: Shuffle not required, tracks <= 1")
		return
	}

	log.Printf("DIAGNOSTICS: Checking current track to save it before shuffle")
	// Save current track to restore it after shuffling
	var currentTrack Track
	if p.current < len(p.tracks) {
		currentTrack = p.tracks[p.current]
		log.Printf("DIAGNOSTICS: Current track before shuffle: %s", currentTrack.Name)
	} else {
		log.Printf("DIAGNOSTICS: Current track index %d is invalid (tracks length: %d)", p.current, len(p.tracks))
	}

	log.Printf("DIAGNOSTICS: Creating log of original track order")
	// Create a copy of the original order for logging
	originalOrder := make([]string, min(5, len(p.tracks)))
	for i := 0; i < len(originalOrder); i++ {
		originalOrder[i] = p.tracks[i].Name
	}
	log.Printf("DIAGNOSTICS: First 5 tracks before shuffle: %v", originalOrder)

	// Use Fisher-Yates shuffling algorithm
	log.Printf("DIAGNOSTICS: Starting Fisher-Yates shuffling algorithm for %d tracks...", len(p.tracks))
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	
	log.Printf("DIAGNOSTICS: Running Fisher-Yates shuffle loop")
	for i := len(p.tracks) - 1; i > 0; i-- {
		if i%100 == 0 {
			log.Printf("DIAGNOSTICS: Shuffling progress: %d/%d tracks processed", len(p.tracks)-i, len(p.tracks))
		}
		j := r.Intn(i + 1)
		p.tracks[i], p.tracks[j] = p.tracks[j], p.tracks[i]
	}
	log.Printf("DIAGNOSTICS: Fisher-Yates shuffle completed")

	log.Printf("DIAGNOSTICS: Creating log of shuffled track order")
	// Log the first few tracks after shuffling
	shuffledOrder := make([]string, min(5, len(p.tracks)))
	for i := 0; i < len(shuffledOrder); i++ {
		shuffledOrder[i] = p.tracks[i].Name
	}
	log.Printf("DIAGNOSTICS: First 5 tracks after shuffle: %v", shuffledOrder)

	log.Printf("DIAGNOSTICS: Attempting to restore current track position")
	// Restore the pointer to the current track if possible
	if len(p.tracks) > 0 && currentTrack.Path != "" {
		found := false
		for i, track := range p.tracks {
			if track.Path == currentTrack.Path {
				p.current = i
				log.Printf("DIAGNOSTICS: Restored current track position to %d", i)
				found = true
				break
			}
		}
		if !found {
			log.Printf("DIAGNOSTICS: Warning - Failed to find original track after shuffle")
		}
	} else {
		log.Printf("DIAGNOSTICS: Skipping track restoration - no valid tracks or current track")
	}

	log.Printf("DIAGNOSTICS: Playlist successfully shuffled: %s, tracks: %d", p.directory, len(p.tracks))
}

// GetTracks returns a copy of the track list
func (p *Playlist) GetTracks() []Track {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	tracks := make([]Track, len(p.tracks))
	copy(tracks, p.tracks)
	return tracks
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
				log.Printf("Change detected in playlist: %s", event.Name)
				
				// Reload playlist
				if err := p.Reload(); err != nil {
					log.Printf("Error reloading playlist: %s", err)
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
			log.Printf("fsnotify error: %s", err)
			sentry.CaptureException(fmt.Errorf("fsnotify error: %w", err))
		}
	}
} 