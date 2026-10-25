package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProbe accepts every file except those whose content is "bad", and counts calls.
type fakeProbe struct{ calls atomic.Int32 }

func (f *fakeProbe) probe(_ context.Context, path string) (Info, error) {
	f.calls.Add(1)
	b, err := os.ReadFile(path)
	if err != nil {
		return Info{}, err
	}
	if strings.TrimSpace(string(b)) == "bad" {
		return Info{}, errors.New("no audio stream")
	}
	return Info{Duration: 10, Codec: "mp3", SampleRate: 44100, Channels: 2}, nil
}

// write creates a file under root and backdates it so it counts as settled.
func write(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	return p
}

func newLib(t *testing.T) (*Library, string, *fakeProbe) {
	t.Helper()
	root := t.TempDir()
	fp := &fakeProbe{}
	return New(root, fp.probe, nil), root, fp
}

func ids(ls []Layer) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.ID
	}
	return out
}

func TestScanIDsCategoriesAndNames(t *testing.T) {
	lib, root, _ := newLib(t)
	write(t, root, "fan.mp3", "x")
	write(t, root, "rain/heavy.mp3", "x")
	write(t, root, "rain/sub/light.OGG", "x")
	write(t, root, "waves/heavy.flac", "x")

	d, err := lib.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"fan.mp3", "rain/heavy.mp3", "rain/sub/light.OGG", "waves/heavy.flac"}; !reflect.DeepEqual(d.Added, want) {
		t.Fatalf("added = %v, want %v", d.Added, want)
	}

	cases := map[string][2]string{ // id -> name, category
		"fan.mp3":            {"fan", ""},
		"rain/heavy.mp3":     {"heavy", "rain"},
		"rain/sub/light.OGG": {"light", "rain"},
		"waves/heavy.flac":   {"heavy", "waves"},
	}
	for id, want := range cases {
		l, ok := lib.Get(id)
		if !ok {
			t.Fatalf("%s missing", id)
		}
		if l.Name != want[0] || l.Category != want[1] || !l.Playable() || l.DurationSec != 10 {
			t.Errorf("%s: got name=%q category=%q err=%q dur=%v", id, l.Name, l.Category, l.Error, l.DurationSec)
		}
	}
}

func TestSameNameInDifferentFoldersDoesNotCollide(t *testing.T) {
	lib, root, _ := newLib(t)
	write(t, root, "rain/heavy.mp3", "x")
	write(t, root, "waves/heavy.mp3", "x")
	if _, err := lib.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(lib.Layers()); n != 2 {
		t.Fatalf("want 2 layers, got %d", n)
	}
}

func TestIgnoresNonAudioHiddenAndPartialFiles(t *testing.T) {
	lib, root, _ := newLib(t)
	write(t, root, "keep.mp3", "x")
	write(t, root, "notes.txt", "x")
	write(t, root, "cover.jpg", "x")
	write(t, root, "download.mp3.part", "x")
	write(t, root, ".hidden.mp3", "x")
	write(t, root, ".trash/old.mp3", "x")
	if _, err := lib.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := ids(lib.Layers()); !reflect.DeepEqual(got, []string{"keep.mp3"}) {
		t.Fatalf("got %v", got)
	}
}

func TestSymlinks(t *testing.T) {
	lib, root, _ := newLib(t)
	outside := t.TempDir()
	target := write(t, outside, "real.mp3", "x")
	write(t, outside, "dir/inside.mp3", "x")
	if err := os.Symlink(target, filepath.Join(root, "linked.mp3")); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	if err := os.Symlink(filepath.Join(outside, "dir"), filepath.Join(root, "linkeddir")); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A symlinked file counts; a symlinked directory is not followed (avoids loops and escaping the root).
	if got := ids(lib.Layers()); !reflect.DeepEqual(got, []string{"linked.mp3"}) {
		t.Fatalf("got %v", got)
	}
}

func TestBadFileIsListedWithErrorAndNotReprobed(t *testing.T) {
	lib, root, fp := newLib(t)
	write(t, root, "ok.mp3", "x")
	write(t, root, "broken.mp3", "bad")
	if _, err := lib.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, ok := lib.Get("broken.mp3")
	if !ok || b.Playable() || b.Error == "" {
		t.Fatalf("broken.mp3 should be listed with an error, got %+v ok=%v", b, ok)
	}
	if o, _ := lib.Get("ok.mp3"); !o.Playable() {
		t.Fatal("ok.mp3 should be playable")
	}
	calls := fp.calls.Load()
	if d, _ := lib.Scan(context.Background()); !d.Empty() {
		t.Fatalf("second scan should be a no-op, got %+v", d)
	}
	if fp.calls.Load() != calls {
		t.Fatalf("unchanged files were probed again (%d -> %d calls)", calls, fp.calls.Load())
	}
}

func TestDetectsAddedChangedRemoved(t *testing.T) {
	lib, root, _ := newLib(t)
	write(t, root, "a.mp3", "x")
	write(t, root, "b.mp3", "x")
	if _, err := lib.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	write(t, root, "c.mp3", "x")
	write(t, root, "a.mp3", "different size")
	if err := os.Remove(filepath.Join(root, "b.mp3")); err != nil {
		t.Fatal(err)
	}
	d, err := lib.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d, Diff{Added: []string{"c.mp3"}, Removed: []string{"b.mp3"}, Changed: []string{"a.mp3"}}) {
		t.Fatalf("diff = %+v", d)
	}
}

