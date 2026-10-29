package http

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// isConnectionClosedError checks if error is result of client closing connection.
func isConnectionClosedError(err error) bool {
	if err == nil {
		return false
	}

	errMsg := err.Error()
	return strings.Contains(errMsg, "broken pipe") ||
		strings.Contains(errMsg, "connection reset by peer") ||
		strings.Contains(errMsg, "use of closed network connection") ||
		strings.Contains(errMsg, "i/o timeout") // сработал дедлайн записи: клиент перестал принимать данные
}

// StreamAudioHandler creates HTTP handler for audio streaming.
func (s *Server) StreamAudioHandler(route string) http.HandlerFunc {
	s.logger.Info("Creating audio stream handler", slog.String("route", route))

	contentType := s.determineContentType()
	s.logger.Info(
		"Audio stream format",
		slog.String("route", route),
		slog.String("MIME", contentType),
	)

	return func(w http.ResponseWriter, r *http.Request) {
		s.logger.Debug(
			"Received request for audio stream",
			slog.String("method", r.Method),
			slog.String("route", route),
			slog.String("remoteAddr", r.RemoteAddr),
		)

		// Verify stream exists.
		stream, exists := s.getStream(route, w)
		if !exists {
			return
		}

		// Set up appropriate headers for streaming.
		s.setupStreamingHeaders(w, contentType)

		// Players that want the track title send "Icy-MetaData: 1"; others get plain MP3.
		wantICYMetadata := r.Header.Get("Icy-MetaData") == "1"
		if wantICYMetadata {
			w.Header().Set("icy-metaint", strconv.Itoa(icyMetaInt))
			w.Header().Set("icy-name", strings.TrimPrefix(route, "/"))
			w.Header().Set("icy-pub", "0")
		}

		// For HEAD requests, check if stream is ready before returning OK.
		if r.Method == http.MethodHead {
			// Check if stream has a current track (files are ready for playback).
			s.trackMutex.RLock()
			currentTrack := s.currentTracks[route]
			s.trackMutex.RUnlock()

			if currentTrack == "" {
				s.logger.Debug("HEAD request - stream not ready, no current track",
					slog.String("route", route),
					slog.String("remoteAddr", r.RemoteAddr))
				http.Error(w, "Stream not ready - files are being processed", http.StatusServiceUnavailable)
				return
			}

			s.logger.Debug("HEAD request - stream ready",
				slog.String("route", route),
				slog.String("remoteAddr", r.RemoteAddr),
				slog.String("currentTrack", currentTrack))
			w.WriteHeader(http.StatusOK)
			return
		}

		// Verify flushing capability.
		flusher, ok := w.(http.Flusher)
		if !ok {
			errorMsg := "Streaming not supported"
			s.logger.Error("ERROR: Streaming not supported")
			s.sentryHelper.CaptureInfo(errorMsg, "http", "handler") // Save as this is an error
			http.Error(w, errorMsg, http.StatusInternalServerError)
			return
		}

		// Flush headers immediately - critical for streaming.
		flusher.Flush()

		// Connect client to stream.
		clientData, err := s.connectClientToStream(stream, route, r.RemoteAddr)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer stream.RemoveClient(clientData.clientID)

		// Start streaming data to client.
		var out io.Writer = w
		if wantICYMetadata {
			out = newICYMetadataWriter(w, func() string {
				trackPath, _, _, _ := stream.GetPlaybackInfo()
				return trackTitleFromPath(trackPath)
			})
		}
		s.streamDataToClient(w, out, flusher, clientData, route, r.Context().Done())
	}
}

// determineContentType sets the appropriate MIME type for audio streaming.
func (s *Server) determineContentType() string {
	// Default to MP3 since that's what we stream
	return "audio/mpeg"
}

// getStream retrieves the stream handler for the given route.
func (s *Server) getStream(route string, w http.ResponseWriter) (StreamHandler, bool) {
	s.mutex.RLock()
	stream, exists := s.streams[route]
	s.mutex.RUnlock()

	if !exists {
		errorMsg := fmt.Sprintf("Stream %s not found", route)
		s.logger.Error("ERROR: Stream not found", slog.String("route", route))
		s.sentryHelper.CaptureInfo(errorMsg, "http", "handler") // Save as this is an error
		http.Error(w, errorMsg, http.StatusNotFound)
		return nil, false
	}

	s.logger.Debug("Stream found, setting up headers", slog.String("route", route))
	return stream, true
}

// setupStreamingHeaders sets appropriate HTTP headers for audio streaming.
func (s *Server) setupStreamingHeaders(w http.ResponseWriter, contentType string) {
	// 1. Content-Type must match audio stream format.
	w.Header().Set("Content-Type", contentType)
	// 2. Disable caching for live stream.
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	// 3. Protect against content type guessing.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// DO NOT set Transfer-Encoding: chunked, Go will do this itself.
	// DO NOT set Connection: keep-alive, this is default behavior for HTTP/1.1.
}

// clientStreamData contains information about a connected client.
type clientStreamData struct {
	clientCh   <-chan []byte
	clientID   int
	remoteAddr string
}

