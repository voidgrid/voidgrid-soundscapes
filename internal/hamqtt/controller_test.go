package hamqtt

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-soundscapes/internal/ha"
	"github.com/voidgrid/voidgrid-soundscapes/internal/playback"
	"github.com/voidgrid/voidgrid-soundscapes/internal/store"
)

type fakeConn struct {
	mu      sync.Mutex
	pubs    map[string]string // topic -> last payload
	count   map[string]int
	subs    map[string]func(topic, payload string)
	failPub error
}

func newConn() *fakeConn {
	return &fakeConn{pubs: map[string]string{}, count: map[string]int{}, subs: map[string]func(string, string){}}
}
func (f *fakeConn) Publish(topic, payload string, retain bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPub != nil {
		return f.failPub
	}
	if !retain {
		panic("everything we publish is retained, so Home Assistant has it after a restart: " + topic)
	}
	f.pubs[topic] = payload
	f.count[topic]++
	return nil
}
func (f *fakeConn) Subscribe(topic string, h func(topic, payload string)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs[topic] = h
	return nil
}
func (f *fakeConn) get(topic string) string { f.mu.Lock(); defer f.mu.Unlock(); return f.pubs[topic] }
func (f *fakeConn) sends(topic string) int  { f.mu.Lock(); defer f.mu.Unlock(); return f.count[topic] }
func (f *fakeConn) send(topic, payload string) {
	f.mu.Lock()
	h := f.subs[topic]
	f.mu.Unlock()
	if h == nil {
		panic("nobody subscribed to " + topic)
	}
	h(topic, payload)
}

type fakePlayback struct {
	mu    sync.Mutex
	state playback.State
	ch    chan struct{}
	calls []string
	err   error
}

func (f *fakePlayback) State() playback.State { f.mu.Lock(); defer f.mu.Unlock(); return f.state }
func (f *fakePlayback) Changed() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ch
}
func (f *fakePlayback) record(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
	return f.err
}
func (f *fakePlayback) SelectSoundscape(_ context.Context, id string) error {
	return f.record("soundscape=" + id)
}
func (f *fakePlayback) SelectPlayer(_ context.Context, id string) error {
	return f.record("player=" + id)
}
func (f *fakePlayback) Play(_ context.Context, s, p string) error {
	return f.record("play " + s + "|" + p)
}
func (f *fakePlayback) Stop(context.Context) error { return f.record("stop") }
func (f *fakePlayback) log() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "; ")
}
func (f *fakePlayback) set(s playback.State) { f.mu.Lock(); f.state = s; f.mu.Unlock() }

type fakeList struct {
	mu sync.Mutex
	l  []store.Soundscape
}

func (f *fakeList) List() []store.Soundscape  { f.mu.Lock(); defer f.mu.Unlock(); return f.l }
func (f *fakeList) set(l ...store.Soundscape) { f.mu.Lock(); f.l = l; f.mu.Unlock() }

