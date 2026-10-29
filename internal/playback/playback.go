// Package playback decides what should be playing where: which soundscape and which Home Assistant media
// player are chosen, whether that player is playing our stream, and the commands to start and stop it. The
// web page and the MQTT entities are both thin fronts on this.
package playback

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/voidgrid/voidgrid-soundscapes/internal/ha"
	"github.com/voidgrid/voidgrid-soundscapes/internal/store"
)

// Soundscapes is the part of the store this package uses.
type Soundscapes interface {
	Get(id string) (store.Soundscape, bool)
	Settings() store.Settings
	SetSettings(store.Settings)
}

// HA is the part of the Home Assistant client this package uses. It is nil when Home Assistant is not
// configured: the choices can still be made, but nothing can be played.
type HA interface {
	Players(ctx context.Context) ([]ha.Player, error)
	State(ctx context.Context, id string) (ha.PlayerState, error)
	Play(ctx context.Context, id, streamURL string) error
	Stop(ctx context.Context, id string) error
}

// Options configure a Service.
type Options struct {
	PublicURL    string        // how a player reaches this program, such as http://192.168.1.117:8007 (no path)
	Poll         time.Duration // how often a player's state is read (default 5s)
	PlayersEvery time.Duration // how often the list of players is read (default 1m)
	Settle       time.Duration // wait after a command before reading the player again (default 1500ms)
}

// State is everything a front shows.
type State struct {
	HAConfigured  bool        `json:"haConfigured"`
	HAError       string      `json:"haError,omitempty"` // the last problem talking to Home Assistant
	Soundscape    string      `json:"soundscape"`        // chosen soundscape ID, "" for none
	Player        string      `json:"player"`            // chosen player entity ID, "" for none
	Players       []ha.Player `json:"players"`
	StreamURL     string      `json:"streamUrl"`     // of the chosen soundscape; "" if none or no public URL
	Streamable    bool        `json:"streamable"`    // a soundscape is chosen and a player could reach it
	Playing       bool        `json:"playing"`       // the chosen player is playing the chosen soundscape's stream
	PlayerPlaying bool        `json:"playerPlaying"` // the chosen player is playing something, ours or not
	NowPlaying    string      `json:"nowPlaying"`    // that soundscape's ID while Playing, otherwise ""
}

// started notes that we told a player to play a stream. Some players (a Cast speaker, say) never report what
// they are playing, so "playing, and we started it" is the best evidence that it is ours.
type started struct {
	url string
	at  time.Time
}

// startedGrace is how long a player may take to begin (or to report it has) before the note is dropped.
const startedGrace = 30 * time.Second

// Service holds the choices and keeps track of the player.
type Service struct {
	sc   Soundscapes
	ha   HA
	opt  Options
	base string // PublicURL without a trailing slash

	opMu sync.Mutex // one command at a time

	mu        sync.Mutex
	players   []ha.Player
	playersAt time.Time
	pstate    ha.PlayerState
	pstateOf  string // the player pstate belongs to
	haErr     string
	started   map[string]started // players we told to play and have not stopped, by entity ID
	changed   chan struct{}
	kick      chan struct{}
}

// New returns a service. h may be nil.
func New(sc Soundscapes, h HA, opt Options) (*Service, error) {
	if opt.Poll <= 0 {
		opt.Poll = 5 * time.Second
	}
	if opt.PlayersEvery <= 0 {
		opt.PlayersEvery = time.Minute
	}
	if opt.Settle <= 0 {
		opt.Settle = 1500 * time.Millisecond
	}
	base := ""
	if opt.PublicURL != "" {
		u, err := url.Parse(opt.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
			return nil, errors.New("the public URL must look like http://192.168.1.117:8007 (a scheme and host, no path)")
		}
		base = strings.TrimRight(opt.PublicURL, "/")
	}
	return &Service{sc: sc, ha: h, opt: opt, base: base, changed: make(chan struct{}), kick: make(chan struct{}, 1), started: map[string]started{}}, nil
}

// Changed returns a channel that is closed the next time the state may have changed. Take a new one after
// each wake-up.
func (s *Service) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

func (s *Service) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Service) streamURL(id string) string {
	if s.base == "" || id == "" {
		return ""
	}
	return s.base + "/stream/" + url.PathEscape(id)
}

