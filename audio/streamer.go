package audio

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sentryhelper "github.com/aposazhennikov/stream-audio-to-web/sentry_helper"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	defaultBufferSize    = 16384 // 16KB: около секунды звука при 128 kbps.
	gracePeriodMs        = 50    // Пауза между треками.
	id3v2HeaderSize      = 10    // Size of ID3v2 header in bytes.
	id3v1TagSize         = 128   // Size of ID3v1 tag in bytes.
	tagCheckSize         = 3     // Size for checking 'TAG' string.
	bitsPerByte          = 8     // Количество бит в байте.
	id3v2SyncSafeShift21 = 21    // Bit shift for sync-safe integer (ID3v2).
	id3v2SyncSafeShift14 = 14
	id3v2SyncSafeShift7  = 7
	id3v2SyncSafeMask    = 0x7F // Mask for sync-safe integer (ID3v2).

	// Канал клиента вмещает около 30 секунд звука. Клиент, который не принял очередной кусок
	// при полном канале, считается зависшим и отключается: ждать его нельзя, это задержит всех.
	defaultClientChannelBuffer = 32

	// burstDuration — сколько последнего звука новый клиент получает сразу при подключении.
	// Это запас в буфере плеера, чтобы короткие сетевые задержки не прерывали звук.
	burstDuration = 4 * time.Second

	// maxPacingLag — насколько отправка может отстать от часов. Небольшое отставание (пауза между
	// треками, медленный диск) нагоняется. Если отстали сильнее (поток долго молчал), часы
	// сбрасываются, чтобы не высыпать клиентам минуту звука разом.
	maxPacingLag = 3 * time.Second

	clientStatusLogInterval    = 10 * time.Minute
	maxReasonableTrackDuration = 12 * time.Hour
)

// Streamer manages audio streaming for a single "radio" stream.
//
// One StreamTrack call at a time reads a file and broadcasts it to all clients at the
// real-time rate of the audio. The rate is kept by a stream-wide clock: chunks are sent
// so that the amount of sent audio stays about one chunk ahead of wall time, across tracks.
type Streamer struct {
	bufferSize      int
	maxClients      int
	bitrate         int // Битрейт потока в kbps; используется, если длительность файла неизвестна.
	normalizeVolume bool

	quit           chan struct{}
	streamDone     chan struct{} // Закрывается, когда текущий вызов StreamTrack завершился
	quitMutex      sync.Mutex    // Защищает quit и streamDone
	stopMutex      sync.Mutex    // Не даёт двум StopCurrentTrack выполняться одновременно
	currentTrackCh chan string

	clientMutex    sync.RWMutex
	clientChannels map[int]chan []byte
	clientCount    int32 // Количество клиентов (дублирует len(clientChannels) для чтения без блокировки)
	nextClientID   int   // ID только растут: повторно выданный ID перезаписал бы канал живого клиента

	burstMutex sync.Mutex
	burst      [][]byte // Последние куски звука общей длительностью около burstDuration
	burstBytes int

	// Часы темпа. Используются только горутиной, которая сейчас выполняет StreamTrack.
	paceStart time.Time
	paceDue   time.Duration // Сколько звука отправлено с paceStart

	isStreaming      int32 // Атомарный флаг: идёт ли сейчас StreamTrack
	lastActivityNano int64 // Время (UnixNano) последнего разосланного куска аудио

	trackMutex       sync.RWMutex // Защищает trackStartTime, currentTrackPath, trackDuration
	trackStartTime   time.Time
	currentTrackPath string
	trackDuration    time.Duration

	lastClientCount int
	lastLogTime     time.Time

	trackSecondsMetric *prometheus.CounterVec
	metricMutex        sync.RWMutex

	logger       *slog.Logger
	sentryHelper *sentryhelper.SentryHelper
}

// NewStreamer creates a new audio streamer. bitrate is in kbps.
func NewStreamer(bufferSize, maxClients int, bitrate int, logger *slog.Logger, sentryHelper *sentryhelper.SentryHelper) *Streamer {
	if bufferSize <= 0 {
		bufferSize = defaultBufferSize
	}
	if logger == nil {
		logger = slog.Default()
	}

	return &Streamer{
		bufferSize:      bufferSize,
		maxClients:      maxClients,
		bitrate:         bitrate,
		normalizeVolume: true,
		quit:            make(chan struct{}),
		currentTrackCh:  make(chan string, 1),
		clientChannels:  make(map[int]chan []byte),
		logger:          logger,
		sentryHelper:    sentryHelper,
	}
}

