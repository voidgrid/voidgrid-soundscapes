package config

import (
	"strings"
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

func TestHomeAssistantSettings(t *testing.T) {
	c, err := load(env(map[string]string{
		"VOIDGRID_SOUNDSCAPES_PUBLIC_URL":       "http://192.168.1.117:8007/",
		"VOIDGRID_SOUNDSCAPES_HA_URL":           "http://192.168.1.211:8123/",
		"VOIDGRID_SOUNDSCAPES_HA_TOKEN":         "tok",
		"VOIDGRID_SOUNDSCAPES_MQTT_URL":         "tcp://192.168.1.211:1883",
		"VOIDGRID_SOUNDSCAPES_MQTT_USER":        "u",
		"VOIDGRID_SOUNDSCAPES_MQTT_PASSWORD":    "pw",
		"VOIDGRID_SOUNDSCAPES_DEVICE_NAME":      "  Sleep sounds ",
		"VOIDGRID_SOUNDSCAPES_DISCOVERY_PREFIX": "/ha/",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "http://192.168.1.117:8007" || c.HAURL != "http://192.168.1.211:8123" || c.HAToken != "tok" ||
		c.MQTTURL != "tcp://192.168.1.211:1883" || c.MQTTUser != "u" || c.MQTTPassword != "pw" || c.DeviceName != "Sleep sounds" || c.DiscoveryPrefix != "ha" {
		t.Fatalf("%+v", c)
	}
	d, _ := load(env(nil))
	if d.DeviceName != "Voidgrid Soundscapes" || d.DiscoveryPrefix != "homeassistant" || d.HAURL != "" || d.MQTTURL != "" {
		t.Fatalf("defaults: %+v", d)
	}
}

func TestRejectsBadHomeAssistantSettings(t *testing.T) {
	for _, m := range []map[string]string{
		{"VOIDGRID_SOUNDSCAPES_HA_URL": "http://h:8123"},                                // token missing
		{"VOIDGRID_SOUNDSCAPES_HA_TOKEN": "t"},                                          // URL missing
		{"VOIDGRID_SOUNDSCAPES_HA_URL": "h:8123", "VOIDGRID_SOUNDSCAPES_HA_TOKEN": "t"}, // no scheme
		{"VOIDGRID_SOUNDSCAPES_HA_URL": "http://u:p@h:8123", "VOIDGRID_SOUNDSCAPES_HA_TOKEN": "t"},
		{"VOIDGRID_SOUNDSCAPES_PUBLIC_URL": "192.168.1.117:8007"},
		{"VOIDGRID_SOUNDSCAPES_MQTT_URL": "http://h:1883"},
		{"VOIDGRID_SOUNDSCAPES_MQTT_URL": "tcp://user:secret@h:1883"},
		{"VOIDGRID_SOUNDSCAPES_DEVICE_NAME": strings.Repeat("x", 65)},
		{"VOIDGRID_SOUNDSCAPES_DISCOVERY_PREFIX": "home#assistant"},
	} {
		_, err := load(env(m))
		if err == nil {
			t.Errorf("expected an error for %v", m)
			continue
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("the error leaks a credential: %v", err)
		}
	}
}

func TestStringLeavesOutSecrets(t *testing.T) {
	c := Config{HAToken: "tok-123", MQTTPassword: "pw-456", HAURL: "http://h:8123", DeviceName: "d"}
	out := c.String()
	if strings.Contains(out, "tok-123") || strings.Contains(out, "pw-456") || !strings.Contains(out, "ha_token=set") || !strings.Contains(out, "mqtt_password=set") {
		t.Fatalf("%s", out)
	}
}
