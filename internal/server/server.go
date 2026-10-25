// Package server holds the HTTP server.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/voidgrid/voidgrid-soundscapes/internal/library"
)

// Layers lists the audio files that can be mixed.
type Layers interface {
	Layers() []library.Layer
}

// New returns the HTTP handler. Health reports only that the process is serving.
func New(lib Layers) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/layers", func(w http.ResponseWriter, r *http.Request) {
		layers := lib.Layers()
		if layers == nil {
			layers = []library.Layer{} // encode as [], not null
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(struct {
			Layers []library.Layer `json:"layers"`
		}{layers})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
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
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	}
}
