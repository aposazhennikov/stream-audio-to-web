package positions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSavedPositionsSurviveReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "positions.json")
	s := Load(path, nil)
	if got := s.Track("/politics"); got != "" {
		t.Fatalf("empty store returned %q", got)
	}
	s.Save("/monosov8", "2019 Пси-заклинания 5.mp3")
	s.Save("/politics", "Редакция - Редакция News： 245-я неделя.mp3")
	s.Save("/monosov8", "2019 Пси-заклинания 6.mp3")

	reloaded := Load(path, nil)
	if got := reloaded.Track("/monosov8"); got != "2019 Пси-заклинания 6.mp3" {
		t.Errorf("monosov8 = %q", got)
	}
	if got := reloaded.Track("/politics"); got != "Редакция - Редакция News： 245-я неделя.mp3" {
		t.Errorf("politics = %q", got)
	}
}

func TestBrokenOrMissingFileGivesEmptyStore(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Load(broken, nil).Track("/x"); got != "" {
		t.Errorf("broken file: %q", got)
	}
	if got := Load(filepath.Join(dir, "missing.json"), nil).Track("/x"); got != "" {
		t.Errorf("missing file: %q", got)
	}
	// A store without a path remembers nothing and writes nothing.
	s := Load("", nil)
	s.Save("/x", "a.mp3")
	if got := s.Track("/x"); got != "" {
		t.Errorf("store without a path: %q", got)
	}
}
