// Package server holds the HTTP server.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/voidgrid/voidgrid-soundscapes/internal/engine"
	"github.com/voidgrid/voidgrid-soundscapes/internal/library"
	"github.com/voidgrid/voidgrid-soundscapes/internal/store"
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
	Remove(id string) bool
}

// Store keeps the soundscape definitions.
type Store interface {
	List() []store.Soundscape
	Get(id string) (store.Soundscape, bool)
	Put(sc store.Soundscape)
	Delete(id string) bool
	NewID(name string) string
}

const (
	maxSpecBytes = 256 << 10
	maxNameLen   = 80 // characters
	// writeTimeout bounds one write to a listener, so a client that stops reading is dropped instead of
	// holding its goroutine forever. The server itself has no WriteTimeout because streams never end.
	writeTimeout = 10 * time.Second
)

type server struct {
	lib   Layers
	mixes Mixes
	store Store

	// mu makes each change to a soundscape (store and engine together) one step, so two requests for the
	// same soundscape cannot leave the engine playing one and the file holding the other.
	mu sync.Mutex
}

// New returns the HTTP handler.
func New(lib Layers, mixes Mixes, st Store) http.Handler {
	s := &server{lib: lib, mixes: mixes, store: st}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /api/layers", s.layers)
	mux.HandleFunc("GET /api/soundscapes", s.list)
	mux.HandleFunc("GET /api/soundscapes/{id}", s.get)
	mux.HandleFunc("POST /api/soundscapes", s.create)
	mux.HandleFunc("PUT /api/soundscapes/{id}", s.put)
	mux.HandleFunc("POST /api/soundscapes/{id}/duplicate", s.duplicate)
	mux.HandleFunc("DELETE /api/soundscapes/{id}", s.remove)
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

// view is a soundscape as the API shows it: its definition and what is playing.
type view struct {
	Name string `json:"name"`
	engine.Status
}

func (s *server) view(sc store.Soundscape) view {
	st, ok := s.mixes.Status(sc.ID)
	if !ok { // cannot happen while every stored soundscape is applied; show the definition rather than nothing
		st = engine.Status{ID: sc.ID, Layers: []engine.LayerStatus{}}
	}
	return view{Name: sc.Name, Status: st}
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	list := s.store.List()
	out := make([]view, 0, len(list))
	for _, sc := range list {
		out = append(out, s.view(sc))
	}
	writeJSON(w, http.StatusOK, struct {
		Soundscapes []view `json:"soundscapes"`
	}{out})
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown soundscape")
		return
	}
	writeJSON(w, http.StatusOK, s.view(sc))
}

// body is what create and put accept. A field left out keeps its current value.
type body struct {
	Name   *string             `json:"name"`
	Layers *[]engine.LayerSpec `json:"layers"`
}

func decode(w http.ResponseWriter, r *http.Request, into any, allowEmpty bool) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSpecBytes))
	dec.DisallowUnknownFields() // a typo such as "enable" must not silently switch nothing on
	err := dec.Decode(into)
	if err == io.EOF && allowEmpty {
		return true
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return false
	}
	return true
}

// cleanName trims name and checks it can be shown as a title.
func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name is required")
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		return "", errors.New("name is longer than 80 characters")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", errors.New("name may not contain control characters")
		}
	}
	return name, nil
}

// save plays sc and records it. The layers saved are the ones the engine accepted (levels clamped,
// duplicates dropped), so the file never holds anything the engine would not play. Callers hold s.mu.
func (s *server) save(sc store.Soundscape) (view, error) {
	st, err := s.mixes.Apply(sc.ID, engine.Spec{Layers: sc.Layers})
	if err != nil {
		return view{}, err
	}
	sc.Layers = sc.Layers[:0:0]
	for _, l := range st.Layers {
		sc.Layers = append(sc.Layers, engine.LayerSpec{ID: l.ID, Level: l.Level, Enabled: l.Enabled})
	}
	s.store.Put(sc)
	return view{Name: sc.Name, Status: st}, nil
}

// create makes a new soundscape; the server picks its ID from the name.
func (s *server) create(w http.ResponseWriter, r *http.Request) {
	var b body
	if !decode(w, r, &b, false) {
		return
	}
	if b.Name == nil {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	name, err := cleanName(*b.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sc := store.Soundscape{Name: name}
	if b.Layers != nil {
		sc.Layers = *b.Layers
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sc.ID = s.store.NewID(name)
	v, err := s.save(sc)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Location", "/api/soundscapes/"+sc.ID)
	writeJSON(w, http.StatusCreated, v)
}

// put defines or replaces a soundscape. Sending the whole mix again is harmless. "name" renames it (a new
// soundscape is named after its ID if none is given); leaving out "layers" keeps the current mix.
func (s *server) put(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !engine.ValidID(id) {
		writeError(w, http.StatusBadRequest, "soundscape IDs are 1-64 letters, digits, '.', '_' or '-', starting with a letter or digit")
		return
	}
	var b body
	if !decode(w, r, &b, false) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, exists := s.store.Get(id)
	if !exists {
		sc = store.Soundscape{ID: id, Name: id}
	}
	if b.Name != nil {
		name, err := cleanName(*b.Name)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		sc.Name = name
	}
	if b.Layers != nil {
		sc.Layers = *b.Layers
	}
	v, err := s.save(sc)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// duplicate copies a soundscape under a new ID. The optional body {"name": "..."} names the copy.
func (s *server) duplicate(w http.ResponseWriter, r *http.Request) {
	var b body
	if !decode(w, r, &b, true) {
		return
	}
	if b.Layers != nil {
		writeError(w, http.StatusBadRequest, "bad request body: only \"name\" can be given when duplicating")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown soundscape")
		return
	}
	name := src.Name + " copy"
	if b.Name != nil {
		name = *b.Name
	}
	if utf8.RuneCountInString(name) > maxNameLen { // the default can overshoot by the suffix
		name = string([]rune(name)[:maxNameLen])
	}
	name, err := cleanName(name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	v, err := s.save(store.Soundscape{ID: s.store.NewID(name), Name: name, Layers: src.Layers})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Location", "/api/soundscapes/"+v.ID)
	writeJSON(w, http.StatusCreated, v)
}

// remove deletes a soundscape. Anyone listening to it is disconnected.
func (s *server) remove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.store.Delete(id) {
		writeError(w, http.StatusNotFound, "unknown soundscape")
		return
	}
	s.mixes.Remove(id)
	w.WriteHeader(http.StatusNoContent)
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
