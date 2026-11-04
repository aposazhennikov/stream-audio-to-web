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
// On the button that station keeps playing, and if the listener stays on the button for
// remoteHoldDefault, it moves one track forward or back and a short signal is played.
// Passing a button while scrolling does nothing. A receiver turned on straight on such a
// preset gets the preset's own station as usual.
//
// A press is told from a reconnect by the URL: reconnects and the duplicate connections some
// receivers open are to the same URL, a press comes from another one. To press the same
// button again, the listener steps to the neighbouring preset and back; within
// remoteSeriesWindow of the last switch the same button acts after remoteSeriesHold, so
// several tracks can be skipped quickly. The other button keeps the full hold: leaving the
// buttons by scrolling past it must not switch back.

const (
	// remotePressWindow: the listened station can be switched this long after the listener
	// left it, enough to scroll through all presets to a button.
	remotePressWindow = 5 * time.Minute
	// remoteMinListenDefault: staying on a station this long makes it the listened one.
	remoteMinListenDefault = 30 * time.Second
	// remoteHoldDefault: how long the listener must stay on a button for it to act.
	remoteHoldDefault = 6 * time.Second
	// remoteSeriesHold: the hold for the same button again within remoteSeriesWindow.
	remoteSeriesHold   = 2 * time.Second
	remoteSeriesWindow = time.Minute
	// remoteSessionTTL: sessions of clients that are gone are forgotten after this.
	remoteSessionTTL = time.Hour

	remoteDirectionNext = "next"
	remoteDirectionPrev = "prev"
)

// Short rising and falling double beeps, MP3 at 128 kbps, 44.1 kHz, stereo, without tags.
var (
	//go:embed sounds/next.mp3
	remoteNextSound []byte
	//go:embed sounds/prev.mp3
	remotePrevSound []byte
)

// listenerSession is what one client address listens to.
type listenerSession struct {
	via       string    // URL the client requested
	route     string    // station actually streamed
	open      int       // open connections of this session
	gen       uint64    // changes when the client starts listening to something else
	startedAt time.Time // first connection of this session
	lastSeen  time.Time // last connect or disconnect
	signals   map[uint64]chan []byte

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
	nextConn   uint64
	hold       time.Duration
	seriesHold time.Duration
	minListen  time.Duration
	lastSwitch map[string]remoteSwitch // last switch by client address
	now        func() time.Time
	// switchTrack moves a station one track in a direction; set by the server.
	switchTrack func(route, direction string) error
	logger      *slog.Logger
}

