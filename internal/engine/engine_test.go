package engine

import (
	"context"
	"errors"
	"io"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-soundscapes/internal/library"
)

// --- fakes -----------------------------------------------------------------------------------------

type fakeLib struct {
	mu sync.Mutex
	m  map[string]library.Layer
}

func newFakeLib(ids ...string) *fakeLib {
	f := &fakeLib{m: map[string]library.Layer{}}
	for _, id := range ids {
		f.m[id] = layer(id)
	}
	return f
}

func (f *fakeLib) Get(id string) (library.Layer, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.m[id]
	return l, ok
}

func (f *fakeLib) set(l library.Layer) { f.mu.Lock(); f.m[l.ID] = l; f.mu.Unlock() }

// fakeEncoder records the PCM it is given and answers every chunk with one valid MP3 frame.
type fakeEncoder struct {
	mu        sync.Mutex
	recent    [][]int16
	starts    atomic.Int32
	stops     atomic.Int32
	failAfter int
	startErr  error
}

func (f *fakeEncoder) factory(ctx context.Context) (*Encoder, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.starts.Add(1)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	var waitErr error
	failAfter := f.failAfter
	go func() {
		defer close(done)
		defer outW.Close()
		defer f.stops.Add(1)
		raw := make([]byte, ChunkSamples*2)
		for n := 1; ; n++ {
			if _, err := io.ReadFull(inR, raw); err != nil {
				return
			}
			chunk := make([]int16, ChunkSamples)
			for i := range chunk {
				chunk[i] = int16(uint16(raw[2*i]) | uint16(raw[2*i+1])<<8)
			}
			f.mu.Lock()
			f.recent = append(f.recent, chunk)
			if len(f.recent) > 400 {
				f.recent = f.recent[1:]
			}
			f.mu.Unlock()
			if failAfter > 0 && n >= failAfter {
				waitErr = errors.New("encoder crashed")
				_ = inR.CloseWithError(waitErr)
				return
			}
			if _, err := outW.Write(mp3Frame(n%3 == 0, byte(n))); err != nil {
				return
			}
			time.Sleep(time.Millisecond) // stand in for real time, so tests do not spin
		}
	}()
	go func() { <-ctx.Done(); _ = inR.Close(); _ = outW.Close() }()
	return &Encoder{In: inW, Out: outR, wait: func() error { <-done; return waitErr }}, nil
}

// mean returns the average sample value of the last n chunks written to the encoder.
func (f *fakeEncoder) mean(n int) (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.recent) < n {
		return 0, false
	}
	var sum float64
	for _, c := range f.recent[len(f.recent)-n:] {
		for _, v := range c {
			sum += float64(v)
		}
	}
	return sum / float64(n*ChunkSamples), true
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// meanNear waits until the stream's recent average level is within tol of want.
func meanNear(t *testing.T, f *fakeEncoder, want, tol float64) {
	t.Helper()
	var last float64
	eventually(t, "level near "+itoa(int(want)), func() bool {
		m, ok := f.mean(8)
		last = m
		return ok && math.Abs(m-want) <= tol
	})
	_ = last
}

type fixture struct {
	t   *testing.T
	e   *Engine
	lib *fakeLib
	enc *fakeEncoder
	// value each fake file decodes to (a constant, so the mixed level is easy to predict)
	values map[string]int16
	calls  atomic.Int32
	cancel context.CancelFunc
}

func newFixture(t *testing.T, ids ...string) *fixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fx := &fixture{t: t, lib: newFakeLib(ids...), enc: &fakeEncoder{}, values: map[string]int16{}, cancel: cancel}
	fx.e = New(ctx, fx.lib, Options{
		Loop: testOpt,
		Decode: func(_ context.Context, path string) ([]int16, error) {
			fx.calls.Add(1)
			id := strings.TrimPrefix(path, "/fake/")
			if id == "bad.mp3" {
				return nil, errors.New("corrupt data")
			}
			v, ok := fx.values[id]
			if !ok {
				v = 10000
			}
			return dc(2000, v), nil
		},
		NewEncoder: fx.enc.factory,
		Grace:      60 * time.Millisecond,
		Unpaced:    true,
	}, testLogger())
	return fx
}

