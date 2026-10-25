// Package library scans the audio directory and keeps a list of the layers found in it.
//
// A layer is one audio file. Its ID is the file's path relative to the audio root with forward slashes
// (for example "rain/heavy.mp3"), so two files with the same name in different folders never collide.
// The first-level folder is the layer's category, used only to group layers in the UI.
//
// Changes are found by polling, not by inotify, because inotify does not see changes on network mounts and
// is unreliable across Docker bind mounts.
package library

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// extensions are the file types treated as audio. ffprobe decides whether a file with one of these is usable.
var extensions = map[string]bool{
	".mp3": true, ".wav": true, ".flac": true, ".ogg": true, ".oga": true, ".opus": true,
	".m4a": true, ".aac": true, ".wma": true, ".aif": true, ".aiff": true,
}

const (
	// settleTime is how long a file must have gone unmodified before it is probed, so a file that is still
	// being copied or downloaded is not read half-written.
	settleTime = 2 * time.Second
	// probeWorkers bounds how many ffprobe processes run at once.
	probeWorkers = 4
)

// Layer is one audio file in the library.
type Layer struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Category    string    `json:"category"`
	Path        string    `json:"-"`
	Size        int64     `json:"size"`
	ModTime     time.Time `json:"modified"`
	DurationSec float64   `json:"duration"`
	Codec       string    `json:"codec"`
	SampleRate  int       `json:"sampleRate"`
	Channels    int       `json:"channels"`
	// Error is set when the file could not be used (not audio, corrupt, unreadable). Such layers are listed so
	// the UI can say so, but must not be mixed.
	Error string `json:"error,omitempty"`
}

// Playable reports whether the layer can be mixed.
func (l Layer) Playable() bool { return l.Error == "" }

// Diff lists the IDs that changed between two scans.
type Diff struct {
	Added, Removed, Changed []string
}

// Empty reports whether nothing changed.
func (d Diff) Empty() bool { return len(d.Added)+len(d.Removed)+len(d.Changed) == 0 }

// Library is a thread-safe view of the audio directory.
type Library struct {
	root   string
	probe  ProbeFunc
	settle time.Duration
	log    *slog.Logger

	mu     sync.RWMutex
	layers map[string]Layer
}

// New returns a library rooted at root. A nil probe uses ffprobe; a nil log discards logs.
func New(root string, probe ProbeFunc, log *slog.Logger) *Library {
	if probe == nil {
		probe = FFProbe
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return &Library{root: root, probe: probe, settle: settleTime, log: log, layers: map[string]Layer{}}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// Layers returns all layers sorted by category, then by ID.
func (lib *Library) Layers() []Layer {
	lib.mu.RLock()
	out := make([]Layer, 0, len(lib.layers))
	for _, l := range lib.layers {
		out = append(out, l)
	}
	lib.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		ci, cj := strings.ToLower(out[i].Category), strings.ToLower(out[j].Category)
		if ci != cj {
			return ci < cj
		}
		return strings.ToLower(out[i].ID) < strings.ToLower(out[j].ID)
	})
	return out
}

// Get returns the layer with the given ID. The ID is only ever looked up in the scanned set, never joined
// onto the root, so a crafted ID cannot reach a file outside the library.
func (lib *Library) Get(id string) (Layer, bool) {
	lib.mu.RLock()
	defer lib.mu.RUnlock()
	l, ok := lib.layers[id]
	return l, ok
}

type found struct {
	id   string
	path string
	size int64
	mod  time.Time
}

