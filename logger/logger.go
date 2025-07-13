package logger

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	slogmulti "github.com/samber/slog-multi"
	slogsampling "github.com/samber/slog-sampling"
)

// LogLevel represents the logging level.
type LogLevel string

const (
	LevelDebug   LogLevel = "DEBUG"
	LevelInfo    LogLevel = "INFO"
	LevelWarning LogLevel = "WARNING"
	LevelError   LogLevel = "ERROR"
)

// Config holds the logger configuration.
type Config struct {
	Level                   LogLevel
	DisableSampling         bool
	SamplingRate            float64
	ThresholdSamplingTick   time.Duration
	ThresholdSamplingMax    uint64
	ThresholdSamplingRate   float64
	EnableCustomSampling    bool
}

// DefaultConfig returns a default logger configuration.
func DefaultConfig() *Config {
	return &Config{
		Level:                   LevelWarning, // Default to WARNING to prevent spam.
		DisableSampling:         false,
		SamplingRate:            0.1, // Sample 10% of repeated logs.
		ThresholdSamplingTick:   5 * time.Second,
		ThresholdSamplingMax:    10,   // Allow first 10 identical messages.
		ThresholdSamplingRate:   0.05, // Then only 5% of subsequent messages.
		EnableCustomSampling:    true,
	}
}

// NewLogger creates a new configured logger with sampling.
func NewLogger(config *Config) *slog.Logger {
	if config == nil {
		config = DefaultConfig()
	}

	// Parse log level from environment or use config.
	level := parseLogLevel(config.Level)

	// Create base handler.
	opts := &slog.HandlerOptions{
		Level: level,
	}
	baseHandler := slog.NewJSONHandler(os.Stdout, opts)

	// Configure sampling if enabled.
	if !config.DisableSampling {
		if config.EnableCustomSampling {
			// Use custom sampling that adapts based on log level and time.
			customSamplingOption := slogsampling.CustomSamplingOption{
				Sampler: func(ctx context.Context, record slog.Record) float64 {
					// During night hours (22:00-06:00), reduce sampling for all levels.
					hour := record.Time.Hour()
					if hour >= 22 || hour <= 6 {
						switch record.Level {
						case slog.LevelError:
							return 1.0 // Always log errors.
						case slog.LevelWarn:
							return 0.8
						case slog.LevelInfo:
							return 0.5
						case slog.LevelDebug:
							return 0.2
						default:
							return 0.1
						}
					}

					// Regular hours - more aggressive sampling for non-critical logs.
					switch record.Level {
					case slog.LevelError:
						return 1.0 // Always log errors.
					case slog.LevelWarn:
						return 0.7
					case slog.LevelInfo:
						return 0.2
					case slog.LevelDebug:
						return 0.05 // Very limited debug logs.
					default:
						return 0.01
					}
				},
				OnDropped: func(ctx context.Context, record slog.Record) {
					// Optionally track dropped logs.
				},
			}

			return slog.New(
				slogmulti.
					Pipe(customSamplingOption.NewMiddleware()).
					Handler(baseHandler),
			)
		} else {
			// Use threshold sampling - allow first N messages, then apply rate.
			thresholdOption := slogsampling.ThresholdSamplingOption{
				Tick:      config.ThresholdSamplingTick,
				Threshold: config.ThresholdSamplingMax,
				Rate:      config.ThresholdSamplingRate,
				Matcher:   slogsampling.MatchByLevelAndMessage(),
			}

			return slog.New(
				slogmulti.
					Pipe(thresholdOption.NewMiddleware()).
					Handler(baseHandler),
			)
		}
	}

	// Return logger without sampling.
	return slog.New(baseHandler)
}

// NewLoggerFromEnv creates a logger using LOG_LEVEL environment variable.
func NewLoggerFromEnv() *slog.Logger {
	// Simple implementation without complex sampling.
	var level slog.Level
	
	logLevelEnv := strings.ToUpper(strings.TrimSpace(os.Getenv("LOG_LEVEL")))
	switch logLevelEnv {
	case "DEBUG":
		level = slog.LevelDebug
	case "INFO":
		level = slog.LevelInfo
	case "WARNING", "WARN":
		level = slog.LevelWarn
	case "ERROR":
		level = slog.LevelError
	default:
		level = slog.LevelWarn // Safe default.
	}

	opts := &slog.HandlerOptions{
		Level: level,
	}
	handler := slog.NewJSONHandler(os.Stdout, opts)
	return slog.New(handler)
}

// parseLogLevel converts LogLevel to slog.Level.
func parseLogLevel(level LogLevel) slog.Level {
	switch level {
	case LevelDebug:
		return slog.LevelDebug
	case LevelInfo:
		return slog.LevelInfo
	case LevelWarning:
		return slog.LevelWarn
	case LevelError:
		return slog.LevelError
	default:
		return slog.LevelWarn // Default to WARNING.
	}
}

// WithComponent adds a component field to the logger for better categorization.
func WithComponent(logger *slog.Logger, component string) *slog.Logger {
	return logger.With("component", component)
}

// WithRoute adds a route field to the logger for stream-specific logging.
func WithRoute(logger *slog.Logger, route string) *slog.Logger {
	return logger.With("route", route)
}

// LogPlaybackEvent logs playback-related events with consistent fields.
func LogPlaybackEvent(logger *slog.Logger, level slog.Level, msg string, route string, trackPath string, attrs ...slog.Attr) {
	allAttrs := []slog.Attr{
		slog.String("route", route),
		slog.String("track", trackPath),
		slog.String("event_type", "playback"),
	}
	allAttrs = append(allAttrs, attrs...)
	
	logger.LogAttrs(context.Background(), level, msg, allAttrs...)
}

// LogNetworkEvent logs network-related events with consistent fields.
func LogNetworkEvent(logger *slog.Logger, level slog.Level, msg string, clientID int, remoteAddr string, attrs ...slog.Attr) {
	allAttrs := []slog.Attr{
		slog.Int("client_id", clientID),
		slog.String("remote_addr", remoteAddr),
		slog.String("event_type", "network"),
	}
	allAttrs = append(allAttrs, attrs...)
	
	logger.LogAttrs(context.Background(), level, msg, allAttrs...)
}

// LogConfigEvent logs configuration-related events.
func LogConfigEvent(logger *slog.Logger, level slog.Level, msg string, attrs ...slog.Attr) {
	allAttrs := []slog.Attr{
		slog.String("event_type", "config"),
	}
	allAttrs = append(allAttrs, attrs...)
	
	logger.LogAttrs(context.Background(), level, msg, allAttrs...)
} 