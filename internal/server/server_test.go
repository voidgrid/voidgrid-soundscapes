package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-soundscapes/internal/engine"
	"github.com/voidgrid/voidgrid-soundscapes/internal/ha"
	"github.com/voidgrid/voidgrid-soundscapes/internal/library"
	"github.com/voidgrid/voidgrid-soundscapes/internal/playback"
	"github.com/voidgrid/voidgrid-soundscapes/internal/store"
)

// --- fakes -----------------------------------------------------------------------------------------

type fakeLib []library.Layer

func (f fakeLib) Layers() []library.Layer { return f }
func (f fakeLib) Get(id string) (library.Layer, bool) {
	for _, l := range f {
		if l.ID == id {
			return l, true
		}
	}
	return library.Layer{}, false
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// frame is a valid MPEG1 Layer III frame header followed by filler (128 kbit/s, 44.1 kHz).
func frame(seq byte) []byte {
	f := bytes.Repeat([]byte{seq}, 417)
	copy(f, []byte{0xFF, 0xFB, 0x90, 0x00})
	return f
}

// fakeEncoder answers each 1024-sample chunk with one valid MP3 frame.
func fakeEncoder(ctx context.Context) (*engine.Encoder, error) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer outW.Close()
		buf := make([]byte, engine.ChunkSamples*2)
		for n := byte(0); ; n++ {
			if _, err := io.ReadFull(inR, buf); err != nil {
				return
			}
			if _, err := outW.Write(frame(n)); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	go func() { <-ctx.Done(); _ = inR.Close(); _ = outW.Close() }()
	return engine.NewTestEncoder(inW, outR, func() error { <-done; return nil }), nil
}

func newTestServer(t *testing.T, layers ...library.Layer) (*httptest.Server, context.CancelFunc) {
	t.Helper()
	ts, cancel, _ := newTestServerHA(t, nil, layers...)
	return ts, cancel
}

// fakeHA is Home Assistant for the player endpoints: playing makes the player report our stream.
type fakeHA struct {
	mu    sync.Mutex
	calls []string
	state map[string]ha.PlayerState
}

func (f *fakeHA) Players(context.Context) ([]ha.Player, error) {
	return []ha.Player{{ID: "media_player.bedroom", Name: "Bedroom"}}, nil
}
func (f *fakeHA) State(_ context.Context, id string) (ha.PlayerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state[id], nil
}
func (f *fakeHA) Play(_ context.Context, id, u string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "play "+id+" "+u)
	f.state[id] = ha.PlayerState{State: "playing", MediaContentID: u}
	return nil
}
func (f *fakeHA) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "stop "+id)
	f.state[id] = ha.PlayerState{State: "idle"}
	return nil
}
func (f *fakeHA) log() string { f.mu.Lock(); defer f.mu.Unlock(); return strings.Join(f.calls, "; ") }

