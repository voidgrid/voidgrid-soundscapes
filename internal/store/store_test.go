package store

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-soundscapes/internal/engine"
)

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

var testLog = slog.New(slog.NewTextHandler(nopWriter{}, nil))

func open(t *testing.T, dir string, delay time.Duration) *Store {
	t.Helper()
	s, err := Open(dir, delay, testLog)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sample(id string) Soundscape {
	return Soundscape{ID: id, Name: "Name of " + id, Layers: []engine.LayerSpec{{ID: "rain/heavy.mp3", Level: 0.25, Enabled: true}, {ID: "fan.wav", Level: 1}}}
}

func TestMissingFileIsAnEmptyStoreAndIsNotCreatedUntilNeeded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not", "yet")
	s := open(t, dir, time.Hour)
	if len(s.List()) != 0 {
		t.Fatal("a new store is not empty")
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Fatalf("opening wrote a file: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Fatalf("closing an untouched store wrote a file: %v", err)
	}
}

func TestSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, time.Hour) // the delay never fires: Close must write
	s.Put(sample("night"))
	s.Put(sample("day"))
	s.Put(Soundscape{ID: "empty", Name: "Empty"})
	if !s.Delete("day") || s.Delete("day") {
		t.Fatal("Delete should report whether the soundscape existed")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	again := open(t, dir, time.Hour)
	got := again.List()
	if len(got) != 2 || got[0].ID != "empty" || got[1].ID != "night" {
		t.Fatalf("after restart: %+v", got)
	}
	n, _ := again.Get("night")
	if n.Name != "Name of night" || len(n.Layers) != 2 || n.Layers[0] != (engine.LayerSpec{ID: "rain/heavy.mp3", Level: 0.25, Enabled: true}) {
		t.Fatalf("night = %+v", n)
	}
	if e, _ := again.Get("empty"); e.Layers == nil {
		t.Error("a soundscape without layers must come back with an empty list, not nil")
	}
}

func TestChangesAreWrittenAfterTheDelayAndCoalesced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	s := open(t, dir, 80*time.Millisecond)
	defer s.Close()

	s.Put(sample("a"))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the change was written immediately instead of after the delay")
	}
	for i := 0; i < 50; i++ { // a slider being dragged
		sc := sample("a")
		sc.Layers[0].Level = float64(i) / 100
		s.Put(sc)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil {
			if !strings.Contains(string(b), `"level": 0.49`) {
				t.Fatalf("the file holds an older state than the last change:\n%s", b)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the change was never written")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWritesLeaveNoTempFilesAndTheFileIsReadable(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, time.Hour)
	for i := 0; i < 5; i++ {
		s.Put(sample("a"))
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != FileName {
		t.Fatalf("directory holds %v, want only %s", entries, FileName)
	}
	if fi, _ := os.Stat(filepath.Join(dir, FileName)); fi.Mode().Perm() != 0o644 {
		t.Errorf("file mode %v, want 0644", fi.Mode().Perm())
	}
}

func TestConcurrentUseIsSafe(t *testing.T) {
	s := open(t, t.TempDir(), time.Millisecond)
	defer s.Close()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				id := string(rune('a' + g))
				s.Put(sample(id))
				s.List()
				s.Get(id)
				s.NewID("x")
				if i%10 == 0 {
					s.Delete(id)
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestReturnedValuesAreCopies(t *testing.T) {
	s := open(t, t.TempDir(), time.Hour)
	in := sample("a")
	s.Put(in)
	in.Layers[0].Level = 0.99 // the caller keeps using its slice
	got, _ := s.Get("a")
	got.Layers[1].Level = 0.77
	again, _ := s.Get("a")
	if again.Layers[0].Level != 0.25 || again.Layers[1].Level != 1 {
		t.Fatalf("the store shares memory with its callers: %+v", again.Layers)
	}
}

func TestDamagedOrUnsupportedFilesAreRefusedAndLeftUntouched(t *testing.T) {
	cases := map[string]string{
		"not json":       `{"version":1,"soundscapes":[`,
		"newer version":  `{"version":2,"soundscapes":[]}`,
		"no version":     `{"soundscapes":[]}`,
		"bad id":         `{"version":1,"soundscapes":[{"id":"../x","name":"x","layers":[]}]}`,
		"duplicate id":   `{"version":1,"soundscapes":[{"id":"a","name":"x","layers":[]},{"id":"a","name":"y","layers":[]}]}`,
		"wrong type":     `{"version":1,"soundscapes":[{"id":"a","name":"x","layers":[{"id":"f","level":"loud"}]}]}`,
		"empty document": ``,
	}
	for name, content := range cases {
		dir := t.TempDir()
		path := filepath.Join(dir, FileName)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Open(dir, time.Hour, testLog)
		if err == nil {
			t.Errorf("%s: Open succeeded", name)
			continue
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("%s: the error should name the file: %v", name, err)
		}
		if b, _ := os.ReadFile(path); string(b) != content {
			t.Errorf("%s: the file was modified", name)
		}
	}
}

func TestUnwritableDirectoryFailsAtOpen(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	if _, err := Open(dir, time.Hour, testLog); err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("Open = %v, want a 'not writable' error", err)
	}
	// An unusable path (a file where the directory should be) fails too.
	f := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(f, nil, 0o644)
	if _, err := Open(filepath.Join(f, "sub"), time.Hour, testLog); err == nil {
		t.Fatal("Open succeeded with a file in the way")
	}
}

func TestNewIDAndSlug(t *testing.T) {
	s := open(t, t.TempDir(), time.Hour)
	for in, want := range map[string]string{
		"Night Rain":        "night-rain",
		"  --Fan!!  &  Sea": "fan-sea",
		"Ünï":               "n",
		"日本語":               "soundscape",
		"":                  "soundscape",
		"x.y_z":             "x-y-z",
	} {
		if got := s.NewID(in); got != want {
			t.Errorf("NewID(%q) = %q, want %q", in, got, want)
		}
	}
	long := s.NewID(strings.Repeat("a", 200))
	if len(long) != maxIDLen || !engine.ValidID(long) {
		t.Errorf("long name gave %q", long)
	}
	s.Put(Soundscape{ID: long, Name: "x"})
	next := s.NewID(strings.Repeat("a", 200))
	if next == long || len(next) > maxIDLen || !engine.ValidID(next) || !strings.HasSuffix(next, "-2") {
		t.Errorf("collision on a maximum-length ID gave %q", next)
	}
	s.Put(Soundscape{ID: "night", Name: "x"})
	s.Put(Soundscape{ID: "night-2", Name: "x"})
	if got := s.NewID("Night"); got != "night-3" {
		t.Errorf("got %q, want night-3", got)
	}
	for _, in := range []string{"Night Rain", "!!!", "ÄÖ", strings.Repeat("-a", 90)} {
		if id := s.NewID(in); !engine.ValidID(id) {
			t.Errorf("NewID(%q) = %q is not a valid ID", in, id)
		}
	}
}
