// Package hamqtt shows the playback controls to Home Assistant as MQTT entities (a select for the soundscape,
// a select for the player, a play/stop switch, and some read-only sensors) and turns commands from them
// into playback calls. It uses Home Assistant's MQTT discovery, so nothing is configured on the HA side.
package hamqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/voidgrid/voidgrid-soundscapes/internal/playback"
	"github.com/voidgrid/voidgrid-soundscapes/internal/store"
)

// Conn is the part of an MQTT connection the controller uses.
type Conn interface {
	Publish(topic, payload string, retain bool) error
	Subscribe(topic string, handler func(topic, payload string)) error
}

// Playback is the part of playback.Service the controller uses.
type Playback interface {
	State() playback.State
	Changed() <-chan struct{}
	SelectSoundscape(ctx context.Context, id string) error
	SelectPlayer(ctx context.Context, id string) error
	Play(ctx context.Context, soundscape, player string) error
	Stop(ctx context.Context) error
}

// Soundscapes lists the saved soundscapes.
type Soundscapes interface {
	List() []store.Soundscape
}

// Options name the Home Assistant device.
type Options struct {
	DeviceName string // shown in Home Assistant; entity IDs derive from it
	Prefix     string // Home Assistant's discovery prefix (homeassistant)
	Version    string // the program version, shown on the device
}

const (
	online  = "online"
	offline = "offline"
	none    = "None" // an empty select: Home Assistant shows "unknown"
)

type entity struct {
	key       string
	component string
	name      string
	command   bool // has a command topic
}

var entities = []entity{
	{"soundscape", "select", "Soundscape", true},
	{"player", "select", "Player", true},
	{"playing", "switch", "Playing", true},
	{"stream_url", "sensor", "Stream URL", false},
	{"streamable", "binary_sensor", "Streamable", false},
	{"now_playing", "sensor", "Now playing", false},
}

// Controller keeps Home Assistant's entities and the playback state in step.
type Controller struct {
	pb  Playback
	sc  Soundscapes
	opt Options
	log *slog.Logger

	deviceID string // the device name as a topic-safe slug
	base     string // topic prefix for everything of ours

	syncMu sync.Mutex // one Sync at a time, so an older state is never published after a newer one

	mu        sync.Mutex
	conn      Conn
	published map[string]string // topic -> last payload sent on this connection
	soundOpts map[string]string // option text -> soundscape ID, as last published
	playOpts  map[string]string // option text -> player entity ID
	ctx       context.Context
}

// New returns a controller. Nothing is sent until a connection is handed to Connected.
func New(pb Playback, sc Soundscapes, opt Options, log *slog.Logger) *Controller {
	id := Slug(opt.DeviceName)
	return &Controller{pb: pb, sc: sc, opt: opt, log: log, deviceID: id, base: "voidgrid-soundscapes/" + id,
		published: map[string]string{}}
}

// Slug turns a device name into something safe in a topic and an entity ID: "Sleep Sounds" -> "sleep_sounds".
func Slug(name string) string {
	var b strings.Builder
	sep := true
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			sep = false
		} else if !sep {
			b.WriteByte('_')
			sep = true
		}
	}
	if out := strings.TrimRight(b.String(), "_"); out != "" {
		return out
	}
	return "voidgrid_soundscapes"
}

// StatusTopic is where the controller says whether it is online; its last will sets it to offline.
func (c *Controller) StatusTopic() string { return c.base + "/status" }

// ClientID is a stable MQTT client ID for this device name, so a restart takes over its own old session.
func (c *Controller) ClientID() string { return "voidgrid-soundscapes-" + c.deviceID }

func (c *Controller) topic(key, suffix string) string { return c.base + "/" + key + "/" + suffix }

// Connected is called on every (re)connection. It subscribes, announces the device and publishes everything.
func (c *Controller) Connected(conn Conn) {
	c.mu.Lock()
	c.conn = conn
	c.published = map[string]string{}
	c.mu.Unlock()

	for _, e := range entities {
		if !e.command {
			continue
		}
		e := e
		if err := conn.Subscribe(c.topic(e.key, "set"), func(_, payload string) { go c.command(e.key, payload) }); err != nil {
			c.log.Error("cannot subscribe", "entity", e.key, "err", err)
		}
	}
	// Home Assistant says "online" here when it (re)starts, and has forgotten the discovery messages by then.
	if err := conn.Subscribe(c.opt.Prefix+"/status", func(_, payload string) {
		if payload == online {
			c.mu.Lock()
			c.published = map[string]string{}
			c.mu.Unlock()
			c.Sync()
		}
	}); err != nil {
		c.log.Error("cannot subscribe to Home Assistant's status", "err", err)
	}
	c.Sync()
}

// Disconnected is called when the connection is lost.
func (c *Controller) Disconnected() {
	c.mu.Lock()
	c.conn = nil
	c.mu.Unlock()
}

// Run keeps the entities up to date until ctx ends.
func (c *Controller) Run(ctx context.Context) {
	c.mu.Lock()
	c.ctx = ctx
	c.mu.Unlock()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		ch := c.pb.Changed()
		c.Sync()
		select {
		case <-ctx.Done():
			return
		case <-ch:
		case <-t.C: // soundscapes are created and renamed elsewhere, which does not notify us
		}
	}
}

