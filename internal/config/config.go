// Package config reads settings from the environment. All variables share the VOIDGRID_SOUNDSCAPES_ prefix.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const prefix = "VOIDGRID_SOUNDSCAPES_"

// Config holds the runtime settings.
type Config struct {
	Listen    string // address the HTTP server binds to
	AudioDir  string // root of the audio library
	ConfigDir string // where soundscapes and settings are stored
	LogLevel  string // debug, info, warn or error

	ScanInterval  time.Duration // how often the audio directory is rescanned
	LoopCrossfade time.Duration // crossfade that joins the end of a layer's audio to its start

	PublicURL string // how players reach this program, such as http://192.168.1.117:8007 (needed to play on a speaker)

	HAURL   string // Home Assistant, such as http://192.168.1.211:8123 (empty: no speaker control)
	HAToken string // long-lived access token; a secret, never log it

	MQTTURL         string // broker, such as tcp://192.168.1.211:1883 (empty: no Home Assistant entities)
	MQTTUser        string
	MQTTPassword    string // a secret, never log it
	DeviceName      string // the Home Assistant device; entity IDs derive from it
	DiscoveryPrefix string // Home Assistant's MQTT discovery prefix
}

// String describes the config for a log line. It leaves the secrets out.
func (c Config) String() string {
	set := func(s string) string {
		if s == "" {
			return "unset"
		}
		return "set"
	}
	return fmt.Sprintf("listen=%s audio=%s config=%s public_url=%q ha_url=%q ha_token=%s mqtt_url=%q mqtt_user=%q mqtt_password=%s device=%q",
		c.Listen, c.AudioDir, c.ConfigDir, c.PublicURL, c.HAURL, set(c.HAToken), c.MQTTURL, c.MQTTUser, set(c.MQTTPassword), c.DeviceName)
}

// Load reads the configuration from the process environment.
func Load() (Config, error) {
	return load(os.LookupEnv)
}

// ListenAddr returns only the listen address. The container health check uses it: Docker runs the check in
// its own process, which does not have the secrets a wrapper injects into the server (the Home Assistant
// token, the MQTT password), so it must not insist on a complete, valid configuration.
func ListenAddr() (string, error) {
	return listenAddr(os.LookupEnv)
}

func listenAddr(lookup func(string) (string, bool)) (string, error) {
	addr := ":8007"
	if v, ok := lookup(prefix + "LISTEN"); ok && v != "" {
		addr = v
	}
	if _, err := strconv.Atoi(portOf(addr)); err != nil {
		return "", fmt.Errorf("%sLISTEN: %q has no numeric port", prefix, addr)
	}
	return addr, nil
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
	x, err := time.ParseDuration(get("LOOP_CROSSFADE", "1500ms"))
	if err != nil || x < 0 || x > 10*time.Second {
		return Config{}, fmt.Errorf("%sLOOP_CROSSFADE: must be a duration from 0 to 10s, such as 1500ms", prefix)
	}
	c.LoopCrossfade = x
	c.PublicURL = strings.TrimRight(get("PUBLIC_URL", ""), "/")
	c.HAURL = strings.TrimRight(get("HA_URL", ""), "/")
	c.HAToken = get("HA_TOKEN", "")
	c.MQTTURL = get("MQTT_URL", "")
	c.MQTTUser = get("MQTT_USER", "")
	c.MQTTPassword = get("MQTT_PASSWORD", "")
	c.DeviceName = strings.TrimSpace(get("DEVICE_NAME", "Voidgrid Soundscapes"))
	c.DiscoveryPrefix = strings.Trim(get("DISCOVERY_PREFIX", "homeassistant"), "/")
	if c.PublicURL != "" {
		if err := checkURL(c.PublicURL, "http", "https"); err != nil {
			return Config{}, fmt.Errorf("%sPUBLIC_URL: %v, such as http://192.168.1.117:8007", prefix, err)
		}
	}
	if (c.HAURL == "") != (c.HAToken == "") {
		return Config{}, fmt.Errorf("%sHA_URL and %sHA_TOKEN must be set together", prefix, prefix)
	}
	if c.HAURL != "" {
		if err := checkURL(c.HAURL, "http", "https"); err != nil {
			return Config{}, fmt.Errorf("%sHA_URL: %v, such as http://192.168.1.211:8123", prefix, err)
		}
	}
	if c.MQTTURL != "" {
		if err := checkURL(c.MQTTURL, "tcp", "mqtt", "ssl", "tls", "mqtts"); err != nil {
			return Config{}, fmt.Errorf("%sMQTT_URL: %v, such as tcp://192.168.1.211:1883", prefix, err)
		}
	}
	if c.DeviceName == "" || len(c.DeviceName) > 64 {
		return Config{}, fmt.Errorf("%sDEVICE_NAME: must be 1 to 64 characters", prefix)
	}
	if c.DiscoveryPrefix == "" || strings.ContainsAny(c.DiscoveryPrefix, "#+ ") {
		return Config{}, fmt.Errorf("%sDISCOVERY_PREFIX: must be a topic prefix such as homeassistant", prefix)
	}
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

// checkURL requires a URL with one of the schemes and a host, and nothing in it that would carry a password.
func checkURL(raw string, schemes ...string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%q is not a URL", redact(raw))
	}
	for _, s := range schemes {
		if u.Scheme == s {
			if u.User != nil {
				return errors.New("must not contain a user name or password (use the separate settings)")
			}
			return nil
		}
	}
	return fmt.Errorf("must start with %s://", strings.Join(schemes, "://, "))
}

// redact keeps a rejected value out of an error message if it could hold a secret.
func redact(raw string) string {
	if strings.Contains(raw, "@") {
		return "(contains @, not shown)"
	}
	return raw
}

func portOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i+1:]
		}
	}
	return ""
}