func apply(t *testing.T, e *Engine, id string, ls ...LayerSpec) Status {
	t.Helper()
	st, err := e.Apply(id, Spec{Layers: ls})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// --- tests -----------------------------------------------------------------------------------------

func TestStreamPlaysTheMixAtTheRequestedLevel(t *testing.T) {
	fx := newFixture(t, "rain.mp3")
	apply(t, fx.e, "night", LayerSpec{ID: "rain.mp3", Level: 0.5, Enabled: true})
	l, err := fx.e.Subscribe("night")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	meanNear(t, fx.enc, 5000, 2)

	select {
	case f := <-l.C:
		if frameLen(f) != len(f) {
			t.Fatalf("listener got a frame whose header says %d bytes but is %d long", frameLen(f), len(f))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no MP3 data reached the listener")
	}
}

func TestChangesAreHeardLiveWithoutReconnecting(t *testing.T) {
	fx := newFixture(t, "rain.mp3", "waves.mp3")
	fx.values["waves.mp3"] = 4000
	apply(t, fx.e, "night", LayerSpec{ID: "rain.mp3", Level: 1, Enabled: true})
	l, _ := fx.e.Subscribe("night")
	defer l.Close()
	meanNear(t, fx.enc, 10000, 2)

	apply(t, fx.e, "night", LayerSpec{ID: "rain.mp3", Level: 0.2, Enabled: true})
	meanNear(t, fx.enc, 2000, 2)

	apply(t, fx.e, "night", // add a second layer
		LayerSpec{ID: "rain.mp3", Level: 0.2, Enabled: true}, LayerSpec{ID: "waves.mp3", Level: 0.5, Enabled: true})
	meanNear(t, fx.enc, 2000+2000, 2)

	apply(t, fx.e, "night", // switch rain off
		LayerSpec{ID: "rain.mp3", Level: 0.2, Enabled: false}, LayerSpec{ID: "waves.mp3", Level: 0.5, Enabled: true})
	meanNear(t, fx.enc, 2000, 2)

	apply(t, fx.e, "night", LayerSpec{ID: "waves.mp3", Level: 0.5, Enabled: true}) // rain dropped from the soundscape
	meanNear(t, fx.enc, 2000, 2)
	apply(t, fx.e, "night") // nothing left
	meanNear(t, fx.enc, 0, 1)

	apply(t, fx.e, "night", LayerSpec{ID: "rain.mp3", Level: 1, Enabled: true}) // and back again
	meanNear(t, fx.enc, 10000, 2)
	select {
	case <-l.Done():
		t.Fatal("the listener was disconnected by a change")
	default:
	}
}

func TestLayerStates(t *testing.T) {
	fx := newFixture(t, "ok.mp3", "off.mp3", "bad.mp3", "unusable.mp3")
	unusable := layer("unusable.mp3")
	unusable.Error = "no audio stream"
	fx.lib.set(unusable)

	st := apply(t, fx.e, "mix",
		LayerSpec{ID: "ok.mp3", Level: 0.5, Enabled: true},
		LayerSpec{ID: "off.mp3", Level: 0.5, Enabled: false},
		LayerSpec{ID: "missing.mp3", Level: 1, Enabled: true},
		LayerSpec{ID: "unusable.mp3", Level: 1, Enabled: true},
		LayerSpec{ID: "bad.mp3", Level: 1, Enabled: true},
		LayerSpec{ID: "ok.mp3", Level: 1, Enabled: true}, // duplicate: the first wins
		LayerSpec{ID: "", Level: 1, Enabled: true},       // no ID: ignored
	)
	if len(st.Layers) != 5 {
		t.Fatalf("want 5 layers (duplicate and empty dropped), got %d: %+v", len(st.Layers), st.Layers)
	}
	if st.Layers[0].Level != 0.5 {
		t.Fatalf("the first of two duplicates should win, got level %v", st.Layers[0].Level)
	}
	want := map[string]string{"ok.mp3": StateReady, "off.mp3": StateOff, "missing.mp3": StateMissing, "unusable.mp3": StateUnusable, "bad.mp3": StateError}
	eventually(t, "layer states", func() bool {
		s, _ := fx.e.Status("mix")
		for _, l := range s.Layers {
			if l.State != want[l.ID] {
				return false
			}
		}
		return true
	})
	s, _ := fx.e.Status("mix")
	for _, l := range s.Layers {
		switch l.ID {
		case "bad.mp3":
			if !strings.Contains(l.Error, "corrupt data") {
				t.Errorf("error not reported: %+v", l)
			}
		case "unusable.mp3":
			if l.Error != "no audio stream" {
				t.Errorf("reason not reported: %+v", l)
			}
		}
	}
}

func TestOneBadLayerDoesNotStopTheOthers(t *testing.T) {
	fx := newFixture(t, "ok.mp3", "bad.mp3")
	apply(t, fx.e, "mix", LayerSpec{ID: "bad.mp3", Level: 1, Enabled: true}, LayerSpec{ID: "ok.mp3", Level: 0.3, Enabled: true})
	l, _ := fx.e.Subscribe("mix")
	defer l.Close()
	meanNear(t, fx.enc, 3000, 2)
}

func TestLoadingLayerIsSilentThenAudible(t *testing.T) {
	gate := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	enc := &fakeEncoder{}
	e := New(ctx, newFakeLib("slow.mp3"), Options{Loop: testOpt, NewEncoder: enc.factory, Grace: time.Second, Unpaced: true,
		Decode: func(context.Context, string) ([]int16, error) { <-gate; return dc(100, 8000), nil }}, testLogger())
	st, _ := e.Apply("mix", Spec{Layers: []LayerSpec{{ID: "slow.mp3", Level: 1, Enabled: true}}})
	if st.Layers[0].State != StateLoading {
		t.Fatalf("state = %s, want loading", st.Layers[0].State)
	}
	l, _ := e.Subscribe("mix")
	defer l.Close()
	meanNear(t, enc, 0, 1)
	close(gate)
	meanNear(t, enc, 8000, 2)
}

func TestFailedLayerIsRetriedOnNextApply(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := New(ctx, newFakeLib("flaky.mp3"), Options{Loop: testOpt, NewEncoder: (&fakeEncoder{}).factory, Unpaced: true,
		Decode: func(context.Context, string) ([]int16, error) {
			if calls.Add(1) == 1 {
				return nil, errors.New("disk hiccup")
			}
			return dc(100, 500), nil
		}}, testLogger())
	spec := Spec{Layers: []LayerSpec{{ID: "flaky.mp3", Level: 1, Enabled: true}}}
	_, _ = e.Apply("mix", spec)
	eventually(t, "first load to fail", func() bool {
		s, _ := e.Status("mix")
		return s.Layers[0].State == StateError
	})
	_, _ = e.Apply("mix", spec)
	eventually(t, "retry to succeed", func() bool {
		s, _ := e.Status("mix")
		return s.Layers[0].State == StateReady
	})
}

func TestSoundscapesShareOneDecode(t *testing.T) {
	fx := newFixture(t, "rain.mp3")
	apply(t, fx.e, "a", LayerSpec{ID: "rain.mp3", Level: 1, Enabled: true})
	apply(t, fx.e, "b", LayerSpec{ID: "rain.mp3", Level: 0.5, Enabled: true})
	eventually(t, "both ready", func() bool {
		a, _ := fx.e.Status("a")
		b, _ := fx.e.Status("b")
		return a.Layers[0].State == StateReady && b.Layers[0].State == StateReady
	})
	if fx.calls.Load() != 1 {
		t.Fatalf("decoded %d times for two soundscapes, want 1", fx.calls.Load())
	}
}

func TestReplacedFileIsReloaded(t *testing.T) {
	fx := newFixture(t, "rain.mp3")
	spec := LayerSpec{ID: "rain.mp3", Level: 1, Enabled: true}
	apply(t, fx.e, "mix", spec)
	l, _ := fx.e.Subscribe("mix")
	defer l.Close()
	meanNear(t, fx.enc, 10000, 2)

	fx.values["rain.mp3"] = 3000
	changed := layer("rain.mp3")
	changed.ModTime = changed.ModTime.Add(time.Minute)
	fx.lib.set(changed)
	apply(t, fx.e, "mix", spec)
	meanNear(t, fx.enc, 3000, 2)
}

func TestSwitchedOffLayerGivesItsMemoryBack(t *testing.T) {
	fx := newFixture(t, "rain.mp3")
	apply(t, fx.e, "mix", LayerSpec{ID: "rain.mp3", Level: 1, Enabled: true})
	l, _ := fx.e.Subscribe("mix")
	defer l.Close()
	meanNear(t, fx.enc, 10000, 2)
	m := fx.e.mixes["mix"]
	apply(t, fx.e, "mix", LayerSpec{ID: "rain.mp3", Level: 1, Enabled: false})
	eventually(t, "audio to be dropped after the fade", func() bool { return !m.mixer.Loaded("rain.mp3") })
}

func TestListenersShareOneEncoderAndItStopsAfterTheLast(t *testing.T) {
	fx := newFixture(t, "rain.mp3")
	apply(t, fx.e, "mix", LayerSpec{ID: "rain.mp3", Level: 0.5, Enabled: true})
	a, _ := fx.e.Subscribe("mix")
	b, _ := fx.e.Subscribe("mix")
	for _, l := range []*Listener{a, b} {
		select {
		case <-l.C:
		case <-time.After(3 * time.Second):
			t.Fatal("a listener got nothing")
		}
	}
	if fx.enc.starts.Load() != 1 {
		t.Fatalf("%d encoders for two listeners, want 1", fx.enc.starts.Load())
	}
	if st, _ := fx.e.Status("mix"); st.Listeners != 2 || !st.Streaming {
		t.Fatalf("status %+v", st)
	}

	a.Close()
	a.Close() // idempotent
	time.Sleep(150 * time.Millisecond)
	if st, _ := fx.e.Status("mix"); !st.Streaming || st.Listeners != 1 {
		t.Fatalf("the stream must keep running for the remaining listener: %+v", st)
	}
	b.Close()
	eventually(t, "the encoder to stop after the grace period", func() bool {
		st, _ := fx.e.Status("mix")
		return !st.Streaming && fx.enc.stops.Load() == 1
	})

	c, _ := fx.e.Subscribe("mix") // a new listener starts a fresh encoder
	defer c.Close()
	select {
	case <-c.C:
	case <-time.After(3 * time.Second):
		t.Fatal("restarted stream produced nothing")
	}
	if fx.enc.starts.Load() != 2 {
		t.Fatalf("encoder starts = %d, want 2", fx.enc.starts.Load())
	}
}

func TestReconnectInsideGraceKeepsTheEncoder(t *testing.T) {
	fx := newFixture(t, "rain.mp3")
	apply(t, fx.e, "mix", LayerSpec{ID: "rain.mp3", Level: 0.5, Enabled: true})
	a, _ := fx.e.Subscribe("mix")
	<-a.C
	a.Close()
	b, _ := fx.e.Subscribe("mix") // straight back, as a player does
	defer b.Close()
	time.Sleep(250 * time.Millisecond) // longer than the grace period
	if st, _ := fx.e.Status("mix"); !st.Streaming {
		t.Fatal("the stream stopped although a listener reconnected within the grace period")
	}
	if fx.enc.starts.Load() != 1 {
		t.Fatalf("encoder restarted (%d starts)", fx.enc.starts.Load())
	}
}

func TestSlowListenerIsCutOffAndOthersCarryOn(t *testing.T) {
	fx := newFixture(t, "rain.mp3")
	apply(t, fx.e, "mix", LayerSpec{ID: "rain.mp3", Level: 0.5, Enabled: true})
	slow, _ := fx.e.Subscribe("mix") // never reads
	fast, _ := fx.e.Subscribe("mix")
	defer fast.Close()
	var got atomic.Int32
	go func() {
		for {
			select {
			case <-fast.C:
				got.Add(1)
			case <-fast.Done():
				return
			}
		}
	}()
	select {
	case <-slow.Done():
		if !errors.Is(slow.Err(), ErrTooSlow) {
			t.Fatalf("slow listener ended with %v", slow.Err())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a listener that never reads was not cut off")
	}
	before := got.Load()
	time.Sleep(100 * time.Millisecond)
	if got.Load() <= before {
		t.Fatal("the fast listener stopped receiving when the slow one was cut off")
	}
}

func TestEncoderCrashCutsListenersAndNextSubscribeRestarts(t *testing.T) {
	fx := newFixture(t, "rain.mp3")
	fx.enc.failAfter = 5
	apply(t, fx.e, "mix", LayerSpec{ID: "rain.mp3", Level: 0.5, Enabled: true})
	l, _ := fx.e.Subscribe("mix")
	select {
	case <-l.Done():
		if l.Err() == nil || !strings.Contains(l.Err().Error(), "crashed") {
			t.Fatalf("listener ended with %v, want the encoder's error", l.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a crashed encoder did not end the listener")
	}
	eventually(t, "streaming to be false", func() bool { st, _ := fx.e.Status("mix"); return !st.Streaming })

	fx.enc.failAfter = 0
	l2, _ := fx.e.Subscribe("mix")
	defer l2.Close()
	select {
	case <-l2.C:
	case <-time.After(3 * time.Second):
		t.Fatal("did not recover after a crash")
	}
}

func TestEncoderThatCannotStart(t *testing.T) {
	fx := newFixture(t, "rain.mp3")
	fx.enc.startErr = errors.New("ffmpeg not found")
	apply(t, fx.e, "mix", LayerSpec{ID: "rain.mp3", Level: 1, Enabled: true})
	l, _ := fx.e.Subscribe("mix")
	select {
	case <-l.Done():
		if l.Err() == nil {
			t.Fatal("expected an error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("listener was left hanging")
	}
}

func TestShutdownEndsStreams(t *testing.T) {
	fx := newFixture(t, "rain.mp3")
	apply(t, fx.e, "mix", LayerSpec{ID: "rain.mp3", Level: 1, Enabled: true})
	l, _ := fx.e.Subscribe("mix")
	<-l.C
	fx.cancel()
	select {
	case <-l.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("cancelling the engine did not end the stream")
	}
}

func TestRemoveStopsTheStreamCutsListenersAndFreesTheLayers(t *testing.T) {
	fx := newFixture(t, "rain.mp3")
	apply(t, fx.e, "mix", LayerSpec{ID: "rain.mp3", Level: 1, Enabled: true})
	apply(t, fx.e, "other", LayerSpec{ID: "rain.mp3", Level: 1, Enabled: true})
	l, _ := fx.e.Subscribe("mix")
	<-l.C
	m := fx.e.mixes["mix"]

	if !fx.e.Remove("mix") || fx.e.Remove("mix") {
		t.Fatal("Remove should report whether the soundscape existed")
	}
	select {
	case <-l.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the listener was not cut off")
	}
	if !errors.Is(l.Err(), ErrRemoved) {
		t.Fatalf("listener ended with %v, want ErrRemoved", l.Err())
	}
	eventually(t, "the encoder to stop", func() bool { return fx.enc.stops.Load() == 1 })
	if _, ok := fx.e.Status("mix"); ok {
		t.Fatal("the soundscape still exists")
	}
	if _, err := fx.e.Subscribe("mix"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("Subscribe after Remove = %v, want ErrUnknown", err)
	}
	// (The mixer may still list a layer: it drops faded-out layers while rendering, which has stopped.
	// Nothing holds the audio, and the unreferenced Mix is collected.)
	if len(m.held) != 0 {
		t.Fatalf("the removed soundscape still holds %d layers", len(m.held))
	}
	if st, ok := fx.e.Status("other"); !ok || st.Layers[0].State != StateReady && st.Layers[0].State != StateLoading {
		t.Fatalf("another soundscape was disturbed: %+v", st)
	}
	apply(t, fx.e, "mix", LayerSpec{ID: "rain.mp3", Level: 1, Enabled: true}) // the ID can be reused
}

func TestIDsAndUnknownSoundscapes(t *testing.T) {
	fx := newFixture(t)
	for _, bad := range []string{"", "../x", "a/b", "with space", ".hidden", strings.Repeat("a", 65), "ünï"} {
		if _, err := fx.e.Apply(bad, Spec{}); err == nil {
			t.Errorf("ID %q should be rejected", bad)
		}
	}
	for _, good := range []string{"night", "Night-2", "a.b_c", "0", strings.Repeat("a", 64)} {
		if _, err := fx.e.Apply(good, Spec{}); err != nil {
			t.Errorf("ID %q should be accepted: %v", good, err)
		}
	}
	if _, err := fx.e.Subscribe("nope"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := fx.e.Status("nope"); ok {
		t.Fatal("unknown soundscape has a status")
	}
	list := fx.e.List()
	for i := 1; i < len(list); i++ {
		if list[i-1].ID > list[i].ID {
			t.Fatal("List is not sorted")
		}
	}
}

func TestEngineConcurrentUse(t *testing.T) {
	fx := newFixture(t, "a.mp3", "b.mp3", "c.mp3")
	ids := []string{"a.mp3", "b.mp3", "c.mp3", "missing.mp3"}
	stop := time.Now().Add(400 * time.Millisecond)
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; time.Now().Before(stop); i++ {
				id := "mix" + itoa(i%2)
				switch (g + i) % 3 {
				case 0:
					var ls []LayerSpec
					for j, lid := range ids {
						if (i+j)%2 == 0 {
							ls = append(ls, LayerSpec{ID: lid, Level: float64(i%10) / 10, Enabled: i%3 != 0})
						}
					}
					_, _ = fx.e.Apply(id, Spec{Layers: ls})
				case 1:
					if l, err := fx.e.Subscribe(id); err == nil {
						select {
						case <-l.C:
						case <-time.After(5 * time.Millisecond):
						}
						l.Close()
					}
				default:
					fx.e.List()
				}
			}
		}(g)
	}
	wg.Wait()
}
