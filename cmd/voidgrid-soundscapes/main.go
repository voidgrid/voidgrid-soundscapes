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
	"github.com/voidgrid/voidgrid-soundscapes/internal/engine"
	"github.com/voidgrid/voidgrid-soundscapes/internal/ha"
	"github.com/voidgrid/voidgrid-soundscapes/internal/hamqtt"
	"github.com/voidgrid/voidgrid-soundscapes/internal/library"
	"github.com/voidgrid/voidgrid-soundscapes/internal/playback"
	"github.com/voidgrid/voidgrid-soundscapes/internal/server"
	"github.com/voidgrid/voidgrid-soundscapes/internal/store"
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
	log.Info("starting", "version", version, "config", cfg.String())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Opened before anything starts, so a config directory that cannot be used stops the program here.
	soundscapes, err := store.Open(cfg.ConfigDir, 0, log)
	if err != nil {
		log.Error("cannot open the soundscape store", "err", err)
		os.Exit(1)
	}

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

	eng := engine.New(ctx, lib, engine.Options{
		Loop: engine.LoopOptions{Crossfade: cfg.LoopCrossfade, SilenceThreshold: engine.DefaultLoopOptions.SilenceThreshold},
	}, log)

	saved := soundscapes.List()
	for _, sc := range saved {
		if _, err := eng.Apply(sc.ID, engine.Spec{Layers: sc.Layers}); err != nil {
			log.Error("cannot start a saved soundscape", "soundscape", sc.ID, "err", err)
		}
	}
	log.Info("soundscapes loaded", "count", len(saved))

	// Home Assistant: talking to it is optional, and so are the MQTT entities.
	var haClient playback.HA
	if cfg.HAURL != "" {
		c, err := ha.New(cfg.HAURL, cfg.HAToken)
		if err != nil {
			log.Error("Home Assistant settings", "err", err)
			os.Exit(2)
		}
		haClient = c
	}
	player, err := playback.New(soundscapes, haClient, playback.Options{PublicURL: cfg.PublicURL})
	if err != nil {
		log.Error("playback settings", "err", err)
		os.Exit(2)
	}
	go player.Run(ctx, log)

	if cfg.MQTTURL != "" {
		ctl := hamqtt.New(player, soundscapes, hamqtt.Options{DeviceName: cfg.DeviceName, Prefix: cfg.DiscoveryPrefix, Version: version}, log)
		disconnect := hamqtt.Dial(hamqtt.BrokerOptions{URL: cfg.MQTTURL, User: cfg.MQTTUser, Password: cfg.MQTTPassword}, ctl, log)
		go ctl.Run(ctx)
		defer disconnect()
	}

	runErr := server.Run(ctx, cfg.Listen, server.New(lib, eng, soundscapes, player), log)
	if err := soundscapes.Close(); err != nil { // writes any change still waiting for its delay
		log.Error("cannot save soundscapes", "err", err)
		os.Exit(1)
	}
	if runErr != nil {
		log.Error("server stopped", "err", runErr)
		os.Exit(1)
	}
	log.Info("stopped")
}
