package engine

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

// Spec says which layers a soundscape plays and how loud. Applying it is idempotent: it always describes
// the whole mix, so sending it twice, or out of order, ends in the last one sent.
type Spec struct {
	Layers []LayerSpec `json:"layers"`
}

// LayerSpec is one layer in a soundscape.
type LayerSpec struct {
	ID      string  `json:"id"`
	Level   float64 `json:"level"` // 0 to 1, linear
	Enabled bool    `json:"enabled"`
}

// Layer states reported by Status.
const (
	StateOff      = "off"      // switched off
	StateLoading  = "loading"  // being decoded; silent until ready
	StateReady    = "ready"    // loaded (audible if switched on)
	StateMissing  = "missing"  // not in the library
	StateUnusable = "unusable" // in the library but not usable audio
	StateError    = "error"    // decoding failed
)

// LayerStatus is a layer's requested settings plus whether it can actually be heard.
type LayerStatus struct {
	ID      string  `json:"id"`
	Level   float64 `json:"level"`
	Enabled bool    `json:"enabled"`
	State   string  `json:"state"`
	Error   string  `json:"error,omitempty"`
}

// Status describes a soundscape's mix.
type Status struct {
	ID        string        `json:"id"`
	Listeners int           `json:"listeners"`
	Streaming bool          `json:"streaming"`
	Layers    []LayerStatus `json:"layers"`
}

// ErrTooSlow is the reason a listener is cut off when it cannot keep up with the stream.
var ErrTooSlow = errors.New("listener too slow")

// listenerBuffer is how many MP3 frames a listener may fall behind before it is cut off (about 13 s).
const listenerBuffer = 512

// Listener receives the MP3 frames of one soundscape. Read frames from C until Done is closed.
type Listener struct {
	C    chan []byte
	mix  *Mix
	done chan struct{}
	once sync.Once
	err  error
}

// Done is closed when the listener has been closed or cut off.
func (l *Listener) Done() <-chan struct{} { return l.done }

// Err says why the listener ended: nil if it called Close, otherwise the reason.
func (l *Listener) Err() error { return l.err }

func (l *Listener) fail(err error) { l.once.Do(func() { l.err = err; close(l.done) }) }

// Close stops receiving. It is safe to call more than once.
func (l *Listener) Close() { l.mix.remove(l) }

type held struct {
	h   *Handle
	key string
}

type layerState struct{ state, err string }

// Mix is one soundscape: a Mixer, an encoder that runs while someone is listening, and the listeners.
type Mix struct {
	id    string
	e     *Engine
	mixer *Mixer

	mu        sync.Mutex
	layers    []LayerSpec
	held      map[string]held
	problems  map[string]layerState
	listeners map[*Listener]struct{}
	running   bool
	closed    bool // removed for good; a late refresh must not load its layers again
	runID     int
	cancel    context.CancelFunc
	grace     *time.Timer
}

func newMix(id string, e *Engine) *Mix {
	return &Mix{id: id, e: e, mixer: NewMixer(), held: map[string]held{},
		problems: map[string]layerState{}, listeners: map[*Listener]struct{}{}}
}

// Apply makes the mix play spec. It never fails because of one bad layer: that layer reports a state
// instead and the rest play.
func (m *Mix) Apply(spec Spec) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applyLocked(spec)
}

// refresh applies the current mix again, to pick up files that appeared, changed or went away.
func (m *Mix) refresh() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.applyLocked(Spec{Layers: m.layers})
}