func newTestServerHA(t *testing.T, h playback.HA, layers ...library.Layer) (*httptest.Server, context.CancelFunc, *playback.Service) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	lib := fakeLib(layers)
	eng := engine.New(ctx, lib, engine.Options{
		Decode: func(context.Context, string) ([]int16, error) {
			s := make([]int16, 2000)
			for i := range s {
				s[i] = 5000
			}
			return s, nil
		},
		NewEncoder: fakeEncoder,
		Grace:      50 * time.Millisecond,
		Unpaced:    true,
	}, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	st, err := store.Open(t.TempDir(), 20*time.Millisecond, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	pb, err := playback.New(st, h, playback.Options{PublicURL: "http://192.168.1.117:8007", Settle: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(lib, eng, st, pb))
	t.Cleanup(ts.Close)
	return ts, cancel, pb
}

func layerOf(id string) library.Layer {
	return library.Layer{ID: id, Name: id, Path: "/secret/host/path/" + id, Size: 1, ModTime: time.Unix(1, 0).UTC(), DurationSec: 10, Codec: "mp3"}
}

func do(t *testing.T, method, url, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// --- tests -----------------------------------------------------------------------------------------

func TestHealthz(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, body := do(t, "GET", ts.URL+"/healthz", "")
	if resp.StatusCode != 200 || string(body) != "ok\n" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
}

func TestLayersEndpoint(t *testing.T) {
	bad := library.Layer{ID: "bad.mp3", Name: "bad", Error: "no audio stream"}
	ts, _ := newTestServer(t, layerOf("rain/heavy.mp3"), bad)
	resp, body := do(t, "GET", ts.URL+"/api/layers", "")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("got %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var got struct {
		Layers []map[string]any `json:"layers"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Layers) != 2 || got.Layers[0]["id"] != "rain/heavy.mp3" || got.Layers[1]["error"] != "no audio stream" {
		t.Fatalf("unexpected body %s", body)
	}
	if strings.Contains(string(body), "/secret/host/path") {
		t.Fatal("the host filesystem path must not be exposed")
	}
}

func TestEmptyListsAreArraysNotNull(t *testing.T) {
	ts, _ := newTestServer(t)
	for path, want := range map[string]string{"/api/layers": `{"layers":[]}`, "/api/soundscapes": `{"soundscapes":[]}`} {
		_, body := do(t, "GET", ts.URL+path, "")
		if strings.TrimSpace(string(body)) != want {
			t.Errorf("%s = %q, want %q", path, body, want)
		}
	}
}

func TestPutThenGetSoundscape(t *testing.T) {
	ts, _ := newTestServer(t, layerOf("rain/heavy.mp3"))
	spec := `{"layers":[{"id":"rain/heavy.mp3","level":0.4,"enabled":true},{"id":"gone.mp3","level":1,"enabled":true}]}`
	resp, body := do(t, "PUT", ts.URL+"/api/soundscapes/night", spec)
	if resp.StatusCode != 200 {
		t.Fatalf("PUT = %d %s", resp.StatusCode, body)
	}
	var st engine.Status
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if st.ID != "night" || len(st.Layers) != 2 || st.Layers[0].Level != 0.4 || st.Layers[1].State != engine.StateMissing {
		t.Fatalf("unexpected status %+v", st)
	}

	resp, body = do(t, "GET", ts.URL+"/api/soundscapes/night", "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"id":"night"`) {
		t.Fatalf("GET = %d %s", resp.StatusCode, body)
	}
	_, body = do(t, "GET", ts.URL+"/api/soundscapes", "")
	if !strings.Contains(string(body), `"id":"night"`) {
		t.Fatalf("list = %s", body)
	}
	// Putting the same thing again is harmless.
	if resp, _ = do(t, "PUT", ts.URL+"/api/soundscapes/night", spec); resp.StatusCode != 200 {
		t.Fatalf("repeat PUT = %d", resp.StatusCode)
	}
}

func TestPutRejectsBadInput(t *testing.T) {
	ts, _ := newTestServer(t)
	cases := map[string]struct{ path, body string }{
		"bad id":         {"/api/soundscapes/.hidden", `{"layers":[]}`},
		"not json":       {"/api/soundscapes/x", `nope`},
		"unknown field":  {"/api/soundscapes/x", `{"layers":[{"id":"a.mp3","level":1,"enable":true}]}`},
		"wrong type":     {"/api/soundscapes/x", `{"layers":[{"id":"a.mp3","level":"loud","enabled":true}]}`},
		"empty body":     {"/api/soundscapes/x", ``},
		"oversized body": {"/api/soundscapes/x", `{"layers":[` + strings.Repeat(`{"id":"a.mp3","level":1,"enabled":true},`, 10000) + `{}]}`},
	}
	for name, c := range cases {
		resp, body := do(t, "PUT", ts.URL+c.path, c.body)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), `"error"`) {
			t.Errorf("%s: got %d %s, want 400 with an error", name, resp.StatusCode, body)
		}
	}
	if _, body := do(t, "GET", ts.URL+"/api/soundscapes", ""); strings.Contains(string(body), `"id":"x"`) {
		t.Error("a rejected PUT must not create the soundscape")
	}
}

func TestUnknownSoundscapeAndWrongMethods(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, path := range []string{"/api/soundscapes/nope", "/stream/nope"} {
		resp, body := do(t, "GET", ts.URL+path, "")
		if resp.StatusCode != 404 || !strings.Contains(string(body), `"error"`) {
			t.Errorf("GET %s = %d %s", path, resp.StatusCode, body)
		}
	}
	for method, path := range map[string]string{"POST": "/api/soundscapes/x", "DELETE": "/stream/x", "PUT": "/api/layers"} {
		if resp, _ := do(t, method, ts.URL+path, ""); resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", method, path, resp.StatusCode)
		}
	}
}

