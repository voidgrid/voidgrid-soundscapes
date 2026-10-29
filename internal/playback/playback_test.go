package playback

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-soundscapes/internal/ha"
	"github.com/voidgrid/voidgrid-soundscapes/internal/store"
)

type fakeStore struct {
	mu  sync.Mutex
	ids map[string]bool
	set store.Settings
}

func (f *fakeStore) Get(id string) (store.Soundscape, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return store.Soundscape{ID: id}, f.ids[id]
}
func (f *fakeStore) Settings() store.Settings { f.mu.Lock(); defer f.mu.Unlock(); return f.set }
func (f *fakeStore) SetSettings(s store.Settings) {
	f.mu.Lock()
	f.set = s
	f.mu.Unlock()
}

// fakeHA plays the part of Home Assistant: a play command makes the player report that it is playing.
type fakeHA struct {
	mu       sync.Mutex
	players  []ha.Player
	states   map[string]ha.PlayerState
	playErr  error
	stateErr error
	quiet    bool // Play makes the player report "playing" without saying what, like a Cast speaker
	calls    []string
}

func (f *fakeHA) Players(context.Context) ([]ha.Player, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "players")
	return f.players, nil
}
func (f *fakeHA) State(_ context.Context, id string) (ha.PlayerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.states[id], f.stateErr
}
func (f *fakeHA) Play(_ context.Context, id, u string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "play "+id+" "+u)
	if f.playErr != nil {
		return f.playErr
	}
	if f.quiet {
		f.states[id] = ha.PlayerState{State: "playing"}
		return nil
	}
	f.states[id] = ha.PlayerState{State: "playing", MediaContentID: "builtin://radio/" + u}
	return nil
}
func (f *fakeHA) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "stop "+id)
	f.states[id] = ha.PlayerState{State: "idle"}
	return nil
}
func (f *fakeHA) log() string { f.mu.Lock(); defer f.mu.Unlock(); return strings.Join(f.calls, "; ") }

