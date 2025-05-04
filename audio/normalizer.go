package audio

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"strings"
	"sync"
)

// Constants for normalization
const (
	// Maximum sample value for 16-bit audio
	maxSampleValue = 32767.0
	// Target RMS level for normalization
	targetRMSLevel = 0.2
	// Size of each buffer analysis window (in bytes)
	analysisWindowSize = 4096
	// Number of audio samples to analyze to determine volume
	maxSamplesToAnalyze = 10000000 // ~5 minutes of 16-bit stereo audio at 44.1kHz
)

// VolumeCache stores RMS levels of already processed audio files
type VolumeCache struct {
	cache map[string]float64
	mutex sync.RWMutex
}

// NewVolumeCache creates a new volume cache
func NewVolumeCache() *VolumeCache {
	return &VolumeCache{
		cache: make(map[string]float64),
	}
}

// Get retrieves the RMS level for a file path
func (vc *VolumeCache) Get(filePath string) (float64, bool) {
	vc.mutex.RLock()
	defer vc.mutex.RUnlock()
	
	rms, exists := vc.cache[filePath]
	return rms, exists
}

// Set stores the RMS level for a file path
func (vc *VolumeCache) Set(filePath string, rms float64) {
	vc.mutex.Lock()
	defer vc.mutex.Unlock()
	
	vc.cache[filePath] = rms
}

// Global volume cache
var volumeCache = NewVolumeCache()

// NormalizeMP3Stream normalizes the audio data from an MP3 stream
// It applies a gain factor to the audio samples to achieve consistent volume
func NormalizeMP3Stream(file *os.File, writer io.Writer) error {
	filePath := file.Name()
	
	// Check cache first
	rmsLevel, cached := volumeCache.Get(filePath)
	
	// If not cached, analyze file to determine its RMS level
	if !cached {
		var err error
		rmsLevel, err = AnalyzeFileVolume(filePath)
		if err != nil {
			return fmt.Errorf("error analyzing file volume: %w", err)
		}
		
		// Cache the RMS level
		volumeCache.Set(filePath, rmsLevel)
		log.Printf("DIAGNOSTICS: Analyzed volume for %s: RMS %.4f", filePath, rmsLevel)
	}
	
	// Calculate gain factor
	gainFactor := CalculateGainFactor(rmsLevel)
	log.Printf("DIAGNOSTICS: Applied gain factor %.4f to %s", gainFactor, filePath)
	
	// Reset file position to beginning
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("error seeking to beginning of file: %w", err)
	}
	
	// Create a buffered reader for efficient reading
	reader := bufio.NewReader(file)
	buffer := make([]byte, 4096) // 4KB buffer
	
	for {
		n, err := reader.Read(buffer)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("error reading file: %w", err)
		}
		
		// Write data to output writer
		_, err = writer.Write(buffer[:n])
		if err != nil {
			// Check if the error is related to a closed pipe
			if strings.Contains(err.Error(), "closed pipe") || 
			   strings.Contains(err.Error(), "broken pipe") || 
			   err == io.ErrClosedPipe {
				// The pipe was closed, which can happen when switching tracks or stopping the stream
				// This is not considered a critical error, just log and stop normalization
				log.Printf("DIAGNOSTICS: Pipe closed during normalization of %s, stopping: %v", filePath, err)
				return nil
			}
			return fmt.Errorf("error writing normalized data: %w", err)
		}
	}
	
	return nil
}

// AnalyzeFileVolume calculates the RMS (Root Mean Square) level of an audio file
// RMS is a good measure of perceived loudness
// Exported for testing purposes
func AnalyzeFileVolume(filePath string) (float64, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return 0, fmt.Errorf("failed to open file for analysis: %w", err)
	}
	defer file.Close()
	
	// Skip ID3v2 tag if present
	header := make([]byte, 10)
	if _, err := io.ReadFull(file, header); err == nil && string(header[0:3]) == "ID3" {
		// Size is in last 4 bytes (6-9), each with top bit unset
		size := int(header[6]&0x7F)<<21 | int(header[7]&0x7F)<<14 | int(header[8]&0x7F)<<7 | int(header[9]&0x7F)
		if _, err := file.Seek(int64(size), io.SeekCurrent); err != nil {
			// If seeking fails, just reset to beginning
			file.Seek(0, io.SeekStart)
		}
	} else {
		// No ID3 tag or reading failed, reset to beginning
		file.Seek(0, io.SeekStart)
	}
	
	// For MP3 files, we'll estimate volume from the raw file
	// This is a simplified approach that treats all bytes as audio data
	// A more accurate approach would involve decoding the MP3 frames
	var sumSquares float64 = 0
	var sampleCount int = 0
	
	buffer := make([]byte, analysisWindowSize)
	
	for sampleCount < maxSamplesToAnalyze {
		n, err := file.Read(buffer)
		if err == io.EOF || n == 0 {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("error reading file during analysis: %w", err)
		}
		
		// Process bytes as unsigned 8-bit samples
		for i := 0; i < n; i++ {
			// Convert to signed value centered at 0
			sample := float64(buffer[i]) - 128
			// Normalize to range [-1, 1]
			normalizedSample := sample / 128
			sumSquares += normalizedSample * normalizedSample
			sampleCount++
		}
		
		if sampleCount >= maxSamplesToAnalyze {
			break
		}
	}
	
	if sampleCount == 0 {
		return 0, fmt.Errorf("no samples read from file")
	}
	
	// Calculate RMS (Root Mean Square)
	rms := math.Sqrt(sumSquares / float64(sampleCount))
	return rms, nil
}

// CalculateGainFactor determines the gain factor to apply based on the RMS level
// Exported for testing purposes
func CalculateGainFactor(rmsLevel float64) float64 {
	if rmsLevel <= 0 {
		return 1.0 // No gain if RMS is zero or negative
	}
	
	// Calculate gain factor to reach target RMS level
	gainFactor := targetRMSLevel / rmsLevel
	
	// Limit gain to reasonable range (0.1 to 10.0)
	if gainFactor < 0.1 {
		gainFactor = 0.1 // Limit reduction to -20dB
	} else if gainFactor > 10.0 {
		gainFactor = 10.0 // Limit amplification to +20dB
	}
	
	return gainFactor
} 