package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aposazhennikov/stream-audio-to-web/announce"
)

// clientClosedReason is how streamDataToClient reports that the client closed the connection.
const clientClosedReason = "client closed the connection"

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

		// The URL may be a remote control button: then another station is streamed.
		decision := s.remote.resolve(r, route, s.IsStreamRegistered)
		switch {
		case decision.direction != "":
			s.logger.Info("Remote control: button pressed again, switching station",
				slog.String("button", route),
				slog.String("direction", decision.direction),
				slog.String("station", decision.target),
				slog.String("remoteAddr", r.RemoteAddr),
				slog.String("userAgent", r.UserAgent()))
			s.recordTrackBeforeSwitch(decision.target)
			if switchErr := s.SwitchTrack(decision.target, decision.direction); switchErr != nil {
				s.logger.Error("Remote control track switch failed",
					slog.String("station", decision.target),
					slog.String("error", switchErr.Error()))
			}
		case decision.arrived:
			s.logger.Info("Remote control: arrived on button, station keeps playing",
				slog.String("button", route),
				slog.String("station", decision.target),
				slog.String("remoteAddr", r.RemoteAddr),
				slog.String("userAgent", r.UserAgent()))
		case decision.duplicate && decision.target != route:
			s.logger.Info("Remote control: duplicate connection, not a press",
				slog.String("button", route),
				slog.String("station", decision.target),
				slog.String("remoteAddr", r.RemoteAddr))
		}
		// Sounds and speech before the station audio are made at the sample rate of the
		// track that follows them: the receiver goes silent when the rate changes.
		var intro []byte
		announcer := s.getAnnouncer()
		voiceClient := r.Method == http.MethodGet && announcer.Wants(r)
		wantsVoice := voiceClient && !decision.duplicate
		var next *nextRecordVoice
		if voiceClient && announcer.AnnouncesNext(decision.target) {
			next = &nextRecordVoice{announcer: announcer, station: decision.target, remoteAddr: r.RemoteAddr}
		}
		if decision.hasIntro() || wantsVoice || next != nil {
			track := s.followingTrack(decision)
			if next != nil {
				next.named = track.path // already named on connecting, or the receiver heard it before
			}
			if decision.hasIntro() || wantsVoice {
				intro = decision.intro(announce.ClipRate(announce.SampleRate(track.path)))
			}
			if wantsVoice {
				intro = append(append([]byte{}, intro...), s.announcement(r, announcer, decision, track)...)
			}
		}
		closedByClient := s.serveStream(w, r, route, decision.target, contentType, intro, next)
		s.remote.release(decision, closedByClient)
	}
}

// nextRecordVoice names the next record when the station moves on to it while the receiver
// keeps listening: the next-record sound and "Дальше: <title>.", at the new record's rate.
type nextRecordVoice struct {
	announcer  *announce.Announcer
	station    string
	remoteAddr string
	named      string // the record the listener already heard named
}

// nextRecordTimeout limits the wait for a clip that is not synthesized yet; the stream waits
// meanwhile, so it is short, and the announcement is skipped if the clip is late.
const nextRecordTimeout = 3 * time.Second

// before returns what to send before chunk: the announcement if chunk starts a new record.
func (v *nextRecordVoice) before(ctx context.Context, stream StreamHandler, chunk []byte, logger *slog.Logger) []byte {
	marks, ok := stream.(interface {
		TrackStartedBy(chunk []byte) (string, bool)
	})
	if !ok {
		return nil
	}
	path, started := marks.TrackStartedBy(chunk)
	if !started || path == v.named {
		return nil
	}
	v.named = path
	ctx, cancel := context.WithTimeout(ctx, nextRecordTimeout)
	defer cancel()
	voice := v.announcer.ForNext(ctx, v.station, path)
	if len(voice) == 0 {
		return nil
	}
	logger.Info("Announced the next record",
		slog.String("station", v.station),
		slog.String("track", filepath.Base(path)),
		slog.String("remoteAddr", v.remoteAddr))
	sound := remoteSound("next", announce.ClipRate(announce.SampleRate(path)))
	return append(append(make([]byte, 0, len(sound)+len(voice)), sound...), voice...)
}

// playingTrack is the track a station plays, where it is in it and how long it is.
type playingTrack struct {
	path           string
	elapsed, total time.Duration
}

