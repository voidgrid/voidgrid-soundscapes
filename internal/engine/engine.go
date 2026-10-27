package engine

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/voidgrid/voidgrid-soundscapes/internal/library"
)

// LayerSource finds layers by ID. *library.Library satisfies it.
type LayerSource interface {
	Get(id string) (library.Layer, bool)
}

// Options configure an Engine. The zero value of every field except Loop is a sensible default.
type Options struct {
	Loop       LoopOptions    // how files become loops
	Decode     Decoder        // default FFmpegDecode
	NewEncoder EncoderFactory // default StartFFmpegEncoder
	Grace      time.Duration  // how long the encoder outlives its last listener (default 5s)
	CacheIdle  time.Duration  // how long a decoded layer is kept after its last use (default 2m)
	Unpaced    bool           // mix as fast as possible instead of in real time (tests)
}

// ErrUnknown is returned for a soundscape that has not been defined.
var ErrUnknown = errors.New("unknown soundscape")

// ErrRemoved is the reason a listener is cut off when its soundscape is deleted.
var ErrRemoved = errors.New("soundscape removed")

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidID reports whether id is acceptable as a soundscape ID (it appears in a URL).
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Engine owns the soundscapes that can be streamed.
type Engine struct {
	ctx        context.Context
	lib        LayerSource
	cache      *Cache
	newEncoder EncoderFactory
	grace      time.Duration
	unpaced    bool
	log        *slog.Logger

	mu    sync.Mutex
	mixes map[string]*Mix
}

// New returns an engine. Everything it starts stops when ctx is cancelled.
func New(ctx context.Context, lib LayerSource, opt Options, log *slog.Logger) *Engine {
	if opt.Decode == nil {
		opt.Decode = FFmpegDecode
	}
	if opt.NewEncoder == nil {
		opt.NewEncoder = StartFFmpegEncoder
	}
	if opt.Grace == 0 {
		opt.Grace = 5 * time.Second
	}
	if opt.CacheIdle == 0 {
		opt.CacheIdle = 2 * time.Minute
	}
	return &Engine{
		ctx: ctx, lib: lib, newEncoder: opt.NewEncoder, grace: opt.Grace, unpaced: opt.Unpaced, log: log,
		cache: NewCache(ctx, opt.Decode, opt.Loop, opt.CacheIdle, log),
		mixes: map[string]*Mix{},
	}
}

// Apply defines or updates a soundscape. A new ID creates it.
func (e *Engine) Apply(id string, spec Spec) (Status, error) {
	if !ValidID(id) {
		return Status{}, errors.New("soundscape IDs are 1-64 letters, digits, '.', '_' or '-', starting with a letter or digit")
	}
	e.mu.Lock()
	m := e.mixes[id]
	if m == nil {
		m = newMix(id, e)
		e.mixes[id] = m
	}
	e.mu.Unlock()
	return m.Apply(spec), nil
}

// Remove deletes a soundscape: its listeners are cut off, its stream stops and its layers are released.
// It reports whether the soundscape existed.
func (e *Engine) Remove(id string) bool {
	e.mu.Lock()
	m := e.mixes[id]
	delete(e.mixes, id)
	e.mu.Unlock()
	if m == nil {
		return false
	}
	m.close()
	return true
}

// Status returns one soundscape's state.
func (e *Engine) Status(id string) (Status, bool) {
	e.mu.Lock()
	m := e.mixes[id]
	e.mu.Unlock()
	if m == nil {
		return Status{}, false
	}
	return m.Status(), true
}

// List returns every soundscape, sorted by ID.
func (e *Engine) List() []Status {
	e.mu.Lock()
	ms := make([]*Mix, 0, len(e.mixes))
	for _, m := range e.mixes {
		ms = append(ms, m)
	}
	e.mu.Unlock()
	out := make([]Status, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Subscribe starts listening to a soundscape.
func (e *Engine) Subscribe(id string) (*Listener, error) {
	e.mu.Lock()
	m := e.mixes[id]
	e.mu.Unlock()
	if m == nil {
		return nil, ErrUnknown
	}
	return m.Subscribe(), nil
}
