package unit

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/user/stream-audio-to-web/playlist"
)

// Минимальные валидные аудиофайлы для тестирования
var (
	// Минимальный валидный MP3 файл (фрейм данных)
	minimumMP3Data = []byte{
		0xFF, 0xFB, 0x90, 0x64, // MPEG-1 Layer 3 header
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // Минимальные данные
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}

	// Минимальный валидный WAV файл (44 байта заголовок + минимальные данные)
	minimumWAVData = []byte{
		// RIFF заголовок
		0x52, 0x49, 0x46, 0x46, // "RIFF"
		0x24, 0x00, 0x00, 0x00, // Размер чанка (36 + размер данных)
		0x57, 0x41, 0x56, 0x45, // "WAVE"
		// fmt субчанк
		0x66, 0x6D, 0x74, 0x20, // "fmt "
		0x10, 0x00, 0x00, 0x00, // Размер подчанка (16 байт)
		0x01, 0x00,             // Аудио формат (1 = PCM)
		0x01, 0x00,             // Число каналов (1 = моно)
		0x44, 0xAC, 0x00, 0x00, // Частота дискретизации (44100 Гц)
		0x88, 0x58, 0x01, 0x00, // Байтрейт (44100 * 1 * 16/8)
		0x02, 0x00,             // Выравнивание блока (каналы * битность / 8)
		0x10, 0x00,             // Битность (16 бит)
		// data субчанк
		0x64, 0x61, 0x74, 0x61, // "data"
		0x00, 0x00, 0x00, 0x00, // Размер данных (0 байт)
		// Минимальные данные (1 сэмпл)
		0x00, 0x00,
	}

	// Минимальный валидный OGG файл (только заголовок без данных)
	minimumOGGData = []byte{
		// OGG заголовок
		0x4F, 0x67, 0x67, 0x53, // "OggS"
		0x00,                   // Версия
		0x02,                   // Тип заголовка
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // Гранульная позиция (0)
		0x01, 0x02, 0x03, 0x04, // Серийный номер (произвольный)
		0x00, 0x00, 0x00, 0x00, // Номер страницы
		0x01, 0x00, 0x00, 0x00, // CRC контрольная сумма
		0x01,                   // Количество сегментов
		0x1E,                   // Размер сегмента (30 байт)
		// Vorbis заголовок (упрощенный)
		0x01, 0x76, 0x6F, 0x72, 0x62, 0x69, 0x73, // "\x01vorbis"
		0x00, 0x00, 0x00, 0x00, // Версия Vorbis
		0x01,                   // Каналы (1)
		0x44, 0xAC, 0x00, 0x00, // Частота дискретизации (44100 Гц)
		0x00, 0x00, 0x00, 0x00, // Битрейт максимальный
		0x00, 0x00, 0x00, 0x00, // Битрейт номинальный
		0x00, 0x00, 0x00, 0x00, // Битрейт минимальный
		0x00                    // Blocksize
	}
)

// Создание различных типов аудиофайлов для тестирования
func createTestAudioFiles(dir string) error {
	files := []struct {
		name string
		data []byte
	}{
		{"test1.mp3", minimumMP3Data},
		{"test2.mp3", minimumMP3Data},
		{"test3.wav", minimumWAVData},
		{"test4.ogg", minimumOGGData},
		{"test5.mp3", minimumMP3Data},
	}

	for _, file := range files {
		filePath := filepath.Join(dir, file.name)
		if err := ioutil.WriteFile(filePath, file.data, 0644); err != nil {
			return err
		}
	}
	return nil
}