// unique returns option texts for items: the names, with " (id)" added where two share a name.
func unique(items [][2]string) (texts []string, byText map[string]string) {
	count := map[string]int{}
	for _, it := range items {
		count[it[1]]++
	}
	byText = map[string]string{}
	for _, it := range items {
		text := it[1]
		if count[text] > 1 {
			text = fmt.Sprintf("%s (%s)", it[1], it[0])
		}
		for byText[text] != "" { // a pathological name that already looks like a suffixed one
			text += "'"
		}
		byText[text] = it[0]
		texts = append(texts, text)
	}
	return texts, byText
}

func textFor(byText map[string]string, id string) string {
	for text, i := range byText {
		if i == id {
			return text
		}
	}
	return none
}

// Sync publishes whatever differs from what this connection has already been sent.
func (c *Controller) Sync() {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return
	}
	st := c.pb.State()

	var sItems, pItems [][2]string
	for _, s := range c.sc.List() {
		sItems = append(sItems, [2]string{s.ID, s.Name})
	}
	for _, p := range st.Players {
		pItems = append(pItems, [2]string{p.ID, p.Name})
	}
	sTexts, sBy := unique(sItems)
	pTexts, pBy := unique(pItems)
	c.mu.Lock()
	c.soundOpts, c.playOpts = sBy, pBy
	c.mu.Unlock()

	msgs := []message{{c.StatusTopic(), online}}
	for _, e := range entities {
		var opts []string
		switch e.key {
		case "soundscape":
			opts = sTexts
		case "player":
			opts = pTexts
		}
		msgs = append(msgs, message{fmt.Sprintf("%s/%s/%s/%s/config", c.opt.Prefix, e.component, c.deviceID, e.key), c.config(e, opts)})
	}
	msgs = append(msgs,
		message{c.topic("soundscape", "state"), textFor(sBy, st.Soundscape)},
		message{c.topic("player", "state"), textFor(pBy, st.Player)},
		message{c.topic("playing", "state"), onOff(st.Playing)},
		message{c.topic("stream_url", "state"), orNone(st.StreamURL)},
		message{c.topic("streamable", "state"), onOff(st.Streamable)},
		message{c.topic("now_playing", "state"), c.nowPlaying(st, sItems)},
	)
	for _, m := range msgs {
		c.mu.Lock()
		prev, sent := c.published[m.topic]
		c.mu.Unlock()
		if sent && prev == m.payload {
			continue
		}
		if err := conn.Publish(m.topic, m.payload, true); err != nil {
			c.log.Warn("cannot publish to MQTT", "topic", m.topic, "err", err)
			return // try again on the next tick
		}
		c.mu.Lock()
		c.published[m.topic] = m.payload
		c.mu.Unlock()
	}
}

type message struct{ topic, payload string }

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

func orNone(s string) string {
	if s == "" {
		return none
	}
	return s
}

func (c *Controller) nowPlaying(st playback.State, sItems [][2]string) string {
	if st.NowPlaying == "" {
		return none
	}
	for _, it := range sItems {
		if it[0] == st.NowPlaying {
			return it[1]
		}
	}
	return st.NowPlaying
}

// config builds one entity's discovery message.
func (c *Controller) config(e entity, options []string) string {
	m := map[string]any{
		"name":            e.name,
		"unique_id":       c.deviceID + "_" + e.key,
		"has_entity_name": true,
		"device": map[string]any{
			"identifiers":  []string{"voidgrid_soundscapes_" + c.deviceID},
			"name":         c.opt.DeviceName,
			"manufacturer": "voidgrid",
			"model":        "voidgrid-soundscapes",
			"sw_version":   c.opt.Version,
		},
		"availability_topic":    c.StatusTopic(),
		"payload_available":     online,
		"payload_not_available": offline,
		"state_topic":           c.topic(e.key, "state"),
	}
	if e.command {
		m["command_topic"] = c.topic(e.key, "set")
	}
	switch e.component {
	case "select":
		if options == nil {
			options = []string{}
		}
		m["options"] = options
	case "switch":
		m["payload_on"], m["payload_off"], m["state_on"], m["state_off"] = "ON", "OFF", "ON", "OFF"
	case "binary_sensor":
		m["payload_on"], m["payload_off"] = "ON", "OFF"
	}
	b, _ := json.Marshal(m) // plain strings and lists cannot fail to encode
	return string(b)
}

// command runs a command from Home Assistant.
func (c *Controller) command(key, payload string) {
	c.mu.Lock()
	ctx := c.ctx
	sBy, pBy := c.soundOpts, c.playOpts
	c.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var err error
	switch key {
	case "soundscape":
		id, ok := sBy[payload]
		if !ok && payload != none {
			err = fmt.Errorf("unknown soundscape %q", payload)
			break
		}
		err = c.pb.SelectSoundscape(ctx, id)
	case "player":
		id, ok := pBy[payload]
		if !ok && payload != none {
			err = fmt.Errorf("unknown player %q", payload)
			break
		}
		err = c.pb.SelectPlayer(ctx, id)
	case "playing":
		switch payload {
		case "ON":
			err = c.pb.Play(ctx, "", "")
		case "OFF":
			err = c.pb.Stop(ctx)
		default:
			err = fmt.Errorf("unknown payload %q", payload)
		}
	}
	if err != nil {
		c.log.Warn("MQTT command failed", "entity", key, "payload", payload, "err", err)
	}
	c.Sync()
}
