package unit

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/user/stream-audio-to-web/audio"
)

func TestVolumeCache(t *testing.T) {
	cache := audio.NewVolumeCache()
	
	// Test setting and getting values
	cache.Set("file1.mp3", 0.5)
	cache.Set("file2.mp3", 0.75)
	
	// Test retrieving values
	if rms, exists := cache.Get("file1.mp3"); !exists || rms != 0.5 {
		t.Errorf("Expected RMS 0.5 for file1.mp3, got %f, exists: %v", rms, exists)
	}
	
	if rms, exists := cache.Get("file2.mp3"); !exists || rms != 0.75 {
		t.Errorf("Expected RMS 0.75 for file2.mp3, got %f, exists: %v", rms, exists)
	}
	
	// Test non-existent file
	if _, exists := cache.Get("nonexistent.mp3"); exists {
		t.Errorf("Expected non-existent file to return exists=false")
	}
}

func TestCalculateGainFactor(t *testing.T) {
	tests := []struct {
		name      string
		rmsLevel  float64
		expected  float64
	}{
		{
			name:     "Zero RMS returns 1.0",
			rmsLevel: 0.0,
			expected: 1.0,
		},
		{
			name:     "Negative RMS returns 1.0",
			rmsLevel: -0.5,
			expected: 1.0,
		},
		{
			name:     "Equal to target returns 1.0",
			rmsLevel: 0.2, // target RMS level
			expected: 1.0,  
		},
		{
			name:     "Louder than target gets reduced",
			rmsLevel: 0.4, // twice as loud as target
			expected: 0.5, // should be halved
		},
		{
			name:     "Quieter than target gets amplified",
			rmsLevel: 0.1, // half as loud as target
			expected: 2.0, // should be doubled
		},
		{
			name:     "Very quiet gets limited to max gain",
			rmsLevel: 0.01, // very quiet
			expected: 10.0, // should be limited to max gain
		},
		{
			name:     "Very loud gets limited to min gain",
			rmsLevel: 3.0, // very loud
			expected: 0.1, // should be limited to min gain
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Make the calculateGainFactor function public for testing
			result := audio.CalculateGainFactor(tt.rmsLevel)
			
			if result != tt.expected {
				t.Errorf("Expected gain factor %f for RMS %f, got %f", 
					tt.expected, tt.rmsLevel, result)
			}
		})
	}
}

func TestAnalyzeFileVolume(t *testing.T) {
	// Skip this test if SKIP_AUDIO_ANALYSIS is set
	if os.Getenv("SKIP_AUDIO_ANALYSIS") != "" {
		t.Skip("Skipping audio analysis test due to SKIP_AUDIO_ANALYSIS environment variable")
	}
	
	// Create a temporary test file
	testDir := t.TempDir()
	testFilePath := filepath.Join(testDir, "test_audio.raw")
	
	// Create test audio data
	// Generate a sine wave at different volumes
	const sampleRate = 44100
	const duration = 1 // 1 second
	buffer := generateTestAudioData(sampleRate, duration)
	
	// Write test data to file
	err := os.WriteFile(testFilePath, buffer, 0644)
	if err != nil {
		t.Fatalf("Failed to write test audio file: %v", err)
	}
	
	// Test the analyzeFileVolume function
	rmsLevel, err := audio.AnalyzeFileVolume(testFilePath)
	if err != nil {
		t.Fatalf("AnalyzeFileVolume returned error: %v", err)
	}
	
	// We expect a non-zero RMS level for our test data
	if rmsLevel <= 0 {
		t.Errorf("Expected non-zero RMS level, got %f", rmsLevel)
	}
	
	// Clean up
	os.Remove(testFilePath)
}

func TestNormalizeMP3Stream(t *testing.T) {
	// Skip this test if SKIP_AUDIO_ANALYSIS is set
	if os.Getenv("SKIP_AUDIO_ANALYSIS") != "" {
		t.Skip("Skipping audio normalization test due to SKIP_AUDIO_ANALYSIS environment variable")
	}
	
	// Create a temporary test file
	testDir := t.TempDir()
	testFilePath := filepath.Join(testDir, "test_audio.mp3")
	
	// Create test audio data
	const sampleRate = 44100
	const duration = 1 // 1 second
	buffer := generateTestAudioData(sampleRate, duration)
	
	// Write test data to file
	err := os.WriteFile(testFilePath, buffer, 0644)
	if err != nil {
		t.Fatalf("Failed to write test audio file: %v", err)
	}
	
	// Open the file for normalization
	file, err := os.Open(testFilePath)
	if err != nil {
		t.Fatalf("Failed to open test audio file: %v", err)
	}
	defer file.Close()
	
	// Create a buffer to capture normalized output
	var outputBuffer bytes.Buffer
	
	// Test the NormalizeMP3Stream function
	err = audio.NormalizeMP3Stream(file, &outputBuffer)
	if err != nil {
		t.Fatalf("NormalizeMP3Stream returned error: %v", err)
	}
	
	// We expect some output data
	if outputBuffer.Len() == 0 {
		t.Error("Expected non-empty output from normalization")
	}
	
	// Clean up
	os.Remove(testFilePath)
}

// generateTestAudioData creates test audio data with varying volumes
func generateTestAudioData(sampleRate int, durationSec float64) []byte {
	// Number of samples
	numSamples := int(float64(sampleRate) * durationSec)
	
	// Create buffer
	buffer := make([]byte, numSamples)
	
	// Generate alternating loud and quiet sections
	for i := 0; i < numSamples; i++ {
		// Calculate sample position (0.0 to 1.0)
		position := float64(i) / float64(numSamples)
		
		// Determine volume factor (alternating between loud and quiet)
		volumeFactor := 0.0
		if position < 0.25 {
			volumeFactor = 0.2 // Quiet
		} else if position < 0.5 {
			volumeFactor = 0.8 // Loud
		} else if position < 0.75 {
			volumeFactor = 0.4 // Medium
		} else {
			volumeFactor = 1.0 // Very loud
		}
		
		// Generate unsigned 8-bit sample centered at 128
		sample := byte(128 + int(volumeFactor*127))
		buffer[i] = sample
	}
	
	return buffer
} 