package engine

import (
	"math"
	"sync"
	"testing"
)

func dc(n int, v int16) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func render(m *Mixer, n int) []int16 {
	out := make([]int16, 0, n)
	buf := make([]int16, ChunkSamples)
	for len(out) < n {
		m.Render(buf)
		out = append(out, buf...)
	}
	return out[:n]
}

func TestEmptyMixerIsSilent(t *testing.T) {
	for _, v := range render(NewMixer(), 4096) {
		if v != 0 {
			t.Fatal("expected silence")
		}
	}
}

func TestLevelAndRamp(t *testing.T) {
	m := NewMixer()
	m.Set("a", 0.5, true)
	m.SetLoop("a", dc(100, 10000))
	out := render(m, 8192)
	if out[0] >= 5000 || out[0] < 0 {
		t.Fatalf("first sample %d should be on its way up to 5000", out[0])
	}
	for i := 1; i < len(out); i++ {
		if out[i] < out[i-1] {
			t.Fatalf("ramp not monotonic at %d", i)
		}
	}
	// 0.5 of full scale takes half the ramp time (~1100 samples); well after that it sits exactly on target.
	for i := 1300; i < len(out); i++ {
		if out[i] != 5000 {
			t.Fatalf("sample %d = %d, want 5000", i, out[i])
		}
	}
}

func TestDisableFadesOutWithoutClicks(t *testing.T) {
	m := NewMixer()
	m.Set("a", 1, true)
	m.SetLoop("a", dc(100, 10000))
	render(m, 8192)
	m.Set("a", 1, false)
	out := render(m, 8192)
	if maxStep(out) > 10 { // 10000 over 2205 samples is about 4.5 per sample
		t.Fatalf("click on disable: largest step %.1f", maxStep(out))
	}
	for _, v := range out[2500:] {
		if v != 0 {
			t.Fatal("not silent after the fade")
		}
	}
	if m.Audible() != 0 {
		t.Fatal("a disabled layer must not count as audible")
	}
}

func TestSliderMoveDoesNotClick(t *testing.T) {
	m := NewMixer()
	m.Set("a", 0.2, true)
	m.SetLoop("a", sine(SampleRate, 440, 10000))
	render(m, 4096)
	before := maxStep(render(m, 4096))
	m.Set("a", 0.9, true)
	after := maxStep(render(m, 4096))
	if after > before*4+1000 {
		t.Fatalf("jump when the level changed: %.0f vs %.0f before", after, before)
	}
}

func TestLayersAdd(t *testing.T) {
	m := NewMixer()
	m.Set("a", 1, true)
	m.Set("b", 1, true)
	m.SetLoop("a", dc(10, 3000))
	m.SetLoop("b", dc(10, 2000))
	out := render(m, 8192)
	if out[len(out)-1] != 5000 {
		t.Fatalf("sum = %d, want 5000", out[len(out)-1])
	}
	if m.Audible() != 2 {
		t.Fatalf("audible = %d", m.Audible())
	}
}

func TestSoftLimiter(t *testing.T) {
	if got := softClip(1234.4); got != 1234 {
		t.Fatalf("quiet samples must pass through, got %d", got)
	}
	var prev int16 = -1
	for x := float32(0); x < 200000; x += 37 {
		v := softClip(x)
		if v > math.MaxInt16 || v < prev {
			t.Fatalf("limiter not monotonic or exceeds full scale at %.0f: %d (prev %d)", x, v, prev)
		}
		if softClip(-x) != -v && !(x == 0) {
			t.Fatalf("limiter not symmetric at %.0f", x)
		}
		prev = v
	}
	if softClip(1e9) != math.MaxInt16 || softClip(-1e9) != -math.MaxInt16 {
		t.Fatal("extreme input must end at full scale")
	}
	m := NewMixer()
	m.Set("a", 1, true)
	m.Set("b", 1, true)
	m.SetLoop("a", dc(10, 30000))
	m.SetLoop("b", dc(10, 30000))
	out := render(m, 8192)
	if last := out[len(out)-1]; last <= 29490 || last > math.MaxInt16 {
		t.Fatalf("two loud layers should be eased below full scale, got %d", last)
	}
}