func newRemoteControl(nextRoute, prevRoute string, hold time.Duration, logger *slog.Logger) *remoteControl {
	if hold <= 0 {
		hold = remoteHoldDefault
	}
	rc := &remoteControl{
		sessions:   make(map[string]*listenerSession),
		hold:       hold,
		seriesHold: remoteSeriesHold,
		minListen:  remoteMinListenDefault,
		lastSwitch: make(map[string]remoteSwitch),
		now:        time.Now,
		logger:     logger,
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
	rc.lastSwitch = make(map[string]remoteSwitch)
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
	direction string // set when this request is a button press
	clientIP  string
	gen       uint64
	connID    uint64
	signals   chan []byte // signals to insert into this connection's audio
	tracked   bool
}

// resolve decides which station a GET request to route streams and whether it is a button
// press, and records the client's session. A press acts after the hold time if the listener
// is still on the button. hasStream reports whether a station exists.
func (rc *remoteControl) resolve(r *http.Request, route string, hasStream func(string) bool) remoteDecision {
	decision := remoteDecision{target: route}
	if r.Method != http.MethodGet {
		return decision
	}
	ip := clientIP(r)
	decision.clientIP = ip
	now := rc.now()

	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.pruneLocked(now)

	sess := rc.sessions[ip]
	listened, listenedLeft := "", time.Time{}
	if sess != nil {
		listened, listenedLeft = sess.listened, sess.listenedLeft
		// The session being left counts as listened if the client stayed on it long enough.
		if end := sess.endedAt(now); end.Sub(sess.startedAt) >= rc.minListen {
			listened, listenedLeft = sess.route, end
		}
	}
	listenedRecently := listened != "" && now.Sub(listenedLeft) <= remotePressWindow && hasStream(listened)
	if direction, isButton := rc.directions[route]; isButton && listenedRecently {
		decision.target = listened
		if sess.via != route {
			// Came from another URL: the listener pressed this button.
			decision.direction = direction
		}
	}

	if sess != nil && sess.via == route && sess.route == decision.target &&
		(sess.open > 0 || now.Sub(sess.lastSeen) <= remotePressWindow) {
		// A reconnect or a duplicate connection of the same session.
		sess.open++
	} else {
		rc.nextGen++
		sess = &listenerSession{
			via:          route,
			route:        decision.target,
			open:         1,
			gen:          rc.nextGen,
			startedAt:    now,
			signals:      make(map[uint64]chan []byte),
			listened:     listened,
			listenedLeft: listenedLeft,
		}
		rc.sessions[ip] = sess
	}
	sess.lastSeen = now
	rc.nextConn++
	decision.connID = rc.nextConn
	decision.signals = make(chan []byte, 1)
	sess.signals[decision.connID] = decision.signals
	decision.gen = sess.gen
	decision.tracked = true

	if decision.direction != "" {
		hold := rc.hold
		if last, ok := rc.lastSwitch[ip]; ok && last.direction == decision.direction && now.Sub(last.at) <= remoteSeriesWindow {
			hold = rc.seriesHold
		}
		gen, target, direction := sess.gen, decision.target, decision.direction
		time.AfterFunc(hold, func() { rc.fire(ip, gen, target, direction) })
	}
	return decision
}

// remoteSwitch is a switch made by a button press.
type remoteSwitch struct {
	direction string
	at        time.Time
}

// fire performs a press if the listener is still on the button it pressed.
func (rc *remoteControl) fire(ip string, gen uint64, target, direction string) {
	rc.mu.Lock()
	sess := rc.sessions[ip]
	if sess == nil || sess.gen != gen || sess.open == 0 {
		rc.mu.Unlock()
		rc.logger.Info("Remote control press dropped: the listener moved on before the hold time",
			slog.String("station", target), slog.String("direction", direction), slog.String("client", ip))
		return
	}
	channels := make([]chan []byte, 0, len(sess.signals))
	for _, ch := range sess.signals {
		channels = append(channels, ch)
	}
	switchTrack := rc.switchTrack
	rc.lastSwitch[ip] = remoteSwitch{direction: direction, at: rc.now()}
	rc.mu.Unlock()

	rc.logger.Info("Remote control press: switching station",
		slog.String("station", target), slog.String("direction", direction), slog.String("client", ip))
	if switchTrack != nil {
		if err := switchTrack(target, direction); err != nil {
			rc.logger.Error("Remote control track switch failed",
				slog.String("station", target), slog.String("error", err.Error()))
			return
		}
	}
	signal := remoteSignal(direction)
	for _, ch := range channels {
		select {
		case ch <- signal:
		default:
		}
	}
}

// release records that a connection resolved by resolve has ended.
func (rc *remoteControl) release(decision remoteDecision) {
	if !decision.tracked {
		return
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	// A connection of an older session (the station the listener left) must not touch the new one.
	if sess := rc.sessions[decision.clientIP]; sess != nil && sess.gen == decision.gen {
		if sess.open > 0 {
			sess.open--
		}
		delete(sess.signals, decision.connID)
		sess.lastSeen = rc.now()
	}
}

func (rc *remoteControl) pruneLocked(now time.Time) {
	for ip, sess := range rc.sessions {
		if sess.open == 0 && now.Sub(sess.lastSeen) > remoteSessionTTL {
			delete(rc.sessions, ip)
		}
	}
	for ip, last := range rc.lastSwitch {
		if now.Sub(last.at) > remoteSeriesWindow {
			delete(rc.lastSwitch, ip)
		}
	}
}

// remoteSignal returns the sound played after a button press switched the station.
func remoteSignal(direction string) []byte {
	switch direction {
	case remoteDirectionNext:
		return remoteNextSound
	case remoteDirectionPrev:
		return remotePrevSound
	}
	return nil
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

// SetRemoteControlTiming sets how long a listener must stay on a button for it to act (the
// first time and again in a series) and how long on a station for it to count as listened.
// Zero keeps the default.
func (s *Server) SetRemoteControlTiming(hold, seriesHold, minListen time.Duration) {
	s.remote.mu.Lock()
	defer s.remote.mu.Unlock()
	if hold <= 0 {
		hold = remoteHoldDefault
	}
	if seriesHold <= 0 {
		seriesHold = remoteSeriesHold
	}
	if minListen <= 0 {
		minListen = remoteMinListenDefault
	}
	s.remote.hold = hold
	s.remote.seriesHold = seriesHold
	s.remote.minListen = minListen
}
