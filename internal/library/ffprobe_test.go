package library

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
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
