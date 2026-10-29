package ha

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const token = "s3cret-token-value"

type call struct{ method, path, auth, body string }

func stub(t *testing.T, handler http.HandlerFunc) (*Client, *[]call) {
	t.Helper()
	var mu sync.Mutex
	var calls []call
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, call{r.Method, r.URL.Path, r.Header.Get("Authorization"), string(b)})
		mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(ts.Close)
	c, err := New(ts.URL+"/", token)
	if err != nil {
		t.Fatal(err)
	}
	return c, &calls
}

func TestPlayersAreMediaPlayersOnlyAndSorted(t *testing.T) {
	c, calls := stub(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"entity_id":"light.kitchen","state":"on","attributes":{"friendly_name":"Kitchen"}},
			{"entity_id":"media_player.zeta","state":"idle","attributes":{"friendly_name":"zeta speaker"}},
			{"entity_id":"media_player.bedroom","state":"playing","attributes":{"friendly_name":"Bedroom"}},
			{"entity_id":"media_player.nameless","state":"off","attributes":{}}]`))
	})
	got, err := c.Players(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Player{{"media_player.bedroom", "Bedroom"}, {"media_player.nameless", "media_player.nameless"}, {"media_player.zeta", "zeta speaker"}}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("got %+v", got)
	}
	if (*calls)[0].auth != "Bearer "+token || (*calls)[0].path != "/api/states" {
		t.Fatalf("call %+v", (*calls)[0])
	}
}

func TestStateReadsWhatAPlayerIsPlaying(t *testing.T) {
	c, calls := stub(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"entity_id":"media_player.bedroom","state":"playing","attributes":{"media_content_id":"builtin://radio/http://h:8007/stream/night"}}`))
	})
	st, err := c.State(context.Background(), "media_player.bedroom")
	if err != nil || st.State != "playing" || st.MediaContentID != "builtin://radio/http://h:8007/stream/night" {
		t.Fatalf("%+v %v", st, err)
	}
	if (*calls)[0].path != "/api/states/media_player.bedroom" {
		t.Fatalf("path %q", (*calls)[0].path)
	}
}

func TestPlayAndStopSendTheRightServiceCalls(t *testing.T) {
	c, calls := stub(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	if err := c.Play(context.Background(), "media_player.bedroom", "http://h:8007/stream/night"); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(context.Background(), "media_player.bedroom"); err != nil {
		t.Fatal(err)
	}
	p, s := (*calls)[0], (*calls)[1]
	if p.method != "POST" || p.path != "/api/services/media_player/play_media" || p.auth != "Bearer "+token {
		t.Fatalf("play call %+v", p)
	}
	var body map[string]string
	_ = json.Unmarshal([]byte(p.body), &body)
	if body["entity_id"] != "media_player.bedroom" || body["media_content_id"] != "http://h:8007/stream/night" || body["media_content_type"] != "music" {
		t.Fatalf("play body %s", p.body)
	}
	if s.path != "/api/services/media_player/media_stop" || !strings.Contains(s.body, `"entity_id":"media_player.bedroom"`) {
		t.Fatalf("stop call %+v", s)
	}
}

func TestErrorsAreClearAndNeverContainTheToken(t *testing.T) {
	for code, want := range map[int]string{401: "refused the token", 404: "not found", 500: "answered 500"} {
		c, _ := stub(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte("oops " + token + " " + strings.Repeat("x", 500)))
		})
		_, err := c.State(context.Background(), "media_player.a")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%d: %v, want %q", code, err, want)
		}
		if err != nil && (strings.Contains(err.Error(), strings.Repeat("x", 250))) {
			t.Errorf("%d: error body not truncated", code)
		}
	}
	// A connection failure must not leak the token either (it is in the request, not the URL, but be sure).
	c, _ := New("http://127.0.0.1:1", token)
	if _, err := c.Players(context.Background()); err == nil || strings.Contains(err.Error(), token) {
		t.Errorf("unreachable: %v", err)
	}
}

func TestRedirectsAreNotFollowedSoTheTokenGoesNowhereElse(t *testing.T) {
	var hits int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer other.Close()
	c, _ := stub(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) })
	if _, err := c.State(context.Background(), "media_player.a"); err == nil {
		t.Error("a redirect should be an error")
	}
	if hits != 0 {
		t.Fatal("the redirect was followed")
	}
}

func TestNewRejectsBadSettingsAndOddEntityIDs(t *testing.T) {
	for _, u := range []string{"", "homeassistant.local", "ftp://x", "http://"} {
		if _, err := New(u, token); err == nil {
			t.Errorf("New(%q) succeeded", u)
		}
	}
	if _, err := New("http://x:8123", ""); err == nil {
		t.Error("an empty token was accepted")
	}
	c, _ := New("http://127.0.0.1:1", token)
	for _, id := range []string{"light.kitchen", "media_player.a/../../x", "media_player.a b", "", "media_player.a?x=1"} {
		if err := c.Stop(context.Background(), id); err == nil || !strings.Contains(err.Error(), "not a media player") {
			t.Errorf("Stop(%q) = %v", id, err)
		}
	}
}
