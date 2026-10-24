package config

import "testing"

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestDefaults(t *testing.T) {
	c, err := load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8007" || c.AudioDir != "/soundscapes/audio" || c.ConfigDir != "/soundscapes/config" || c.LogLevel != "info" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestOverrides(t *testing.T) {
	c, err := load(env(map[string]string{
		"VOIDGRID_SOUNDSCAPES_LISTEN":    "127.0.0.1:9000",
		"VOIDGRID_SOUNDSCAPES_AUDIO_DIR": "/a",
		"VOIDGRID_SOUNDSCAPES_LOG_LEVEL": "debug",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:9000" || c.AudioDir != "/a" || c.LogLevel != "debug" {
		t.Fatalf("unexpected config: %+v", c)
	}
}

func TestRejectsBadValues(t *testing.T) {
	for _, m := range []map[string]string{
		{"VOIDGRID_SOUNDSCAPES_LOG_LEVEL": "loud"},
		{"VOIDGRID_SOUNDSCAPES_LISTEN": "nonsense"},
	} {
		if _, err := load(env(m)); err == nil {
			t.Fatalf("expected an error for %v", m)
		}
	}
}
