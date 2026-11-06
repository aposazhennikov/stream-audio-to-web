package listeners

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/oschwald/maxminddb-golang"
)

// The free DB-IP "IP to City Lite" database (CC BY 4.0, https://db-ip.com), published monthly
// at a fixed address; no key is needed.
const (
	dbipFileName  = "dbip-city-lite.mmdb"
	dbipURLFormat = "https://download.db-ip.com/free/dbip-city-lite-%s.mmdb.gz"
	dbipMaxAge    = 35 * 24 * time.Hour // a month's edition plus a few days
)

// DBIP looks addresses up in the DB-IP city database, downloading it when it is missing or
// older than a month.
type DBIP struct {
	dir    string
	logger *slog.Logger
	client *http.Client

	mu     sync.RWMutex
	reader *maxminddb.Reader
	cache  map[string][2]string
}

// OpenDBIP opens the database in dir; with download set, it also keeps it up to date in the
// background until ctx is done.
func OpenDBIP(ctx context.Context, dir string, download bool, logger *slog.Logger) *DBIP {
	if logger == nil {
		logger = slog.Default()
	}
	g := &DBIP{dir: dir, logger: logger, client: &http.Client{Timeout: 10 * time.Minute}, cache: map[string][2]string{}}
	g.open()
	if download {
		go g.keepFresh(ctx)
	}
	return g
}

// Lookup returns the country and city of ip, in Russian when the database has it.
func (g *DBIP) Lookup(ip net.IP) (string, string) {
	if g == nil || ip == nil {
		return "", ""
	}
	key := ip.String()
	g.mu.RLock()
	reader := g.reader
	cached, ok := g.cache[key]
	g.mu.RUnlock()
	if ok {
		return cached[0], cached[1]
	}
	if reader == nil {
		return "", ""
	}
	var rec struct {
		City struct {
			Names map[string]string `maxminddb:"names"`
		} `maxminddb:"city"`
		Country struct {
			Names map[string]string `maxminddb:"names"`
		} `maxminddb:"country"`
	}
	if err := reader.Lookup(ip, &rec); err != nil {
		return "", ""
	}
	country, city := name(rec.Country.Names), name(rec.City.Names)
	g.mu.Lock()
	if len(g.cache) > 10000 {
		g.cache = map[string][2]string{}
	}
	g.cache[key] = [2]string{country, city}
	g.mu.Unlock()
	return country, city
}

func name(names map[string]string) string {
	if n := names["ru"]; n != "" {
		return n
	}
	return names["en"]
}

func (g *DBIP) path() string { return filepath.Join(g.dir, dbipFileName) }

func (g *DBIP) open() {
	reader, err := maxminddb.Open(g.path())
	if err != nil {
		if !os.IsNotExist(err) {
			g.logger.Warn("Cannot open the address database", slog.String("error", err.Error()))
		}
		return
	}
	g.mu.Lock()
	old := g.reader
	g.reader = reader
	g.cache = map[string][2]string{}
	g.mu.Unlock()
	if old != nil {
		// Lookups in flight may still use the old reader for a moment.
		time.AfterFunc(time.Minute, func() { _ = old.Close() })
	}
}

func (g *DBIP) keepFresh(ctx context.Context) {
	for {
		if info, err := os.Stat(g.path()); err != nil || time.Since(info.ModTime()) > dbipMaxAge {
			if err := g.download(ctx); err != nil {
				g.logger.Warn("Cannot download the address database", slog.String("error", err.Error()))
			} else {
				g.open()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(24 * time.Hour):
		}
	}
}

// download fetches this month's edition, or last month's early in a month before the new
// one is out.
func (g *DBIP) download(ctx context.Context) error {
	now := time.Now().UTC()
	var lastErr error
	for _, month := range []time.Time{now, now.AddDate(0, -1, -now.Day()+1)} {
		url := fmt.Sprintf(dbipURLFormat, month.Format("2006-01"))
		if lastErr = g.fetch(ctx, url); lastErr == nil {
			g.logger.Info("Address database updated", slog.String("url", url))
			return nil
		}
	}
	return lastErr
}

func (g *DBIP) fetch(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "stream-audio-to-web")
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	unzipped, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(g.dir, 0o755); err != nil {
		return err
	}
	tmp := g.path() + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, unzipped)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	// Make sure it is a database before replacing the old one.
	if reader, err := maxminddb.Open(tmp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("downloaded file is not a database: %w", err)
	} else {
		_ = reader.Close()
	}
	return os.Rename(tmp, g.path())
}