// SetVolumeNormalization enables or disables volume normalization.
// Runtime normalization is not applied by the streamer (audio is streamed as is);
// the flag is kept for configuration compatibility.
func (s *Streamer) SetVolumeNormalization(enabled bool) {
	s.normalizeVolume = enabled
	s.logger.Debug("Volume normalization", "enabled", enabled)
}

// Close closes the streamer and all client connections.
func (s *Streamer) Close() {
	s.quitMutex.Lock()
	select {
	case <-s.quit:
	default:
		close(s.quit)
	}
	s.quitMutex.Unlock()

	s.clearBurst()

	s.clientMutex.Lock()
	defer s.clientMutex.Unlock()
	for _, ch := range s.clientChannels {
		close(ch)
	}
	s.clientChannels = make(map[int]chan []byte)
	atomic.StoreInt32(&s.clientCount, 0)

	s.logger.Debug("Streamer completely closed")
}

// StopCurrentTrack immediately stops the playback of the current track and waits for it to end.
// Used when manually switching tracks.
func (s *Streamer) StopCurrentTrack() {
	// Остановку может одновременно запросить HTTP-обработчик и цикл станции.
	s.stopMutex.Lock()
	defer s.stopMutex.Unlock()

	startTime := time.Now()

	s.quitMutex.Lock()
	select {
	case <-s.quit:
	default:
		close(s.quit)
	}
	streamDone := s.streamDone
	s.quitMutex.Unlock()

	// Новые клиенты не должны получить хвост прерванного трека.
	s.clearBurst()

	// Ждём завершения последнего StreamTrack. Если он уже завершён, канал закрыт и ожидания нет.
	if streamDone != nil {
		select {
		case <-streamDone:
		case <-time.After(1 * time.Second):
			// Зависший StreamTrack держит старый, уже закрытый quit и завершится, как только продолжит работу.
			s.logger.Warn("Track stop timeout after 1 second",
				"isStreaming", atomic.LoadInt32(&s.isStreaming))
			s.sentryHelper.CaptureError(fmt.Errorf("CRITICAL: 1-second track stop timeout"), "audio", "streaming")
		}
	}

	s.quitMutex.Lock()
	s.quit = make(chan struct{})
	s.quitMutex.Unlock()

	s.logger.Debug("Track stop completed", "totalStopTimeMs", time.Since(startTime).Milliseconds())
}

// GetCurrentTrackChannel returns a channel with information about the current track.
func (s *Streamer) GetCurrentTrackChannel() <-chan string {
	return s.currentTrackCh
}

// StreamTrack streams a track to all connected clients at the real-time rate of the audio.
// Returns nil when the track was played to the end or interrupted by StopCurrentTrack.
func (s *Streamer) StreamTrack(trackPath string) error {
	// quit фиксируется один раз на весь трек: StopCurrentTrack закрывает именно его,
	// и трек не подхватит новый канал, созданный для следующего трека.
	quit, streamDone := s.beginStreaming()

	atomic.StoreInt32(&s.isStreaming, 1)
	defer func() {
		atomic.StoreInt32(&s.isStreaming, 0)
		close(streamDone)
	}()

	if trackPath == "" {
		err := errors.New("empty audio file path")
		s.sentryHelper.CaptureError(err, "audio", "stream_track")
		return err
	}

	fileInfo, statErr := os.Stat(trackPath)
	if statErr != nil {
		s.logger.Warn("Cannot stat audio file", "trackPath", trackPath, "error", statErr.Error())
		err := errors.New("error checking file")
		s.sentryHelper.CaptureError(err, "audio", "file_check")
		return err
	}
	if fileInfo.IsDir() {
		err := fmt.Errorf("specified path %s is a directory, not a file", trackPath)
		s.sentryHelper.CaptureError(err, "audio", "file_validation")
		return err
	}

	file, openErr := os.Open(trackPath)
	if openErr != nil {
		s.sentryHelper.CaptureError(errors.New("error opening file"), "audio", "file_open")
		return openErr
	}
	defer file.Close()

	duration := s.calculateMP3Duration(trackPath)
	s.trackMutex.Lock()
	s.trackStartTime = time.Now()
	s.currentTrackPath = trackPath
	s.trackDuration = duration
	s.trackMutex.Unlock()

	// Send information about current track to channel; keep only the newest value.
	select {
	case <-s.currentTrackCh:
	default:
	}
	select {
	case s.currentTrackCh <- filepath.Base(trackPath):
	default:
	}

	startTime := time.Now()
	audioReader, audioBytes, err := s.openAudioData(file, fileInfo)
	if err != nil {
		return err
	}

	bytesPerSecond := s.bytesPerSecond(audioBytes, duration, trackPath)
	bytesSent, err := s.streamAudioLoop(audioReader, trackPath, bytesPerSecond, quit)
	if err != nil {
		return err
	}

	s.logPlaybackCompletion(trackPath, bytesSent, time.Since(startTime))

	// Небольшая пауза между треками; часы темпа её компенсируют.
	select {
	case <-time.After(gracePeriodMs * time.Millisecond):
	case <-quit:
	}
	return nil
}

