package engine

import (
	"bytes"
	"math/rand"
	"os/exec"
	"testing"
)

// mp3Frame builds a frame of the right length with a valid MPEG1 Layer III header (128 kbit/s, 44.1 kHz).
func mp3Frame(pad bool, fill byte) []byte {
	n, b2 := 417, byte(0x90)
	if pad {
		n, b2 = 418, 0x92
	}
	f := bytes.Repeat([]byte{fill}, n)
	copy(f, []byte{0xFF, 0xFB, b2, 0x00})
	return f
}

func TestFrameLen(t *testing.T) {
	cases := []struct {
		name string
		hdr  []byte
		want int
	}{
		{"MPEG1 L3 128k 44.1k", []byte{0xFF, 0xFB, 0x90, 0x00}, 417},
		{"MPEG1 L3 128k 44.1k padded", []byte{0xFF, 0xFB, 0x92, 0x00}, 418},
		{"MPEG1 L3 320k 48k", []byte{0xFF, 0xFB, 0xE4, 0x00}, 144 * 320000 / 48000},
		{"MPEG2 L3 80k 22.05k", []byte{0xFF, 0xF3, 0x90, 0x00}, 72 * 80000 / 22050},
		{"layer II", []byte{0xFF, 0xFD, 0x90, 0x00}, 0},
		{"free-format bitrate", []byte{0xFF, 0xFB, 0x00, 0x00}, 0},
		{"bad bitrate", []byte{0xFF, 0xFB, 0xF0, 0x00}, 0},
		{"reserved sample rate", []byte{0xFF, 0xFB, 0x9C, 0x00}, 0},
		{"reserved version", []byte{0xFF, 0xEB, 0x90, 0x00}, 0},
		{"no sync", []byte{0x00, 0xFB, 0x90, 0x00}, 0},
		{"too short", []byte{0xFF, 0xFB}, 0},
	}
	for _, c := range cases {
		if got := frameLen(c.hdr); got != c.want {
			t.Errorf("%s: frameLen(% x) = %d, want %d", c.name, c.hdr, got, c.want)
		}
	}
}

func TestSplitterCutsWholeFramesFromArbitraryChunks(t *testing.T) {
	var stream []byte
	var want [][]byte
	for i := 0; i < 60; i++ {
		f := mp3Frame(i%3 == 0, byte(i))
		want = append(want, f)
		stream = append(stream, f...)
	}
	stream = append(stream, mp3Frame(false, 0)...) // a final frame is only emitted once a next header proves it

	var s FrameSplitter
	var got [][]byte
	r := rand.New(rand.NewSource(7))
	for rest := stream; len(rest) > 0; {
		n := 1 + r.Intn(700)
		if n > len(rest) {
			n = len(rest)
		}
		got = append(got, s.Write(rest[:n])...)
		rest = rest[n:]
	}
	if len(got) != len(want) {
		t.Fatalf("got %d frames, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("frame %d differs", i)
		}
	}
}

func TestSplitterSkipsLeadingJunkAndFalseSync(t *testing.T) {
	var stream []byte
	stream = append(stream, 0x12, 0x34, 0xFF, 0xFB, 0x00, 0x99, 0xFF) // junk, including header-like bytes that are not real
	var want [][]byte
	for i := 0; i < 5; i++ {
		f := mp3Frame(false, byte(i+1))
		want = append(want, f)
		stream = append(stream, f...)
	}
	stream = append(stream, mp3Frame(false, 9)...)
	var s FrameSplitter
	got := s.Write(stream)
	if len(got) != len(want) {
		t.Fatalf("got %d frames, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("frame %d differs", i)
		}
	}
}

func TestSplitterReturnsIndependentSlices(t *testing.T) {
	var s FrameSplitter
	frames := s.Write(append(append(mp3Frame(false, 1), mp3Frame(false, 2)...), mp3Frame(false, 3)...))
	if len(frames) < 2 {
		t.Fatal("need frames")
	}
	first := append([]byte(nil), frames[0]...)
	s.Write(bytes.Repeat([]byte{0xAA}, 2000)) // reuses the internal buffer
	if !bytes.Equal(frames[0], first) {
		t.Fatal("a returned frame was overwritten by later writes")
	}
}

// TestSplitterOnRealFFmpegOutput feeds actual encoder output, cut at random places, through the splitter.
func TestSplitterOnRealFFmpegOutput(t *testing.T) {
	haveFFmpeg(t)
	out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-ac", "1", "-ar", "44100", "-c:a", "libmp3lame", "-b:a", "128k", "-write_xing", "0", "-id3v2_version", "0",
		"-f", "mp3", "-").Output()
	if err != nil {
		t.Skip("cannot encode:", err)
	}
	var s FrameSplitter
	var joined []byte
	frames := 0
	r := rand.New(rand.NewSource(3))
	for rest := out; len(rest) > 0; {
		n := 1 + r.Intn(1500)
		if n > len(rest) {
			n = len(rest)
		}
		for _, f := range s.Write(rest[:n]) {
			if n := frameLen(f); n != len(f) {
				t.Fatalf("frame of %d bytes but its header says %d", len(f), n)
			}
			joined = append(joined, f...)
			frames++
		}
		rest = rest[n:]
	}
	if frames < 100 {
		t.Fatalf("only %d frames from 3 s of audio", frames)
	}
	// everything except the last (unverifiable) frame must come back byte for byte
	if !bytes.HasPrefix(out, joined) || len(out)-len(joined) > 420 {
		t.Fatalf("output differs from input: got %d of %d bytes", len(joined), len(out))
	}
}