func (m *Mix) applyLocked(spec Spec) Status {
	cleaned := make([]LayerSpec, 0, len(spec.Layers))
	want := map[string]bool{}
	for _, ls := range spec.Layers {
		if ls.ID == "" || want[ls.ID] {
			continue
		}
		ls.Level = clampLevel(ls.Level)
		want[ls.ID] = true
		cleaned = append(cleaned, ls)
	}
	m.layers = cleaned

	// layers that are no longer part of the soundscape
	for id := range m.held {
		if !want[id] {
			m.releaseLocked(id)
		}
	}
	for id := range m.problems {
		if !want[id] {
			delete(m.problems, id)
		}
	}
	for _, id := range m.mixer.IDs() {
		if !want[id] {
			m.mixer.Remove(id)
		}
	}

	for _, ls := range cleaned {
		delete(m.problems, ls.ID)
		layer, ok := m.e.lib.Get(ls.ID)
		switch {
		case !ok:
			m.problems[ls.ID] = layerState{StateMissing, "not in the library"}
			m.releaseLocked(ls.ID)
			m.mixer.Remove(ls.ID)
			continue
		case !layer.Playable():
			m.problems[ls.ID] = layerState{StateUnusable, layer.Error}
			m.releaseLocked(ls.ID)
			m.mixer.Remove(ls.ID)
			continue
		}

		m.mixer.Set(ls.ID, ls.Level, ls.Enabled)
		if !ls.Enabled {
			m.releaseLocked(ls.ID)
			continue
		}
		k := key(layer)
		if cur, ok := m.held[ls.ID]; ok && cur.key == k && !cur.h.Failed() {
			continue // already loading or loaded; a failed load is retried
		}
		m.releaseLocked(ls.ID) // the file was replaced: load the new one
		h := m.e.cache.Acquire(layer)
		m.held[ls.ID] = held{h: h, key: k}
		go m.await(ls.ID, h)
	}
	if !m.running {
		// Nobody is listening, so nothing is rendered and there is no fade to wait for: give back the audio of
		// layers that were switched off or removed now, not when the next listener connects.
		m.mixer.Settle()
	}
	return m.statusLocked()
}

// await hands the loop to the mixer once it has loaded.
func (m *Mix) await(id string, h *Handle) {
	select {
	case <-h.Done():
	case <-m.e.ctx.Done():
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.held[id]; !ok || cur.h != h {
		return // superseded while loading
	}
	loop, err := h.Result()
	if err != nil {
		m.problems[id] = layerState{StateError, err.Error()}
		return
	}
	m.mixer.SetLoop(id, loop)
}

func (m *Mix) releaseLocked(id string) {
	if h, ok := m.held[id]; ok {
		h.h.Release()
		delete(m.held, id)
	}
}

// close ends the soundscape for good: no more listeners, no encoder, no held layers.
func (m *Mix) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for id := range m.held {
		m.releaseLocked(id)
	}
	for _, id := range m.mixer.IDs() {
		m.mixer.Remove(id)
	}
	m.layers, m.problems = nil, map[string]layerState{}
	if m.grace != nil {
		m.grace.Stop()
		m.grace = nil
	}
	for l := range m.listeners {
		l.fail(ErrRemoved)
	}
	m.listeners = map[*Listener]struct{}{}
	if m.cancel != nil {
		m.cancel() // run's finish then marks the stream stopped
	}
}

// Status returns the current layer states.
func (m *Mix) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusLocked()
}

func (m *Mix) statusLocked() Status {
	st := Status{ID: m.id, Listeners: len(m.listeners), Streaming: m.running, Layers: make([]LayerStatus, 0, len(m.layers))}
	for _, ls := range m.layers {
		s := LayerStatus{ID: ls.ID, Level: ls.Level, Enabled: ls.Enabled}
		switch p, bad := m.problems[ls.ID]; {
		case bad:
			s.State, s.Error = p.state, p.err
		case !ls.Enabled:
			s.State = StateOff
		case m.mixer.Loaded(ls.ID):
			s.State = StateReady
		default:
			s.State = StateLoading
		}
		st.Layers = append(st.Layers, s)
	}
	return st
}

// Subscribe starts the stream if it is not running and returns a listener for it.
func (m *Mix) Subscribe() *Listener {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := &Listener{C: make(chan []byte, listenerBuffer), mix: m, done: make(chan struct{})}
	m.listeners[l] = struct{}{}
	if m.grace != nil {
		m.grace.Stop()
		m.grace = nil
	}
	if !m.running {
		m.startLocked()
	}
	return l
}

