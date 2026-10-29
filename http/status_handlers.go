package http

import (
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	html "html"

	"github.com/aposazhennikov/stream-audio-to-web/version"
)

// statusLoginHandler displays login form page.
func (s *Server) statusLoginHandler(w http.ResponseWriter, r *http.Request) {
	// If already authenticated, redirect to status page.
	if s.checkAuth(r) {
		http.Redirect(w, r, "/status-page", http.StatusFound)
		return
	}

	// Load and render login template page.
	tmpl, tmplErr := template.ParseFiles(
		"templates/login.html",
		"templates/partials/head.html",
	)
	if tmplErr != nil {
		s.logger.Error("Unable to load login.html template", slog.String("error", tmplErr.Error()))
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	data := map[string]interface{}{
		"Title":    "Login - Audio Stream Status",
		"Redirect": r.URL.Query().Get("redirect"),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if executeErr := tmpl.Execute(w, data); executeErr != nil {
		s.logger.Error("Failed to execute template", slog.String("error", executeErr.Error()))
		return
	}
}

// statusLoginSubmitHandler handles login form submission.
func (s *Server) statusLoginSubmitHandler(w http.ResponseWriter, r *http.Request) {
	parseErr := r.ParseForm()
	if parseErr != nil {
		s.logger.Error("Unable to parse login form", slog.String("error", parseErr.Error()))
		http.Error(w, "Form processing error", http.StatusInternalServerError)
		return
	}

	// Get password from form.
	password := r.FormValue("password")

	// Check password.
	if password == s.statusPassword {
		// Create cookies for authentication.
		http.SetCookie(w, &http.Cookie{
			Name:     "status_auth",
			Value:    s.statusPassword,
			Path:     "/",
			Expires:  time.Now().Add(cookieExpireHours * time.Hour),
			HttpOnly: true,
		})

		// Check if there's a saved redirect URL from the form
		redirectURL := r.FormValue("redirect")
		if redirectURL != "" {
			s.logger.Info("Redirecting to original URL after successful login", slog.String("redirectURL", redirectURL))
			http.Redirect(w, r, redirectURL, http.StatusFound)
		} else {
			// If no redirect URL, redirect to status-page by default
			http.Redirect(w, r, "/status-page", http.StatusFound)
		}
		return
	}

	// If password is incorrect, show error message.
	tmpl, tmplErr := template.ParseFiles(
		"templates/login.html",
		"templates/partials/head.html",
	)
	if tmplErr != nil {
		s.logger.Error("Unable to load login.html template", slog.String("error", tmplErr.Error()))
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	data := map[string]interface{}{
		"Title":        "Login - Audio Stream Status",
		"ErrorMessage": "Invalid password",
		"Redirect":     r.FormValue("redirect"),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if executeErr := tmpl.Execute(w, data); executeErr != nil {
		s.logger.Error("Failed to execute template", slog.String("error", executeErr.Error()))
		return
	}
}

// checkAuth checks authentication for accessing status page.
func (s *Server) checkAuth(r *http.Request) bool {
	cookie, err := r.Cookie("status_auth")
	return err == nil && cookie.Value == s.statusPassword
}

// statusPageHandler handles requests for the status page.
func (s *Server) statusPageHandler(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(r) {
		s.redirectToLogin(w, r)
		return
	}

	s.mutex.RLock()
	defer s.mutex.RUnlock()

	type StreamInfo struct {
		Route          string
		RouteID        string
		DisplayName    string
		StartTime      string
		CurrentTrack   string
		Listeners      int
		HistoryHTML    template.HTML
		ShuffleEnabled bool
	}

	streams := make([]StreamInfo, 0, len(s.streams))
	for route, stream := range s.streams {
		s.trackMutex.RLock()
		currentTrack := s.currentTracks[route]
		s.trackMutex.RUnlock()

		// Get playlist history.
		history := s.playlists[route].GetHistory()
		var historyHTMLBuilder strings.Builder

		if len(history) == 0 {
			// Show message when no history is available
			historyHTMLBuilder.WriteString(`<div class="no-data">No tracks in history yet.</div>`)
		} else {
			historyHTMLBuilder.WriteString("<ul>")
			for _, item := range history {
				var trackName string

				// Extract track name properly based on item type
				switch track := item.(type) {
				case interface{ GetPath() string }:
					trackName = filepath.Base(track.GetPath())
				case interface{ GetNormalizedPath() string }:
					trackName = filepath.Base(track.GetNormalizedPath())
				case *struct {
					Path string
					Name string
				}:
					if track != nil {
						trackName = track.Name
						if trackName == "" {
							trackName = filepath.Base(track.Path)
						}
					}
				case string:
					trackName = filepath.Base(track)
				default:
					// Fallback to string representation but clean it up
					str := fmt.Sprintf("%v", item)
					if strings.Contains(str, "{") && strings.Contains(str, "}") {
						// This looks like a struct, try to extract filename
						trackName = "Unknown track"
					} else {
						trackName = filepath.Base(str)
					}
				}

				if trackName == "" {
					trackName = "Unknown track"
				}

				escapedTrackName := html.EscapeString(trackName)
				historyHTMLBuilder.WriteString(fmt.Sprintf(
					`<li><span>%s</span><button class="copy-button" data-track="%s"><span class="copy-icon">📋</span><span class="copy-feedback"></span></button></li>`,
					escapedTrackName,
					escapedTrackName,
				))
			}
			historyHTMLBuilder.WriteString("</ul>")
		}
		historyHTML := historyHTMLBuilder.String()

		streams = append(streams, StreamInfo{
			Route:          route,
			RouteID:        strings.TrimPrefix(route, "/"),
			DisplayName:    route,
			StartTime:      s.playlists[route].GetStartTime().Format("15:04:05"),
			CurrentTrack:   currentTrack,
			Listeners:      stream.GetClientCount(),
			HistoryHTML:    template.HTML(historyHTML),
			ShuffleEnabled: s.getShuffleStatusForRoute(route),
		})
	}

	// Sort streams by route for consistent output.
	sort.Slice(streams, func(i, j int) bool {
		return streams[i].Route < streams[j].Route
	})

	// Загружаем шаблон с правильными путями.
	tmpl, tmplErr := template.ParseFiles(
		"templates/status.html",
		"templates/partials/head.html",
	)
	if tmplErr != nil {
		s.logger.Error("Unable to load status.html template", slog.String("error", tmplErr.Error()))
		http.Error(w, "Server error: "+tmplErr.Error(), http.StatusInternalServerError)
		return
	}

	// Устанавливаем тип контента перед выполнением шаблона.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// CRITICAL DEADLOCK FIX: Don't call IsEnabled() in template execution
	// as it can cause deadlock with background monitoring
	telegramActive := false
	if s.telegramManager != nil {
		// Use a simple null check instead of IsEnabled() to avoid deadlock
		telegramActive = s.telegramFunctionalityEnabled
	}

	// Передаем данные в шаблон и обрабатываем ошибки.
	if executeErr := tmpl.Execute(w, map[string]interface{}{
		"Streams":           streams,
		"Title":             "Audio Streams Status",
		"RelayEnabled":      s.relayManager != nil,
		"RelayActive":       s.relayManager != nil && s.relayManager.IsActive(),
		"RelayClickable":    s.relayFunctionalityEnabled,
		"TelegramEnabled":   s.telegramManager != nil,
		"TelegramActive":    telegramActive,
		"TelegramClickable": s.telegramFunctionalityEnabled,
		"GlobalShuffle":     s.getGlobalShuffleStatus(),
		"Version":           version.Get(),
	}); executeErr != nil {
		s.logger.Error("Failed to execute status template", slog.String("error", executeErr.Error()))
		http.Error(w, "Server error: "+executeErr.Error(), http.StatusInternalServerError)
		return
	}
}

// notFoundHandler handles 404 not found requests.
func (s *Server) notFoundHandler(w http.ResponseWriter, r *http.Request) {
	s.logger.Info(
		"404 not found request",
		slog.String("path", r.URL.Path),
		slog.String("method", r.Method),
		slog.String("remoteAddr", r.RemoteAddr),
	)

	// Load and render 404 template page.
	tmpl, err := template.ParseFiles(
		"templates/404.html",
		"templates/partials/head.html",
	)
	if err != nil {
		s.logger.Error("Unable to load 404.html template", slog.String("error", err.Error()))
		http.Error(w, "Server error", http.StatusInternalServerError)
		return
	}

	data := map[string]interface{}{
		"Title": "404 - Page not found",
		"Path":  r.URL.Path,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	if executeErr := tmpl.Execute(w, data); executeErr != nil {
		s.logger.Error("Failed to execute template", slog.String("error", executeErr.Error()))
		return
	}
}

// redirectToLogin redirects to login page.
func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	// Пароль и значения cookie в лог не пишем.
	s.logger.Info(
		"Redirecting to login page",
		slog.String("remoteAddr", r.RemoteAddr),
		slog.String("path", r.URL.Path),
		slog.String("userAgent", r.UserAgent()),
	)

	// ИСПРАВЛЕНИЕ: Сохраняем оригинальный URL как query параметр
	// чтобы после авторизации вернуть пользователя туда, где он хотел быть
	originalURL := r.URL.Path
	if r.URL.RawQuery != "" {
		originalURL += "?" + r.URL.RawQuery
	}

	// Только сохраняем URL если это не сама страница логина
	if originalURL != "/status" && originalURL != "/status-page" {
		encodedURL := url.QueryEscape(originalURL)
		http.Redirect(w, r, "/status?redirect="+encodedURL, http.StatusFound)
	} else {
		http.Redirect(w, r, "/status", http.StatusFound)
	}
}