func TestStreamDeliversWholeMP3FramesAndReleasesTheListener(t *testing.T) {
	ts, _ := newTestServer(t, layerOf("rain.mp3"))
	do(t, "PUT", ts.URL+"/api/soundscapes/night", `{"layers":[{"id":"rain.mp3","level":0.5,"enabled":true}]}`)

	resp, err := http.Get(ts.URL + "/stream/night")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/mpeg" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers: %d %v", resp.StatusCode, resp.Header)
	}
	buf := make([]byte, 417*6)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	for i := 0; i+4 <= len(buf); i += 417 {
		if buf[i] != 0xFF || buf[i+1] != 0xFB {
			t.Fatalf("stream is not made of whole frames: byte %d is % x", i, buf[i:i+4])
		}
	}

	var st engine.Status
	_, body := do(t, "GET", ts.URL+"/api/soundscapes/night", "")
	_ = json.Unmarshal(body, &st)
	if st.Listeners != 1 || !st.Streaming {
		t.Fatalf("status while streaming: %+v", st)
	}

	resp.Body.Close() // the player goes away
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, body = do(t, "GET", ts.URL+"/api/soundscapes/night", "")
		_ = json.Unmarshal(body, &st)
		if st.Listeners == 0 && !st.Streaming {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("listener was not released after the client left: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStreamEndsWhenTheEngineStops(t *testing.T) {
	ts, cancel := newTestServer(t, layerOf("rain.mp3"))
	do(t, "PUT", ts.URL+"/api/soundscapes/night", `{"layers":[{"id":"rain.mp3","level":0.5,"enabled":true}]}`)
	resp, err := http.Get(ts.URL + "/stream/night")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadFull(resp.Body, make([]byte, 417*2)); err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, resp.Body); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream did not end when the engine stopped")
	}
}

// --- creating, renaming, duplicating, deleting -----------------------------------------------------------

func decodeView(t *testing.T, b []byte) view {
	t.Helper()
	var v view
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%v in %s", err, b)
	}
	return v
}

func TestCreateGivesAUniqueIDAndNameIsRequired(t *testing.T) {
	ts, _ := newTestServer(t, layerOf("rain.mp3"))
	resp, body := do(t, "POST", ts.URL+"/api/soundscapes", `{"name":"  Night Rain! ","layers":[{"id":"rain.mp3","level":0.5,"enabled":true}]}`)
	v := decodeView(t, body)
	if resp.StatusCode != http.StatusCreated || v.ID != "night-rain" || v.Name != "Night Rain!" || len(v.Layers) != 1 {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Location") != "/api/soundscapes/night-rain" {
		t.Fatalf("Location = %q", resp.Header.Get("Location"))
	}
	// The same name again must not replace the first.
	_, body = do(t, "POST", ts.URL+"/api/soundscapes", `{"name":"Night Rain!"}`)
	if v2 := decodeView(t, body); v2.ID != "night-rain-2" || len(v2.Layers) != 0 {
		t.Fatalf("second create = %s", body)
	}
	for name, b := range map[string]string{"no name": `{}`, "blank name": `{"name":"   "}`, "control character": `{"name":"a\u0007b"}`,
		"too long": `{"name":"` + strings.Repeat("x", 81) + `"}`, "unknown field": `{"name":"a","colour":"red"}`, "empty body": ``} {
		if resp, body := do(t, "POST", ts.URL+"/api/soundscapes", b); resp.StatusCode != 400 {
			t.Errorf("%s: got %d %s, want 400", name, resp.StatusCode, body)
		}
	}
	if _, body := do(t, "GET", ts.URL+"/api/soundscapes", ""); strings.Count(string(body), `"streaming"`) != 2 {
		t.Errorf("rejected creates left soundscapes behind: %s", body)
	}
}

