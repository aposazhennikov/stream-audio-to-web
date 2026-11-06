// Package listeners keeps track of who listens to which station: the connections open now
// (for the status page) and every finished listening session, one JSON line per session in a
// file per month (for the monthly statistics).
package listeners

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// minRecorded is the shortest session written to the monthly file: players open a stream
// for a fraction of a second to probe it before playing.
const minRecorded = 3 * time.Second

// checkpointEvery is how often the open sessions are saved, so a restart loses at most that
// much of them.
const checkpointEvery = time.Minute

// Session is one connection of a listener to a stream.
type Session struct {
	IP        string    `json:"ip"`
	UserAgent string    `json:"userAgent"`
	Device    string    `json:"device"`
	Country   string    `json:"country,omitempty"`
	City      string    `json:"city,omitempty"`
	Route     string    `json:"route"`   // URL the listener asked for (a button URL streams another station)
	Station   string    `json:"station"` // station streamed
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Seconds   float64   `json:"seconds"`
	Service   bool      `json:"service,omitempty"` // our own: the server itself or the docker network
}

// Geo finds the country and city of an address; empty when unknown.
type Geo interface {
	Lookup(ip net.IP) (country, city string)
}

// Tracker records listening sessions.
type Tracker struct {
	dir    string
	geo    Geo
	logger *slog.Logger
	now    func() time.Time

	mu   sync.Mutex
	open map[int64]*Session
	next int64

	fileMu sync.Mutex
}

// New returns a tracker writing to dir; empty dir keeps nothing on disk. Sessions that were
// open when the server stopped are recorded as ended at the last checkpoint.
func New(dir string, geo Geo, logger *slog.Logger) *Tracker {
	if logger == nil {
		logger = slog.Default()
	}
	t := &Tracker{dir: dir, geo: geo, logger: logger, now: time.Now, open: map[int64]*Session{}}
	t.recoverOpen()
	return t
}

// Start records a new connection and returns its id for End.
func (t *Tracker) Start(remoteAddr, userAgent, route, station string) int64 {
	if t == nil {
		return 0
	}
	ip := hostOf(remoteAddr)
	sess := &Session{
		IP:        ip,
		UserAgent: userAgent,
		Device:    DeviceName(userAgent),
		Route:     route,
		Station:   station,
		Start:     t.now(),
		Service:   isServiceAddress(ip),
	}
	if t.geo != nil && !sess.Service {
		if parsed := net.ParseIP(ip); parsed != nil {
			sess.Country, sess.City = t.geo.Lookup(parsed)
		}
	}
	t.mu.Lock()
	t.next++
	id := t.next
	t.open[id] = sess
	t.mu.Unlock()
	return id
}

// End records that a connection closed.
func (t *Tracker) End(id int64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	sess, ok := t.open[id]
	delete(t.open, id)
	t.mu.Unlock()
	if !ok {
		return
	}
	sess.End = t.now()
	sess.Seconds = sess.End.Sub(sess.Start).Seconds()
	t.record(*sess)
}

// Live returns the open connections, longest first, with their time so far.
func (t *Tracker) Live() []Session {
	if t == nil {
		return nil
	}
	now := t.now()
	t.mu.Lock()
	ids := make([]int64, 0, len(t.open))
	for id := range t.open {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] }) // ids grow with time
	out := make([]Session, 0, len(ids))
	for _, id := range ids {
		s := *t.open[id]
		s.End = now
		s.Seconds = now.Sub(s.Start).Seconds()
		out = append(out, s)
	}
	t.mu.Unlock()
	return out
}

// Run saves the open sessions every minute until ctx is done, then once more.
func (t *Tracker) Run(ctx context.Context) {
	if t == nil || t.dir == "" {
		return
	}
	ticker := time.NewTicker(checkpointEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.checkpoint()
			return
		case <-ticker.C:
			t.checkpoint()
		}
	}
}

// Month returns the finished sessions of a month ("2026-11"), plus the open ones if it is the
// current month.
func (t *Tracker) Month(month string) []Session {
	if t == nil {
		return nil
	}
	var out []Session
	if t.dir != "" {
		t.fileMu.Lock()
		f, err := os.Open(filepath.Join(t.dir, month+".jsonl"))
		if err == nil {
			scanner := bufio.NewScanner(f)
			scanner.Buffer(make([]byte, 64*1024), 1024*1024)
			for scanner.Scan() {
				var s Session
				if json.Unmarshal(scanner.Bytes(), &s) == nil {
					out = append(out, s)
				}
			}
			f.Close()
		}
		t.fileMu.Unlock()
	}
	if month == t.now().Format("2006-01") {
		for _, s := range t.Live() {
			if s.Seconds >= minRecorded.Seconds() {
				out = append(out, s)
			}
		}
	}
	return out
}

// Months lists the months that have statistics, newest first; the current month is always
// there.
func (t *Tracker) Months() []string {
	if t == nil {
		return nil
	}
	current := t.now().Format("2006-01")
	months := []string{current}
	if t.dir != "" {
		entries, _ := os.ReadDir(t.dir)
		for _, e := range entries {
			name := strings.TrimSuffix(e.Name(), ".jsonl")
			if name != e.Name() && name != current && len(name) == 7 {
				months = append(months, name)
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(months)))
	return months
}

func (t *Tracker) record(s Session) {
	if t.dir == "" || s.Seconds < minRecorded.Seconds() {
		return
	}
	line, err := json.Marshal(s)
	if err != nil {
		return
	}
	t.fileMu.Lock()
	defer t.fileMu.Unlock()
	if err := os.MkdirAll(t.dir, 0o755); err != nil {
		t.logger.Warn("Listening statistics: cannot create directory", slog.String("error", err.Error()))
		return
	}
	f, err := os.OpenFile(filepath.Join(t.dir, s.Start.Format("2006-01")+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.logger.Warn("Listening statistics: cannot write", slog.String("error", err.Error()))
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

func (t *Tracker) openFile() string { return filepath.Join(t.dir, "open.json") }

// checkpoint saves the open sessions as if they ended now.
func (t *Tracker) checkpoint() {
	data, err := json.Marshal(t.Live())
	if err != nil {
		return
	}
	if err := os.MkdirAll(t.dir, 0o755); err != nil {
		return
	}
	tmp := t.openFile() + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, t.openFile())
	}
}

// recoverOpen records the sessions the previous run had open at its last checkpoint.
func (t *Tracker) recoverOpen() {
	if t.dir == "" {
		return
	}
	data, err := os.ReadFile(t.openFile())
	if err != nil {
		return
	}
	var sessions []Session
	if json.Unmarshal(data, &sessions) == nil {
		// Connections closed during a clean shutdown are already recorded.
		recorded := map[string]bool{}
		for _, s := range sessions {
			month := s.Start.Format("2006-01")
			if _, read := recorded["month:"+month]; !read {
				recorded["month:"+month] = true
				for _, old := range t.Month(month) {
					recorded[sessionKey(old)] = true
				}
			}
			if !recorded[sessionKey(s)] {
				t.record(s)
			}
		}
	}
	_ = os.Remove(t.openFile())
}

func sessionKey(s Session) string {
	return s.IP + "|" + s.UserAgent + "|" + s.Route + "|" + s.Start.UTC().Format(time.RFC3339Nano)
}

func hostOf(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// isServiceAddress: connections from the server itself or the docker network are ours
// (checks, recordings), not listeners.
func isServiceAddress(ip string) bool {
	parsed := net.ParseIP(ip)
	return parsed != nil && (parsed.IsLoopback() || parsed.IsPrivate())
}
