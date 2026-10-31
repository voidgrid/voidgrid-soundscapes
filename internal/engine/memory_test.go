package engine

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestLoadAllocation guards the memory cost of loading a layer: decoding and building the loop should
// allocate little more than the loop itself, not a raw byte copy, a sample copy and a loop copy on top.
func TestLoadAllocation(t *testing.T) {
	haveFFmpeg(t)
	const seconds = 60
	path := filepath.Join(t.TempDir(), "noise.flac")
	out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "anoisesrc=d=60:c=pink:r=44100:a=0.2",
		"-c:a", "flac", path).CombinedOutput()
	if err != nil {
		t.Skipf("cannot make a test file: %v: %s", err, out)
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	pcm, err := FFmpegDecode(context.Background(), path, seconds)
	if err != nil {
		t.Fatal(err)
	}
	loop, err := BuildLoopConsuming(pcm, DefaultLoopOptions)
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)

	loopBytes := float64(len(loop) * 2)
	allocated := float64(after.TotalAlloc - before.TotalAlloc)
	ratio := allocated / loopBytes
	t.Logf("loop %.1f MB, allocated %.1f MB while loading: %.2fx", loopBytes/1e6, allocated/1e6, ratio)
	if ratio > 1.5 {
		t.Errorf("loading allocated %.2fx the loop size, want at most 1.5x", ratio)
	}
}