// followingTrack returns the track the client will hear after the intro: the new one after a
// press, otherwise what the station plays now.
func (s *Server) followingTrack(decision remoteDecision) playingTrack {
	if decision.direction != "" {
		path, total := s.trackAfterSwitch(decision.target)
		return playingTrack{path: path, total: total}
	}
	s.mutex.RLock()
	stream, ok := s.streams[decision.target]
	s.mutex.RUnlock()
	if !ok {
		return playingTrack{}
	}
	path, _, elapsed, total := stream.GetPlaybackInfo()
	return playingTrack{path: path, elapsed: elapsed, total: total}
}

// announcement returns the spoken announcement for this connection: the button on arrival,
// the new record after a press, the station and its record on a new connection. Nothing for
// duplicate connections and reconnects.
func (s *Server) announcement(r *http.Request, announcer *announce.Announcer, decision remoteDecision, track playingTrack) []byte {
	ctx, cancel := context.WithTimeout(r.Context(), announceTimeout)
	defer cancel()
	switch {
	case decision.direction != "":
		return announcer.ForSwitch(ctx, track.path, track.total)
	case decision.arrived:
		return announcer.ForButton(ctx, decision.button == remoteDirectionNext, decision.target, track.path)
	case decision.duplicate:
		return nil
	}
	if track.path == "" {
		return nil
	}
	return announcer.ForStation(ctx, decision.target, track.path, track.elapsed, track.total)
}

// recordTrackBeforeSwitch remembers the track a station plays before a button switches it,
// so trackAfterSwitch can tell when the new one has started.
func (s *Server) recordTrackBeforeSwitch(route string) {
	s.mutex.RLock()
	stream, ok := s.streams[route]
	s.mutex.RUnlock()
	if !ok {
		return
	}
	trackPath, _, _, _ := stream.GetPlaybackInfo()
	s.trackMutex.Lock()
	s.switchedFrom[route] = trackPath
	s.trackMutex.Unlock()
}

// trackAfterSwitch waits briefly for the station to start the track a switch moved it to and
// returns that track and its length.
func (s *Server) trackAfterSwitch(route string) (string, time.Duration) {
	s.mutex.RLock()
	stream, ok := s.streams[route]
	s.mutex.RUnlock()
	if !ok {
		return "", 0
	}
	s.trackMutex.RLock()
	before := s.switchedFrom[route]
	s.trackMutex.RUnlock()
	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		trackPath, _, _, total := stream.GetPlaybackInfo()
		if (trackPath != before && trackPath != "") || time.Now().After(deadline) {
			return trackPath, total
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// serveStream streams the station to the client; requested is the URL the client asked for
// (a button URL streams another station). intro, if any, is sent before the station audio.
// next, if set, names each next record the station moves on to. Returns true if the client
// itself closed the connection.
func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, requested, route, contentType string, intro []byte, next *nextRecordVoice) bool {
	// Verify stream exists.
	stream, exists := s.getStream(route, w)
	if !exists {
		return false
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
			return false
		}

		s.logger.Debug("HEAD request - stream ready",
			slog.String("route", route),
			slog.String("remoteAddr", r.RemoteAddr),
			slog.String("currentTrack", currentTrack))
		w.WriteHeader(http.StatusOK)
		return false
	}

	// Verify flushing capability.
	flusher, ok := w.(http.Flusher)
	if !ok {
		errorMsg := "Streaming not supported"
		s.logger.Error("ERROR: Streaming not supported")
		s.sentryHelper.CaptureInfo(errorMsg, "http", "handler") // Save as this is an error
		http.Error(w, errorMsg, http.StatusInternalServerError)
		return false
	}

	// Flush headers immediately - critical for streaming.
	flusher.Flush()

	// Connect client to stream.
	clientData, err := s.connectClientToStream(stream, route, r.RemoteAddr, r.UserAgent())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return false
	}
	defer stream.RemoveClient(clientData.clientID)
	tracker := s.getListeners()
	sessionID := tracker.Start(r.RemoteAddr, r.UserAgent(), requested, route)
	defer tracker.End(sessionID)

	// Start streaming data to client.
	var out io.Writer = w
	if wantICYMetadata {
		out = newICYMetadataWriter(w, func() string {
			trackPath, _, _, _ := stream.GetPlaybackInfo()
			return trackTitleFromPath(trackPath)
		})
	}
	startedAt := time.Now()
	// The intro goes out before the station audio queued for this client.
	if len(intro) > 0 {
		if _, writeErr := out.Write(intro); writeErr != nil {
			return false
		}
		flusher.Flush()
	}
	var before func([]byte) []byte
	if next != nil {
		before = func(chunk []byte) []byte { return next.before(r.Context(), stream, chunk, s.logger) }
	}
	reason := s.streamDataToClient(w, out, flusher, clientData, route, r.Context().Done(), before)
	// Kept at info level: how long connections last and how they end tells a press from a
	// reconnect on the receivers that use the remote control buttons.
	s.logger.Info("Client connection ended",
		slog.String("route", requested),
		slog.String("station", route),
		slog.String("reason", reason),
		slog.Float64("seconds", time.Since(startedAt).Seconds()),
		slog.String("remoteAddr", r.RemoteAddr),
		slog.String("userAgent", r.UserAgent()))
	return reason == clientClosedReason
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
func (s *Server) connectClientToStream(stream StreamHandler, route, remoteAddr, userAgent string) (*clientStreamData, error) {
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
		slog.String("userAgent", userAgent),
		slog.Int("clientID", clientID),
	)

	return &clientStreamData{
		clientCh:   clientCh,
		clientID:   clientID,
		remoteAddr: remoteAddr,
	}, nil
}

