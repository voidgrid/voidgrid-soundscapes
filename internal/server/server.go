// Package server holds the HTTP server.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/voidgrid/voidgrid-soundscapes/internal/engine"
	"github.com/voidgrid/voidgrid-soundscapes/internal/library"
)

// Layers lists the audio files that can be mixed.
type Layers interface {
	Layers() []library.Layer
}

// Mixes is the part of the engine the server uses.
type Mixes interface {
	Apply(id string, spec engine.Spec) (engine.Status, error)
	Status(id string) (engine.Status, bool)
	List() []engine.Status
	Subscribe(id string) (*engine.Listener, error)
}

const (
	maxSpecBytes = 256 << 10
	// writeTimeout bounds one write to a listener, so a client that stops reading is dropped instead of
	// holding its goroutine forever. The server itself has no WriteTimeout because streams never end.
	writeTimeout = 10 * time.Second
)

type server struct {
	lib   Layers
	mixes Mixes
}

// New returns the HTTP handler.
func New(lib Layers, mixes Mixes) http.Handler {
	s := &server{lib: lib, mixes: mixes}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /api/layers", s.layers)
	mux.HandleFunc("GET /api/soundscapes", s.list)
	mux.HandleFunc("GET /api/soundscapes/{id}", s.get)
	mux.HandleFunc("PUT /api/soundscapes/{id}", s.put)
	mux.HandleFunc("GET /stream/{id}", s.stream)
	return mux
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *server) layers(w http.ResponseWriter, r *http.Request) {
	layers := s.lib.Layers()
	if layers == nil {
		layers = []library.Layer{} // encode as [], not null
	}
	writeJSON(w, http.StatusOK, struct {
		Layers []library.Layer `json:"layers"`
	}{layers})
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	list := s.mixes.List()
	if list == nil {
		list = []engine.Status{}
	}
	writeJSON(w, http.StatusOK, struct {
		Soundscapes []engine.Status `json:"soundscapes"`
	}{list})
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	st, ok := s.mixes.Status(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown soundscape")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// put defines or replaces a soundscape. The body is the whole mix, so sending it again is harmless.
func (s *server) put(w http.ResponseWriter, r *http.Request) {
	var spec engine.Spec
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSpecBytes))
	dec.DisallowUnknownFields() // a typo such as "enable" must not silently switch nothing on
	if err := dec.Decode(&spec); err != nil {
		writeError(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return
	}
	st, err := s.mixes.Apply(r.PathValue("id"), spec)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// stream sends a soundscape as MP3 until the client goes away or the stream stops.
func (s *server) stream(w http.ResponseWriter, r *http.Request) {
	l, err := s.mixes.Subscribe(r.PathValue("id"))
	if errors.Is(err, engine.ErrUnknown) {
		writeError(w, http.StatusNotFound, "unknown soundscape")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer l.Close()

	h := w.Header()
	h.Set("Content-Type", "audio/mpeg")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()

	for {
		select {
		case frame := <-l.C:
			_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
			if _, err := w.Write(frame); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		case <-l.Done():
			return
		case <-r.Context().Done():
			return
		}
	}
}

// Run serves h on addr until ctx is cancelled, then shuts down gracefully.
func Run(ctx context.Context, addr string, h http.Handler, log *slog.Logger) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", addr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		// Streams never finish on their own, so Shutdown would wait for them; close them first.
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		_ = srv.Close()
		return nil
	}
}