// Scan reads the directory once and updates the library. If the root cannot be read (for example an
// unmounted volume) it returns an error and leaves the previous layers untouched, so a flaky mount does not
// wipe soundscapes.
func (lib *Library) Scan(ctx context.Context) (Diff, error) {
	prev := lib.snapshot()
	files, kept, err := lib.walk(prev)
	if err != nil {
		return Diff{}, err
	}

	next := make(map[string]Layer, len(files)+len(kept))
	for id, l := range kept {
		next[id] = l
	}

	type job struct{ f found }
	var toProbe []job
	for _, f := range files {
		if p, ok := prev[f.id]; ok && p.Size == f.size && p.ModTime.Equal(f.mod) {
			next[f.id] = p // unchanged: keep the earlier probe result, error included
			continue
		}
		toProbe = append(toProbe, job{f})
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, probeWorkers)
	for _, j := range toProbe {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(f found) {
			defer wg.Done()
			defer func() { <-sem }()
			l := newLayer(f)
			info, perr := lib.probe(ctx, f.path)
			if perr != nil {
				l.Error = perr.Error()
				lib.log.Warn("unusable audio file", "id", f.id, "err", perr)
			} else {
				l.DurationSec, l.Codec, l.SampleRate, l.Channels = info.Duration, info.Codec, info.SampleRate, info.Channels
			}
			mu.Lock()
			next[f.id] = l
			mu.Unlock()
		}(j.f)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return Diff{}, err // a cancelled scan applies nothing
	}

	lib.mu.Lock()
	lib.layers = next
	lib.mu.Unlock()
	return diff(prev, next), nil
}

func (lib *Library) snapshot() map[string]Layer {
	lib.mu.RLock()
	defer lib.mu.RUnlock()
	m := make(map[string]Layer, len(lib.layers))
	for k, v := range lib.layers {
		m[k] = v
	}
	return m
}

// walk lists the audio files under the root. kept holds layers that were seen before but are being
// rewritten right now (not yet settled); they keep their earlier entry instead of vanishing.
func (lib *Library) walk(prev map[string]Layer) (files []found, kept map[string]Layer, err error) {
	if _, serr := os.Stat(lib.root); serr != nil {
		return nil, nil, serr
	}
	kept = map[string]Layer{}
	err = filepath.WalkDir(lib.root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			if path == lib.root {
				return werr
			}
			lib.log.Warn("cannot read", "path", path, "err", werr)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if path != lib.root && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !extensions[strings.ToLower(filepath.Ext(d.Name()))] {
			return nil
		}
		fi, serr := os.Stat(path) // follows a symlink to a file; WalkDir itself never descends into symlinked directories
		if serr != nil || !fi.Mode().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(lib.root, path)
		if rerr != nil {
			return nil
		}
		id := filepath.ToSlash(rel)
		if time.Since(fi.ModTime()) < lib.settle {
			if p, ok := prev[id]; ok {
				kept[id] = p
			}
			return nil
		}
		files = append(files, found{id: id, path: path, size: fi.Size(), mod: fi.ModTime()})
		return nil
	})
	return files, kept, err
}

func newLayer(f found) Layer {
	base := filepath.Base(f.path)
	l := Layer{
		ID:      f.id,
		Name:    strings.TrimSuffix(base, filepath.Ext(base)),
		Path:    f.path,
		Size:    f.size,
		ModTime: f.mod,
	}
	if i := strings.Index(f.id, "/"); i >= 0 {
		l.Category = f.id[:i]
	}
	return l
}

func diff(prev, next map[string]Layer) Diff {
	var d Diff
	for id, n := range next {
		p, ok := prev[id]
		switch {
		case !ok:
			d.Added = append(d.Added, id)
		case p.Size != n.Size || !p.ModTime.Equal(n.ModTime):
			d.Changed = append(d.Changed, id)
		}
	}
	for id := range prev {
		if _, ok := next[id]; !ok {
			d.Removed = append(d.Removed, id)
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Removed)
	sort.Strings(d.Changed)
	return d
}

// Run scans immediately and then every interval until ctx is cancelled. onChange, if not nil, is called after
// any scan that changed something.
func (lib *Library) Run(ctx context.Context, interval time.Duration, onChange func(Diff)) {
	lastErr := ""
	scan := func() {
		d, err := lib.Scan(ctx)
		if err != nil {
			// Log a failure once, not on every poll, so an unmounted volume does not flood the log.
			if !errors.Is(err, context.Canceled) && err.Error() != lastErr {
				lib.log.Error("scan failed", "root", lib.root, "err", err)
			}
			lastErr = err.Error()
			return
		}
		if lastErr != "" {
			lib.log.Info("scan recovered", "root", lib.root)
			lastErr = ""
		}
		if !d.Empty() && onChange != nil {
			onChange(d)
		}
	}
	scan()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			scan()
		}
	}
}
