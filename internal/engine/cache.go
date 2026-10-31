package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/voidgrid/voidgrid-soundscapes/internal/library"
)

// decodeSlots bounds how many files are decoded at once; each decode holds a whole file in memory.
const decodeSlots = 2

type entry struct {
	key   string
	ready chan struct{} // closed when loop or err is set
	loop  []int16
	err   error
	refs  int
	idle  *time.Timer
}

// Cache holds decoded, loop-ready audio so a layer used by several soundscapes is decoded once. An entry
// is kept while anything holds it and for idleTTL afterwards, so switching a layer off and on again is
// quick. A failed decode is forgotten as soon as it is released, so fixing the file and trying again works.
type Cache struct {
	ctx     context.Context
	decode  Decoder
	opt     LoopOptions
	idleTTL time.Duration
	log     *slog.Logger
	slots   chan struct{}

	mu sync.Mutex
	m  map[string]*entry
}

// NewCache returns a cache whose background decodes stop when ctx is cancelled.
func NewCache(ctx context.Context, decode Decoder, opt LoopOptions, idleTTL time.Duration, log *slog.Logger) *Cache {
	return &Cache{ctx: ctx, decode: decode, opt: opt, idleTTL: idleTTL, log: log,
		slots: make(chan struct{}, decodeSlots), m: map[string]*entry{}}
}

// Handle is a hold on one cache entry. Release it when the audio is no longer needed.
type Handle struct {
	c    *Cache
	e    *entry
	once sync.Once
}

// key identifies a layer's content: replacing the file changes its size or time, which means a new entry.
func key(l library.Layer) string {
	return fmt.Sprintf("%s|%d|%d", l.ID, l.Size, l.ModTime.UnixNano())
}

// Acquire starts decoding l if nobody has, and returns a handle to wait on.
func (c *Cache) Acquire(l library.Layer) *Handle {
	k := key(l)
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.m[k]
	if e == nil {
		e = &entry{key: k, ready: make(chan struct{})}
		c.m[k] = e
		go c.load(e, l)
	}
	e.refs++
	if e.idle != nil {
		e.idle.Stop()
		e.idle = nil
	}
	return &Handle{c: c, e: e}
}

func (c *Cache) load(e *entry, l library.Layer) {
	defer close(e.ready)
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-c.ctx.Done():
		e.err = c.ctx.Err()
		return
	}
	pcm, err := c.decode(c.ctx, l.Path, l.DurationSec)
	if err == nil {
		e.loop, err = BuildLoopConsuming(pcm, c.opt)
	}
	e.err = err
	if err != nil {
		c.log.Warn("cannot load layer", "id", l.ID, "err", err)
		return
	}
	c.log.Debug("layer loaded", "id", l.ID, "seconds", float64(len(e.loop))/SampleRate)
}

// Len returns how many entries are held.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// Done is closed when the audio is ready or has failed.
func (h *Handle) Done() <-chan struct{} { return h.e.ready }

// Ready reports whether the audio is loaded successfully.
func (h *Handle) Ready() bool {
	select {
	case <-h.e.ready:
		return h.e.err == nil
	default:
		return false
	}
}

// Failed reports whether loading finished with an error.
func (h *Handle) Failed() bool {
	select {
	case <-h.e.ready:
		return h.e.err != nil
	default:
		return false
	}
}

// Result returns the loop, or the error. It must only be called after Done is closed.
func (h *Handle) Result() ([]int16, error) { return h.e.loop, h.e.err }

// Release gives the hold back. It is safe to call more than once.
func (h *Handle) Release() {
	h.once.Do(func() {
		c, e := h.c, h.e
		c.mu.Lock()
		defer c.mu.Unlock()
		if e.refs--; e.refs > 0 {
			return
		}
		failed := false
		select {
		case <-e.ready:
			failed = e.err != nil
		default:
		}
		if failed || c.idleTTL <= 0 {
			delete(c.m, e.key)
			return
		}
		e.idle = time.AfterFunc(c.idleTTL, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if e.refs == 0 && c.m[e.key] == e {
				delete(c.m, e.key)
			}
		})
	})
}
