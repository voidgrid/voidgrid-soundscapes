package engine

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-soundscapes/internal/library"
)

// goertzel returns the amplitude of the freq component in pcm.
func goertzel(pcm []int16, freq float64) float64 {
	w := 2 * math.Pi * freq / SampleRate
	coeff := 2 * math.Cos(w)
	var s1, s2 float64
	for _, v := range pcm {
		s0 := float64(v) + coeff*s1 - s2
		s2, s1 = s1, s0
	}
	power := s1*s1 + s2*s2 - coeff*s1*s2
	return 2 * math.Sqrt(math.Max(power, 0)) / float64(len(pcm))
}

type format struct {
	name string
	file string
	args []string // ffmpeg output args
	// video adds a video track to the input
	video bool
	rate  int
	// core formats must be creatable and must pass; the others are skipped if this ffmpeg cannot make them.
	core bool
}

func formats() []format {
	return []format{
		{core: true, name: "mp3", file: "a.mp3", args: []string{"-c:a", "libmp3lame"}, rate: 44100},
		{name: "mp3 24kHz", file: "a24.mp3", args: []string{"-c:a", "libmp3lame"}, rate: 24000},
		{core: true, name: "flac", file: "a.flac", args: []string{"-c:a", "flac"}, rate: 44100},
		{core: true, name: "wav", file: "a.wav", args: []string{"-c:a", "pcm_s16le"}, rate: 48000},
		{name: "wav 8-bit mono 8kHz", file: "a8.wav", args: []string{"-c:a", "pcm_u8"}, rate: 8000},
		{core: true, name: "opus", file: "a.opus", args: []string{"-c:a", "libopus"}, rate: 48000},
		{name: "ogg vorbis", file: "a.ogg", args: []string{"-c:a", "libvorbis"}, rate: 44100},
		{core: true, name: "webm audio only", file: "a.webm", args: []string{"-c:a", "libopus"}, rate: 48000},
		{name: "webm with video", file: "v.webm", args: []string{"-c:v", "libvpx-vp9", "-b:v", "50k", "-c:a", "libopus"}, video: true, rate: 48000},
		{core: true, name: "mp4 audio only", file: "a.mp4", args: []string{"-c:a", "aac"}, rate: 44100},
		{name: "mp4 with video", file: "v.mp4", args: []string{"-c:v", "mpeg4", "-c:a", "aac"}, video: true, rate: 44100},
		{name: "m4a", file: "a.m4a", args: []string{"-c:a", "aac"}, rate: 44100},
	}
}

func haveFFmpeg(t *testing.T) {
	t.Helper()
	for _, b := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skip(b, "not installed")
		}
	}
}

// makeFile creates the test file and reports whether it could.
func makeFile(t *testing.T, dir string, f format, freq float64) (string, bool) {
	t.Helper()
	path := filepath.Join(dir, f.file)
	args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=" + ftoa(freq) + ":duration=3:sample_rate=" + itoa(f.rate)}
	if f.video {
		args = append(args, "-f", "lavfi", "-i", "testsrc=duration=3:size=64x64:rate=5")
	}
	args = append(args, "-ac", "1")
	args = append(args, f.args...)
	args = append(args, path)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		if f.core {
			t.Fatalf("this ffmpeg cannot create %s, a required format: %v: %s", f.name, err, out)
		}
		t.Logf("SKIPPED %s: this ffmpeg cannot create it (%v)", f.name, err)
		return path, false
	}
	return path, true
}

func ftoa(f float64) string { return itoa(int(f)) }
func itoa(i int) string {
	b := []byte{}
	if i == 0 {
		return "0"
	}
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestEveryFormatProbesDecodesAndLoops makes a 440 Hz tone in each format and follows it through the same
// calls the server makes: scan (ffprobe), decode (ffmpeg) and BuildLoop. A tone that arrives at 440 Hz,
// at a sensible level, in every one proves the format end to end, including resampling to 44.1 kHz.
func TestEveryFormatProbesDecodesAndLoops(t *testing.T) {
	haveFFmpeg(t)
	dir := t.TempDir()
	var made []format
	for _, f := range formats() {
		if _, ok := makeFile(t, dir, f, 440); ok {
			made = append(made, f)
		}
	}
	lib := library.New(dir, nil, nil)
	// A scan ignores files changed in the last two seconds, so scan until they have all settled.
	waitSettled(t, lib, len(made))

	for _, f := range made {
		t.Run(f.name, func(t *testing.T) {
			l, ok := lib.Get(f.file)
			if !ok {
				t.Fatalf("%s was not listed by the library", f.file)
			}
			if !l.Playable() {
				t.Fatalf("not playable: %s", l.Error)
			}
			if l.DurationSec < 2.5 || l.DurationSec > 3.6 {
				t.Errorf("duration %.2f, want about 3 s", l.DurationSec)
			}

			pcm, err := FFmpegDecode(context.Background(), l.Path, l.DurationSec)
			if err != nil {
				t.Fatal(err)
			}
			if secs := float64(len(pcm)) / SampleRate; secs < 2.5 || secs > 3.6 {
				t.Errorf("decoded %.2f s, want about 3 s (wrong resampling?)", secs)
			}
			loop, err := BuildLoop(pcm, LoopOptions{Crossfade: 0, SilenceThreshold: 8})
			if err != nil {
				t.Fatal(err)
			}
			mid := loop[len(loop)/4 : len(loop)*3/4]
			at := goertzel(mid, 440)
			off := goertzel(mid, 1000)
			if at < 1500 { // sine=... defaults to amplitude 0.125 of 32767, about 4000
				t.Errorf("440 Hz amplitude %.0f is too low: tone lost or badly attenuated", at)
			}
			if off > at/20 {
				t.Errorf("1 kHz amplitude %.0f against %.0f at 440 Hz: the signal is distorted", off, at)
			}
		})
	}
}

func waitSettled(t *testing.T, lib *library.Library, want int) {
	t.Helper()
	for i := 0; i < 40; i++ {
		_, _ = lib.Scan(context.Background())
		if len(lib.Layers()) >= want {
			return
		}
		sleepMs(150)
	}
	t.Fatalf("library found %d of %d files", len(lib.Layers()), want)
}

func TestVideoOnlyFileIsUnusable(t *testing.T) {
	haveFFmpeg(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "silent-film.mp4")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=duration=2:size=64x64:rate=5",
		"-c:v", "mpeg4", path).CombinedOutput(); err != nil {
		t.Skipf("cannot create video-only file: %v: %s", err, out)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err := library.FFProbe(context.Background(), path); err == nil {
		t.Fatal("a video with no audio track must be rejected by the probe")
	}
}

func sleepMs(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }
