package http

import (
	_ "embed"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Remote control for radio receivers that can only play a stream URL.
//
// Two station URLs (for example /shaov and /troshin, stored in the receiver presets) act as
// "next" and "previous" buttons for the station the listener was listening to: the last
// station of ours it stayed on for at least remoteMinListenDefault. Stations passed on the
// way (a receiver without number keys is scrolled preset by preset) do not count.
//
// Arriving on a button switches nothing: the button introduces itself with a short sound and
// the listened station keeps playing. Pressing the same preset again (the receiver opens a
// new connection to the same URL) is the press: the station moves one track and a signal is
// played. A listener who stays on a preset to find out by ear what is playing can stay as
// long as it likes; only the repeated press acts. The other button undoes a wrong press the
// same way. A receiver turned on straight on such a preset gets the preset's own station.
//
// Some receivers open two connections for one press within a few seconds: a connection to
// the same URL sooner than remoteRepeatGap after the previous one is that duplicate.

const (
	// remotePressWindow: the listened station can be switched this long after the listener
	// left it, enough to scroll through all presets to a button.
	remotePressWindow = 5 * time.Minute
	// remoteMinListenDefault: staying on a station this long makes it the listened one.
	remoteMinListenDefault = 15 * time.Second
	// remoteRepeatGapDefault: a new connection to the same button sooner than this after the
	// previous one is a duplicate of it, later — a repeated press.
	remoteRepeatGapDefault = 8 * time.Second
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
	via           string    // URL the client requested
	route         string    // station actually streamed
	open          int       // open connections of this session
	gen           uint64    // changes when the client starts listening to something else
	startedAt     time.Time // first connection of this session
	lastConnectAt time.Time // latest connection of this session
	lastSeen      time.Time // last connect or disconnect

	// The station the client really listened to (stayed on long enough) and when it left it.
	listened     string
	listenedLeft time.Time
}

// endedAt is when the session stopped streaming (now if it still does).
func (sess *listenerSession) endedAt(now time.Time) time.Time {
	if sess.open > 0 {
		return now
	}
	return sess.lastSeen
}

type remoteControl struct {
	mu         sync.Mutex
	directions map[string]string // button URL -> "next" or "prev"
	sessions   map[string]*listenerSession
	nextGen    uint64
	repeatGap  time.Duration
	minListen  time.Duration
	now        func() time.Time
}

func newRemoteControl(nextRoute, prevRoute string, repeatGap, minListen time.Duration) *remoteControl {
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
	}
	rc.setRoutes(nextRoute, prevRoute)
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
	arrived   bool   // the client has just come to a button: play the button sound
	duplicate bool   // another connection of the same press or a reconnect
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
	now := rc.now()

	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.pruneLocked(now)

	sess := rc.sessions[key]
	listened, listenedLeft := "", time.Time{}
	if sess != nil {
		listened, listenedLeft = sess.listened, sess.listenedLeft
		// The session being left counts as listened if the client stayed on it long enough.
		if end := sess.endedAt(now); end.Sub(sess.startedAt) >= rc.minListen {
			listened, listenedLeft = sess.route, end
		}
	}
	listenedRecently := listened != "" && now.Sub(listenedLeft) <= remotePressWindow && hasStream(listened)

	direction, isButton := rc.directions[route]
	sameSession := sess != nil && sess.via == route && (sess.open > 0 || now.Sub(sess.lastSeen) <= remotePressWindow)
	switch {
	case isButton && listenedRecently && sameSession && sess.route == listened:
		decision.target = listened
		if now.Sub(sess.lastConnectAt) >= rc.repeatGap {
			decision.direction = direction // the same preset pressed again
		} else {
			decision.duplicate = true
		}
	case isButton && listenedRecently:
		decision.target = listened
		decision.arrived = true
	case sameSession && sess.route == route:
		decision.duplicate = true
	}

	if sameSession && sess.route == decision.target {
		sess.open++
	} else {
		rc.nextGen++
		sess = &listenerSession{
			via:          route,
			route:        decision.target,
			open:         1,
			gen:          rc.nextGen,
			startedAt:    now,
			listened:     listened,
			listenedLeft: listenedLeft,
		}
		rc.sessions[key] = sess
	}
	sess.lastConnectAt = now
	sess.lastSeen = now
	decision.gen = sess.gen
	decision.tracked = true
	return decision
}

// release records that a connection resolved by resolve has ended.
func (rc *remoteControl) release(decision remoteDecision) {
	if !decision.tracked {
		return
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	// A connection of an older session (the station the listener left) must not touch the new one.
	if sess := rc.sessions[decision.clientKey]; sess != nil && sess.gen == decision.gen {
		if sess.open > 0 {
			sess.open--
		}
		sess.lastSeen = rc.now()
	}
}

func (rc *remoteControl) pruneLocked(now time.Time) {
	for key, sess := range rc.sessions {
		if sess.open == 0 && now.Sub(sess.lastSeen) > remoteSessionTTL {
			delete(rc.sessions, key)
		}
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

// SetRemoteControlTiming sets the gap after which a new connection to the same button is a
// repeated press (sooner — a duplicate) and how long a client must stay on a station for it
// to count as listened. Zero keeps the default.
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
