// Package store keeps the soundscapes in a JSON file in the config directory.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/voidgrid/voidgrid-soundscapes/internal/engine"
)

const (
	// FileName is the file inside the config directory.
	FileName = "soundscapes.json"
	// Version is the schema version written to the file. A file with a higher one is refused rather than
	// read wrongly (and then overwritten by the next save).
	Version = 1

	// DefaultDelay is how long a change may wait before it is written. A slider being dragged changes a
	// soundscape many times a second; this turns that into one write.
	DefaultDelay = time.Second

	maxIDLen = 64
)

// Soundscape is a named mix. The ID is part of its stream URL and never changes; the name is what people see.
type Soundscape struct {
	ID     string             `json:"id"`
	Name   string             `json:"name"`
	Layers []engine.LayerSpec `json:"layers"`
}

type file struct {
	Version     int          `json:"version"`
	Soundscapes []Soundscape `json:"soundscapes"`
}

// Store holds the soundscapes in memory and writes them out shortly after every change.
type Store struct {
	path  string
	delay time.Duration
	log   *slog.Logger

	mu     sync.Mutex
	items  map[string]Soundscape
	timer  *time.Timer // set while a write is waiting
	dirty  bool        // changes not yet written
	closed bool
	writes sync.WaitGroup // delayed writes that have started; Close waits for them

	writeMu sync.Mutex // one write at a time, so an older snapshot can never land after a newer one
}

// Open reads dir/soundscapes.json, creating dir if needed. A missing file is an empty store. A file that
// cannot be understood is an error and is left untouched, so a damaged or newer file is never overwritten.
// delay 0 means DefaultDelay.
func Open(dir string, delay time.Duration, log *slog.Logger) (*Store, error) {
	if delay <= 0 {
		delay = DefaultDelay
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("config directory: %w", err)
	}
	// Find out now, not at the first save an hour later, that nothing can be written here.
	probe, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return nil, fmt.Errorf("config directory %s is not writable: %w", dir, err)
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())

	s := &Store{path: filepath.Join(dir, FileName), delay: delay, log: log, items: map[string]Soundscape{}}
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", s.path, err)
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s is not valid: %w (fix or move the file; it has not been changed)", s.path, err)
	}
	if f.Version < 1 || f.Version > Version {
		return nil, fmt.Errorf("%s has schema version %d; this build understands up to %d (it has not been changed)", s.path, f.Version, Version)
	}
	for _, sc := range f.Soundscapes {
		if !engine.ValidID(sc.ID) {
			return nil, fmt.Errorf("%s: %q is not a valid soundscape ID (it has not been changed)", s.path, sc.ID)
		}
		if _, dup := s.items[sc.ID]; dup {
			return nil, fmt.Errorf("%s: soundscape %q appears twice (it has not been changed)", s.path, sc.ID)
		}
		if sc.Name == "" {
			sc.Name = sc.ID
		}
		s.items[sc.ID] = clone(sc)
	}
	return s, nil
}

func clone(sc Soundscape) Soundscape {
	sc.Layers = append([]engine.LayerSpec{}, sc.Layers...) // also makes nil a non-nil empty list
	return sc
}

// List returns every soundscape, ordered by name and then ID.
func (s *Store) List() []Soundscape {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

func (s *Store) listLocked() []Soundscape {
	out := make([]Soundscape, 0, len(s.items))
	for _, sc := range s.items {
		out = append(out, clone(sc))
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Get returns one soundscape.
func (s *Store) Get(id string) (Soundscape, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.items[id]
	return clone(sc), ok
}

// Put creates or replaces a soundscape and schedules a write. The ID must already be valid.
func (s *Store) Put(sc Soundscape) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[sc.ID] = clone(sc)
	s.dirtyLocked()
}

// Delete removes a soundscape and reports whether it existed.
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[id]; !ok {
		return false
	}
	delete(s.items, id)
	s.dirtyLocked()
	return true
}

// NewID returns an unused ID made from name, such as "night-rain" for "Night rain".
func (s *Store) NewID(name string) string {
	base := slug(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	id := base
	for n := 2; ; n++ {
		if _, taken := s.items[id]; !taken {
			return id
		}
		suffix := "-" + strconv.Itoa(n)
		id = base[:min(len(base), maxIDLen-len(suffix))] + suffix
	}
}

// slug lowercases name and turns every run of other characters into one '-'.
func slug(name string) string {
	var b strings.Builder
	dash := true // no leading dash
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if len(out) > maxIDLen {
		out = strings.TrimRight(out[:maxIDLen], "-")
	}
	if out == "" {
		return "soundscape"
	}
	return out
}

// dirtyLocked records a change and schedules a write unless one is already waiting. The timer is not pushed
// back by further changes, so a continuous stream of them still reaches the disk once per delay.
func (s *Store) dirtyLocked() {
	s.dirty = true
	if s.timer != nil || s.closed {
		return
	}
	s.timer = time.AfterFunc(s.delay, s.delayedWrite)
}

func (s *Store) delayedWrite() {
	s.mu.Lock()
	if s.closed { // Close writes whatever is still unsaved
		s.mu.Unlock()
		return
	}
	s.timer = nil
	s.writes.Add(1) // under mu, while not closed, so it cannot race with Close's Wait
	s.mu.Unlock()
	defer s.writes.Done()
	if err := s.Flush(); err != nil {
		s.log.Error("cannot save soundscapes; will retry", "err", err)
		s.mu.Lock()
		s.dirtyLocked()
		s.mu.Unlock()
	}
}

// Flush writes the current state now. The file is replaced atomically: a crash leaves the old or the new
// content, never a mix.
func (s *Store) Flush() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	data, err := json.MarshalIndent(file{Version: Version, Soundscapes: s.listLocked()}, "", "  ")
	s.dirty = false
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := writeAtomic(s.path, append(data, '\n')); err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return err
	}
	return nil
}

// Close writes any change that has not been saved yet and waits for a write already under way, so nothing
// is written after it returns.
func (s *Store) Close() error {
	s.mu.Lock()
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.mu.Unlock()
	s.writes.Wait()
	s.mu.Lock()
	dirty := s.dirty
	s.mu.Unlock()
	if !dirty {
		return nil
	}
	return s.Flush()
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".soundscapes-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	if d, err := os.Open(dir); err == nil { // make the rename itself durable; best effort
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
