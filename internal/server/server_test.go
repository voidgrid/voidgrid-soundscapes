package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-soundscapes/internal/engine"
	"github.com/voidgrid/voidgrid-soundscapes/internal/library"
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
	ts := httptest.NewServer(New(lib, eng, st))
	t.Cleanup(ts.Close)
	return ts, cancel
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
