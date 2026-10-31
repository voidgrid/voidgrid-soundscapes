package library

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These run against the real ffmpeg and ffprobe and are skipped where they are not installed (the
// image build stage has neither). `scripts/smoke.sh` covers the same ground inside the final image.
func needFFmpeg(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin, "not installed")
		}
	}
}

func TestFFProbeRealFiles(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()

	wav := filepath.Join(dir, "tone.wav")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-ac", "1", "-ar", "22050", wav).CombinedOutput(); err != nil {
		t.Fatalf("make tone: %v: %s", err, out)
	}
	info, err := FFProbe(context.Background(), wav)
	if err != nil {
		t.Fatal(err)
	}
	if info.Codec != "pcm_s16le" || info.SampleRate != 22050 || info.Channels != 1 || math.Abs(info.Duration-2) > 0.1 {
		t.Fatalf("unexpected info %+v", info)
	}

	garbage := filepath.Join(dir, "garbage.mp3")
	if err := os.WriteFile(garbage, []byte("this is not audio at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := FFProbe(context.Background(), garbage); err == nil {
		t.Fatal("garbage should not probe as audio")
	}

	if _, err := FFProbe(context.Background(), filepath.Join(dir, "missing.mp3")); err == nil {
		t.Fatal("a missing file should fail")
	}
}

func TestScanWithRealFFProbe(t *testing.T) {
	needFFmpeg(t)
	root := t.TempDir()
	rain := filepath.Join(root, "rain")
	if err := os.MkdirAll(rain, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=300:duration=1",
		filepath.Join(rain, "tone.flac")).CombinedOutput(); err != nil {
		t.Fatalf("make tone: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(root, "fake.mp3"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := New(root, nil, nil)
	lib.settle = 0
	if _, err := lib.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l, ok := lib.Get("rain/tone.flac"); !ok || !l.Playable() || l.Category != "rain" || l.Codec != "flac" {
		t.Fatalf("rain/tone.flac: %+v ok=%v", l, ok)
	}
	if l, ok := lib.Get("fake.mp3"); !ok || l.Playable() {
		t.Fatalf("fake.mp3 should be listed as unusable: %+v ok=%v", l, ok)
	}
}

// Tags live in different places and are capitalised differently per container (ID3 on the format,
// Vorbis comments and Opus tags on the stream), so check each against the real ffprobe.
func TestFFProbeTags(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()
	for _, c := range []struct{ file, codec string }{
		{"a.mp3", "libmp3lame"}, {"a.flac", "flac"}, {"a.ogg", "libvorbis"}, {"a.opus", "libopus"},
		{"a.m4a", "aac"}, {"a.wav", "pcm_s16le"},
	} {
		path := filepath.Join(dir, c.file)
		out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1",
			"-c:a", c.codec, "-metadata", "title=Rain on a roof", "-metadata", "artist=A. Recordist",
			"-metadata", "comment=Recorded at\x07 dawn", "-metadata", "copyright=CC0", path).CombinedOutput()
		if err != nil {
			t.Logf("cannot make %s here: %v: %s", c.file, err, out)
			continue
		}
		info, err := FFProbe(context.Background(), path)
		if err != nil {
			t.Errorf("%s: %v", c.file, err)
			continue
		}
		if info.Title != "Rain on a roof" || info.Artist != "A. Recordist" {
			t.Errorf("%s: title %q artist %q", c.file, info.Title, info.Artist)
		}
		if info.License != "CC0" && c.file != "a.wav" { // WAV has no copyright field ffmpeg keeps
			t.Errorf("%s: license %q", c.file, info.License)
		}
		if info.Comment != "" && info.Comment != "Recorded at dawn" {
			t.Errorf("%s: control character not removed from comment %q", c.file, info.Comment)
		}
	}
}

func TestTagCleaning(t *testing.T) {
	m := []map[string]string{{"TITLE": "  \x00 Loud \t rain  "}, {"title": "ignored"}}
	if got := tag(120, m, "title"); got != "Loud  rain" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("é", 500)
	if got := tag(10, []map[string]string{{"comment": long}}, "comment"); len([]rune(got)) != 11 || !strings.HasSuffix(got, "…") {
		t.Errorf("not capped on characters: %q", got)
	}
	if got := tag(10, []map[string]string{{"title": "   "}}, "title"); got != "" {
		t.Errorf("blank tag should be empty, got %q", got)
	}
	if got := tag(10, []map[string]string{{"author": "B"}, {"artist": "A"}}, "artist", "author"); got != "A" {
		t.Errorf("keys should be tried in order, got %q", got)
	}
}