func TestPutRenamesWithoutTouchingTheMix(t *testing.T) {
	ts, _ := newTestServer(t, layerOf("rain.mp3"))
	do(t, "PUT", ts.URL+"/api/soundscapes/night", `{"layers":[{"id":"rain.mp3","level":0.4,"enabled":true}]}`)
	_, body := do(t, "GET", ts.URL+"/api/soundscapes/night", "")
	if v := decodeView(t, body); v.Name != "night" {
		t.Fatalf("a soundscape created by PUT is named after its ID, got %q", v.Name)
	}
	resp, body := do(t, "PUT", ts.URL+"/api/soundscapes/night", `{"name":"Sleep"}`)
	v := decodeView(t, body)
	if resp.StatusCode != 200 || v.ID != "night" || v.Name != "Sleep" || len(v.Layers) != 1 || v.Layers[0].Level != 0.4 {
		t.Fatalf("rename = %d %s", resp.StatusCode, body)
	}
	// Sending layers without a name keeps the name.
	_, body = do(t, "PUT", ts.URL+"/api/soundscapes/night", `{"layers":[]}`)
	if v := decodeView(t, body); v.Name != "Sleep" || len(v.Layers) != 0 {
		t.Fatalf("after clearing the layers: %s", body)
	}
	if resp, _ := do(t, "PUT", ts.URL+"/api/soundscapes/night", `{"name":""}`); resp.StatusCode != 400 {
		t.Errorf("a blank name was accepted: %d", resp.StatusCode)
	}
}

func TestDuplicateCopiesTheMixUnderANewID(t *testing.T) {
	ts, _ := newTestServer(t, layerOf("rain.mp3"))
	do(t, "PUT", ts.URL+"/api/soundscapes/night", `{"name":"Night","layers":[{"id":"rain.mp3","level":0.4,"enabled":true}]}`)
	resp, body := do(t, "POST", ts.URL+"/api/soundscapes/night/duplicate", "")
	v := decodeView(t, body)
	if resp.StatusCode != 201 || v.ID != "night-copy" || v.Name != "Night copy" || len(v.Layers) != 1 || v.Layers[0].Level != 0.4 {
		t.Fatalf("duplicate = %d %s", resp.StatusCode, body)
	}
	// Changing the copy leaves the original alone.
	do(t, "PUT", ts.URL+"/api/soundscapes/night-copy", `{"layers":[{"id":"rain.mp3","level":0.9,"enabled":false}]}`)
	_, body = do(t, "GET", ts.URL+"/api/soundscapes/night", "")
	if v := decodeView(t, body); v.Layers[0].Level != 0.4 || !v.Layers[0].Enabled {
		t.Fatalf("the original changed: %s", body)
	}
	_, body = do(t, "POST", ts.URL+"/api/soundscapes/night/duplicate", `{"name":"Weekend"}`)
	if v := decodeView(t, body); v.Name != "Weekend" || v.ID != "weekend" {
		t.Fatalf("named duplicate = %s", body)
	}
	if resp, _ := do(t, "POST", ts.URL+"/api/soundscapes/nope/duplicate", ""); resp.StatusCode != 404 {
		t.Errorf("duplicate of an unknown soundscape = %d", resp.StatusCode)
	}
	if resp, _ := do(t, "POST", ts.URL+"/api/soundscapes/night/duplicate", `{"layers":[]}`); resp.StatusCode != 400 {
		t.Errorf("duplicate with layers = %d, want 400", resp.StatusCode)
	}
}

func TestDeleteDisconnectsListenersAndForgetsTheSoundscape(t *testing.T) {
	ts, _ := newTestServer(t, layerOf("rain.mp3"))
	do(t, "PUT", ts.URL+"/api/soundscapes/night", `{"layers":[{"id":"rain.mp3","level":0.5,"enabled":true}]}`)
	resp, err := http.Get(ts.URL + "/stream/night")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadFull(resp.Body, make([]byte, 417*2)); err != nil {
		t.Fatal(err)
	}

	if r, b := do(t, "DELETE", ts.URL+"/api/soundscapes/night", ""); r.StatusCode != http.StatusNoContent || len(b) != 0 {
		t.Fatalf("DELETE = %d %s", r.StatusCode, b)
	}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, resp.Body); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream kept going after its soundscape was deleted")
	}
	for _, path := range []string{"/api/soundscapes/night", "/stream/night"} {
		if r, _ := do(t, "GET", ts.URL+path, ""); r.StatusCode != 404 {
			t.Errorf("GET %s after delete = %d", path, r.StatusCode)
		}
	}
	if r, _ := do(t, "DELETE", ts.URL+"/api/soundscapes/night", ""); r.StatusCode != 404 {
		t.Errorf("second DELETE = %d, want 404", r.StatusCode)
	}
	// The ID can be used again.
	if r, _ := do(t, "PUT", ts.URL+"/api/soundscapes/night", `{"layers":[]}`); r.StatusCode != 200 {
		t.Errorf("re-creating the ID = %d", r.StatusCode)
	}
}

