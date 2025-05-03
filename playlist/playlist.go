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

	// Add current track to history to ensure history has at least one track
	if len(p.tracks) > 0 {
		log.Printf("DIAGNOSTICS: Adding initial track %s to history", p.tracks[p.current].Name)
		p.addTrackToHistory(p.tracks[p.current])
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
		log.Printf("DIAGNOSTICS: NextTrack() called but no tracks available")
		return nil
	}

	// Add current track to history before moving to the next
	currentTrack := p.tracks[p.current]
	log.Printf("DIAGNOSTICS: Adding current track %s to history before moving to next", currentTrack.Name)
	p.addTrackToHistory(currentTrack)

	// Move to the next track
	p.current = (p.current + 1) % len(p.tracks)
	nextTrack := p.tracks[p.current]
	log.Printf("DIAGNOSTICS: Moved to next track: %s (position: %d)", nextTrack.Name, p.current)
	
	// If we reached the end of playlist and shuffle is enabled, reshuffle for the next cycle
	if p.current == 0 && p.shuffle {
		log.Printf("DIAGNOSTICS: Reached end of playlist, will reshuffle for next cycle")
		go p.reshuffleAtEnd() // Launch reshuffling in a separate goroutine
	}
	
	// Verify history has been updated
	p.historyMutex.RLock()
	historyLength := len(p.history)
	p.historyMutex.RUnlock()
	log.Printf("DIAGNOSTICS: History length after NextTrack(): %d", historyLength)
	
	return &p.tracks[p.current]
}

// reshuffleAtEnd reshuffles the playlist after a small delay
// to avoid issues with current playback
func (p *Playlist) reshuffleAtEnd() {
	// Small delay to allow current track to start playing
	time.Sleep(10 * time.Millisecond)
	
	// Perform shuffle in a simple goroutine
	go func() {
		log.Printf("DIAGNOSTICS: Performing deferred shuffle for playlist %s", p.directory)
		p.Shuffle()
	}()
}

// PreviousTrack moves to the previous track and returns it
func (p *Playlist) PreviousTrack() interface{} {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if len(p.tracks) == 0 {
		log.Printf("DIAGNOSTICS: PreviousTrack() called but no tracks available")
		return nil
	}

	// Add current track to history before moving to the previous
	currentTrack := p.tracks[p.current]
	log.Printf("DIAGNOSTICS: Adding current track %s to history before moving to previous", currentTrack.Name)
	p.addTrackToHistory(currentTrack)

	// Move to the previous track considering the possibility of going to a negative index
	if p.current == 0 {
		p.current = len(p.tracks) - 1
	} else {
		p.current--
	}
	
	prevTrack := p.tracks[p.current]
	log.Printf("DIAGNOSTICS: Switching to previous track: %s (position: %d)", prevTrack.Name, p.current)
	
	// Verify history has been updated
	p.historyMutex.RLock()
	historyLength := len(p.history)
	p.historyMutex.RUnlock()
	log.Printf("DIAGNOSTICS: History length after PreviousTrack(): %d", historyLength)
	
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
	
	historyLen := len(p.history)
	log.Printf("DIAGNOSTICS: GetHistory() called, history length: %d", historyLen)
	
	if historyLen == 0 {
		log.Printf("DIAGNOSTICS: Warning - History is empty!")
		return []interface{}{}
	}
	
	history := make([]interface{}, historyLen)
	for i, track := range p.history {
		history[i] = &track
	}
	
	// Log first few tracks in history
	if historyLen > 0 {
		trackNames := make([]string, 0, min(3, historyLen))
		for i := 0; i < min(3, historyLen); i++ {
			if track, ok := history[i].(interface{ GetPath() string }); ok {
				trackNames = append(trackNames, filepath.Base(track.GetPath()))
			}
		}
		log.Printf("DIAGNOSTICS: First tracks in history: %v", trackNames)
	}
	
	return history
}

// GetStartTime returns the playlist start time
func (p *Playlist) GetStartTime() time.Time {
	return p.startTime
}