func fixture(t *testing.T) (*Controller, *fakeConn, *fakePlayback, *fakeList) {
	t.Helper()
	pb := &fakePlayback{ch: make(chan struct{}), state: playback.State{
		Players: []ha.Player{{ID: "media_player.bedroom", Name: "Bedroom"}, {ID: "media_player.kitchen", Name: "Kitchen"}},
	}}
	sc := &fakeList{}
	sc.set(store.Soundscape{ID: "night", Name: "Night rain"}, store.Soundscape{ID: "day", Name: "Day"})
	c := New(pb, sc, Options{DeviceName: "Sleep Sounds", Prefix: "homeassistant", Version: "1.2.3"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	conn := newConn()
	c.Connected(conn)
	return c, conn, pb, sc
}

func configOf(t *testing.T, conn *fakeConn, component, key string) map[string]any {
	t.Helper()
	raw := conn.get("homeassistant/" + component + "/sleep_sounds/" + key + "/config")
	if raw == "" {
		t.Fatalf("no discovery message for %s/%s", component, key)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("%s/%s: %v in %s", component, key, err, raw)
	}
	return m
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"Sleep Sounds": "sleep_sounds", "  Voidgrid -- Soundscapes! ": "voidgrid_soundscapes", "ÄÖ": "voidgrid_soundscapes", "a/b#c+d": "a_b_c_d"} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAnnouncesTheDeviceAndEveryEntity(t *testing.T) {
	_, conn, _, _ := fixture(t)
	if conn.get("voidgrid-soundscapes/sleep_sounds/status") != "online" {
		t.Fatal("not marked online")
	}
	seen := map[string]bool{}
	for _, e := range entities {
		m := configOf(t, conn, e.component, e.key)
		uid := m["unique_id"].(string)
		if seen[uid] || !strings.HasPrefix(uid, "sleep_sounds_") {
			t.Errorf("unique_id %q is repeated or not tied to the device", uid)
		}
		seen[uid] = true
		dev := m["device"].(map[string]any)
		if dev["name"] != "Sleep Sounds" || dev["sw_version"] != "1.2.3" || dev["identifiers"].([]any)[0] != "voidgrid_soundscapes_sleep_sounds" {
			t.Errorf("%s: device %v", e.key, dev)
		}
		if m["has_entity_name"] != true || m["availability_topic"] != "voidgrid-soundscapes/sleep_sounds/status" || m["payload_not_available"] != "offline" {
			t.Errorf("%s: %v", e.key, m)
		}
		if _, old := m["object_id"]; old {
			t.Errorf("%s uses object_id, which Home Assistant has deprecated", e.key)
		}
		if _, has := m["command_topic"]; has != e.command {
			t.Errorf("%s: command_topic present = %t, want %t", e.key, has, e.command)
		}
	}
	sel := configOf(t, conn, "select", "soundscape")
	opts := sel["options"].([]any)
	if len(opts) != 2 || opts[0] != "Night rain" || opts[1] != "Day" {
		t.Errorf("soundscape options %v", opts)
	}
	if p := configOf(t, conn, "select", "player")["options"].([]any); len(p) != 2 || p[0] != "Bedroom" {
		t.Errorf("player options %v", p)
	}
	if sw := configOf(t, conn, "switch", "playing"); sw["payload_on"] != "ON" || sw["state_off"] != "OFF" {
		t.Errorf("switch %v", sw)
	}
}

func TestStatesFollowPlayback(t *testing.T) {
	c, conn, pb, _ := fixture(t)
	st := func(k string) string { return conn.get("voidgrid-soundscapes/sleep_sounds/" + k + "/state") }
	if st("soundscape") != "None" || st("player") != "None" || st("playing") != "OFF" || st("streamable") != "OFF" || st("stream_url") != "None" || st("now_playing") != "None" {
		t.Fatal("initial states wrong")
	}
	pb.set(playback.State{
		Soundscape: "night", Player: "media_player.kitchen", Playing: true, NowPlaying: "night", Streamable: true,
		StreamURL: "http://h:8007/stream/night",
		Players:   []ha.Player{{ID: "media_player.bedroom", Name: "Bedroom"}, {ID: "media_player.kitchen", Name: "Kitchen"}},
	})
	c.Sync()
	if st("soundscape") != "Night rain" || st("player") != "Kitchen" || st("playing") != "ON" || st("streamable") != "ON" ||
		st("stream_url") != "http://h:8007/stream/night" || st("now_playing") != "Night rain" {
		t.Fatalf("states: %q %q %q %q %q %q", st("soundscape"), st("player"), st("playing"), st("streamable"), st("stream_url"), st("now_playing"))
	}
}

func TestOnlyChangesArePublished(t *testing.T) {
	c, conn, pb, sc := fixture(t)
	topic := "voidgrid-soundscapes/sleep_sounds/playing/state"
	cfg := "homeassistant/select/sleep_sounds/soundscape/config"
	before, cfgBefore := conn.sends(topic), conn.sends(cfg)
	for i := 0; i < 5; i++ {
		c.Sync()
	}
	if conn.sends(topic) != before || conn.sends(cfg) != cfgBefore {
		t.Fatal("unchanged state was published again")
	}
	pb.set(playback.State{Playing: true, NowPlaying: "x", Players: pb.State().Players})
	c.Sync()
	if conn.sends(topic) != before+1 || conn.sends(cfg) != cfgBefore {
		t.Fatalf("a change should publish its own state only: %d, %d", conn.sends(topic), conn.sends(cfg))
	}
	sc.set(store.Soundscape{ID: "night", Name: "Night rain"}, store.Soundscape{ID: "day", Name: "Day"}, store.Soundscape{ID: "new", Name: "New one"})
	c.Sync()
	if conn.sends(cfg) != cfgBefore+1 || !strings.Contains(conn.get(cfg), "New one") {
		t.Fatal("a new soundscape should update the select's options")
	}
}

func TestDuplicateNamesGetTheirIDAdded(t *testing.T) {
	c, conn, pb, sc := fixture(t)
	sc.set(store.Soundscape{ID: "a", Name: "Rain"}, store.Soundscape{ID: "b", Name: "Rain"}, store.Soundscape{ID: "c", Name: "Sea"})
	pb.set(playback.State{Soundscape: "b"})
	c.Sync()
	opts := configOf(t, conn, "select", "soundscape")["options"].([]any)
	if opts[0] != "Rain (a)" || opts[1] != "Rain (b)" || opts[2] != "Sea" {
		t.Fatalf("options %v", opts)
	}
	if got := conn.get("voidgrid-soundscapes/sleep_sounds/soundscape/state"); got != "Rain (b)" {
		t.Fatalf("state %q", got)
	}
	conn.send("voidgrid-soundscapes/sleep_sounds/soundscape/set", "Rain (a)")
	waitFor(t, func() bool { return strings.Contains(pb.log(), "soundscape=a") })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCommandsReachPlayback(t *testing.T) {
	_, conn, pb, _ := fixture(t)
	base := "voidgrid-soundscapes/sleep_sounds/"
	conn.send(base+"soundscape/set", "Day")
	conn.send(base+"player/set", "Kitchen")
	conn.send(base+"playing/set", "ON")
	conn.send(base+"playing/set", "OFF")
	conn.send(base+"soundscape/set", "None")
	waitFor(t, func() bool { return strings.Count(pb.log(), ";") == 4 })
	got := pb.log()
	for _, want := range []string{"soundscape=day", "player=media_player.kitchen", "play |", "stop", "soundscape="} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestBadCommandsDoNothing(t *testing.T) {
	_, conn, pb, _ := fixture(t)
	base := "voidgrid-soundscapes/sleep_sounds/"
	conn.send(base+"soundscape/set", "No such soundscape")
	conn.send(base+"player/set", "Garage")
	conn.send(base+"playing/set", "TOGGLE")
	time.Sleep(100 * time.Millisecond)
	if pb.log() != "" {
		t.Fatalf("unexpected calls: %s", pb.log())
	}
}

func TestFailedCommandKeepsRunning(t *testing.T) {
	_, conn, pb, _ := fixture(t)
	pb.err = errors.New("Home Assistant answered 500")
	conn.send("voidgrid-soundscapes/sleep_sounds/playing/set", "ON")
	waitFor(t, func() bool { return strings.Contains(pb.log(), "play") })
	pb.err = nil
	conn.send("voidgrid-soundscapes/sleep_sounds/playing/set", "OFF")
	waitFor(t, func() bool { return strings.Contains(pb.log(), "stop") })
}

func TestReconnectAndHomeAssistantRestartRepublishEverything(t *testing.T) {
	c, conn, _, _ := fixture(t)
	cfg := "homeassistant/switch/sleep_sounds/playing/config"
	n := conn.sends(cfg)

	// Home Assistant restarted and says so.
	conn.send("homeassistant/status", "online")
	if conn.sends(cfg) != n+1 {
		t.Fatalf("discovery was not republished when Home Assistant came back (%d -> %d)", n, conn.sends(cfg))
	}
	conn.send("homeassistant/status", "offline") // nothing to do
	if conn.sends(cfg) != n+1 {
		t.Fatal("republished on 'offline'")
	}

	// The broker connection was lost and came back: a new connection object, everything sent again.
	c.Disconnected()
	c.Sync() // while disconnected nothing is sent, and nothing panics
	conn2 := newConn()
	c.Connected(conn2)
	if conn2.get("voidgrid-soundscapes/sleep_sounds/status") != "online" || conn2.get(cfg) == "" || conn2.get("voidgrid-soundscapes/sleep_sounds/playing/state") != "OFF" {
		t.Fatal("not everything was sent after reconnecting")
	}
	for _, topic := range []string{"soundscape/set", "player/set", "playing/set"} {
		if conn2.subs["voidgrid-soundscapes/sleep_sounds/"+topic] == nil {
			t.Errorf("not subscribed to %s after reconnecting", topic)
		}
	}
}

func TestPublishFailureIsRetriedOnTheNextSync(t *testing.T) {
	c, conn, pb, _ := fixture(t)
	conn.mu.Lock()
	conn.failPub = errors.New("broker went away")
	conn.mu.Unlock()
	pb.set(playback.State{Playing: true, NowPlaying: "x", Players: pb.State().Players})
	c.Sync()
	topic := "voidgrid-soundscapes/sleep_sounds/playing/state"
	if conn.get(topic) != "OFF" {
		t.Fatal("published despite the failure")
	}
	conn.mu.Lock()
	conn.failPub = nil
	conn.mu.Unlock()
	c.Sync()
	if conn.get(topic) != "ON" {
		t.Fatal("not retried")
	}
}

func TestRunPublishesChangesFromPlayback(t *testing.T) {
	c, conn, pb, _ := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	time.Sleep(20 * time.Millisecond)
	pb.set(playback.State{Playing: true, NowPlaying: "night", Players: pb.State().Players})
	pb.mu.Lock()
	close(pb.ch)
	pb.ch = make(chan struct{})
	pb.mu.Unlock()
	waitFor(t, func() bool { return conn.get("voidgrid-soundscapes/sleep_sounds/playing/state") == "ON" })
}
