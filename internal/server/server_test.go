package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-soundscapes/internal/library"
)

type fakeLayers []library.Layer

func (f fakeLayers) Layers() []library.Layer { return f }

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	New(fakeLayers(nil)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestLayersEndpoint(t *testing.T) {
	h := New(fakeLayers{
		{ID: "rain/heavy.mp3", Name: "heavy", Category: "rain", Path: "/secret/host/path/rain/heavy.mp3", DurationSec: 35.4, Codec: "mp3", ModTime: time.Unix(0, 0).UTC()},
		{ID: "bad.mp3", Name: "bad", Error: "no audio stream"},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/layers", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var got struct {
		Layers []map[string]any `json:"layers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Layers) != 2 || got.Layers[0]["id"] != "rain/heavy.mp3" || got.Layers[0]["category"] != "rain" {
		t.Fatalf("unexpected body %s", rec.Body.String())
	}
	if got.Layers[1]["error"] != "no audio stream" {
		t.Fatalf("error not reported: %v", got.Layers[1])
	}
	if strings.Contains(rec.Body.String(), "/secret/host/path") {
		t.Fatal("the host filesystem path must not be exposed")
	}
}

func TestLayersEndpointEmptyIsAnArrayNotNull(t *testing.T) {
	rec := httptest.NewRecorder()
	New(fakeLayers(nil)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/layers", nil))
	if want := `{"layers":[]}`; rec.Body.String() != want+"\n" {
		t.Fatalf("got %q", rec.Body.String())
	}
}