// Shuffle randomizes the track list
func (p *Playlist) Shuffle() {
	callerID := fmt.Sprintf("%p", p) // Unique ID for this playlist instance
	log.Printf("SHUFFLE-%s: Starting playlist shuffle for directory %s...", callerID, p.directory)
	
	// Create a copy of tracks slice to avoid long mutex locking
	var tracksToShuffle []Track
	
	// Get a snapshot of tracks with a short lock
	func() {
		// Try to acquire the lock (short operation)
		lockStart := time.Now()
		log.Printf("SHUFFLE-%s: Attempting to acquire mutex lock", callerID)
		p.mutex.Lock()
		lockTime := time.Since(lockStart)
		log.Printf("SHUFFLE-%s: Mutex lock acquired in %v", callerID, lockTime)
		defer func() {
			unlockStart := time.Now()
			p.mutex.Unlock()
			log.Printf("SHUFFLE-%s: Mutex lock released in %v", callerID, time.Since(unlockStart))
		}()
		
		// Measure the time of the copy operation
		copyStart := time.Now()
		log.Printf("SHUFFLE-%s: Making a copy of tracks list (%d tracks)", callerID, len(p.tracks))
		
		// Make a copy of the tracks
		tracksToShuffle = make([]Track, len(p.tracks))
		copy(tracksToShuffle, p.tracks)
		
		copyTime := time.Since(copyStart)
		log.Printf("SHUFFLE-%s: Track list copied in %v", callerID, copyTime)
		
		// Save current track pointer for restoration
		if len(p.tracks) > 0 && p.current < len(p.tracks) {
			log.Printf("SHUFFLE-%s: Current track is %s (position: %d)", 
				callerID, p.tracks[p.current].Name, p.current)
		}
	}()
	
	// Check if we have enough tracks to shuffle
	if len(tracksToShuffle) <= 1 {
		log.Printf("SHUFFLE-%s: Not enough tracks to shuffle (%d)", callerID, len(tracksToShuffle))
		return
	}
	
	// Create a new random source (independent of global state)
	randomStart := time.Now()
	log.Printf("SHUFFLE-%s: Creating random number generator", callerID)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	
	// Shuffle the copy outside of lock
	log.Printf("SHUFFLE-%s: Starting to shuffle %d tracks outside of lock", callerID, len(tracksToShuffle)) 
	
	// Use memory-efficient shuffling for large playlists
	trackCount := len(tracksToShuffle)
	if trackCount > 500 {
		// For very large playlists, just shuffle a portion to be efficient
		log.Printf("SHUFFLE-%s: Using optimized shuffling for large playlist (%d tracks)", callerID, trackCount)
		for i := 0; i < min(100, trackCount-1); i++ {
			j := r.Intn(trackCount-i) + i
			tracksToShuffle[i], tracksToShuffle[j] = tracksToShuffle[j], tracksToShuffle[i]
		}
	} else {
		// Standard Fisher-Yates for smaller playlists
		for i := trackCount - 1; i > 0; i-- {
			j := r.Intn(i + 1)
			tracksToShuffle[i], tracksToShuffle[j] = tracksToShuffle[j], tracksToShuffle[i]
		}
	}
	
	shuffleTime := time.Since(randomStart)
	log.Printf("SHUFFLE-%s: Tracks shuffled in %v", callerID, shuffleTime)
	
	// Apply shuffled tracks with a short lock
	func() {
		applyStart := time.Now()
		log.Printf("SHUFFLE-%s: Attempting to acquire mutex lock to apply shuffled tracks", callerID)
		// Try to acquire the lock again (short operation)
		lockStart := time.Now()
		p.mutex.Lock()
		lockTime := time.Since(lockStart)
		log.Printf("SHUFFLE-%s: Mutex lock for applying changes acquired in %v", callerID, lockTime)
		defer func() {
			unlockStart := time.Now()
			p.mutex.Unlock()
			log.Printf("SHUFFLE-%s: Final mutex lock released in %v", callerID, time.Since(unlockStart))
		}()
		
		// Get current track before replacing
		var currentTrackPath string
		if len(p.tracks) > 0 && p.current < len(p.tracks) {
			currentTrackPath = p.tracks[p.current].Path
		}
		
		// Replace the tracks with shuffled version
		p.tracks = tracksToShuffle
		
		// If we had a current track, try to find it in shuffled list
		if currentTrackPath != "" {
			found := false
			for i, track := range p.tracks {
				if track.Path == currentTrackPath {
					p.current = i
					log.Printf("SHUFFLE-%s: Restored current track position to %d", callerID, i)
					found = true
					break
				}
			}
			
			if !found && len(p.tracks) > 0 {
				p.current = 0
				log.Printf("SHUFFLE-%s: Could not find original track after shuffle, reset to beginning", callerID)
			}
		}
		
		applyTime := time.Since(applyStart)
		log.Printf("SHUFFLE-%s: Applied shuffled tracks in %v", callerID, applyTime)
	}()
	
	totalTime := time.Since(randomStart)
	log.Printf("SHUFFLE-%s: Playlist shuffle completed for %s in total %v", callerID, p.directory, totalTime)
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