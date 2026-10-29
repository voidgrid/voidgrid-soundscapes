package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-soundscapes/internal/engine"
	"github.com/voidgrid/voidgrid-soundscapes/internal/library"
	"github.com/voidgrid/voidgrid-soundscapes/internal/playback"
	"github.com/voidgrid/voidgrid-soundscapes/internal/store"
)

func goertzel(pcm []int16, freq float64) float64 {
	w := 2 * math.Pi * freq / engine.SampleRate
	coeff := 2 * math.Cos(w)
	var s1, s2 float64
	for _, v := range pcm {
		s0 := float64(v) + coeff*s1 - s2
		s2, s1 = s1, s0
	}
	return 2 * math.Sqrt(math.Max(s1*s1+s2*s2-coeff*s1*s2, 0)) / float64(len(pcm))
}

// capture reads a stream into a buffer until stop is closed, noting when the first byte arrived.
type capture struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	first time.Time
}

func (c *capture) run(body io.Reader) {
	b := make([]byte, 4096)
	for {
		n, err := body.Read(b)
		if n > 0 {
			c.mu.Lock()
			if c.first.IsZero() {
				c.first = time.Now()
			}
			c.buf.Write(b[:n])
			c.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (c *capture) bytes() ([]byte, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf.Bytes()...), c.first
}

func decodeMP3(t *testing.T, mp3 []byte) []int16 {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-v", "error", "-f", "mp3", "-i", "-", "-f", "s16le", "-ac", "1", "-ar", "44100", "-")
	cmd.Stdin = bytes.NewReader(mp3)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the captured stream is not valid MP3: %v", err)
	}
	pcm := make([]int16, len(out)/2)
	for i := range pcm {
		pcm[i] = int16(uint16(out[2*i]) | uint16(out[2*i+1])<<8)
	}
	return pcm
}

// TestRealStreamLiveMixing runs the whole path with real files and real ffmpeg: library scan, decode,
// loop, mix, MP3 encode, HTTP stream. It then switches one tone off and another on while a client is
// connected, decodes what the client received, and checks the change arrived quickly and cleanly.
func TestRealStreamLiveMixing(t *testing.T) {
	for _, b := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skip(b, "not installed")
		}
	}
	dir := t.TempDir()
	for name, freq := range map[string]string{"low.wav": "440", "high.wav": "1000"} {
		p := filepath.Join(dir, name)
		if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency="+freq+":duration=2",
			"-ac", "1", "-ar", "44100", p).CombinedOutput(); err != nil {
			t.Fatalf("make %s: %v: %s", name, err, out)
		}
		old := time.Now().Add(-time.Minute)
		_ = os.Chtimes(p, old, old)
	}
	lib := library.New(dir, nil, nil)
	if _, err := lib.Scan(context.Background()); err != nil || len(lib.Layers()) != 2 {
		t.Fatalf("scan: %v, %d layers", err, len(lib.Layers()))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng := engine.New(ctx, lib, engine.Options{Loop: engine.LoopOptions{Crossfade: 500 * time.Millisecond, SilenceThreshold: 8}},
		slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	st, err := store.Open(t.TempDir(), 0, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pb, _ := playback.New(st, nil, playback.Options{})
	ts := httptest.NewServer(New(lib, eng, st, pb))
	defer ts.Close()

	type tone struct {
		level float64
		on    bool
	}
	put := func(low, high tone) time.Time {
		body := fmt.Sprintf(`{"layers":[{"id":"low.wav","level":%g,"enabled":%t},{"id":"high.wav","level":%g,"enabled":%t}]}`,
			low.level, low.on, high.level, high.on)
		at := time.Now()
		if resp, b := do(t, "PUT", ts.URL+"/api/soundscapes/tones", body); resp.StatusCode != 200 {
			t.Fatalf("PUT: %d %s", resp.StatusCode, b)
		}
		return at
	}
	// The high tone is switched on at level 0 first, so it is already decoded when it is brought in: this
	// test is about how fast a level change is heard, not how long a file takes to load (a layer that has
	// not been loaded is silent until it is, and the unit tests cover that).
	put(tone{1, true}, tone{0, true})
	deadline := time.Now().Add(15 * time.Second)
	for {
		st, _ := eng.Status("tones")
		if st.Layers[0].State == engine.StateReady && st.Layers[1].State == engine.StateReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("layers never loaded: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}

	resp, err := http.Get(ts.URL + "/stream/tones")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var cap capture
	go cap.run(resp.Body)

	time.Sleep(2 * time.Second)
	// A second client joining mid-stream must start on a frame boundary.
	late, err := http.Get(ts.URL + "/stream/tones")
	if err != nil {
		t.Fatal(err)
	}
	lateHead := make([]byte, 8)
	if _, err := io.ReadFull(late.Body, lateHead); err != nil {
		t.Fatal(err)
	}
	late.Body.Close()
	if lateHead[0] != 0xFF || lateHead[1]&0xE0 != 0xE0 {
		t.Fatalf("a late joiner got % x, not the start of an MP3 frame", lateHead)
	}

	switchedAt := put(tone{1, false}, tone{1, true})
	time.Sleep(3 * time.Second)
	data, first := cap.bytes()
	resp.Body.Close()

	if len(data) < 4 || data[0] != 0xFF || data[1]&0xE0 != 0xE0 {
		t.Fatalf("the stream does not start with an MP3 frame header: % x", data[:min(8, len(data))])
	}
	if bytes.Contains(data[:min(len(data), 64)], []byte("ID3")) {
		t.Fatal("the stream starts with an ID3 tag")
	}

	pcm := decodeMP3(t, data)
	const win = engine.SampleRate / 20 // 50 ms
	type pt struct{ t, low, high float64 }
	var pts []pt
	for i := 0; i+win <= len(pcm); i += win {
		w := pcm[i : i+win]
		pts = append(pts, pt{float64(i) / engine.SampleRate, goertzel(w, 440), goertzel(w, 1000)})
	}
	const loud = 1000.0 // sine=... is about 4000; anything above this counts as "on"
	changeAt := switchedAt.Sub(first).Seconds()

	var heardHigh, lowGone = -1.0, -1.0
	for _, p := range pts {
		if p.t > 0.3 && p.t < changeAt-0.3 { // before the switch: only the low tone
			if p.low < loud || p.high > loud/10 {
				t.Fatalf("at %.2fs (before the change) low=%.0f high=%.0f: want only the 440 Hz tone", p.t, p.low, p.high)
			}
		}
		if heardHigh < 0 && p.t > changeAt-0.2 && p.high > loud {
			heardHigh = p.t
		}
		if lowGone < 0 && p.t > changeAt-0.2 && p.low < loud/10 {
			lowGone = p.t
		}
	}
	if heardHigh < 0 || lowGone < 0 {
		t.Fatalf("the change was never heard (high at %.2f, low gone at %.2f, switched at %.2f)", heardHigh, lowGone, changeAt)
	}
	lag := math.Max(heardHigh, lowGone) - changeAt
	t.Logf("level change reached the output %.0f ms after the PUT (server side; a player adds its own buffer)", lag*1000)
	// Normally about 0.2 s. The bound is loose because this test also runs beside other packages' tests,
	// under the race detector; it exists to catch a delay of seconds, not of milliseconds.
	if lag > 1.5 {
		t.Errorf("the change took %.2f s to reach the stream, want under 1.5 s", lag)
	}

	// After the switch only the high tone remains, with no dropouts in the steady part.
	for _, p := range pts {
		if p.t > changeAt+0.5 && p.t < float64(len(pcm))/engine.SampleRate-0.3 {
			if p.high < loud || p.low > loud/10 {
				t.Fatalf("at %.2fs (after the change) low=%.0f high=%.0f", p.t, p.low, p.high)
			}
		}
	}

	// Looping: the tones are 2 s long (1.5 s once the crossfade is taken off) and the high tone plays for 3 s, so it wraps
	// at least once. The join may not leave a hole.
	weakest, weakestAt := math.MaxFloat64, 0.0
	for _, p := range pts {
		if p.t > changeAt+0.5 && p.t < float64(len(pcm))/engine.SampleRate-0.3 && p.high < weakest {
			weakest, weakestAt = p.high, p.t
		}
	}
	t.Logf("weakest 1 kHz window after the change: %.0f at %.2fs (steady level is about 4000)", weakest, weakestAt)
	for i := win; i+win <= len(pcm); i += win {
		if rms := goertzel(pcm[i:i+win], 440) + goertzel(pcm[i:i+win], 1000); rms < loud && float64(i)/engine.SampleRate > 0.3 {
			t.Fatalf("a gap in the stream at %.2fs", float64(i)/engine.SampleRate)
		}
	}
}