// beginStreaming registers a new StreamTrack call and returns its quit channel
// and the channel to close when the call finishes.
func (s *Streamer) beginStreaming() (chan struct{}, chan struct{}) {
	s.quitMutex.Lock()
	defer s.quitMutex.Unlock()

	streamDone := make(chan struct{})
	s.streamDone = streamDone
	return s.quit, streamDone
}

// openAudioData skips ID3v2 at the start and ID3v1 at the end of the file and returns
// a reader over the audio frames only, with their size.
func (s *Streamer) openAudioData(file *os.File, fileInfo os.FileInfo) (io.Reader, int64, error) {
	start := int64(0)
	header := make([]byte, id3v2HeaderSize)
	if _, headerErr := io.ReadFull(file, header); headerErr == nil && string(header[0:3]) == "ID3" {
		tagSize := int64(header[6]&id3v2SyncSafeMask)<<id3v2SyncSafeShift21 |
			int64(header[7]&id3v2SyncSafeMask)<<id3v2SyncSafeShift14 |
			int64(header[8]&id3v2SyncSafeMask)<<id3v2SyncSafeShift7 |
			int64(header[9]&id3v2SyncSafeMask)
		start = id3v2HeaderSize + tagSize
	}

	end := fileInfo.Size()
	hasID3v1, err := s.checkID3v1Tag(file, end)
	if err != nil {
		return nil, 0, err
	}
	if hasID3v1 {
		end -= id3v1TagSize
	}
	if start > end {
		start = 0
	}

	if _, seekErr := file.Seek(start, io.SeekStart); seekErr != nil {
		return nil, 0, fmt.Errorf("error seeking to audio data: %w", seekErr)
	}
	return io.LimitReader(file, end-start), end - start, nil
}

// bytesPerSecond returns the real-time byte rate of the track: from its duration when known,
// otherwise from the configured bitrate.
func (s *Streamer) bytesPerSecond(audioBytes int64, duration time.Duration, trackPath string) float64 {
	configured := float64(s.bitrate*1000) / bitsPerByte
	if configured <= 0 {
		configured = 128000 / bitsPerByte
	}
	if duration <= 0 || audioBytes <= 0 {
		return configured
	}

	fromFile := float64(audioBytes) / duration.Seconds()
	// Защита от явно неверной длительности (битый заголовок): такой темп сломал бы поток.
	if fromFile < configured/4 || fromFile > configured*4 {
		s.logger.Warn("Track byte rate looks wrong, using configured bitrate",
			"trackPath", trackPath,
			"fileBytesPerSecond", int(fromFile),
			"configuredBytesPerSecond", int(configured))
		return configured
	}
	return fromFile
}