// State returns the current state.
func (s *Service) State() State {
	set := s.sc.Settings()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked(set)
}

func (s *Service) stateLocked(set store.Settings) State {
	st := State{HAConfigured: s.ha != nil, HAError: s.haErr, Player: set.Player, Players: append([]ha.Player{}, s.players...)}
	if _, ok := s.sc.Get(set.Soundscape); ok {
		st.Soundscape = set.Soundscape
	}
	st.StreamURL = s.streamURL(st.Soundscape)
	st.Streamable = st.StreamURL != ""
	if st.Player != "" && s.pstateOf == st.Player && (s.pstate.State == "playing" || s.pstate.State == "buffering") {
		st.PlayerPlaying = true
		switch {
		case !st.Streamable:
		case containsStream(s.pstate.MediaContentID, st.StreamURL):
			st.Playing = true
		case s.pstate.MediaContentID == "" && s.started[st.Player].url == st.StreamURL:
			st.Playing = true // it does not say what it plays, but we started this stream on it
		}
		if st.Playing {
			st.NowPlaying = st.Soundscape
		}
	}
	return st
}

// containsStream reports whether what a player is playing is the stream at u. Integrations may wrap the URL
// (builtin://radio/<url>), so it is searched for, but it must end there: .../stream/nighttime is not .../stream/night.
func containsStream(playing, u string) bool {
	for from := 0; ; {
		i := strings.Index(playing[from:], u)
		if i < 0 {
			return false
		}
		end := from + i + len(u)
		if end == len(playing) || !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._-%", rune(playing[end])) {
			return true
		}
		from += i + 1
	}
}

// SelectSoundscape chooses a soundscape ("" for none). If the chosen player is playing our stream it is
// pointed at the new one.
func (s *Service) SelectSoundscape(ctx context.Context, id string) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if id != "" {
		if _, ok := s.sc.Get(id); !ok {
			return errors.New("unknown soundscape")
		}
	}
	was := s.State()
	set := s.sc.Settings()
	set.Soundscape = id
	s.sc.SetSettings(set)
	s.changedNow()
	if was.Playing && id != "" {
		return s.playLocked(ctx, id, was.Player)
	}
	return nil
}

// SelectPlayer chooses a player ("" for none). If the old player was playing our stream, that is stopped and
// the new player starts it.
func (s *Service) SelectPlayer(ctx context.Context, id string) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if id != "" && s.ha != nil {
		if err := s.checkPlayer(id); err != nil {
			return err
		}
	}
	was := s.State()
	set := s.sc.Settings()
	set.Player = id
	s.sc.SetSettings(set)
	s.changedNow()
	if was.Playing {
		if err := s.ha.Stop(ctx, was.Player); err != nil {
			return err
		}
		if id != "" {
			return s.playLocked(ctx, was.Soundscape, id)
		}
	}
	return nil
}

// checkPlayer accepts a player that is in the list, or any ID when the list has not been read yet.
func (s *Service) checkPlayer(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.players) == 0 {
		return nil
	}
	for _, p := range s.players {
		if p.ID == id {
			return nil
		}
	}
	return errors.New("unknown player")
}

// Play starts a soundscape on a player. Empty arguments mean the current choices; non-empty ones become the
// choices.
func (s *Service) Play(ctx context.Context, soundscape, player string) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	set := s.sc.Settings()
	if soundscape == "" {
		soundscape = set.Soundscape
	}
	if player == "" {
		player = set.Player
	}
	if soundscape == "" {
		return errors.New("no soundscape is chosen")
	}
	if _, ok := s.sc.Get(soundscape); !ok {
		return errors.New("unknown soundscape")
	}
	if player == "" {
		return errors.New("no player is chosen")
	}
	if s.ha != nil {
		if err := s.checkPlayer(player); err != nil {
			return err
		}
	}
	return s.playLocked(ctx, soundscape, player)
}

// playLocked records the choices and sends the command. Callers hold opMu.
func (s *Service) playLocked(ctx context.Context, soundscape, player string) error {
	if s.ha == nil {
		return errors.New("Home Assistant is not configured (set VOIDGRID_SOUNDSCAPES_HA_URL and VOIDGRID_SOUNDSCAPES_HA_TOKEN)")
	}
	u := s.streamURL(soundscape)
	if u == "" {
		return errors.New("the public URL is not configured (set VOIDGRID_SOUNDSCAPES_PUBLIC_URL), so a player cannot be told where the stream is")
	}
	s.sc.SetSettings(store.Settings{Soundscape: soundscape, Player: player})
	s.changedNow()
	if err := s.ha.Play(ctx, player, u); err != nil {
		return err
	}
	s.mu.Lock()
	s.started[player] = started{url: u, at: time.Now()}
	s.mu.Unlock()
	s.pollSoon()
	return nil
}

