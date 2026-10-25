// Command voidgrid-soundscapes serves ambient soundscapes to Home Assistant media players.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/voidgrid/voidgrid-soundscapes/internal/config"
	"github.com/voidgrid/voidgrid-soundscapes/internal/library"
	"github.com/voidgrid/voidgrid-soundscapes/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}

	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	log.Info("starting", "version", version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	lib := library.New(cfg.AudioDir, nil, log)
	go lib.Run(ctx, cfg.ScanInterval, func(d library.Diff) {
		layers := lib.Layers()
		bad := 0
		for _, l := range layers {
			if !l.Playable() {
				bad++
			}
		}
		log.Info("library changed", "added", len(d.Added), "removed", len(d.Removed), "changed", len(d.Changed), "total", len(layers), "unusable", bad)
	})

	if err := server.Run(ctx, cfg.Listen, server.New(lib), log); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
	log.Info("stopped")
}