func TestListIsOrderedByName(t *testing.T) {
	ts, _ := newTestServer(t)
	for id, name := range map[string]string{"a": "Zebra", "b": "apple", "c": "Mango"} {
		do(t, "PUT", ts.URL+"/api/soundscapes/"+id, `{"name":"`+name+`"}`)
	}
	_, body := do(t, "GET", ts.URL+"/api/soundscapes", "")
	var got struct{ Soundscapes []view }
	_ = json.Unmarshal(body, &got)
	var names []string
	for _, v := range got.Soundscapes {
		names = append(names, v.Name)
	}
	if strings.Join(names, ",") != "apple,Mango,Zebra" {
		t.Fatalf("order = %v", names)
	}
}

// --- single-layer changes, events, and the web page ------------------------------------------------------

func TestPatchChangesOneLayerAndLeavesTheRest(t *testing.T) {
	ts, _ := newTestServer(t, layerOf("rain.mp3"), layerOf("fan.wav"), library.Layer{ID: "bad.mp3", Name: "bad", Error: "no audio"})
	do(t, "PUT", ts.URL+"/api/soundscapes/night", `{"layers":[{"id":"rain.mp3","level":0.4,"enabled":true}]}`)
	patch := func(body string) (int, view) {
		resp, b := do(t, "PATCH", ts.URL+"/api/soundscapes/night/layers", body)
		var v view
		_ = json.Unmarshal(b, &v)
		return resp.StatusCode, v
	}

	// Adding a layer from the library: off at the default level unless told otherwise.
	if code, v := patch(`{"id":"fan.wav"}`); code != 200 || len(v.Layers) != 2 || v.Layers[1].ID != "fan.wav" || v.Layers[1].Enabled || v.Layers[1].Level != 0.5 {
		t.Fatalf("add = %d %+v", code, v.Layers)
	}
	// Level only, then enabled only: each keeps the other.
	if code, v := patch(`{"id":"fan.wav","level":0.8}`); code != 200 || v.Layers[1].Level != 0.8 || v.Layers[1].Enabled {
		t.Fatalf("level = %d %+v", code, v.Layers)
	}
	if code, v := patch(`{"id":"fan.wav","enabled":true}`); code != 200 || v.Layers[1].Level != 0.8 || !v.Layers[1].Enabled {
		t.Fatalf("enable = %d %+v", code, v.Layers)
	}
	// The first layer was never touched.
	_, b := do(t, "GET", ts.URL+"/api/soundscapes/night", "")
	if v := decodeView(t, b); v.Layers[0].ID != "rain.mp3" || v.Layers[0].Level != 0.4 || !v.Layers[0].Enabled {
		t.Fatalf("the other layer changed: %s", b)
	}
	// Add with explicit settings, remove, and removing something that is not there.
	if code, v := patch(`{"id":"bad.mp3","level":1,"enabled":true}`); code != 200 || v.Layers[2].State != engine.StateUnusable {
		t.Fatalf("adding an unusable layer = %d %+v", code, v.Layers)
	}
	if code, v := patch(`{"id":"bad.mp3","remove":true}`); code != 200 || len(v.Layers) != 2 {
		t.Fatalf("remove = %d %+v", code, v.Layers)
	}
	if code, _ := patch(`{"id":"bad.mp3","remove":true}`); code != 200 {
		t.Fatalf("removing an absent layer = %d, want 200", code)
	}
	for name, body := range map[string]string{"no id": `{"level":1}`, "not in the library": `{"id":"nope.mp3"}`, "unknown field": `{"id":"fan.wav","volume":1}`, "empty": ``} {
		if code, _ := patch(body); code != 400 {
			t.Errorf("%s: got %d, want 400", name, code)
		}
	}
	if resp, _ := do(t, "PATCH", ts.URL+"/api/soundscapes/nope/layers", `{"id":"fan.wav"}`); resp.StatusCode != 404 {
		t.Errorf("patch of an unknown soundscape = %d", resp.StatusCode)
	}
}