// streamAudioLoop reads the audio and broadcasts it chunk by chunk at bytesPerSecond.
func (s *Streamer) streamAudioLoop(reader io.Reader, trackPath string, bytesPerSecond float64, quit chan struct{}) (int64, error) {
	var bytesSent int64

	for {
		select {
		case <-quit:
			s.logger.Debug("Quit signal received in streamAudioLoop",
				"trackPath", trackPath,
				"bytesSent", bytesSent)
			return bytesSent, nil
		default:
		}

		// Каждый кусок — отдельный слайс: клиенты держат его в своих каналах,
		// переиспользовать буфер для следующего чтения нельзя.
		chunk := make([]byte, s.bufferSize)
		n, readErr := io.ReadFull(reader, chunk)
		if n > 0 {
			chunk = chunk[:n]
			s.publishChunk(chunk)
			bytesSent += int64(n)
			s.paceDue += time.Duration(float64(n) / bytesPerSecond * float64(time.Second))
		}

		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			return bytesSent, nil
		}
		if readErr != nil {
			s.sentryHelper.CaptureError(readErr, "audio", "streaming")
			return bytesSent, fmt.Errorf("error reading data: %w", readErr)
		}

		if !s.waitForPace(quit) {
			return bytesSent, nil
		}
	}
}

// waitForPace sleeps until the stream clock catches up with the sent audio.
// Returns false if playback was interrupted while waiting.
func (s *Streamer) waitForPace(quit chan struct{}) bool {
	wait := time.Until(s.paceStart.Add(s.paceDue))
	if wait <= 0 {
		return true
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-quit:
		return false
	}
}

// publishChunk sends a chunk to all clients, stores it for new clients and moves the stream clock.
func (s *Streamer) publishChunk(chunk []byte) {
	now := time.Now()
	// Часы темпа: первый кусок после долгой тишины начинает отсчёт заново.
	if s.paceStart.IsZero() || now.Sub(s.paceStart.Add(s.paceDue)) > maxPacingLag {
		s.paceStart = now
		s.paceDue = 0
	}

	s.appendBurst(chunk)
	s.broadcastToClients(chunk)
	atomic.StoreInt64(&s.lastActivityNano, now.UnixNano())
}

// appendBurst stores the chunk in the burst buffer, keeping about burstDuration of audio.
func (s *Streamer) appendBurst(chunk []byte) {
	maxBytes := int(burstDuration.Seconds() * float64(s.bitrate*1000) / bitsPerByte)
	if maxBytes < len(chunk) {
		maxBytes = len(chunk)
	}

	s.burstMutex.Lock()
	defer s.burstMutex.Unlock()
	s.burst = append(s.burst, chunk)
	s.burstBytes += len(chunk)
	for len(s.burst) > 1 && s.burstBytes-len(s.burst[0]) >= maxBytes {
		s.burstBytes -= len(s.burst[0])
		s.burst[0] = nil
		s.burst = s.burst[1:]
	}
}

// clearBurst drops the stored audio so new clients don't get the tail of an interrupted track.
func (s *Streamer) clearBurst() {
	s.burstMutex.Lock()
	s.burst = nil
	s.burstBytes = 0
	s.burstMutex.Unlock()
}

// broadcastToClients sends data to all connected clients without waiting for any of them.
func (s *Streamer) broadcastToClients(data []byte) {
	var stuckClients []int

	s.clientMutex.RLock()
	clientCount := len(s.clientChannels)
	for clientID, ch := range s.clientChannels {
		select {
		case ch <- data:
		default:
			stuckClients = append(stuckClients, clientID)
		}
	}
	s.clientMutex.RUnlock()

	// Удаляем только после снятия RLock: RemoveClient берёт Lock, взять его под RLock — взаимная блокировка.
	for _, clientID := range stuckClients {
		s.logger.Warn("Client does not read the stream, disconnecting",
			"clientID", clientID,
			"bufferedChunks", defaultClientChannelBuffer,
			"route", getRouteFromTrackPath(s.currentTrack()))
		s.RemoveClient(clientID)
	}

	s.logClientStatusIfNeeded(clientCount, len(data))
}

// currentTrack returns the path of the track being played.
func (s *Streamer) currentTrack() string {
	s.trackMutex.RLock()
	defer s.trackMutex.RUnlock()
	return s.currentTrackPath
}