func TestFixedFileBecomesPlayable(t *testing.T) {
	lib, root, _ := newLib(t)
	write(t, root, "x.mp3", "bad")
	_, _ = lib.Scan(context.Background())
	write(t, root, "x.mp3", "now a real file")
	d, _ := lib.Scan(context.Background())
	if !reflect.DeepEqual(d.Changed, []string{"x.mp3"}) {
		t.Fatalf("diff = %+v", d)
	}
	if l, _ := lib.Get("x.mp3"); !l.Playable() {
		t.Fatalf("should be playable after the fix: %+v", l)
	}
}

func TestFileStillBeingWrittenIsSkippedUntilSettled(t *testing.T) {
	lib, root, _ := newLib(t)
	p := filepath.Join(root, "new.mp3")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil { // mtime = now, not settled
		t.Fatal(err)
	}
	if d, _ := lib.Scan(context.Background()); !d.Empty() {
		t.Fatalf("an unsettled file must not appear: %+v", d)
	}
	old := time.Now().Add(-time.Minute)
	_ = os.Chtimes(p, old, old)
	if d, _ := lib.Scan(context.Background()); !reflect.DeepEqual(d.Added, []string{"new.mp3"}) {
		t.Fatalf("settled file should appear: %+v", d)
	}
}

func TestKnownFileBeingRewrittenIsNotDropped(t *testing.T) {
	lib, root, _ := newLib(t)
	p := write(t, root, "a.mp3", "x")
	_, _ = lib.Scan(context.Background())
	if err := os.WriteFile(p, []byte("rewriting"), 0o644); err != nil { // now unsettled again
		t.Fatal(err)
	}
	d, _ := lib.Scan(context.Background())
	if !d.Empty() {
		t.Fatalf("a file being rewritten must keep its entry, got %+v", d)
	}
	if _, ok := lib.Get("a.mp3"); !ok {
		t.Fatal("a.mp3 vanished while being rewritten")
	}
}

func TestMissingRootKeepsPreviousLayers(t *testing.T) {
	lib, root, _ := newLib(t)
	write(t, root, "a.mp3", "x")
	if _, err := lib.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	moved := root + "-gone"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(moved, root) })
	if _, err := lib.Scan(context.Background()); err == nil {
		t.Fatal("expected an error for a missing root")
	}
	if _, ok := lib.Get("a.mp3"); !ok {
		t.Fatal("layers were wiped by an unreadable root")
	}
}

func TestGetOnlyReturnsScannedIDs(t *testing.T) {
	lib, root, _ := newLib(t)
	write(t, root, "a.mp3", "x")
	_, _ = lib.Scan(context.Background())
	for _, id := range []string{"../etc/passwd", "/etc/passwd", "a.mp3/../../x", "missing.mp3", ""} {
		if _, ok := lib.Get(id); ok {
			t.Errorf("Get(%q) should not resolve", id)
		}
	}
}

func TestLayersSortedByCategoryThenID(t *testing.T) {
	lib, root, _ := newLib(t)
	for _, f := range []string{"z.mp3", "Waves/b.mp3", "rain/B.mp3", "rain/a.mp3", "a.mp3"} {
		write(t, root, f, "x")
	}
	_, _ = lib.Scan(context.Background())
	want := []string{"a.mp3", "z.mp3", "rain/a.mp3", "rain/B.mp3", "Waves/b.mp3"}
	if got := ids(lib.Layers()); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestRunScansAtOnceAndOnInterval(t *testing.T) {
	lib, root, _ := newLib(t)
	write(t, root, "a.mp3", "x")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	changes := make(chan Diff, 4)
	go func() {
		lib.Run(ctx, 20*time.Millisecond, func(d Diff) { changes <- d })
		close(done)
	}()
	select {
	case d := <-changes:
		if !reflect.DeepEqual(d.Added, []string{"a.mp3"}) {
			t.Fatalf("first diff = %+v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no initial scan")
	}
	write(t, root, "b.mp3", "x")
	select {
	case d := <-changes:
		if !reflect.DeepEqual(d.Added, []string{"b.mp3"}) {
			t.Fatalf("second diff = %+v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("new file not noticed")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}

func TestParseProbe(t *testing.T) {
	good := `{"streams":[{"codec_name":"mp3","sample_rate":"44100","channels":2}],"format":{"duration":"35.4"}}`
	info, err := parseProbe([]byte(good))
	if err != nil || info.Codec != "mp3" || info.SampleRate != 44100 || info.Channels != 2 || info.Duration != 35.4 {
		t.Fatalf("got %+v err=%v", info, err)
	}
	for name, in := range map[string]string{
		"no stream":     `{"streams":[],"format":{"duration":"3"}}`,
		"no duration":   `{"streams":[{"codec_name":"mp3"}],"format":{}}`,
		"zero duration": `{"streams":[{"codec_name":"mp3"}],"format":{"duration":"0"}}`,
		"not json":      `nope`,
	} {
		if _, err := parseProbe([]byte(in)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestFirstLineStripsFFmpegContext(t *testing.T) {
	cases := map[string]string{
		"[mp3 @ 0x7197a57ce500] Failed to find two consecutive MPEG audio frames.\nmore": "Failed to find two consecutive MPEG audio frames.",
		"plain message\r\nnext": "plain message",
		"no newline":            "no newline",
	}
	for in, want := range cases {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}
