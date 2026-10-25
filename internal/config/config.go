// Package config reads settings from the environment. All variables share the VOIDGRID_SOUNDSCAPES_ prefix.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

const prefix = "VOIDGRID_SOUNDSCAPES_"

// Config holds the runtime settings.
type Config struct {
	Listen    string // address the HTTP server binds to
	AudioDir  string // root of the audio library
	ConfigDir string // where soundscapes and settings are stored
	LogLevel  string // debug, info, warn or error

	ScanInterval time.Duration // how often the audio directory is rescanned
}

// Load reads the configuration from the process environment.
func Load() (Config, error) {
	return load(os.LookupEnv)
}

func load(lookup func(string) (string, bool)) (Config, error) {
	get := func(name, def string) string {
		if v, ok := lookup(prefix + name); ok && v != "" {
			return v
		}
		return def
	}
	c := Config{
		Listen:    get("LISTEN", ":8007"),
		AudioDir:  get("AUDIO_DIR", "/soundscapes/audio"),
		ConfigDir: get("CONFIG_DIR", "/soundscapes/config"),
		LogLevel:  get("LOG_LEVEL", "info"),
	}
	d, err := time.ParseDuration(get("SCAN_INTERVAL", "10s"))
	if err != nil || d < time.Second {
		return Config{}, fmt.Errorf("%sSCAN_INTERVAL: must be a duration of at least 1s, such as 10s or 1m", prefix)
	}
	c.ScanInterval = d
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, fmt.Errorf("%sLOG_LEVEL: %q is not one of debug, info, warn, error", prefix, c.LogLevel)
	}
	if _, err := strconv.Atoi(portOf(c.Listen)); err != nil {
		return Config{}, fmt.Errorf("%sLISTEN: %q has no numeric port", prefix, c.Listen)
	}
	return c, nil
}

func portOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i+1:]
		}
	}
	return ""
}