// logClientStatusIfNeeded logs client status when the client count changes and periodically.
func (s *Streamer) logClientStatusIfNeeded(clientCount, dataSize int) {
	if clientCount == s.lastClientCount && time.Since(s.lastLogTime) < clientStatusLogInterval {
		return
	}
	s.logger.Debug("Client streaming activity",
		"clients", clientCount,
		"previousClients", s.lastClientCount,
		"bytes", dataSize,
		"route", getRouteFromTrackPath(s.currentTrack()))
	s.lastClientCount = clientCount
	s.lastLogTime = time.Now()
}

// getRouteFromTrackPath tries to extract route name from file path.
func getRouteFromTrackPath(trackPath string) string {
	route := filepath.Base(filepath.Dir(trackPath))
	if !strings.HasPrefix(route, "/") {
		route = "/" + route
	}
	return route
}

// AddClient adds a new client and returns a channel for receiving data.
// The client immediately gets the last few seconds of audio so its player starts with a buffer.
func (s *Streamer) AddClient() (<-chan []byte, int, error) {
	s.clientMutex.Lock()
	defer s.clientMutex.Unlock()

	if s.maxClients > 0 && len(s.clientChannels) >= s.maxClients {
		err := fmt.Errorf("maximum number of clients exceeded (%d)", s.maxClients)
		s.sentryHelper.CaptureError(err, "audio", "streaming")
		return nil, 0, err
	}

	s.nextClientID++
	clientID := s.nextClientID
	clientChannel := make(chan []byte, defaultClientChannelBuffer)
	s.clientChannels[clientID] = clientChannel
	atomic.StoreInt32(&s.clientCount, int32(len(s.clientChannels)))

	s.burstMutex.Lock()
	for _, chunk := range s.burst {
		select {
		case clientChannel <- chunk:
		default:
		}
	}
	s.burstMutex.Unlock()

	s.logger.Debug("Client connected",
		"clientID", clientID,
		"totalClients", len(s.clientChannels))

	return clientChannel, clientID, nil
}

// RemoveClient removes a client and closes its channel. Removing an unknown client is a no-op.
func (s *Streamer) RemoveClient(clientID int) {
	s.clientMutex.Lock()
	defer s.clientMutex.Unlock()

	if channel, exists := s.clientChannels[clientID]; exists {
		close(channel)
		delete(s.clientChannels, clientID)
		atomic.StoreInt32(&s.clientCount, int32(len(s.clientChannels)))
		s.logger.Debug("Client disconnected",
			"clientID", clientID,
			"totalClients", len(s.clientChannels))
	}
}

// CloseClient closes a client connection (same as RemoveClient).
func (s *Streamer) CloseClient(clientID int) {
	s.RemoveClient(clientID)
}

// GetClientCount returns the current number of clients.
func (s *Streamer) GetClientCount() int {
	return int(atomic.LoadInt32(&s.clientCount))
}

// GetLastActivity returns the time when the last audio chunk was streamed (zero if nothing was streamed yet).
// Used by monitoring to detect a station that is "playing" a track but sends no data.
func (s *Streamer) GetLastActivity() time.Time {
	nano := atomic.LoadInt64(&s.lastActivityNano)
	if nano == 0 {
		return time.Time{}
	}
	return time.Unix(0, nano)
}

// GetPlaybackInfo returns current playback information (track path, start time, elapsed time, total duration).
func (s *Streamer) GetPlaybackInfo() (string, time.Time, time.Duration, time.Duration) {
	s.trackMutex.RLock()
	defer s.trackMutex.RUnlock()

	elapsed := time.Duration(0)
	if !s.trackStartTime.IsZero() {
		elapsed = time.Since(s.trackStartTime)
	}

	return s.currentTrackPath, s.trackStartTime, elapsed, s.trackDuration
}

// calculateMP3Duration calculates the duration of an MP3 file using ffprobe (most accurate).
func (s *Streamer) calculateMP3Duration(trackPath string) time.Duration {
	if duration := s.calculateDurationWithFFProbe(trackPath); duration > 0 {
		s.logger.Debug("MP3 duration calculated with ffprobe",
			"trackPath", trackPath,
			"duration", duration.String())
		return duration
	}
	return s.calculateDurationBySimpleEstimation(trackPath)
}

