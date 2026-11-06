package http

import (
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/aposazhennikov/stream-audio-to-web/listeners"
	"github.com/aposazhennikov/stream-audio-to-web/version"
)

// moscow is the family's time zone (no daylight saving).
var moscow = time.FixedZone("MSK", 3*60*60)

var monthPattern = regexp.MustCompile(`^\d{4}-\d{2}$`)

// stationTitle names a station the way the listener's receiver has it: "4 · Политика".
func (s *Server) stationTitle(route string) string {
	if label, ok := s.stationLabel(route); ok && label.Name != "" {
		if label.Preset > 0 {
			return strconv.Itoa(label.Preset) + " · " + label.Name
		}
		return label.Name
	}
	return route
}

// liveListener is one open connection as the status page shows it.
type liveListener struct {
	IP       string `json:"ip"`
	Device   string `json:"device"`
	Place    string `json:"place"`
	Since    string `json:"since"`
	Duration string `json:"duration"`
	Via      string `json:"via,omitempty"` // the button URL, when the listener came through one
	Service  bool   `json:"service"`
}

// liveListenersHandler returns who listens now, by station: GET /api/listeners.
func (s *Server) liveListenersHandler(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	byStation := map[string][]liveListener{}
	for _, sess := range s.getListeners().Live() {
		item := liveListener{
			IP:       sess.IP,
			Device:   sess.Device,
			Place:    place(sess.Country, sess.City),
			Since:    sess.Start.In(moscow).Format("15:04"),
			Duration: listeners.FormatDuration(sess.Seconds),
			Service:  sess.Service,
		}
		if sess.Route != sess.Station {
			item.Via = s.stationTitle(sess.Route)
		}
		byStation[sess.Station] = append(byStation[sess.Station], item)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(byStation)
}

func place(country, city string) string {
	switch {
	case country != "" && city != "":
		return city + ", " + country
	case country != "":
		return country
	}
	return ""
}

// listeningStatsHandler shows the month's listening by listener: GET /listening-stats?month=2026-11.
func (s *Server) listeningStatsHandler(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(r) {
		s.redirectToLogin(w, r)
		return
	}
	tracker := s.getListeners()
	months := tracker.Months()
	month := r.URL.Query().Get("month")
	if !monthPattern.MatchString(month) && len(months) > 0 {
		month = months[0]
	}
	showService := r.URL.Query().Get("service") == "1"

	type stationRow struct {
		Title    string
		Duration string
		Percent  float64
	}
	type sessionRow struct {
		Date     string
		Time     string
		Station  string
		Via      string
		Duration string
	}
	type listenerCard struct {
		IP       string
		Device   string
		Place    string
		Service  bool
		Total    string
		Count    int
		First    string
		Last     string
		Stations []stationRow
		Sessions []sessionRow
	}

	var cards []listenerCard
	var totalSeconds float64
	for _, sum := range listeners.Summarize(tracker.Month(month)) {
		if sum.Service && !showService {
			continue
		}
		card := listenerCard{
			IP:      sum.IP,
			Device:  sum.Device,
			Place:   place(sum.Country, sum.City),
			Service: sum.Service,
			Total:   listeners.FormatDuration(sum.Seconds),
			Count:   len(sum.Sessions),
			First:   sum.First.In(moscow).Format("02.01 15:04"),
			Last:    sum.Last.In(moscow).Format("02.01 15:04"),
		}
		for _, st := range sum.Stations {
			percent := 0.0
			if sum.Seconds > 0 {
				percent = st.Seconds / sum.Seconds * 100
			}
			card.Stations = append(card.Stations, stationRow{Title: s.stationTitle(st.Station), Duration: listeners.FormatDuration(st.Seconds), Percent: percent})
		}
		for _, sess := range sum.Sessions {
			row := sessionRow{
				Date:     sess.Start.In(moscow).Format("02.01"),
				Time:     sess.Start.In(moscow).Format("15:04") + "–" + sess.End.In(moscow).Format("15:04"),
				Station:  s.stationTitle(sess.Station),
				Duration: listeners.FormatDuration(sess.Seconds),
			}
			if sess.Route != sess.Station {
				row.Via = s.stationTitle(sess.Route)
			}
			card.Sessions = append(card.Sessions, row)
		}
		if !sum.Service {
			totalSeconds += sum.Seconds
		}
		cards = append(cards, card)
	}

	tmpl, err := template.ParseFiles("templates/listening_stats.html", "templates/partials/head.html")
	if err != nil {
		s.logger.Error("Unable to load listening_stats.html template", slog.String("error", err.Error()))
		http.Error(w, "Server error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, map[string]interface{}{
		"Title":       "Статистика слушателей",
		"Month":       month,
		"Months":      months,
		"Cards":       cards,
		"Total":       listeners.FormatDuration(totalSeconds),
		"ShowService": showService,
		"Version":     version.Get(),
	}); err != nil {
		s.logger.Error("Failed to execute listening stats template", slog.String("error", err.Error()))
	}
}
