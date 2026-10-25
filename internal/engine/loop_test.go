package engine

import (
	"errors"
	"math"
	"math/rand"
	"testing"
	"time"
)

func sine(n int, freq, amp float64) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(amp * math.Sin(0.3+2*math.Pi*freq*float64(i)/SampleRate)) // phase offset: a sine starting at exactly 0 would be trimmed as silence
	}
	return out
}

func noise(n int, amp float64, seed int64) []int16 {
	r := rand.New(rand.NewSource(seed))
	out := make([]int16, n)
	for i := range out {
		out[i] = int16((r.Float64()*2 - 1) * amp)
	}
	return out
}

func padded(pcm []int16, lead, tail int) []int16 {
	return append(append(make([]int16, lead), pcm...), make([]int16, tail)...)
}

func rms(s []int16) float64 {
	var sum float64
	for _, v := range s {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(s)))
}

func maxStep(s []int16) float64 {
	var m float64
	for i := 1; i < len(s); i++ {
		if d := math.Abs(float64(s[i]) - float64(s[i-1])); d > m {
			m = d
		}
	}
	return m
}

func TestBuildLoopTrimsSilenceAtBothEnds(t *testing.T) {
	body := sine(SampleRate, 440, 8000) // 1 s
	got, err := BuildLoop(padded(body, 5000, 70000), LoopOptions{SilenceThreshold: 8})
	if err != nil {
		t.Fatal(err)
	}
	// No crossfade: the loop is exactly the body, from its first loud sample to its last.
	first, last := 0, len(body)-1
	for abs16(body[first]) <= 8 {
		first++
	}
	for abs16(body[last]) <= 8 {
		last--
	}
	if want := last - first + 1; len(got) != want {
		t.Fatalf("len %d, want %d", len(got), want)
	}
	if abs16(got[0]) <= 8 || abs16(got[len(got)-1]) <= 8 {
		t.Fatal("loop still starts or ends in silence")
	}
}

func TestBuildLoopRejectsSilence(t *testing.T) {
	for name, pcm := range map[string][]int16{"empty": nil, "zeros": make([]int16, 1000), "below threshold": {1, -2, 3, 0, 5}} {
		if _, err := BuildLoop(pcm, DefaultLoopOptions); !errors.Is(err, ErrSilent) {
			t.Errorf("%s: err = %v, want ErrSilent", name, err)
		}
	}
}

func TestBuildLoopDoesNotModifyInput(t *testing.T) {
	in := padded(sine(SampleRate*3, 300, 9000), 100, 100)
	orig := append([]int16(nil), in...)
	if _, err := BuildLoop(in, DefaultLoopOptions); err != nil {
		t.Fatal(err)
	}
	for i := range in {
		if in[i] != orig[i] {
			t.Fatalf("input changed at %d", i)
		}
	}
}

func TestBuildLoopLengthAndShortFiles(t *testing.T) {
	long := sine(SampleRate*10, 200, 5000)
	got, _ := BuildLoop(long, LoopOptions{Crossfade: time.Second, SilenceThreshold: 0})
	// A tonal file may have its join moved back (by up to maxAlignShift) to line the phases up.
	if max := len(long) - SampleRate; len(got) > max || len(got) < max-maxAlignShift {
		t.Fatalf("len %d, want between %d and %d", len(got), max-maxAlignShift, max)
	}
	noisy := noise(SampleRate*10, 5000, 9)
	got, _ = BuildLoop(noisy, LoopOptions{Crossfade: time.Second, SilenceThreshold: 0})
	if want := len(noisy) - SampleRate; len(got) != want {
		t.Fatalf("noise must not have its join moved: len %d, want %d", len(got), want)
	}
	// A file shorter than three crossfades gets a shorter fade (a third of its length) instead of failing.
	short := sine(SampleRate*6/10, 200, 5000)
	got, err := BuildLoop(short, LoopOptions{Crossfade: 1500 * time.Millisecond, SilenceThreshold: 0})
	if err != nil || len(got) != len(short)-len(short)/3 {
		t.Fatalf("short file: len %d err %v, want %d", len(got), err, len(short)-len(short)/3)
	}
}

