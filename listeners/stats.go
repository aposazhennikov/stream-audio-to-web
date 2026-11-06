package listeners

import (
	"fmt"
	"sort"
	"time"
)

// StationTime is how long a listener spent on one station.
type StationTime struct {
	Station string
	Seconds float64
}

// ListenerSummary is one listener's month: who (address and device), where from, how long,
// on which stations, and every session.
type ListenerSummary struct {
	IP       string
	Device   string
	Country  string
	City     string
	Service  bool
	Seconds  float64
	First    time.Time
	Last     time.Time
	Stations []StationTime // longest first
	Sessions []Session     // newest first
}

// Summarize groups sessions by listener (address and device), longest listening first.
func Summarize(sessions []Session) []ListenerSummary {
	byKey := map[string]*ListenerSummary{}
	stationSeconds := map[string]map[string]float64{}
	for _, s := range sessions {
		key := s.IP + "|" + s.Device
		sum, ok := byKey[key]
		if !ok {
			sum = &ListenerSummary{IP: s.IP, Device: s.Device, Service: s.Service, First: s.Start, Last: s.End}
			byKey[key] = sum
			stationSeconds[key] = map[string]float64{}
		}
		if s.Country != "" {
			sum.Country, sum.City = s.Country, s.City
		}
		sum.Seconds += s.Seconds
		if s.Start.Before(sum.First) {
			sum.First = s.Start
		}
		if s.End.After(sum.Last) {
			sum.Last = s.End
		}
		stationSeconds[key][s.Station] += s.Seconds
		sum.Sessions = append(sum.Sessions, s)
	}
	out := make([]ListenerSummary, 0, len(byKey))
	for key, sum := range byKey {
		for station, seconds := range stationSeconds[key] {
			sum.Stations = append(sum.Stations, StationTime{Station: station, Seconds: seconds})
		}
		sort.Slice(sum.Stations, func(i, j int) bool { return sum.Stations[i].Seconds > sum.Stations[j].Seconds })
		sort.Slice(sum.Sessions, func(i, j int) bool { return sum.Sessions[i].Start.After(sum.Sessions[j].Start) })
		out = append(out, *sum)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return !out[i].Service
		}
		return out[i].Seconds > out[j].Seconds
	})
	return out
}

// FormatDuration says a duration the way the status page shows it: "2 ч 05 мин", "12 мин",
// "40 с".
func FormatDuration(seconds float64) string {
	d := time.Duration(seconds) * time.Second
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%d ч %02d мин", h, m)
	case m > 0:
		return fmt.Sprintf("%d мин", m)
	default:
		return fmt.Sprintf("%d с", int(d.Seconds()))
	}
}