// readEvent returns the data of the next "state" event.
func readEvent(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	type res struct {
		data string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		var event string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				ch <- res{err: err}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && event == "state":
				ch <- res{data: strings.TrimPrefix(line, "data: ")}
				return
			}
		}
	}()
	select {
	case v := <-ch:
		if v.err != nil {
			t.Fatal(v.err)
		}
		return v.data
	case <-time.After(3 * time.Second):
		t.Fatal("no event arrived")
		return ""
	}
}

func TestEventsSendTheStateAtOnceAndAgainOnEveryChange(t *testing.T) {
	ts, _ := newTestServer(t, layerOf("rain.mp3"))
	resp, err := http.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	r := bufio.NewReader(resp.Body)
	first := readEvent(t, r)
	if !strings.Contains(first, `"layers":[{"id":"rain.mp3"`) || !strings.Contains(first, `"soundscapes":[]`) || strings.Contains(first, "/secret/host/path") {
		t.Fatalf("first event: %s", first)
	}

	start := time.Now()
	do(t, "PUT", ts.URL+"/api/soundscapes/night", `{"name":"Night"}`)
	next := readEvent(t, r)
	if !strings.Contains(next, `"name":"Night"`) {
		t.Fatalf("event after a change: %s", next)
	}
	if d := time.Since(start); d > 400*time.Millisecond {
		t.Errorf("the change took %v to reach the page; a change made through the API should be pushed at once, not at the next poll", d)
	}

	// Something that changes by itself (a listener arriving) is picked up by the poll.
	stream, err := http.Get(ts.URL + "/stream/night")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if got := readEvent(t, r); !strings.Contains(got, `"listeners":1`) {
		t.Fatalf("event after a listener joined: %s", got)
	}
}

func TestEventStreamEndsWhenTheServerShutsDown(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	srv := httptest.NewUnstartedServer(nil)
	lib := fakeLib{layerOf("rain.mp3")}
	eng := engine.New(ctx, lib, engine.Options{}, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	st, _ := store.Open(t.TempDir(), 0, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	defer st.Close()
	pb, _ := playback.New(st, nil, playback.Options{})
	srv.Config.Handler = New(lib, eng, st, pb)
	srv.Config.BaseContext = func(net.Listener) context.Context { return ctx }
	srv.Start()
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	readEvent(t, bufio.NewReader(resp.Body))
	stop()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, resp.Body); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the event stream outlived the server's context")
	}
}

func TestWebPageIsServedWithItsAssets(t *testing.T) {
	ts, _ := newTestServer(t)
	for path, want := range map[string]string{"/": "text/html", "/index.html": "", "/app.js": "javascript", "/style.css": "text/css"} {
		resp, body := do(t, "GET", ts.URL+path, "")
		if path == "/index.html" {
			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusMovedPermanently {
				t.Errorf("%s = %d", path, resp.StatusCode)
			}
			continue
		}
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), want) || len(body) < 100 {
			t.Errorf("%s = %d %q (%d bytes)", path, resp.StatusCode, resp.Header.Get("Content-Type"), len(body))
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || resp.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: CSP %q, Cache-Control %q", path, csp, resp.Header.Get("Cache-Control"))
		}
	}
	_, page := do(t, "GET", ts.URL+"/", "")
	if strings.Contains(string(page), "<script>") || strings.Contains(string(page), "onclick") {
		t.Error("the page uses inline script, which its Content-Security-Policy forbids")
	}
	if resp, _ := do(t, "GET", ts.URL+"/nope.txt", ""); resp.StatusCode != 404 {
		t.Errorf("unknown path = %d", resp.StatusCode)
	}
}

// --- player -------------------------------------------------------------------------------------------

