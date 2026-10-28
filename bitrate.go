package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// checkAndConvertBitrate checks all audio files in directory and converts them to target bitrate if needed.
func checkAndConvertBitrate(logger *slog.Logger, dir, route string, targetBitrate int) bool {
	logger.Info("BITRATE CONVERSION: Starting bitrate check for directory",
		slog.String("directory", dir),
		slog.String("route", route),
		slog.Int("targetBitrate", targetBitrate))

	files, readErr := os.ReadDir(dir)
	if readErr != nil {
		logger.Error("ERROR: When reading directory for bitrate conversion",
			"directory", dir,
			"error", readErr,
		)
		if globalSentryHelper != nil {
			globalSentryHelper.CaptureError(fmt.Errorf("error reading directory %s for bitrate conversion: %w", dir, readErr), "main", "bitrate_conversion")
		}
		return false
	}

	convertedFiles := 0
	skippedFiles := 0
	totalAudioFiles := 0
	filesToProcess := 0

	// First pass: count total audio files and files that need conversion.
	for _, file := range files {
		fileName := file.Name()
		lowerFileName := strings.ToLower(fileName)

		// Check if it's an audio file.
		isMP3 := strings.HasSuffix(lowerFileName, ".mp3")
		isOGG := strings.HasSuffix(lowerFileName, ".ogg")
		isAAC := strings.HasSuffix(lowerFileName, ".aac")
		isWAV := strings.HasSuffix(lowerFileName, ".wav")
		isFLAC := strings.HasSuffix(lowerFileName, ".flac")
		isHidden := strings.HasPrefix(fileName, ".")

		// Skip non-audio files, directories, and hidden files.
		if file.IsDir() || isHidden || !(isMP3 || isOGG || isAAC || isWAV || isFLAC) {
			continue
		}

		totalAudioFiles++
		filePath := filepath.Join(dir, fileName)

		// Check current bitrate of the file.
		currentBitrate, err := getAudioBitrate(logger, filePath)
		if err != nil {
			logger.Error("BITRATE CONVERSION: Failed to get bitrate for file",
				slog.String("file", filePath),
				slog.String("error", err.Error()))
			continue
		}

		// Check if conversion is needed.
		if currentBitrate != targetBitrate {
			filesToProcess++
		}
	}

	if filesToProcess == 0 {
		logger.Info("BITRATE CONVERSION: All files already have target bitrate",
			slog.String("route", route),
			slog.Int("totalFiles", totalAudioFiles),
			slog.Int("targetBitrate", targetBitrate))
		return true
	}

	logger.Info("BITRATE CONVERSION: Files analysis completed",
		slog.String("route", route),
		slog.Int("totalFiles", totalAudioFiles),
		slog.Int("filesToProcess", filesToProcess),
		slog.Int("targetBitrate", targetBitrate))

	startTime := time.Now()

	// Second pass: convert files that need conversion.
	for _, file := range files {
		fileName := file.Name()
		lowerFileName := strings.ToLower(fileName)

		// Check if it's an audio file.
		isMP3 := strings.HasSuffix(lowerFileName, ".mp3")
		isOGG := strings.HasSuffix(lowerFileName, ".ogg")
		isAAC := strings.HasSuffix(lowerFileName, ".aac")
		isWAV := strings.HasSuffix(lowerFileName, ".wav")
		isFLAC := strings.HasSuffix(lowerFileName, ".flac")
		isHidden := strings.HasPrefix(fileName, ".")

		// Skip non-audio files, directories, and hidden files.
		if file.IsDir() || isHidden || !(isMP3 || isOGG || isAAC || isWAV || isFLAC) {
			continue
		}

		filePath := filepath.Join(dir, fileName)

		// Check current bitrate of the file.
		currentBitrate, err := getAudioBitrate(logger, filePath)
		if err != nil {
			logger.Error("BITRATE CONVERSION: Failed to get bitrate for file",
				slog.String("file", filePath),
				slog.String("error", err.Error()))
			continue
		}

		logger.Debug("BITRATE CONVERSION: File bitrate detected",
			slog.String("file", fileName),
			slog.Int("currentBitrate", currentBitrate),
			slog.Int("targetBitrate", targetBitrate))

		// Check if conversion is needed.
		if currentBitrate == targetBitrate {
			logger.Debug("BITRATE CONVERSION: File already has target bitrate, skipping",
				slog.String("file", fileName),
				slog.Int("bitrate", currentBitrate))
			skippedFiles++
			continue
		}

		// Convert file to target bitrate.
		fileStartTime := time.Now()
		if !convertAudioBitrate(logger, filePath, targetBitrate) {
			logger.Error("BITRATE CONVERSION: Failed to convert file",
				slog.String("file", filePath),
				slog.Int("fromBitrate", currentBitrate),
				slog.Int("toBitrate", targetBitrate))
			continue
		}

		convertedFiles++
		processingTime := time.Since(fileStartTime)

		// Calculate estimated time remaining.
		remainingFiles := filesToProcess - convertedFiles
		var estimatedTimeRemaining time.Duration
		if convertedFiles > 0 {
			avgTimePerFile := time.Since(startTime) / time.Duration(convertedFiles)
			estimatedTimeRemaining = avgTimePerFile * time.Duration(remainingFiles)
		}

		logger.Info("BITRATE CONVERSION: File successfully converted",
			slog.String("file", fileName),
			slog.String("route", route),
			slog.Int("fromBitrate", currentBitrate),
			slog.Int("toBitrate", targetBitrate),
			slog.Int("processed", convertedFiles),
			slog.Int("remaining", remainingFiles),
			slog.Int("total", filesToProcess),
			slog.String("processingTime", processingTime.String()),
			slog.String("estimatedTimeRemaining", estimatedTimeRemaining.String()))
	}

	totalTime := time.Since(startTime)
	logger.Info("BITRATE CONVERSION: Completed for directory",
		slog.String("directory", dir),
		slog.String("route", route),
		slog.Int("totalAudioFiles", totalAudioFiles),
		slog.Int("convertedFiles", convertedFiles),
		slog.Int("skippedFiles", skippedFiles),
		slog.Int("targetBitrate", targetBitrate),
		slog.String("totalTime", totalTime.String()))

	return true
}

