package engine

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-soundscapes/internal/library"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(discardW{}, nil)) }

type discardW struct{}

func (discardW) Write(p []byte) (int, error) { return len(p), nil }

func layer(id string) library.Layer {
	return library.Layer{ID: id, Path: "/fake/" + id, Size: 100, ModTime: time.Unix(1000, 0)}
}

var testOpt = LoopOptions{Crossfade: 0, SilenceThreshold: 0}

func TestCacheDecodesOnceForManyHolders(t *testing.T) {
	var calls atomic.Int32
	c := NewCache(context.Background(), func(context.Context, string) ([]int16, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return dc(4000, 500), nil
	}, testOpt, time.Minute, testLogger())

	var wg sync.WaitGroup
	handles := make([]*Handle, 8)
	for i := range handles {
		wg.Add(1)
		go func(i int) { defer wg.Done(); handles[i] = c.Acquire(layer("rain.mp3")) }(i)
	}
	wg.Wait()
	for _, h := range handles {
		<-h.Done()
		if loop, err := h.Result(); err != nil || len(loop) != 4000 {
			t.Fatalf("loop %d err %v", len(loop), err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("decoded %d times, want 1", calls.Load())
	}
}

func TestCacheKeepsEntryBrieflyThenFreesIt(t *testing.T) {
	var calls atomic.Int32
	c := NewCache(context.Background(), func(context.Context, string) ([]int16, error) {
		calls.Add(1)
		return dc(100, 500), nil
	}, testOpt, 60*time.Millisecond, testLogger())

	h := c.Acquire(layer("a.mp3"))
	<-h.Done()
	h.Release()
	h.Release() // releasing twice is harmless

	h2 := c.Acquire(layer("a.mp3")) // within the idle window: no second decode
	<-h2.Done()
	h2.Release()
	if calls.Load() != 1 {
		t.Fatalf("decoded %d times inside the idle window, want 1", calls.Load())
	}
	time.Sleep(200 * time.Millisecond)
	if c.Len() != 0 {
		t.Fatalf("entry still held after the idle time (%d)", c.Len())
	}
	h3 := c.Acquire(layer("a.mp3"))
	<-h3.Done()
	if calls.Load() != 2 {
		t.Fatalf("decoded %d times, want 2 after eviction", calls.Load())
	}
}

func TestCacheForgetsFailuresAtOnce(t *testing.T) {
	var calls atomic.Int32
	c := NewCache(context.Background(), func(context.Context, string) ([]int16, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("corrupt")
		}
		return dc(100, 500), nil
	}, testOpt, time.Minute, testLogger())

	h := c.Acquire(layer("a.mp3"))
	<-h.Done()
	if !h.Failed() || h.Ready() {
		t.Fatal("first load should have failed")
	}
	h.Release()
	if c.Len() != 0 {
		t.Fatal("a failed entry must not be kept")
	}
	h2 := c.Acquire(layer("a.mp3"))
	<-h2.Done()
	if !h2.Ready() {
		t.Fatal("second attempt should succeed")
	}
}

func TestCacheNewEntryWhenFileChanges(t *testing.T) {
	var calls atomic.Int32
	c := NewCache(context.Background(), func(context.Context, string) ([]int16, error) {
		calls.Add(1)
		return dc(100, 500), nil
	}, testOpt, time.Minute, testLogger())
	l := layer("a.mp3")
	h1 := c.Acquire(l)
	l.ModTime = l.ModTime.Add(time.Second)
	h2 := c.Acquire(l)
	<-h1.Done()
	<-h2.Done()
	if calls.Load() != 2 {
		t.Fatalf("a replaced file must be decoded again, got %d decodes", calls.Load())
	}
}

func TestCacheSilentFileIsAFailure(t *testing.T) {
	c := NewCache(context.Background(), func(context.Context, string) ([]int16, error) {
		return make([]int16, 1000), nil
	}, DefaultLoopOptions, time.Minute, testLogger())
	h := c.Acquire(layer("quiet.mp3"))
	<-h.Done()
	if _, err := h.Result(); !errors.Is(err, ErrSilent) {
		t.Fatalf("err = %v, want ErrSilent", err)
	}
}

func TestCacheLimitsConcurrentDecodes(t *testing.T) {
	var running, peak atomic.Int32
	c := NewCache(context.Background(), func(context.Context, string) ([]int16, error) {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		running.Add(-1)
		return dc(100, 500), nil
	}, testOpt, time.Minute, testLogger())
	var hs []*Handle
	for i := 0; i < 8; i++ {
		hs = append(hs, c.Acquire(layer(string(rune('a'+i))+".mp3")))
	}
	for _, h := range hs {
		<-h.Done()
	}
	if peak.Load() > decodeSlots {
		t.Fatalf("%d decodes ran at once, limit is %d", peak.Load(), decodeSlots)
	}
}