// calculateDurationWithFFProbe uses ffprobe to get exact duration.
func (s *Streamer) calculateDurationWithFFProbe(trackPath string) time.Duration {
	cmd := exec.Command("ffprobe", "-v", "quiet", "-show_entries", "format=duration", "-of", "csv=p=0", trackPath)
	output, err := cmd.Output()
	if err != nil {
		s.logger.Debug("ffprobe not available or failed",
			"trackPath", trackPath,
			"error", err.Error())
		return 0
	}

	durationStr := strings.TrimSpace(string(output))
	durationFloat, err := strconv.ParseFloat(durationStr, 64)
	if err != nil {
		s.logger.Error("Failed to parse ffprobe duration",
			"trackPath", trackPath,
			"output", durationStr,
			"error", err.Error())
		return 0
	}

	duration := time.Duration(durationFloat * float64(time.Second))
	if duration < time.Second || duration > maxReasonableTrackDuration {
		s.logger.Error("ffprobe returned unreasonable duration",
			"trackPath", trackPath,
			"duration", duration.String())
		return 0
	}
	return duration
}

// calculateDurationBySimpleEstimation estimates duration from file size and the configured bitrate.
func (s *Streamer) calculateDurationBySimpleEstimation(trackPath string) time.Duration {
	fileInfo, err := os.Stat(trackPath)
	if err != nil {
		return 0
	}

	bitrate := s.bitrate
	if bitrate <= 0 {
		bitrate = 128
	}
	bytesPerSecond := float64(bitrate*1000) / bitsPerByte
	estimated := time.Duration(float64(fileInfo.Size()) / bytesPerSecond * float64(time.Second))

	// Нижнюю границу не ставим: завышенная длительность короткого трека замедлила бы поток.
	if estimated > maxReasonableTrackDuration {
		estimated = maxReasonableTrackDuration
	}

	s.logger.Debug("MP3 duration calculated by simple estimation",
		"trackPath", trackPath,
		"duration", estimated.String(),
		"fileSize", fileInfo.Size())
	return estimated
}

// SetTrackSecondsMetric sets the metric for the streamer.
func (s *Streamer) SetTrackSecondsMetric(metric *prometheus.CounterVec) {
	s.metricMutex.Lock()
	defer s.metricMutex.Unlock()
	s.trackSecondsMetric = metric
}

// GetTrackSecondsMetric returns the metric if it's set for the streamer.
func (s *Streamer) GetTrackSecondsMetric() (*prometheus.CounterVec, bool) {
	s.metricMutex.RLock()
	defer s.metricMutex.RUnlock()
	if s.trackSecondsMetric == nil {
		return nil, false
	}
	return s.trackSecondsMetric, true
}

// checkID3v1Tag checks for ID3v1 tag at the end of a file. The file position is restored.
func (s *Streamer) checkID3v1Tag(file *os.File, fileSize int64) (bool, error) {
	if fileSize <= id3v1TagSize {
		return false, nil
	}

	currentPos, posErr := file.Seek(0, io.SeekCurrent)
	if posErr != nil {
		return false, posErr
	}
	defer func() {
		if _, seekErr := file.Seek(currentPos, io.SeekStart); seekErr != nil {
			s.logger.Warn("Error seeking back to position", "error", seekErr.Error())
		}
	}()

	if _, seekErr := file.Seek(fileSize-id3v1TagSize, io.SeekStart); seekErr != nil {
		return false, seekErr
	}
	tagCheck := make([]byte, tagCheckSize)
	if _, readErr := io.ReadFull(file, tagCheck); readErr != nil {
		return false, readErr
	}
	return string(tagCheck) == "TAG", nil
}

// logPlaybackCompletion logs completion of playback and updates metrics.
func (s *Streamer) logPlaybackCompletion(trackPath string, bytesSent int64, duration time.Duration) {
	s.logger.Debug("Playback completed",
		"trackPath", trackPath,
		"bytesSent", bytesSent,
		"duration", duration.Seconds())

	if trackSecondsTotal, ok := s.GetTrackSecondsMetric(); ok {
		trackSecondsTotal.WithLabelValues(getRouteFromTrackPath(trackPath)).Add(duration.Seconds())
	}
}