func TestPlaylist_GetCurrentTrack(t *testing.T) {
	// Создаем временную директорию для тестов
	tmpDir, err := ioutil.TempDir("", "playlist-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Создаем разнообразные аудиофайлы для тестирования
	if err := createTestAudioFiles(tmpDir); err != nil {
		t.Fatalf("Failed to create test audio files: %v", err)
	}

	// Инициализируем плейлист
	pl, err := playlist.NewPlaylist(tmpDir, nil, false)
	if err != nil {
		t.Fatalf("Failed to create playlist: %v", err)
	}

	// Даем время на инициализацию плейлиста
	time.Sleep(200 * time.Millisecond)

	// Проверяем, что текущий трек не пустой
	track := pl.GetCurrentTrack()
	if track == nil {
		t.Fatalf("Expected current track to not be nil")
	}

	// Проверяем, что трек имеет путь
	if track, ok := track.(interface{ GetPath() string }); !ok || track.GetPath() == "" {
		t.Fatalf("Expected track to have a valid path")
	}

	// Проверяем, что история начинается с текущего трека
	history := pl.GetHistory()
	if len(history) == 0 {
		t.Fatalf("Expected history to contain at least one item")
	}

	t.Logf("Current track: %v", track)
	t.Logf("History: %v", history)
}

func TestPlaylist_NextTrack(t *testing.T) {
	// Создаем временную директорию для тестов
	tmpDir, err := ioutil.TempDir("", "playlist-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Создаем разнообразные аудиофайлы для тестирования
	if err := createTestAudioFiles(tmpDir); err != nil {
		t.Fatalf("Failed to create test audio files: %v", err)
	}

	// Инициализируем плейлист
	pl, err := playlist.NewPlaylist(tmpDir, nil, false)
	if err != nil {
		t.Fatalf("Failed to create playlist: %v", err)
	}

	// Даем время на инициализацию плейлиста
	time.Sleep(200 * time.Millisecond)

	// Получаем текущий трек
	currentTrack := pl.GetCurrentTrack()
	if currentTrack == nil {
		t.Fatalf("Expected current track to not be nil before switching")
	}

	// Логируем текущее состояние для отладки
	t.Logf("Initial track: %v", currentTrack)
	t.Logf("Initial history: %v", pl.GetHistory())

	// Переходим к следующему треку
	nextTrack := pl.NextTrack()

	// Даем время на обновление истории
	time.Sleep(200 * time.Millisecond)

	// Проверяем, что следующий трек не пустой
	if nextTrack == nil {
		t.Fatalf("Expected next track to not be nil")
	}

	// Проверяем, что трек имеет путь
	if track, ok := nextTrack.(interface{ GetPath() string }); !ok || track.GetPath() == "" {
		t.Fatalf("Expected next track to have a valid path")
	}

	// Проверяем, что следующий трек отличается от текущего
	if nextTrack == currentTrack {
		if currentTrackPath, ok := currentTrack.(interface{ GetPath() string }); ok {
			if nextTrackPath, ok := nextTrack.(interface{ GetPath() string }); ok {
				if currentTrackPath.GetPath() == nextTrackPath.GetPath() {
					t.Fatalf("Expected next track to be different from current track. Both have path: %s", currentTrackPath.GetPath())
				}
			}
		}
	}

	// Проверяем, что история обновилась
	history := pl.GetHistory()
	if len(history) < 2 {
		t.Fatalf("Expected history to contain at least two items, but got %d", len(history))
	}

	// Логируем результаты для отладки
	t.Logf("After next track operation:")
	t.Logf("Next track: %v", nextTrack)
	t.Logf("Updated history: %v", history)
}

func TestPlaylist_PreviousTrack(t *testing.T) {
	// Создаем временную директорию для тестов
	tmpDir, err := ioutil.TempDir("", "playlist-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Создаем разнообразные аудиофайлы для тестирования
	if err := createTestAudioFiles(tmpDir); err != nil {
		t.Fatalf("Failed to create test audio files: %v", err)
	}

	// Инициализируем плейлист
	pl, err := playlist.NewPlaylist(tmpDir, nil, false)
	if err != nil {
		t.Fatalf("Failed to create playlist: %v", err)
	}

	// Даем время на инициализацию плейлиста
	time.Sleep(200 * time.Millisecond)

	// Запоминаем текущий трек
	currentTrack := pl.GetCurrentTrack()
	currentTrackPath := ""
	if track, ok := currentTrack.(interface{ GetPath() string }); ok {
		currentTrackPath = track.GetPath()
		t.Logf("Initial track path: %s", currentTrackPath)
	}

	// Переходим к следующему треку
	pl.NextTrack()

	// Даем время на обновление плейлиста
	time.Sleep(200 * time.Millisecond)

	// Проверяем, что трек действительно изменился
	midTrack := pl.GetCurrentTrack()
	midTrackPath := ""
	if track, ok := midTrack.(interface{ GetPath() string }); ok {
		midTrackPath = track.GetPath()
		t.Logf("Middle track path (after next): %s", midTrackPath)
	}

	// Возвращаемся к предыдущему треку
	previousTrack := pl.PreviousTrack()

	// Даем время на обновление плейлиста
	time.Sleep(200 * time.Millisecond)

	// Проверяем, что предыдущий трек не пустой
	if previousTrack == nil {
		t.Fatalf("Expected previous track to not be nil")
	}

	// Получаем путь предыдущего трека
	previousTrackPath := ""
	if track, ok := previousTrack.(interface{ GetPath() string }); ok {
		previousTrackPath = track.GetPath()
		t.Logf("Previous track path (after prev): %s", previousTrackPath)
	}

	// Проверяем, что предыдущий трек соответствует первоначальному
	if previousTrackPath != currentTrackPath {
		t.Fatalf("Expected previous track path (%s) to be the same as original track path (%s)", 
			previousTrackPath, currentTrackPath)
	}

	// Логируем состояние истории
	t.Logf("Final history: %v", pl.GetHistory())
}

func TestPlaylist_ShuffleMode(t *testing.T) {
	// Создаем временную директорию для тестов
	tmpDir, err := ioutil.TempDir("", "playlist-test-shuffle")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Создаем больше аудиофайлов для лучшего тестирования перемешивания
	files := []struct {
		name string
		data []byte
	}{
		{"01_track.mp3", minimumMP3Data},
		{"02_track.mp3", minimumMP3Data},
		{"03_track.mp3", minimumMP3Data},
		{"04_track.mp3", minimumMP3Data},
		{"05_track.mp3", minimumMP3Data},
		{"06_track.mp3", minimumMP3Data},
		{"07_track.mp3", minimumMP3Data},
		{"08_track.mp3", minimumMP3Data},
		{"09_track.mp3", minimumMP3Data},
		{"10_track.mp3", minimumMP3Data},
	}

	// Создаем файлы
	for _, file := range files {
		filePath := filepath.Join(tmpDir, file.name)
		if err := ioutil.WriteFile(filePath, file.data, 0644); err != nil {
			t.Fatalf("Failed to create test file %s: %v", file.name, err)
		}
	}

	// Инициализируем плейлист без перемешивания для сохранения исходного порядка
	regularPl, err := playlist.NewPlaylist(tmpDir, nil, false)
	if err != nil {
		t.Fatalf("Failed to create regular playlist: %v", err)
	}

	// Даем время на инициализацию
	time.Sleep(200 * time.Millisecond)

	// Получаем треки из обычного плейлиста (должны быть в алфавитном порядке)
	var regularTracks []string
	currentTrack := regularPl.GetCurrentTrack()
	if track, ok := currentTrack.(interface{ GetPath() string }); ok {
		regularTracks = append(regularTracks, filepath.Base(track.GetPath()))
	}

	// Перебираем треки в порядке следования
	for i := 0; i < 9; i++ {
		nextTrack := regularPl.NextTrack()
		time.Sleep(50 * time.Millisecond)
		if track, ok := nextTrack.(interface{ GetPath() string }); ok {
			regularTracks = append(regularTracks, filepath.Base(track.GetPath()))
		}
	}

	// Инициализируем плейлист с перемешиванием
	shufflePl, err := playlist.NewPlaylist(tmpDir, nil, true)
	if err != nil {
		t.Fatalf("Failed to create shuffle playlist: %v", err)
	}

	// Даем время на инициализацию
	time.Sleep(200 * time.Millisecond)

	// Получаем треки из перемешанного плейлиста
	var shuffleTracks []string
	currentTrack = shufflePl.GetCurrentTrack()
	if track, ok := currentTrack.(interface{ GetPath() string }); ok {
		shuffleTracks = append(shuffleTracks, filepath.Base(track.GetPath()))
	}

	// Перебираем треки в порядке следования
	for i := 0; i < 9; i++ {
		nextTrack := shufflePl.NextTrack()
		time.Sleep(50 * time.Millisecond)
		if track, ok := nextTrack.(interface{ GetPath() string }); ok {
			shuffleTracks = append(shuffleTracks, filepath.Base(track.GetPath()))
		}
	}

	// Выводим порядок треков для отладки
	t.Logf("Regular playlist tracks order: %v", regularTracks)
	t.Logf("Shuffled playlist tracks order: %v", shuffleTracks)

	// Проверяем, что в обоих плейлистах одинаковое количество треков
	if len(regularTracks) != len(shuffleTracks) {
		t.Fatalf("Expected both playlists to have the same number of tracks, but got %d and %d", 
			len(regularTracks), len(shuffleTracks))
	}

	// Проверяем, что порядок треков отличается в режиме SHUFFLE
	different := false
	for i, track := range regularTracks {
		if i < len(shuffleTracks) && track != shuffleTracks[i] {
			different = true
			break
		}
	}

	if !different {
		t.Errorf("Expected shuffled playlist to have different order than regular playlist, but they appear identical")
	}

	// Проверяем, что все файлы присутствуют в обоих плейлистах
	regularMap := make(map[string]bool)
	shuffleMap := make(map[string]bool)

	for _, track := range regularTracks {
		regularMap[track] = true
	}

	for _, track := range shuffleTracks {
		shuffleMap[track] = true
	}

	// Все файлы из обычного плейлиста должны быть в перемешанном
	for track := range regularMap {
		if !shuffleMap[track] {
			t.Errorf("Track %s is missing in shuffled playlist", track)
		}
	}

	// Все файлы из перемешанного плейлиста должны быть в обычном
	for track := range shuffleMap {
		if !regularMap[track] {
			t.Errorf("Track %s is missing in regular playlist", track)
		}
	}
} 