// connectClientToStream adds a client to the stream.
func (s *Server) connectClientToStream(stream StreamHandler, route, remoteAddr string) (*clientStreamData, error) {
	s.logger.Info("Adding client to stream", slog.String("route", route))

	// Get data channel and client ID.
	clientCh, clientID, addErr := stream.AddClient()
	if addErr != nil {
		s.logger.Error(
			"ERROR: Error adding client to stream",
			slog.String("route", route),
			slog.String("error", addErr.Error()),
		)
		s.sentryHelper.CaptureError(addErr, "http", "handler") // Save as this is an error
		return nil, addErr
	}

	// Update metrics ONLY AFTER successful connection and headers sent.
	s.listenerCount.WithLabelValues(route).Inc()
	defer s.listenerCount.WithLabelValues(route).Dec()

	// Log client connection.
	s.logger.Info(
		"Client connected to stream",
		slog.String("route", route),
		slog.String("remoteAddr", remoteAddr),
		slog.Int("clientID", clientID),
	)

	return &clientStreamData{
		clientCh:   clientCh,
		clientID:   clientID,
		remoteAddr: remoteAddr,
	}, nil
}

// streamDataToClient manages the streaming of audio data to a connected client.
// out is where audio is written (w itself or a writer that adds ICY metadata).
func (s *Server) streamDataToClient(
	w http.ResponseWriter,
	out io.Writer,
	flusher http.Flusher,
	clientData *clientStreamData,
	route string,
	clientClosed <-chan struct{},
) {
	s.logger.Info("Starting data transmission", slog.Int("clientID", clientData.clientID), slog.String("route", route))

	// Counter for tracking sent data.
	var totalBytesSent int64
	var lastLogTime = time.Now()

	// Сервер запущен без WriteTimeout (поток бесконечный), поэтому дедлайн ставится на каждую
	// запись: иначе пропавший без закрытия соединения клиент висит на Write до таймаута TCP.
	responseController := http.NewResponseController(w)

	// Send data to client.
	for {
		select {
		case <-clientClosed:
			// Client disconnected.
			s.logger.Debug("Client disconnected from stream",
				slog.String("route", route),
				slog.String("remoteAddr", clientData.remoteAddr),
				slog.Int("clientID", clientData.clientID),
				slog.Int64("totalBytesSent", totalBytesSent))
			return
		case data, ok := <-clientData.clientCh:
			if !ok {
				// Channel closed.
				s.logger.Info("Channel closed for client",
					slog.String("remoteAddr", clientData.remoteAddr),
					slog.Int("clientID", clientData.clientID),
					slog.Int64("totalBytesSent", totalBytesSent))
				return
			}

			// Send data and handle errors.
			if deadlineErr := responseController.SetWriteDeadline(time.Now().Add(clientWriteTimeout)); deadlineErr != nil &&
				!errors.Is(deadlineErr, http.ErrNotSupported) {
				s.logger.Debug("Failed to set write deadline", slog.String("error", deadlineErr.Error()))
			}
			if err := s.sendDataToClient(out, data, clientData, route, &totalBytesSent); err != nil {
				return
			}

			// Update metrics and log periodically.
			s.bytesSent.WithLabelValues(route).Add(float64(len(data)))
			s.logDataTransfer(clientData, &totalBytesSent, &lastLogTime)

			// MUST call Flush after EACH data sent!.
			// This ensures data is sent immediately to client.
			flusher.Flush()
		}
	}
}

// sendDataToClient writes data to the client and handles potential errors.
func (s *Server) sendDataToClient(
	w io.Writer,
	data []byte,
	clientData *clientStreamData,
	_ string,
	totalBytesSent *int64,
) error {
	n, writeErr := w.Write(data)
	if writeErr != nil {
		if isConnectionClosedError(writeErr) {
			// Just log, don't send to Sentry.
			s.logger.Debug("Client disconnected",
				slog.Int("clientID", clientData.clientID),
				slog.String("error", writeErr.Error()),
				slog.Int64("totalBytesSent", *totalBytesSent))
		} else {
			// Send to Sentry only for unusual errors.
			s.logger.Error("ERROR: Error sending data to client",
				slog.Int("clientID", clientData.clientID),
				slog.String("error", writeErr.Error()),
				slog.Int64("totalBytesSent", *totalBytesSent))
			s.sentryHelper.CaptureError(fmt.Errorf("error sending data to client %d: %w", clientData.clientID, writeErr), "http", "send_data")
		}
		return writeErr
	}

	*totalBytesSent += int64(n)
	return nil
}

// logDataTransfer logs information about data transfer periodically.
func (s *Server) logDataTransfer(clientData *clientStreamData, totalBytesSent *int64, lastLogTime *time.Time) {
	if *totalBytesSent >= logEveryBytes && time.Since(*lastLogTime) > logIntervalSec*time.Second {
		s.logger.Debug("Sent data to client",
			slog.Int("mbytes", int(*totalBytesSent/mb)),
			slog.Int("clientID", clientData.clientID),
			slog.String("remoteAddr", clientData.remoteAddr))
		*lastLogTime = time.Now()
	}
}
