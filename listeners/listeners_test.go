package listeners

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeGeo struct{}

func (fakeGeo) Lookup(ip net.IP) (string, string) {
	if ip.String() == "94.188.82.133" {
		return "Россия", "Москва"
	}
	return "", ""
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestTrackerRecordsSessionsByMonth(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)}
	tr := New(dir, fakeGeo{}, nil)
	tr.now = c.now

	radio := tr.Start("94.188.82.133:52591", "WR-230SF/1.0", "/politics", "/politics")
	probe := tr.Start("185.213.230.46:7520", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/156.0", "/monosov10", "/monosov10")
	ours := tr.Start("172.18.0.1:53166", "VoiceCheck/1.0", "/monosov11", "/monosov11")

	c.t = c.t.Add(time.Second)
	tr.End(probe) // a one-second probe is not kept

	live := tr.Live()
	if len(live) != 2 {
		t.Fatalf("live = %d, want 2", len(live))
	}
	if live[0].Device != "Радиоприёмник WR-230SF" || live[0].City != "Москва" || live[0].Service {
		t.Errorf("receiver session = %+v", live[0])
	}
	if !live[1].Service {
		t.Errorf("a connection from the docker network is ours: %+v", live[1])
	}

	c.t = c.t.Add(40 * time.Minute)
	tr.End(radio)
	tr.End(ours)

	month := tr.Month("2026-11")
	if len(month) != 2 {
		t.Fatalf("recorded = %d, want 2: %+v", len(month), month)
	}
	if got := month[0].Seconds; got < 2400 || got > 2402 {
		t.Errorf("seconds = %v", got)
	}
	if months := tr.Months(); len(months) != 1 || months[0] != "2026-11" {
		t.Errorf("months = %v", months)
	}

	sums := Summarize(month)
	if len(sums) != 2 || sums[0].Service || sums[0].Stations[0].Station != "/politics" {
		t.Errorf("summary = %+v", sums)
	}
}

func TestTrackerRecoversSessionsOpenAtRestart(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)}
	tr := New(dir, nil, nil)
	tr.now = c.now
	open := tr.Start("94.188.82.133:1", "WR-230SF/1.0", "/nature", "/nature")
	closed := tr.Start("94.188.82.133:2", "WR-230SF/1.0", "/monosov4", "/monosov4")
	c.t = c.t.Add(10 * time.Minute)
	tr.checkpoint()
	tr.End(closed) // closed during the shutdown: recorded, and still in the checkpoint
	_ = open

	again := New(dir, nil, nil)
	again.now = c.now
	month := again.Month("2026-11")
	if len(month) != 2 {
		t.Fatalf("after restart = %d sessions, want 2 (no duplicate): %+v", len(month), month)
	}
	if _, err := os.Stat(filepath.Join(dir, "open.json")); !os.IsNotExist(err) {
		t.Errorf("open.json should be gone after recovery")
	}
}

func TestDeviceName(t *testing.T) {
	cases := map[string]string{
		"WR-230SF/1.0": "Радиоприёмник WR-230SF",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36": "Chrome, Windows",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Version/18.0 Mobile Safari/604.1":    "Safari, iPhone",
		"VLC/3.0.20 LibVLC/3.0.20": "VLC",
		"VoiceCheck/1.0":           "проверка с сервера",
		"":                         "неизвестное устройство",
	}
	for ua, want := range cases {
		if got := DeviceName(ua); got != want {
			t.Errorf("DeviceName(%q) = %q, want %q", ua, got, want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	for seconds, want := range map[float64]string{40: "40 с", 720: "12 мин", 7500: "2 ч 05 мин"} {
		if got := FormatDuration(seconds); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", seconds, got, want)
		}
	}
}