func (m *Mix) remove(l *Listener) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.listeners, l)
	l.fail(nil)
	if len(m.listeners) == 0 && m.running && m.grace == nil {
		// Keep the encoder a few seconds so a player that reconnects straight away does not pay for a restart.
		m.grace = time.AfterFunc(m.e.grace, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if len(m.listeners) == 0 && m.running {
				// Marked stopped here, not when the encoder has exited: a listener that connects while it is
				// exiting then starts a new run instead of joining the one that is about to cut it off.
				m.cancel()
				m.cancel = nil
				m.running = false
				m.mixer.Settle()
				m.e.log.Info("stream stopped", "soundscape", m.id)
			}
			m.grace = nil
		})
	}
}

func (m *Mix) startLocked() {
	ctx, cancel := context.WithCancel(m.e.ctx)
	m.runID++
	m.running = true
	m.cancel = cancel
	go m.run(ctx, m.runID)
}

// run mixes and encodes in real time while the stream is wanted.
func (m *Mix) run(ctx context.Context, id int) {
	var err error
	defer func() { m.finish(id, err) }()

	enc, err := m.e.newEncoder(ctx)
	if err != nil {
		m.e.log.Error("cannot start encoder", "soundscape", m.id, "err", err)
		return
	}
	m.e.log.Info("streaming", "soundscape", m.id)

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		var fs FrameSplitter
		buf := make([]byte, 4096)
		for {
			n, rerr := enc.Out.Read(buf)
			for _, f := range fs.Write(buf[:n]) {
				m.broadcast(id, f)
			}
			if rerr != nil {
				return
			}
		}
	}()

	pcm := make([]int16, ChunkSamples)
	raw := make([]byte, ChunkSamples*2)
	step := time.Duration(ChunkSamples) * time.Second / SampleRate
	next := time.Now()
	for ctx.Err() == nil {
		m.mixer.Render(pcm)
		for i, s := range pcm {
			raw[2*i], raw[2*i+1] = byte(s&0xff), byte((s>>8)&0xff)
		}
		if _, werr := enc.In.Write(raw); werr != nil {
			break
		}
		if !m.e.unpaced {
			next = next.Add(step)
			d := time.Until(next)
			if d < -time.Second {
				next = time.Now() // fell far behind (the host stalled): do not burst to catch up
			} else if d > 0 {
				select {
				case <-time.After(d):
				case <-ctx.Done():
				}
			}
		}
	}
	_ = enc.In.Close()
	if werr := enc.Wait(); werr != nil && ctx.Err() == nil {
		err = werr
		m.e.log.Error("encoder stopped", "soundscape", m.id, "err", werr)
	}
	<-readDone
	if err == nil && ctx.Err() == nil {
		err = io.ErrUnexpectedEOF
	}
}

// finish records that a run ended and, if it ended on its own, cuts off the listeners so they reconnect.
func (m *Mix) finish(id int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runID != id || !m.running {
		return // a newer run has taken over, or this one was already marked stopped when its grace ran out
	}
	m.running = false
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	if err == nil {
		err = io.EOF
	}
	for l := range m.listeners {
		l.fail(err)
	}
	m.listeners = map[*Listener]struct{}{}
	m.mixer.Settle()
	m.e.log.Info("stream stopped", "soundscape", m.id)
}

// broadcast hands a frame of run id to the listeners. Frames from a run that has been stopped or replaced
// are dropped, so the last output of an encoder that is exiting never lands in a newer run's stream.
func (m *Mix) broadcast(id int, frame []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runID != id || !m.running {
		return
	}
	for l := range m.listeners {
		select {
		case l.C <- frame:
		default:
			delete(m.listeners, l)
			l.fail(ErrTooSlow)
			m.e.log.Warn("listener cut off: too slow", "soundscape", m.id)
		}
	}
}