func TestBuildLoopJoinIsContinuousAndKeepsLevel(t *testing.T) {
	src := padded(noise(SampleRate*20, 6000, 1), 3000, 80000) // a long silent tail, like a real recording
	loop, err := BuildLoop(src, DefaultLoopOptions)
	if err != nil {
		t.Fatal(err)
	}
	// Unroll three laps and look at the join.
	three := append(append(append([]int16(nil), loop...), loop...), loop...)
	join := len(loop)
	if step, typical := maxStep(three[join-5:join+5]), maxStep(loop[SampleRate:SampleRate+20000]); step > typical {
		t.Fatalf("step at join %.0f exceeds the file's own largest step %.0f", step, typical)
	}
	// Equal-power crossfade of unrelated material keeps the level: no dip, no bump.
	x := int(DefaultLoopOptions.Crossfade * SampleRate / time.Second)
	body := rms(loop[x : x+SampleRate*5])
	for i := 0; i < x; i += x / 10 {
		w := rms(loop[i : i+x/10])
		if math.Abs(w-body)/body > 0.2 {
			t.Fatalf("level in crossfade window %d is %.0f, body is %.0f", i, w, body)
		}
	}
	// No silent gap anywhere around the join.
	for i := join - SampleRate/2; i < join+SampleRate/2; i += 441 {
		if rms(three[i:i+441]) < body/4 {
			t.Fatalf("silence near the join at %d", i)
		}
	}
}

func TestBuildLoopZeroCrossfadeIsHardCut(t *testing.T) {
	src := sine(SampleRate, 440, 8000)
	got, _ := BuildLoop(src, LoopOptions{SilenceThreshold: 0})
	if len(got) != len(src) {
		t.Fatalf("len %d, want %d", len(got), len(src))
	}
}

// joinRange returns the quietest and loudest 10 ms windows in the first x samples of a loop (where the
// crossfade is) and the rms of a steady stretch after it for comparison.
func joinRange(s []int16, x int) (quietest, loudest, steady float64) {
	steady = rms(s[x+2000 : x+2000+4410])
	quietest, loudest = steady, steady
	for i := 0; i+441 <= x; i += 441 {
		w := rms(s[i : i+441])
		quietest, loudest = math.Min(quietest, w), math.Max(loudest, w)
	}
	return
}

// A tone crossfaded with a copy of itself that is out of phase used to cancel in the middle of the fade
// (440.37 Hz fell to 24% of its level). The join must now hold the level for any frequency.
func TestBuildLoopHoldsLevelForTonesOfAnyPhase(t *testing.T) {
	x := SampleRate / 2
	for _, freq := range []float64{440, 440.37, 440.5, 123.4, 97.31, 1000.17, 55.9, 2000.77, 3000.5} {
		for _, n := range []int{SampleRate * 2, SampleRate*2 + 777, SampleRate * 3} {
			loop, err := BuildLoop(sine(n, freq, 8000), LoopOptions{Crossfade: 500 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			q, l, steady := joinRange(loop, x)
			if q < steady*0.85 {
				t.Errorf("%.2f Hz, %d samples: the join dips to %.0f%% of the steady level", freq, n, 100*q/steady)
			}
			if l > steady*1.15 {
				t.Errorf("%.2f Hz, %d samples: the join swells to %.0f%% of the steady level", freq, n, 100*l/steady)
			}
		}
	}
}

func TestBuildLoopTonalJoinIsContinuous(t *testing.T) {
	for _, freq := range []float64{440.37, 97.31, 1000.17} {
		loop, _ := BuildLoop(sine(SampleRate*3, freq, 8000), LoopOptions{Crossfade: 500 * time.Millisecond})
		three := append(append(append([]int16(nil), loop...), loop...), loop...)
		j := len(loop)
		if step, own := maxStep(three[j-5:j+5]), maxStep(loop[SampleRate:SampleRate+4000]); step > own*1.2 {
			t.Errorf("%.2f Hz: step %.0f at the join against %.0f in the tone itself", freq, step, own)
		}
	}
}

func TestBuildLoopSumOfTonesAndNoise(t *testing.T) {
	// a hum with noise on top, like a real fan recording
	n := SampleRate * 4
	hum, nz := sine(n, 118.3, 5000), noise(n, 1500, 5)
	mix := make([]int16, n)
	for i := range mix {
		mix[i] = hum[i] + nz[i]
	}
	loop, _ := BuildLoop(mix, LoopOptions{Crossfade: time.Second})
	q, l, steady := joinRange(loop, SampleRate)
	if q < steady*0.85 || l > steady*1.15 {
		t.Fatalf("hum plus noise leaves the level range %.0f%%..%.0f%% of steady", 100*q/steady, 100*l/steady)
	}
}