// streamDataToClient manages the streaming of audio data to a connected client.
// out is where audio is written (w itself or a writer that adds ICY metadata). before, if
// set, returns audio to insert before a chunk (an announcement at the start of a record).
// Returns why the streaming ended.
func (s *Server) streamDataToClient(
	w http.ResponseWriter,
	out io.Writer,
	flusher http.Flusher,
	clientData *clientStreamData,
	route string,
	clientClosed <-chan struct{},
	before func([]byte) []byte,
) string {
	inserted := false
	s.logger.Info("Starting data transmission", slog.Int("clientID", clientData.clientID), slog.String("route", route))

	// Counter for tracking sent data.
	var totalBytesSent int64
	var lastLogTime = time.Now()

	// Сервер запущен без WriteTimeout (поток бесконечный), поэтому дедлайн ставится на каждую
	// запись: иначе пропавший без закрытия соединения клиент висит на Write до таймаута TCP.
	responseController := http.NewResponseController(w)

	// Send data to client.
	for {
		var data []byte
		select {
		case <-clientClosed:
			// Client disconnected.
			s.logger.Debug("Client disconnected from stream",
				slog.String("route", route),
				slog.String("remoteAddr", clientData.remoteAddr),
				slog.Int("clientID", clientData.clientID),
				slog.Int64("totalBytesSent", totalBytesSent))
			return clientClosedReason
		case chunk, ok := <-clientData.clientCh:
			if !ok {
				// Channel closed.
				s.logger.Info("Channel closed for client",
					slog.String("remoteAddr", clientData.remoteAddr),
					slog.Int("clientID", clientData.clientID),
					slog.Int64("totalBytesSent", totalBytesSent))
				return "dropped by the station (client too slow or station stopped)"
			}
			data = chunk
		}

		if before != nil {
			if extra := before(data); len(extra) > 0 {
				// The announcement goes out in front of the record; the listener then hears
				// the station that much later, and the queue of this client grows by it.
				data = append(append(make([]byte, 0, len(extra)+len(data)), extra...), data...)
				inserted = true
			}
		}
		// Announcements add up over hours of listening. Before the queue fills up and the
		// station drops the client, skip some of the queued audio.
		if inserted && cap(clientData.clientCh) > 0 && len(clientData.clientCh) > cap(clientData.clientCh)*3/4 {
			skipped := 0
			for len(clientData.clientCh) > cap(clientData.clientCh)/2 {
				if _, ok := <-clientData.clientCh; !ok {
					break
				}
				skipped++
			}
			s.logger.Info("Skipped queued audio after announcements",
				slog.Int("chunks", skipped), slog.String("route", route),
				slog.String("remoteAddr", clientData.remoteAddr))
		}

		// Send data and handle errors.
		if deadlineErr := responseController.SetWriteDeadline(time.Now().Add(clientWriteTimeout)); deadlineErr != nil &&
			!errors.Is(deadlineErr, http.ErrNotSupported) {
			s.logger.Debug("Failed to set write deadline", slog.String("error", deadlineErr.Error()))
		}
		if err := s.sendDataToClient(out, data, clientData, route, &totalBytesSent); err != nil {
			return "write failed: " + err.Error()
		}

		// Update metrics and log periodically.
		s.bytesSent.WithLabelValues(route).Add(float64(len(data)))
		s.logDataTransfer(clientData, &totalBytesSent, &lastLogTime)

		// MUST call Flush after EACH data sent!.
		// This ensures data is sent immediately to client.
		flusher.Flush()
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
