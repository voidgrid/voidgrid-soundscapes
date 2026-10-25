package config

import (
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestDefaults(t *testing.T) {
	c, err := load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8007" || c.AudioDir != "/soundscapes/audio" || c.ConfigDir != "/soundscapes/config" || c.LogLevel != "info" || c.ScanInterval != 10*time.Second || c.LoopCrossfade != 1500*time.Millisecond {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestOverrides(t *testing.T) {
	c, err := load(env(map[string]string{
		"VOIDGRID_SOUNDSCAPES_LISTEN":         "127.0.0.1:9000",
		"VOIDGRID_SOUNDSCAPES_AUDIO_DIR":      "/a",
		"VOIDGRID_SOUNDSCAPES_LOG_LEVEL":      "debug",
		"VOIDGRID_SOUNDSCAPES_SCAN_INTERVAL":  "30s",
		"VOIDGRID_SOUNDSCAPES_LOOP_CROSSFADE": "250ms",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:9000" || c.AudioDir != "/a" || c.LogLevel != "debug" || c.ScanInterval != 30*time.Second || c.LoopCrossfade != 250*time.Millisecond {
		t.Fatalf("unexpected config: %+v", c)
	}
}

func TestRejectsBadValues(t *testing.T) {
	for _, m := range []map[string]string{
		{"VOIDGRID_SOUNDSCAPES_LOG_LEVEL": "loud"},
		{"VOIDGRID_SOUNDSCAPES_LISTEN": "nonsense"},
		{"VOIDGRID_SOUNDSCAPES_SCAN_INTERVAL": "soon"},
		{"VOIDGRID_SOUNDSCAPES_SCAN_INTERVAL": "100ms"},
		{"VOIDGRID_SOUNDSCAPES_LOOP_CROSSFADE": "-1s"},
		{"VOIDGRID_SOUNDSCAPES_LOOP_CROSSFADE": "30s"},
		{"VOIDGRID_SOUNDSCAPES_LOOP_CROSSFADE": "long"},
	} {
		if _, err := load(env(m)); err == nil {
			t.Fatalf("expected an error for %v", m)
		}
	}
}

func TestZeroCrossfadeIsAllowed(t *testing.T) {
	c, err := load(env(map[string]string{"VOIDGRID_SOUNDSCAPES_LOOP_CROSSFADE": "0s"}))
	if err != nil || c.LoopCrossfade != 0 {
		t.Fatalf("0s should mean a hard cut: %+v %v", c, err)
	}
}
