package unit

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/user/stream-audio-to-web/playlist"
)

func TestPlaylist_GetCurrentTrack(t *testing.T) {
	// Создаем временную директорию для тестов
	tmpDir, err := ioutil.TempDir("", "playlist-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Создаем тестовые аудио файлы
	testFiles := []string{
		"test1.mp3",
		"test2.mp3",
		"test3.mp3",
	}

	for _, file := range testFiles {
		filePath := filepath.Join(tmpDir, file)
		if err := ioutil.WriteFile(filePath, []byte("test data"), 0644); err != nil {
			t.Fatalf("Failed to create test file %s: %v", file, err)
		}
	}

	// Инициализируем плейлист
	pl, err := playlist.NewPlaylist(tmpDir, nil, false)
	if err != nil {
		t.Fatalf("Failed to create playlist: %v", err)
	}

	// Проверяем, что текущий трек не пустой
	track := pl.GetCurrentTrack()
	if track == nil {
		t.Fatalf("Expected current track to not be nil")
	}

	// Проверяем, что история начинается с текущего трека
	history := pl.GetHistory()
	if len(history) == 0 {
		t.Fatalf("Expected history to contain at least one item")
	}
}

func TestPlaylist_NextTrack(t *testing.T) {
	// Создаем временную директорию для тестов
	tmpDir, err := ioutil.TempDir("", "playlist-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Создаем тестовые аудио файлы
	testFiles := []string{
		"test1.mp3",
		"test2.mp3",
		"test3.mp3",
	}

	for _, file := range testFiles {
		filePath := filepath.Join(tmpDir, file)
		if err := ioutil.WriteFile(filePath, []byte("test data"), 0644); err != nil {
			t.Fatalf("Failed to create test file %s: %v", file, err)
		}
	}

	// Инициализируем плейлист
	pl, err := playlist.NewPlaylist(tmpDir, nil, false)
	if err != nil {
		t.Fatalf("Failed to create playlist: %v", err)
	}

	// Получаем текущий трек
	currentTrack := pl.GetCurrentTrack()

	// Переходим к следующему треку
	nextTrack := pl.NextTrack()

	// Проверяем, что следующий трек не пустой
	if nextTrack == nil {
		t.Fatalf("Expected next track to not be nil")
	}

	// Проверяем, что следующий трек отличается от текущего
	if nextTrack == currentTrack {
		t.Fatalf("Expected next track to be different from current track")
	}

	// Проверяем, что история обновилась
	history := pl.GetHistory()
	if len(history) < 2 {
		t.Fatalf("Expected history to contain at least two items")
	}
}

func TestPlaylist_PreviousTrack(t *testing.T) {
	// Создаем временную директорию для тестов
	tmpDir, err := ioutil.TempDir("", "playlist-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Создаем тестовые аудио файлы
	testFiles := []string{
		"test1.mp3",
		"test2.mp3",
		"test3.mp3",
	}

	for _, file := range testFiles {
		filePath := filepath.Join(tmpDir, file)
		if err := ioutil.WriteFile(filePath, []byte("test data"), 0644); err != nil {
			t.Fatalf("Failed to create test file %s: %v", file, err)
		}
	}

	// Инициализируем плейлист
	pl, err := playlist.NewPlaylist(tmpDir, nil, false)
	if err != nil {
		t.Fatalf("Failed to create playlist: %v", err)
	}

	// Запоминаем текущий трек
	currentTrack := pl.GetCurrentTrack()

	// Переходим к следующему треку
	pl.NextTrack()

	// Возвращаемся к предыдущему треку
	previousTrack := pl.PreviousTrack()

	// Проверяем, что предыдущий трек не пустой
	if previousTrack == nil {
		t.Fatalf("Expected previous track to not be nil")
	}

	// Проверяем, что предыдущий трек соответствует первоначальному
	if previousTrack != currentTrack && previousTrack.(interface{ GetPath() string }).GetPath() != currentTrack.(interface{ GetPath() string }).GetPath() {
		t.Fatalf("Expected previous track to be the same as original current track")
	}
} 