// Stop stops the chosen player, but only if it is playing our stream: a speaker that is playing something
// else is left alone, and the error says so rather than doing nothing silently.
func (s *Service) Stop(ctx context.Context) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.ha == nil {
		return errors.New("Home Assistant is not configured")
	}
	if err := s.refresh(ctx, false); err != nil { // read the player now rather than trust a poll that may be seconds old
		return err
	}
	st := s.State()
	switch {
	case st.Playing:
		return s.stopLocked(ctx, st.Player)
	case st.PlayerPlaying:
		return errors.New("the speaker is playing something that is not identified as this soundscape, so it was left alone (use Stop speaker on the web page to stop it anyway)")
	}
	return nil
}

// ForceStop stops the chosen player whatever it is playing. It is for an explicit request about a named
// speaker, such as the Stop button on the web page.
func (s *Service) ForceStop(ctx context.Context) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.ha == nil {
		return errors.New("Home Assistant is not configured")
	}
	player := s.sc.Settings().Player
	if player == "" {
		return errors.New("no player is chosen")
	}
	return s.stopLocked(ctx, player)
}

func (s *Service) stopLocked(ctx context.Context, player string) error {
	if err := s.ha.Stop(ctx, player); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.started, player)
	s.mu.Unlock()
	s.pollSoon()
	return nil
}

func (s *Service) changedNow() {
	s.mu.Lock()
	s.notifyLocked()
	s.mu.Unlock()
}

// pollSoon asks Run to read the player again shortly, once the player has had time to react.
func (s *Service) pollSoon() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// refresh reads the chosen player's state, and the list of players if it is due (or force is set).
func (s *Service) refresh(ctx context.Context, force bool) error {
	if s.ha == nil {
		return nil
	}
	s.mu.Lock()
	dueList := force || s.playersAt.IsZero() || time.Since(s.playersAt) >= s.opt.PlayersEvery
	s.mu.Unlock()

	var firstErr error
	if dueList {
		list, err := s.ha.Players(ctx)
		s.mu.Lock()
		if err != nil {
			firstErr = err
		} else {
			s.players, s.playersAt = list, time.Now()
		}
		s.mu.Unlock()
	}
	player := s.sc.Settings().Player
	var ps ha.PlayerState
	if player != "" {
		var err error
		if ps, err = s.ha.State(ctx, player); err != nil && firstErr == nil {
			firstErr = err
		}
		if err != nil {
			ps = ha.PlayerState{}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if player != "" && ps.State != "playing" && ps.State != "buffering" {
		if st, ok := s.started[player]; ok && time.Since(st.at) > startedGrace {
			delete(s.started, player) // it stopped (or never started): do not claim the next thing it plays
		}
	}
	msg := ""
	if firstErr != nil {
		msg = firstErr.Error()
	}
	if msg != s.haErr || ps != s.pstate || player != s.pstateOf {
		s.haErr, s.pstate, s.pstateOf = msg, ps, player
		s.notifyLocked()
	}
	return firstErr
}

// Run keeps the player's state up to date until ctx ends.
func (s *Service) Run(ctx context.Context, log interface{ Warn(string, ...any) }) {
	if s.ha == nil {
		return
	}
	var lastErr string
	read := func() {
		c, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if err := s.refresh(c, false); err != nil {
			if err.Error() != lastErr { // say it once, not every poll
				log.Warn("cannot read from Home Assistant", "err", err)
			}
			lastErr = err.Error()
		} else {
			lastErr = ""
		}
	}
	read()
	t := time.NewTicker(s.opt.Poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			read()
		case <-s.kick:
			select {
			case <-time.After(s.opt.Settle):
			case <-ctx.Done():
				return
			}
			read()
		}
	}
}

// String is for logs; it names the choices only.
func (st State) String() string {
	return fmt.Sprintf("soundscape=%q player=%q playing=%t", st.Soundscape, st.Player, st.Playing)
}
