package http

import (
	_ "embed"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Remote control for radio receivers that can only play a stream URL.
//
// Two station URLs (for example /shaov and /troshin, stored in the receiver presets) act as
// "next" and "previous" buttons for the station the listener was listening to: the last
// station of ours it stayed on for at least remoteMinListenDefault. Stations passed on the
// way (a receiver without number keys is scrolled preset by preset) and the button stations
// themselves do not count.
//
// Arriving on a button switches nothing: the button introduces itself with a short sound and
// the listened station keeps playing. Pressing the same preset again is the press: the
// station moves one track and a signal is played. A listener who stays on a preset to find
// out by ear what is playing can stay as long as it likes. The other button undoes a wrong
// press the same way. A receiver turned on straight on such a preset gets its own station.
//
// A press is told from a reconnect by how the previous connection ended: on a press the
// receiver closes it itself and opens a new one at once (seen on WR-230SF), while a dropped
// network or a server restart ends it from our side or leaves it hanging. Duplicate
// connections opened while the first one is still open are not presses either.
//
// The sessions are saved to a file, so a server restart does not make the receiver on a
// button lose the station it was playing.

const (
	// remotePressWindow: the listened station can be switched this long after the listener
	// left it, enough to scroll through all presets to a button.
	remotePressWindow = 5 * time.Minute
	// remoteMinListenDefault: staying on a station this long makes it the listened one.
	remoteMinListenDefault = 15 * time.Second
	// remoteRepeatGapDefault: a new connection to the same button sooner than this after the
	// previous one is never a press.
	remoteRepeatGapDefault = 3 * time.Second
	// remoteCleanCloseWindow: a press opens the new connection right after the receiver
	// closed the previous one.
	remoteCleanCloseWindow = 3 * time.Second
	// remoteCloseWait: the server may see the new connection of a press before it notices
	// that the receiver closed the previous one; it waits this long for that.
	remoteCloseWait = 1500 * time.Millisecond
	// remoteSessionTTL: sessions of clients that are gone are forgotten after this.
	remoteSessionTTL = time.Hour

	remoteDirectionNext = "next"
	remoteDirectionPrev = "prev"
)

// Sounds inserted into the stream, MP3 at 128 kbps, 44.1 kHz, stereo, without tags:
// rising and falling double beeps after a switch, three soft ticks when arriving on a button.
var (
	//go:embed sounds/next.mp3
	remoteNextSound []byte
	//go:embed sounds/prev.mp3
	remotePrevSound []byte
	//go:embed sounds/button.mp3
	remoteButtonSound []byte
)

// listenerSession is what one client (address and receiver model) listens to.
type listenerSession struct {
	Via           string    `json:"via"`   // URL the client requested
	Route         string    `json:"route"` // station actually streamed
	Open          int       `json:"open"`  // open connections of this session
	StartedAt     time.Time `json:"startedAt"`
	LastConnectAt time.Time `json:"lastConnectAt"`
	LastSeen      time.Time `json:"lastSeen"` // last connect or disconnect

	// The station the client really listened to (stayed on long enough) and when it left it.
	Listened     string    `json:"listened"`
	ListenedLeft time.Time `json:"listenedLeft"`

	gen           uint64    // changes when the client starts listening to something else
	closedCleanAt time.Time // the client itself closed the last connection of this session
}

// endedAt is when the session stopped streaming (now if it still does).
func (sess *listenerSession) endedAt(now time.Time) time.Time {
	if sess.Open > 0 {
		return now
	}
	return sess.LastSeen
}

type remoteControl struct {
	mu         sync.Mutex
	directions map[string]string // button URL -> "next" or "prev"
	sessions   map[string]*listenerSession
	nextGen    uint64
	repeatGap  time.Duration
	minListen  time.Duration
	now        func() time.Time

	stateFile string // where sessions are saved; empty — not saved
	saveMu    sync.Mutex
	logger    *slog.Logger
}

func newRemoteControl(nextRoute, prevRoute string, repeatGap, minListen time.Duration, stateFile string, logger *slog.Logger) *remoteControl {
	if repeatGap <= 0 {
		repeatGap = remoteRepeatGapDefault
	}
	if minListen <= 0 {
		minListen = remoteMinListenDefault
	}
	rc := &remoteControl{
		sessions:  make(map[string]*listenerSession),
		repeatGap: repeatGap,
		minListen: minListen,
		now:       time.Now,
		stateFile: stateFile,
		logger:    logger,
	}
	rc.setRoutes(nextRoute, prevRoute)
	rc.load()
	return rc
}

func (rc *remoteControl) setRoutes(nextRoute, prevRoute string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.directions = make(map[string]string)
	if route := normalizeRoute(nextRoute); route != "" {
		rc.directions[route] = remoteDirectionNext
	}
	if route := normalizeRoute(prevRoute); route != "" {
		rc.directions[route] = remoteDirectionPrev
	}
	// Forget who listened to what: a new configuration starts clean.
	rc.nextGen++
	rc.sessions = make(map[string]*listenerSession)
}

func normalizeRoute(route string) string {
	route = strings.TrimSpace(route)
	if route == "" {
		return ""
	}
	if !strings.HasPrefix(route, "/") {
		route = "/" + route
	}
	return route
}

// remoteDecision is the result of resolving a stream request.
type remoteDecision struct {
	target    string // station to stream
	direction string // set when this request is a button press: switch the target this way
	button    string // the requested URL is this button ("next" or "prev"), whatever the request does
	arrived   bool   // the client has just come to a button: play the button sound
	duplicate bool   // another connection of the same session (duplicate or reconnect)
	clientKey string
	gen       uint64
	tracked   bool
}

// intro returns the sound to send before the station audio.
func (d remoteDecision) intro() []byte {
	switch {
	case d.direction == remoteDirectionNext:
		return remoteNextSound
	case d.direction == remoteDirectionPrev:
		return remotePrevSound
	case d.arrived:
		return remoteButtonSound
	}
	return nil
}

// resolve decides which station a GET request to route streams and whether it is a button
// press, and records the client's session. hasStream reports whether a station exists.
func (rc *remoteControl) resolve(r *http.Request, route string, hasStream func(string) bool) remoteDecision {
	decision := remoteDecision{target: route}
	if r.Method != http.MethodGet {
		return decision
	}
	key := clientKey(r)
	decision.clientKey = key
	rc.waitForPreviousClose(key, route)
	now := rc.now()

	rc.mu.Lock()
	rc.pruneLocked(now)

	sess := rc.sessions[key]
	listened, listenedLeft := "", time.Time{}
	if sess != nil {
		listened, listenedLeft = sess.Listened, sess.ListenedLeft
		// The session being left counts as listened if the client stayed on it long enough;
		// a button station is never the one listened to.
		_, routeIsButton := rc.directions[sess.Route]
		if end := sess.endedAt(now); !routeIsButton && end.Sub(sess.StartedAt) >= rc.minListen {
			listened, listenedLeft = sess.Route, end
		}
	}
	listenedRecently := listened != "" && now.Sub(listenedLeft) <= remotePressWindow && hasStream(listened)

	direction, isButton := rc.directions[route]
	decision.button = direction
	sameSession := sess != nil && sess.Via == route && (sess.Open > 0 || now.Sub(sess.LastSeen) <= remotePressWindow)
	switch {
	case isButton && listenedRecently && sameSession && sess.Route == listened:
		decision.target = listened
		closedJustNow := sess.Open == 0 && !sess.closedCleanAt.IsZero() && now.Sub(sess.closedCleanAt) <= remoteCleanCloseWindow
		if closedJustNow && now.Sub(sess.LastConnectAt) >= rc.repeatGap {
			decision.direction = direction // the same preset pressed again
		} else {
			decision.duplicate = true
		}
	case isButton && listenedRecently:
		decision.target = listened
		decision.arrived = true
	case sameSession && sess.Route == route:
		// The same station pressed again (the client closed it itself just now): a request to
		// hear what plays. Otherwise a duplicate connection or a reconnect after a drop.
		closedJustNow := sess.Open == 0 && !sess.closedCleanAt.IsZero() && now.Sub(sess.closedCleanAt) <= remoteCleanCloseWindow
		decision.duplicate = !closedJustNow
	}

	if sameSession && sess.Route == decision.target {
		sess.Open++
	} else {
		rc.nextGen++
		sess = &listenerSession{
			Via:          route,
			Route:        decision.target,
			Open:         1,
			gen:          rc.nextGen,
			StartedAt:    now,
			Listened:     listened,
			ListenedLeft: listenedLeft,
		}
		rc.sessions[key] = sess
	}
	sess.LastConnectAt = now
	sess.LastSeen = now
	sess.closedCleanAt = time.Time{}
	decision.gen = sess.gen
	decision.tracked = true
	snapshot := rc.snapshotLocked()
	rc.mu.Unlock()

	rc.save(snapshot)
	return decision
}

// waitForPreviousClose waits up to remoteCloseWait while a connection of the same client to
// the same URL is still open: on a press the receiver has just closed it.
func (rc *remoteControl) waitForPreviousClose(key, route string) {
	deadline := time.Now().Add(remoteCloseWait)
	for {
		rc.mu.Lock()
		sess := rc.sessions[key]
		stillOpen := sess != nil && sess.Via == route && sess.Open > 0
		rc.mu.Unlock()
		if !stillOpen || time.Now().After(deadline) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// release records that a connection resolved by resolve has ended. clean: the client
// itself closed it (as a receiver does when its preset is pressed).
func (rc *remoteControl) release(decision remoteDecision, clean bool) {
	if !decision.tracked {
		return
	}
	rc.mu.Lock()
	// A connection of an older session (the station the listener left) must not touch the new one.
	sess := rc.sessions[decision.clientKey]
	if sess == nil || sess.gen != decision.gen {
		rc.mu.Unlock()
		return
	}
	if sess.Open > 0 {
		sess.Open--
	}
	now := rc.now()
	sess.LastSeen = now
	if clean && sess.Open == 0 {
		sess.closedCleanAt = now
	}
	snapshot := rc.snapshotLocked()
	rc.mu.Unlock()

	rc.save(snapshot)
}

func (rc *remoteControl) pruneLocked(now time.Time) {
	for key, sess := range rc.sessions {
		if sess.Open == 0 && now.Sub(sess.LastSeen) > remoteSessionTTL {
			delete(rc.sessions, key)
		}
	}
}

// snapshotLocked returns the sessions to save, or nil when they are not saved.
func (rc *remoteControl) snapshotLocked() map[string]listenerSession {
	if rc.stateFile == "" {
		return nil
	}
	snapshot := make(map[string]listenerSession, len(rc.sessions))
	for key, sess := range rc.sessions {
		snapshot[key] = *sess
	}
	return snapshot
}

// save writes the sessions to the state file (temporary file and rename, so a crash never
// leaves a half-written file).
func (rc *remoteControl) save(snapshot map[string]listenerSession) {
	if snapshot == nil {
		return
	}
	rc.saveMu.Lock()
	defer rc.saveMu.Unlock()
	data, err := json.Marshal(snapshot)
	if err == nil {
		tmp := rc.stateFile + ".tmp"
		if err = os.WriteFile(tmp, data, 0o644); err == nil {
			err = os.Rename(tmp, rc.stateFile)
		}
	}
	if err != nil && rc.logger != nil {
		rc.logger.Warn("Remote control: cannot save sessions", slog.String("file", rc.stateFile), slog.String("error", err.Error()))
	}
}

// load restores the sessions saved before a restart. Connections that were open then were
// cut by the restart: they count as ended now, and the reconnect that follows is not a press.
func (rc *remoteControl) load() {
	if rc.stateFile == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(rc.stateFile), 0o755); err != nil && rc.logger != nil {
		rc.logger.Warn("Remote control: cannot create state directory", slog.String("error", err.Error()))
	}
	data, err := os.ReadFile(rc.stateFile)
	if err != nil {
		return
	}
	var saved map[string]listenerSession
	if err := json.Unmarshal(data, &saved); err != nil {
		if rc.logger != nil {
			rc.logger.Warn("Remote control: cannot read saved sessions", slog.String("error", err.Error()))
		}
		return
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	now := rc.now()
	for key, s := range saved {
		sess := s
		if sess.Open > 0 {
			sess.LastSeen = now
		}
		sess.Open = 0
		rc.nextGen++
		sess.gen = rc.nextGen
		rc.sessions[key] = &sess
	}
	if rc.logger != nil {
		rc.logger.Info("Remote control: sessions restored", slog.Int("count", len(saved)))
	}
}

// clientKey identifies a listener: its address and receiver model, so that a phone behind the
// same router as the receiver has a session of its own.
func clientKey(r *http.Request) string {
	return clientIP(r) + "|" + r.UserAgent()
}

// clientIP returns the client address without the port.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// SetRemoteControlRoutes sets the station URLs that work as "next" and "previous" buttons.
// An empty route disables that button.
func (s *Server) SetRemoteControlRoutes(nextRoute, prevRoute string) {
	s.remote.setRoutes(nextRoute, prevRoute)
	s.logger.Info("Remote control buttons configured",
		slog.String("next", normalizeRoute(nextRoute)),
		slog.String("prev", normalizeRoute(prevRoute)))
}

// SetRemoteControlTiming sets the gap before which a new connection to the same button is
// never a press and how long a client must stay on a station for it to count as listened.
// Zero keeps the default.
func (s *Server) SetRemoteControlTiming(repeatGap, minListen time.Duration) {
	s.remote.mu.Lock()
	defer s.remote.mu.Unlock()
	if repeatGap <= 0 {
		repeatGap = remoteRepeatGapDefault
	}
	if minListen <= 0 {
		minListen = remoteMinListenDefault
	}
	s.remote.repeatGap = repeatGap
	s.remote.minListen = minListen
}
