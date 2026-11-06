package listeners

import "strings"

// DeviceName turns a User-Agent into a short readable name: "Радиоприёмник WR-230SF",
// "Chrome, Windows", "VLC".
func DeviceName(ua string) string {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return "неизвестное устройство"
	}
	lower := strings.ToLower(ua)
	switch {
	case strings.Contains(ua, "WR-230SF"):
		return "Радиоприёмник WR-230SF"
	case strings.HasPrefix(ua, "VoiceCheck"):
		return "проверка с сервера"
	case strings.HasPrefix(lower, "curl/"), strings.HasPrefix(lower, "python"), strings.HasPrefix(lower, "go-http-client"):
		return "программа (" + strings.SplitN(ua, " ", 2)[0] + ")"
	}

	app := ""
	for _, p := range []struct{ marker, name string }{
		{"vlc", "VLC"},
		{"winamp", "Winamp"},
		{"foobar", "foobar2000"},
		{"lavf", "плеер на FFmpeg"},
		{"edg/", "Edge"},
		{"yabrowser", "Яндекс Браузер"},
		{"opr/", "Opera"},
		{"firefox/", "Firefox"},
		{"chrome/", "Chrome"},
		{"crios/", "Chrome"},
		{"safari/", "Safari"},
	} {
		if strings.Contains(lower, p.marker) {
			app = p.name
			break
		}
	}
	system := ""
	for _, p := range []struct{ marker, name string }{
		{"iphone", "iPhone"},
		{"ipad", "iPad"},
		{"android", "Android"},
		{"windows", "Windows"},
		{"mac os x", "Mac"},
		{"linux", "Linux"},
	} {
		if strings.Contains(lower, p.marker) {
			system = p.name
			break
		}
	}
	switch {
	case app != "" && system != "":
		return app + ", " + system
	case app != "":
		return app
	case system != "":
		return system
	}
	// Something else: its own name, without the version details.
	name := strings.SplitN(ua, " ", 2)[0]
	if len(name) > 40 {
		name = name[:40]
	}
	return name
}