// getAudioBitrate returns the bitrate of an audio file using ffprobe.
func getAudioBitrate(logger *slog.Logger, filePath string) (int, error) {
	cmd := exec.Command("ffprobe", "-v", "quiet", "-select_streams", "a:0", "-show_entries", "stream=bit_rate", "-of", "csv=p=0", filePath)
	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe failed: %w", err)
	}

	// Clean and split the output
	bitrateStr := strings.TrimSpace(string(output))
	if bitrateStr == "" || bitrateStr == "N/A" {
		return 0, fmt.Errorf("could not determine bitrate")
	}

	// Split by newline and take first non-empty value
	parts := strings.Split(bitrateStr, "\n")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" && part != "N/A" {
			bitrateFloat, err := strconv.ParseFloat(part, 64)
			if err != nil {
				continue
			}
			// Convert from bits per second to kilobits per second
			bitrateKbps := int(bitrateFloat / 1000)
			if bitrateKbps > 0 {
				return bitrateKbps, nil
			}
		}
	}

	// If we couldn't get bitrate, try alternative method
	cmd = exec.Command("ffprobe", "-v", "quiet", "-select_streams", "a:0", "-show_entries", "format=bit_rate", "-of", "csv=p=0", filePath)
	output, err = cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe format check failed: %w", err)
	}

	bitrateStr = strings.TrimSpace(string(output))
	if bitrateStr == "" || bitrateStr == "N/A" {
		return 0, fmt.Errorf("could not determine bitrate from format")
	}

	parts = strings.Split(bitrateStr, "\n")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" && part != "N/A" {
			bitrateFloat, err := strconv.ParseFloat(part, 64)
			if err != nil {
				continue
			}
			// Convert from bits per second to kilobits per second
			bitrateKbps := int(bitrateFloat / 1000)
			if bitrateKbps > 0 {
				return bitrateKbps, nil
			}
		}
	}

	return 0, fmt.Errorf("could not determine bitrate after all attempts")
}

// convertAudioBitrate converts an audio file to the target bitrate using ffmpeg.
func convertAudioBitrate(logger *slog.Logger, filePath string, targetBitrate int) bool {
	// Create temporary file for conversion.
	tempFile := filePath + ".temp"

	// Remove temp file if it exists.
	if _, err := os.Stat(tempFile); err == nil {
		os.Remove(tempFile)
	}

	// Build ffmpeg command with explicit format specification.
	cmd := exec.Command("ffmpeg", "-y", "-i", filePath, "-b:a", fmt.Sprintf("%dk", targetBitrate), "-codec:a", "libmp3lame", "-f", "mp3", tempFile)

	// Execute conversion.
	output, err := cmd.CombinedOutput()
	if err != nil {
		logger.Error("BITRATE CONVERSION: ffmpeg conversion failed",
			slog.String("file", filePath),
			slog.Int("targetBitrate", targetBitrate),
			slog.String("error", err.Error()),
			slog.String("output", string(output)))

		// Clean up temp file.
		os.Remove(tempFile)
		return false
	}

	// Replace original file with converted file.
	if err := os.Rename(tempFile, filePath); err != nil {
		logger.Error("BITRATE CONVERSION: Failed to replace original file",
			slog.String("file", filePath),
			slog.String("error", err.Error()))

		// Clean up temp file.
		os.Remove(tempFile)
		return false
	}

	logger.Debug("BITRATE CONVERSION: Successfully converted file",
		slog.String("file", filePath),
		slog.Int("targetBitrate", targetBitrate))

	return true
}