func TestPlayerWithoutHomeAssistant(t *testing.T) {
	ts, _ := newTestServer(t)
	_, body := do(t, "GET", ts.URL+"/api/player", "")
	if !strings.Contains(string(body), `"haConfigured":false`) {
		t.Fatalf("GET /api/player = %s", body)
	}
	do(t, "PUT", ts.URL+"/api/soundscapes/night", `{"layers":[]}`)
	if resp, body := do(t, "POST", ts.URL+"/api/player/play", `{"soundscape":"night","player":"media_player.x"}`); resp.StatusCode != 400 || !strings.Contains(string(body), "not configured") {
		t.Fatalf("play = %d %s", resp.StatusCode, body)
	}
	// Choosing a soundscape still works and is remembered.
	if resp, body := do(t, "PUT", ts.URL+"/api/player", `{"soundscape":"night"}`); resp.StatusCode != 200 || !strings.Contains(string(body), `"soundscape":"night"`) || !strings.Contains(string(body), `"streamable":true`) {
		t.Fatalf("select = %d %s", resp.StatusCode, body)
	}
}

func TestPlayerChooseAndPlayAndStop(t *testing.T) {
	h := &fakeHA{state: map[string]ha.PlayerState{}}
	ts, _, pb := newTestServerHA(t, h)
	// The service reads the list of players when it runs; do one read by hand.
	go pb.Run(context.Background(), noWarn{})
	do(t, "PUT", ts.URL+"/api/soundscapes/night", `{"layers":[]}`)
	deadline := time.Now().Add(2 * time.Second)
	for len(pb.State().Players) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if resp, body := do(t, "PUT", ts.URL+"/api/player", `{"soundscape":"night","player":"media_player.bedroom"}`); resp.StatusCode != 200 {
		t.Fatalf("select = %d %s", resp.StatusCode, body)
	}
	if resp, _ := do(t, "PUT", ts.URL+"/api/player", `{"player":"media_player.nope"}`); resp.StatusCode != 400 {
		t.Errorf("unknown player = %d", resp.StatusCode)
	}
	if resp, _ := do(t, "PUT", ts.URL+"/api/player", `{"soundscape":"nope"}`); resp.StatusCode != 400 {
		t.Errorf("unknown soundscape = %d", resp.StatusCode)
	}
	if resp, body := do(t, "POST", ts.URL+"/api/player/play", ``); resp.StatusCode != 200 {
		t.Fatalf("play = %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(h.log(), "play media_player.bedroom http://192.168.1.117:8007/stream/night") {
		t.Fatalf("Home Assistant was sent: %s", h.log())
	}
	deadline = time.Now().Add(3 * time.Second)
	for !pb.State().Playing && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !pb.State().Playing {
		t.Fatal("never saw the player playing")
	}
	if resp, body := do(t, "POST", ts.URL+"/api/player/stop", ``); resp.StatusCode != 200 {
		t.Fatalf("stop = %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(h.log(), "stop media_player.bedroom") {
		t.Fatalf("Home Assistant was sent: %s", h.log())
	}
}

func TestEventsCarryThePlayerState(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if first := readEvent(t, bufio.NewReader(resp.Body)); !strings.Contains(first, `"player":{"haConfigured":false`) {
		t.Fatalf("first event: %s", first)
	}
}

type noWarn struct{}

func (noWarn) Warn(string, ...any) {}

func TestStopRefusesAForeignStreamUnlessForced(t *testing.T) {
	h := &fakeHA{state: map[string]ha.PlayerState{"media_player.bedroom": {State: "playing", MediaContentID: "spotify:track:1"}}}
	ts, _, pb := newTestServerHA(t, h)
	go pb.Run(context.Background(), noWarn{})
	do(t, "PUT", ts.URL+"/api/soundscapes/night", `{"layers":[]}`)
	deadline := time.Now().Add(2 * time.Second)
	for len(pb.State().Players) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	do(t, "PUT", ts.URL+"/api/player", `{"soundscape":"night","player":"media_player.bedroom"}`)

	resp, body := do(t, "POST", ts.URL+"/api/player/stop", ``)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "left alone") || strings.Contains(h.log(), "stop") {
		t.Fatalf("plain stop = %d %s (HA: %s)", resp.StatusCode, body, h.log())
	}
	resp, body = do(t, "POST", ts.URL+"/api/player/stop", `{"force":true}`)
	if resp.StatusCode != 200 || !strings.Contains(h.log(), "stop media_player.bedroom") {
		t.Fatalf("forced stop = %d %s (HA: %s)", resp.StatusCode, body, h.log())
	}
	if resp, _ := do(t, "POST", ts.URL+"/api/player/stop", `{"forse":true}`); resp.StatusCode != 400 {
		t.Errorf("a misspelt field was accepted: %d", resp.StatusCode)
	}
}