func TestLoopWrapsSeamlessly(t *testing.T) {
	m := NewMixer()
	m.Set("a", 1, true)
	m.SetLoop("a", []int16{1000, 2000, 3000})
	out := render(m, 8192)
	next := map[int16]int16{1000: 2000, 2000: 3000, 3000: 1000}
	for i := 4000; i < len(out)-1; i++ {
		if out[i+1] != next[out[i]] {
			t.Fatalf("sequence broken at %d: %d then %d", i, out[i], out[i+1])
		}
	}
}

func TestLayerWithoutAudioIsSilentUntilItArrives(t *testing.T) {
	m := NewMixer()
	m.Set("a", 1, true)
	for _, v := range render(m, 2048) {
		if v != 0 {
			t.Fatal("a layer that has not loaded must be silent")
		}
	}
	m.SetLoop("nope", dc(10, 1000)) // unknown layer: ignored
	m.SetLoop("a", dc(10, 1000))
	if got := render(m, 8192); got[len(got)-1] != 1000 {
		t.Fatalf("after SetLoop got %d", got[len(got)-1])
	}
}

func TestDisabledLayerDoesNotAdvance(t *testing.T) {
	m := NewMixer()
	m.Set("a", 1, false)
	m.SetLoop("a", dc(10, 1000))
	render(m, 4096)
	if m.voices["a"].pos != 0 {
		t.Fatal("a layer that is off must not advance")
	}
}

func TestRemoveFadesThenForgets(t *testing.T) {
	m := NewMixer()
	m.Set("a", 1, true)
	m.SetLoop("a", dc(10, 1000))
	render(m, 4096)
	m.Remove("a")
	render(m, 8192)
	if len(m.voices) != 0 || len(m.order) != 0 {
		t.Fatalf("layer not forgotten: %d voices, %d ordered", len(m.voices), len(m.order))
	}
	m.Set("a", 1, true) // can come back
	m.SetLoop("a", dc(10, 700))
	if got := render(m, 8192); got[len(got)-1] != 700 {
		t.Fatalf("re-added layer got %d", got[len(got)-1])
	}
}

func TestLevelIsClamped(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want float32
	}{{5, 1}, {-1, 0}, {math.NaN(), 0}, {0.25, 0.25}} {
		m := NewMixer()
		m.Set("a", c.in, true)
		if got := m.voices["a"].target; got != c.want {
			t.Errorf("level %v -> target %v, want %v", c.in, got, c.want)
		}
	}
}

func TestDeterministic(t *testing.T) {
	run := func() []int16 {
		m := NewMixer()
		for i, id := range []string{"a", "b", "c"} {
			m.Set(id, 0.3+float64(i)*0.2, true)
			m.SetLoop(id, noise(5000+i*777, 9000, int64(i)))
		}
		return render(m, 20000)
	}
	a, b := run(), run()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("differs at %d", i)
		}
	}
}

func TestConcurrentUse(t *testing.T) {
	m := NewMixer()
	loop := sine(SampleRate, 440, 8000)
	var wg sync.WaitGroup
	wg.Add(2)
	stop := make(chan struct{})
	go func() {
		defer wg.Done()
		buf := make([]int16, ChunkSamples)
		for {
			select {
			case <-stop:
				return
			default:
				m.Render(buf)
			}
		}
	}()
	go func() {
		defer wg.Done()
		defer close(stop)
		ids := []string{"a", "b", "c"}
		for i := 0; i < 3000; i++ {
			id := ids[i%3]
			m.Set(id, float64(i%10)/10, i%2 == 0)
			m.SetLoop(id, loop)
			if i%7 == 0 {
				m.Remove(id)
			}
			m.Audible()
		}
	}()
	wg.Wait()
}
