package engine

import (
	"math"
	"sync"
)

// rampSeconds is how long a gain change takes from silence to full level. It is short enough to feel
// immediate and long enough that a slider drag or a switch never clicks.
const rampSeconds = 0.05

// softKnee is the fraction of full scale below which the limiter does nothing.
const softKnee = 0.9

type voice struct {
	loop    []int16 // nil until the layer has been decoded
	pos     int
	gain    float32
	target  float32
	removed bool // fade out, then forget
	// dropWhenSilent frees the audio once the layer is off and has faded out, so switching a layer off
	// gives its memory back. It is loaded again, by SetLoop, when the layer is switched on.
	dropWhenSilent bool
}

// Mixer sums looping layers with a live gain per layer. Set, SetLoop and Remove may be called from any
// goroutine while Render runs on another.
type Mixer struct {
	mu     sync.Mutex
	voices map[string]*voice
	order  []*voice // creation order, so the float sum is deterministic
	acc    []float32
}

// NewMixer returns an empty mixer, which renders silence.
func NewMixer() *Mixer {
	return &Mixer{voices: map[string]*voice{}, acc: make([]float32, ChunkSamples)}
}

// Set creates the layer if needed and sets its level (0 to 1, linear) and whether it is on. The gain moves
// to the new value over a few tens of milliseconds.
func (m *Mixer) Set(id string, level float64, enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.voices[id]
	if v == nil {
		v = &voice{}
		m.voices[id] = v
		m.order = append(m.order, v)
	}
	v.removed = false
	v.target = 0
	v.dropWhenSilent = !enabled
	if enabled {
		v.target = float32(clampLevel(level))
	}
}

// SetLoop supplies, or replaces, the audio for a layer. It is ignored for a layer that is not in the
// mixer. When the audio is replaced the position is kept, wrapped to the new length.
func (m *Mixer) SetLoop(id string, loop []int16) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v := m.voices[id]; v != nil {
		v.loop = loop
		if len(loop) > 0 {
			v.pos %= len(loop)
		} else {
			v.pos = 0
		}
	}
}

// IDs returns the IDs of all layers in the mixer, including ones that are fading out.
func (m *Mixer) IDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.voices))
	for id := range m.voices {
		ids = append(ids, id)
	}
	return ids
}

// Loaded reports whether the layer has audio.
func (m *Mixer) Loaded(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.voices[id]
	return v != nil && len(v.loop) > 0
}

// Remove fades the layer out and then forgets it.
func (m *Mixer) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v := m.voices[id]; v != nil {
		v.target = 0
		v.removed = true
	}
}

// Audible returns how many layers are currently contributing sound.
func (m *Mixer) Audible() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, v := range m.order {
		if v.loop != nil && (v.gain > 0 || v.target > 0) {
			n++
		}
	}
	return n
}

// Render mixes the next len(dst) samples (at most ChunkSamples) into dst.
func (m *Mixer) Render(dst []int16) {
	m.mu.Lock()
	defer m.mu.Unlock()

	n := len(dst)
	if n > len(m.acc) {
		m.acc = make([]float32, n)
	}
	acc := m.acc[:n]
	for i := range acc {
		acc[i] = 0
	}

	const step = float32(1 / (SampleRate * rampSeconds))
	kept := m.order[:0]
	for _, v := range m.order {
		if v.removed && v.gain == 0 {
			delete(m.voices, m.keyOf(v))
			continue
		}
		kept = append(kept, v)
		if v.gain == 0 && v.target == 0 && v.dropWhenSilent && v.loop != nil {
			v.loop, v.pos = nil, 0
		}
		if len(v.loop) == 0 || (v.gain == 0 && v.target == 0) {
			continue
		}
		pos, g, t, loop := v.pos, v.gain, v.target, v.loop
		for i := range acc {
			switch {
			case g < t:
				if g += step; g > t {
					g = t
				}
			case g > t:
				if g -= step; g < t {
					g = t
				}
			}
			acc[i] += float32(loop[pos]) * g
			if pos++; pos == len(loop) {
				pos = 0
			}
		}
		v.pos, v.gain = pos, g
	}
	m.order = kept

	for i, s := range acc {
		dst[i] = softClip(s)
	}
}

// keyOf finds the map key of v. Removal is rare, so a scan is fine.
func (m *Mixer) keyOf(v *voice) string {
	for k, x := range m.voices {
		if x == v {
			return k
		}
	}
	return ""
}

// softClip passes quiet samples through unchanged and eases loud ones towards full scale instead of
// hard-clipping them, which sounds much harsher when several layers add up.
func softClip(x float32) int16 {
	const full = float32(math.MaxInt16)
	const knee = full * softKnee
	a := x
	if a < 0 {
		a = -a
	}
	if a <= knee {
		return int16(math.Round(float64(x)))
	}
	room := full - knee
	out := knee + room*float32(math.Tanh(float64((a-knee)/room)))
	if x < 0 {
		out = -out
	}
	return int16(math.Round(float64(out)))
}

func clampLevel(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