func setup(t *testing.T) (*Service, *fakeStore, *fakeHA) {
	t.Helper()
	st := &fakeStore{ids: map[string]bool{"night": true, "day": true}}
	h := &fakeHA{
		players: []ha.Player{{ID: "media_player.bedroom", Name: "Bedroom"}, {ID: "media_player.kitchen", Name: "Kitchen"}},
		states:  map[string]ha.PlayerState{},
	}
	s, err := New(st, h, Options{PublicURL: "http://192.168.1.117:8007/", Settle: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.refresh(context.Background(), true); err != nil { // read the players, as Run does first
		t.Fatal(err)
	}
	return s, st, h
}

func TestStreamURLAndStreamable(t *testing.T) {
	s, st, _ := setup(t)
	if got := s.State(); got.Streamable || got.StreamURL != "" || got.Soundscape != "" {
		t.Fatalf("nothing chosen yet: %+v", got)
	}
	st.set.Soundscape = "night"
	if got := s.State(); !got.Streamable || got.StreamURL != "http://192.168.1.117:8007/stream/night" {
		t.Fatalf("%+v", got)
	}
	st.set.Soundscape = "deleted" // a soundscape that no longer exists counts as none
	if got := s.State(); got.Soundscape != "" || got.Streamable {
		t.Fatalf("%+v", got)
	}
	st.ids["odd id"] = true
	st.set.Soundscape = "odd id"
	if got := s.State().StreamURL; got != "http://192.168.1.117:8007/stream/odd%20id" {
		t.Fatalf("URL not escaped: %q", got)
	}
}

func TestPublicURLIsValidated(t *testing.T) {
	for _, bad := range []string{"192.168.1.117:8007", "ftp://x", "http://x/stream", "http://x?y=1", "http://"} {
		if _, err := New(&fakeStore{}, nil, Options{PublicURL: bad}); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	if _, err := New(&fakeStore{}, nil, Options{PublicURL: "https://soundscapes.lan"}); err != nil {
		t.Error(err)
	}
}

func TestPlayRecordsTheChoicesAndPlayingIsReadBackFromThePlayer(t *testing.T) {
	s, st, h := setup(t)
	ctx := context.Background()
	if err := s.Play(ctx, "night", "media_player.bedroom"); err != nil {
		t.Fatal(err)
	}
	if st.set != (store.Settings{Soundscape: "night", Player: "media_player.bedroom"}) {
		t.Fatalf("choices not recorded: %+v", st.set)
	}
	if !strings.Contains(h.log(), "play media_player.bedroom http://192.168.1.117:8007/stream/night") {
		t.Fatalf("calls: %s", h.log())
	}
	// Not playing until the player says so.
	if s.State().Playing {
		t.Fatal("reported playing before the player was read")
	}
	if err := s.refresh(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := s.State(); !got.Playing || got.NowPlaying != "night" {
		t.Fatalf("%+v", got)
	}
}

func TestPlayingMeansOurStreamOnTheChosenPlayer(t *testing.T) {
	s, st, h := setup(t)
	st.set = store.Settings{Soundscape: "night", Player: "media_player.bedroom"}
	ctx := context.Background()
	for name, c := range map[string]struct {
		ps   ha.PlayerState
		want bool
	}{
		"idle":                              {ha.PlayerState{State: "idle"}, false},
		"playing something else":            {ha.PlayerState{State: "playing", MediaContentID: "spotify:track:1"}, false},
		"paused on ours":                    {ha.PlayerState{State: "paused", MediaContentID: "http://192.168.1.117:8007/stream/night"}, false},
		"playing the other stream":          {ha.PlayerState{State: "playing", MediaContentID: "http://192.168.1.117:8007/stream/day"}, false},
		"playing ours":                      {ha.PlayerState{State: "playing", MediaContentID: "http://192.168.1.117:8007/stream/night"}, true},
		"playing ours, wrapped":             {ha.PlayerState{State: "playing", MediaContentID: "builtin://radio/http://192.168.1.117:8007/stream/night"}, true},
		"buffering ours":                    {ha.PlayerState{State: "buffering", MediaContentID: "http://192.168.1.117:8007/stream/night"}, true},
		"a longer id with ours as a prefix": {ha.PlayerState{State: "playing", MediaContentID: "http://192.168.1.117:8007/stream/nighttime"}, false},
		"ours followed by a query":          {ha.PlayerState{State: "playing", MediaContentID: "http://192.168.1.117:8007/stream/night?x=1"}, true},
	} {
		h.states["media_player.bedroom"] = c.ps
		_ = s.refresh(ctx, false)
		got := s.State().Playing
		if got != c.want {
			t.Errorf("%s: Playing = %t, want %t", name, got, c.want)
		}
	}
}

func TestStopOnlyStopsOurStreamAndSaysWhenItDoesNot(t *testing.T) {
	s, st, h := setup(t)
	ctx := context.Background()
	st.set = store.Settings{Soundscape: "night", Player: "media_player.bedroom"}
	h.states["media_player.bedroom"] = ha.PlayerState{State: "playing", MediaContentID: "spotify:track:1"}
	err := s.Stop(ctx)
	if err == nil || !strings.Contains(err.Error(), "left alone") {
		t.Fatalf("Stop on a speaker playing something else = %v, want an explanation", err)
	}
	if strings.Contains(h.log(), "stop") {
		t.Fatalf("stopped a speaker that was playing something else: %s", h.log())
	}
	if got := s.State(); got.Playing || !got.PlayerPlaying {
		t.Fatalf("%+v", got)
	}
	// Idle speaker: nothing to do, and that is not an error.
	h.states["media_player.bedroom"] = ha.PlayerState{State: "idle"}
	if err := s.Stop(ctx); err != nil || strings.Contains(h.log(), "stop") {
		t.Fatalf("Stop on an idle speaker = %v (%s)", err, h.log())
	}
	if err := s.Play(ctx, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.log(), "stop media_player.bedroom") {
		t.Fatalf("calls: %s", h.log())
	}
}

// Some players (a Cast speaker) say they are playing but never say what. That is the real speaker this was
// first tried on: Stop used to do nothing there, silently.
func TestAPlayerThatDoesNotSayWhatItPlaysIsOursOnlyIfWeStartedIt(t *testing.T) {
	s, st, h := setup(t)
	ctx := context.Background()
	st.set = store.Settings{Soundscape: "night", Player: "media_player.bedroom"}
	h.states["media_player.bedroom"] = ha.PlayerState{State: "playing"} // no media_content_id
	_ = s.refresh(ctx, false)
	if got := s.State(); got.Playing || !got.PlayerPlaying {
		t.Fatalf("playing something unidentified and not started by us: %+v", got)
	}

	h.quiet = true // Play makes the fake report "playing" with no media id, like that speaker
	if err := s.Play(ctx, "night", "media_player.bedroom"); err != nil {
		t.Fatal(err)
	}
	_ = s.refresh(ctx, false)
	if got := s.State(); !got.Playing || got.NowPlaying != "night" {
		t.Fatalf("started by us and reported playing, but not recognised: %+v", got)
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.log(), "stop media_player.bedroom") {
		t.Fatalf("Stop did not stop it: %s", h.log())
	}
	_ = s.refresh(ctx, false)
	if got := s.State(); got.Playing || got.PlayerPlaying {
		t.Fatalf("still playing after Stop: %+v", got)
	}

	// Something else starts later on the same speaker, still unidentified: it is not ours any more.
	h.states["media_player.bedroom"] = ha.PlayerState{State: "playing"}
	_ = s.refresh(ctx, false)
	if got := s.State(); got.Playing {
		t.Fatalf("claimed something we did not start: %+v", got)
	}
}

func TestOurNoteExpiresIfTheSpeakerNeverPlayed(t *testing.T) {
	s, st, h := setup(t)
	ctx := context.Background()
	st.set = store.Settings{Soundscape: "night", Player: "media_player.bedroom"}
	h.quiet = true
	_ = s.Play(ctx, "night", "media_player.bedroom")
	h.states["media_player.bedroom"] = ha.PlayerState{State: "idle"} // it never started
	_ = s.refresh(ctx, false)
	s.mu.Lock()
	n := s.started["media_player.bedroom"]
	n.at = time.Now().Add(-2 * startedGrace)
	s.started["media_player.bedroom"] = n
	s.mu.Unlock()
	_ = s.refresh(ctx, false)
	h.states["media_player.bedroom"] = ha.PlayerState{State: "playing"}
	_ = s.refresh(ctx, false)
	if got := s.State(); got.Playing {
		t.Fatalf("a stale note made something else look like ours: %+v", got)
	}
}

func TestForceStopStopsWhateverIsPlaying(t *testing.T) {
	s, st, h := setup(t)
	ctx := context.Background()
	if err := s.ForceStop(ctx); err == nil {
		t.Error("ForceStop with no player chosen should say so")
	}
	st.set = store.Settings{Soundscape: "night", Player: "media_player.bedroom"}
	h.states["media_player.bedroom"] = ha.PlayerState{State: "playing", MediaContentID: "spotify:track:1"}
	if err := s.ForceStop(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.log(), "stop media_player.bedroom") {
		t.Fatalf("calls: %s", h.log())
	}
}

func TestChangingTheSoundscapeWhilePlayingRepointsThePlayer(t *testing.T) {
	s, _, h := setup(t)
	ctx := context.Background()
	if err := s.Play(ctx, "night", "media_player.bedroom"); err != nil {
		t.Fatal(err)
	}
	_ = s.refresh(ctx, false)
	if err := s.SelectSoundscape(ctx, "day"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(h.log(), "play media_player.bedroom http://192.168.1.117:8007/stream/day") {
		t.Fatalf("calls: %s", h.log())
	}
	// Not playing: choosing only records the choice.
	h2log := len(h.calls)
	_ = s.Stop(ctx)
	_ = s.refresh(ctx, false)
	if err := s.SelectSoundscape(ctx, "night"); err != nil {
		t.Fatal(err)
	}
	if got := h.calls[h2log:]; len(got) != 1 || !strings.HasPrefix(got[0], "stop") {
		t.Fatalf("selecting while idle sent commands: %v", got)
	}
	if err := s.SelectSoundscape(ctx, "nope"); err == nil {
		t.Error("an unknown soundscape was accepted")
	}
}

func TestChangingThePlayerWhilePlayingMovesPlayback(t *testing.T) {
	s, _, h := setup(t)
	ctx := context.Background()
	_ = s.Play(ctx, "night", "media_player.bedroom")
	_ = s.refresh(ctx, false)
	if err := s.SelectPlayer(ctx, "media_player.kitchen"); err != nil {
		t.Fatal(err)
	}
	log := h.log()
	if !strings.Contains(log, "stop media_player.bedroom; play media_player.kitchen http://192.168.1.117:8007/stream/night") {
		t.Fatalf("calls: %s", log)
	}
	if err := s.SelectPlayer(ctx, "media_player.nope"); err == nil {
		t.Error("an unknown player was accepted")
	}
}

func TestCommandsExplainWhatIsMissing(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	for name, c := range map[string]struct{ sc, pl, want string }{
		"nothing chosen":     {"", "", "no soundscape"},
		"no player":          {"night", "", "no player"},
		"unknown soundscape": {"nope", "media_player.bedroom", "unknown soundscape"},
		"unknown player":     {"night", "media_player.nope", "unknown player"},
	} {
		if err := s.Play(ctx, c.sc, c.pl); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}

	noHA, _ := New(&fakeStore{ids: map[string]bool{"night": true}}, nil, Options{PublicURL: "http://h:1"})
	if err := noHA.Play(ctx, "night", "media_player.x"); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("without Home Assistant: %v", err)
	}
	if got := noHA.State(); got.HAConfigured {
		t.Error("HAConfigured is true without Home Assistant")
	}
	noURL, _ := New(&fakeStore{ids: map[string]bool{"night": true}}, &fakeHA{states: map[string]ha.PlayerState{}}, Options{})
	if err := noURL.Play(ctx, "night", "media_player.x"); err == nil || !strings.Contains(err.Error(), "public URL") {
		t.Errorf("without a public URL: %v", err)
	}
}

func TestHomeAssistantErrorsAreReportedAndClear(t *testing.T) {
	s, st, h := setup(t)
	ctx := context.Background()
	st.set = store.Settings{Soundscape: "night", Player: "media_player.bedroom"}
	h.stateErr = errors.New("Home Assistant answered 500")
	if err := s.refresh(ctx, false); err == nil {
		t.Fatal("expected an error")
	}
	if got := s.State(); got.HAError == "" || got.Playing {
		t.Fatalf("%+v", got)
	}
	h.stateErr = nil
	_ = s.refresh(ctx, false)
	if got := s.State(); got.HAError != "" {
		t.Fatalf("the error did not clear: %+v", got)
	}
	h.playErr = errors.New("Home Assistant answered 400: no such entity")
	if err := s.Play(ctx, "night", "media_player.bedroom"); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("Play = %v", err)
	}
}

func TestChangedFiresWhenTheStateMoves(t *testing.T) {
	s, _, h := setup(t)
	ch := s.Changed()
	h.states["media_player.bedroom"] = ha.PlayerState{State: "playing", MediaContentID: "x"}
	_ = s.SelectPlayer(context.Background(), "media_player.bedroom")
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no change notification")
	}
}

func TestRunPollsAndReactsToACommand(t *testing.T) {
	s, _, h := setup(t)
	s.opt.Poll = time.Hour // only the kick after a command may cause a read
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx, nopLog{})
	time.Sleep(20 * time.Millisecond)
	ch := s.Changed()
	if err := s.Play(ctx, "night", "media_player.bedroom"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for !s.State().Playing {
		select {
		case <-ch:
			ch = s.Changed()
		case <-deadline:
			t.Fatalf("never saw the player start: %+v (%s)", s.State(), h.log())
		}
	}
}

type nopLog struct{}

func (nopLog) Warn(string, ...any) {}
