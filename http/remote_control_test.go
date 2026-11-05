package http

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// remoteClock is a manual clock for the remote control logic.
type remoteClock struct{ t time.Time }

func (c *remoteClock) now() time.Time          { return c.t }
func (c *remoteClock) advance(d time.Duration) { c.t = c.t.Add(d) }
func remoteRequest(method string) *http.Request {
	r, _ := http.NewRequest(method, "http://radio/x", nil)
	r.RemoteAddr = "203.0.113.7:40000"
	r.Header.Set("User-Agent", "WR-230SF/1.0")
	return r
}

func newTestRemote(t *testing.T, stateFile string, clock *remoteClock) *remoteControl {
	t.Helper()
	rc := newRemoteControl("/b-next", "/b-prev", 3*time.Second, 15*time.Second, "", nil)
	rc.now = clock.now
	rc.stateFile = stateFile
	rc.load()
	return rc
}

func allStations(string) bool { return true }

// A press is a new connection right after the receiver itself closed the previous one;
// a network drop (connection ended from our side) is not a press.
func TestRemotePressNeedsCleanClose(t *testing.T) {
	clock := &remoteClock{t: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}
	rc := newTestRemote(t, "", clock)

	listen := rc.resolve(remoteRequest(http.MethodGet), "/station", allStations)
	clock.advance(time.Minute)
	rc.release(listen, true)

	clock.advance(time.Second)
	arrive := rc.resolve(remoteRequest(http.MethodGet), "/b-next", allStations)
	if !arrive.arrived || arrive.direction != "" || arrive.target != "/station" {
		t.Fatalf("arrival: %+v", arrive)
	}
	clock.advance(20 * time.Second)
	rc.release(arrive, true)

	clock.advance(500 * time.Millisecond)
	press := rc.resolve(remoteRequest(http.MethodGet), "/b-next", allStations)
	if press.direction != remoteDirectionNext || press.target != "/station" {
		t.Fatalf("repeated press after a clean close: %+v", press)
	}

	clock.advance(30 * time.Second)
	rc.release(press, false) // the network dropped: ended from our side
	clock.advance(time.Second)
	reconnect := rc.resolve(remoteRequest(http.MethodGet), "/b-next", allStations)
	if reconnect.direction != "" || reconnect.target != "/station" {
		t.Fatalf("reconnect after a network drop must continue the station without switching: %+v", reconnect)
	}

	clock.advance(time.Minute)
	rc.release(reconnect, true)
	clock.advance(10 * time.Second) // too long after the close to be the same press
	late := rc.resolve(remoteRequest(http.MethodGet), "/b-next", allStations)
	if late.direction != "" || late.target != "/station" {
		t.Fatalf("a connection long after the close is a reconnect, not a press: %+v", late)
	}
}

// After a restart the receiver on a button reconnects to it: the station it was playing
// continues, nothing is switched.
func TestRemoteSessionsSurviveRestart(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state", "remote.json")
	clock := &remoteClock{t: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}
	rc := newTestRemote(t, stateFile, clock)

	listen := rc.resolve(remoteRequest(http.MethodGet), "/station", allStations)
	clock.advance(time.Minute)
	rc.release(listen, true)
	clock.advance(time.Second)
	onButton := rc.resolve(remoteRequest(http.MethodGet), "/b-next", allStations)
	if onButton.target != "/station" {
		t.Fatalf("button must play the listened station: %+v", onButton)
	}
	clock.advance(10 * time.Minute) // listening on the button, then the server restarts

	restarted := newTestRemote(t, stateFile, clock)
	clock.advance(30 * time.Second)
	again := restarted.resolve(remoteRequest(http.MethodGet), "/b-next", allStations)
	if again.direction != "" || again.arrived || again.target != "/station" {
		t.Fatalf("after a restart the button must continue the station without switching: %+v", again)
	}
}

// A button station is never the listened one: listening to the button's own station and
// pressing it again does not switch it.
func TestRemoteButtonStationIsNeverListened(t *testing.T) {
	clock := &remoteClock{t: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}
	rc := newTestRemote(t, "", clock)

	own := rc.resolve(remoteRequest(http.MethodGet), "/b-next", allStations)
	if own.target != "/b-next" || own.arrived {
		t.Fatalf("turned on straight on the button: %+v", own)
	}
	clock.advance(5 * time.Minute)
	rc.release(own, true)
	clock.advance(500 * time.Millisecond)
	again := rc.resolve(remoteRequest(http.MethodGet), "/b-next", allStations)
	if again.direction != "" || again.target != "/b-next" {
		t.Fatalf("pressing the button again must not switch its own station: %+v", again)
	}
}

// A HEAD request is neither tracked nor a press.
func TestRemoteHeadIsIgnored(t *testing.T) {
	clock := &remoteClock{t: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}
	rc := newTestRemote(t, "", clock)
	if d := rc.resolve(remoteRequest(http.MethodHead), "/b-next", allStations); d.tracked || d.direction != "" {
		t.Fatalf("HEAD: %+v", d)
	}
}
