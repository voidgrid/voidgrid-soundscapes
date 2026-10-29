// Package ha is the small part of the Home Assistant REST API this program uses: listing media players,
// reading one player's state, and telling one to play a URL or stop.
package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Player is a media player entity.
type Player struct {
	ID   string `json:"id"`   // entity ID, such as media_player.bedroom
	Name string `json:"name"` // friendly name, or the ID if it has none
}

// PlayerState is what a player is doing.
type PlayerState struct {
	State          string // playing, paused, idle, off, unavailable, ...
	MediaContentID string // what it is playing, as the integration reports it (may be wrapped, e.g. builtin://radio/<url>)
}

// Client talks to one Home Assistant. The zero value is not usable; use New.
type Client struct {
	base  string
	token string
	http  *http.Client
}

const (
	maxStates  = 32 << 20 // /api/states lists every entity; a big house is a few MB
	maxError   = 200      // characters of an error body kept in messages
	callTimout = 15 * time.Second
)

// New returns a client for the Home Assistant at baseURL (http://host:8123). The token is a long-lived access
// token. It is only ever sent to baseURL: redirects are not followed.
func New(baseURL, token string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("the Home Assistant URL must look like http://host:8123")
	}
	if token == "" {
		return nil, errors.New("the Home Assistant token is empty")
	}
	return &Client{
		base:  strings.TrimRight(u.String(), "/"),
		token: token,
		http: &http.Client{
			Timeout:       callTimout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, scrub(err, c.token)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxStates))
	if err != nil {
		return nil, scrub(err, c.token)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errors.New("Home Assistant refused the token (401)")
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("not found in Home Assistant (404): %s", path)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		msg := strings.TrimSpace(string(b))
		if len(msg) > maxError {
			msg = msg[:maxError] + "…"
		}
		return nil, fmt.Errorf("Home Assistant answered %d: %s", resp.StatusCode, msg)
	}
	return b, nil
}

// scrub makes sure an error never carries the token.
func scrub(err error, token string) error {
	return errors.New(strings.ReplaceAll(err.Error(), token, "***"))
}

type entity struct {
	ID         string `json:"entity_id"`
	State      string `json:"state"`
	Attributes struct {
		FriendlyName   string `json:"friendly_name"`
		MediaContentID string `json:"media_content_id"`
	} `json:"attributes"`
}

// Players lists the media players, ordered by name.
func (c *Client) Players(ctx context.Context) ([]Player, error) {
	b, err := c.do(ctx, http.MethodGet, "/api/states", nil)
	if err != nil {
		return nil, err
	}
	var all []entity
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, fmt.Errorf("unexpected answer from Home Assistant: %w", err)
	}
	out := []Player{}
	for _, e := range all {
		if !strings.HasPrefix(e.ID, "media_player.") {
			continue
		}
		name := e.Attributes.FriendlyName
		if name == "" {
			name = e.ID
		}
		out = append(out, Player{ID: e.ID, Name: name})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func validPlayer(id string) error {
	if !strings.HasPrefix(id, "media_player.") || strings.ContainsAny(id, "/?#% ") {
		return fmt.Errorf("%q is not a media player", id)
	}
	return nil
}

// State reads one player.
func (c *Client) State(ctx context.Context, id string) (PlayerState, error) {
	if err := validPlayer(id); err != nil {
		return PlayerState{}, err
	}
	b, err := c.do(ctx, http.MethodGet, "/api/states/"+id, nil)
	if err != nil {
		return PlayerState{}, err
	}
	var e entity
	if err := json.Unmarshal(b, &e); err != nil {
		return PlayerState{}, fmt.Errorf("unexpected answer from Home Assistant: %w", err)
	}
	return PlayerState{State: e.State, MediaContentID: e.Attributes.MediaContentID}, nil
}

// Play tells a player to fetch and play streamURL.
func (c *Client) Play(ctx context.Context, id, streamURL string) error {
	if err := validPlayer(id); err != nil {
		return err
	}
	_, err := c.do(ctx, http.MethodPost, "/api/services/media_player/play_media", map[string]string{
		"entity_id": id, "media_content_id": streamURL, "media_content_type": "music",
	})
	return err
}

// Stop tells a player to stop.
func (c *Client) Stop(ctx context.Context, id string) error {
	if err := validPlayer(id); err != nil {
		return err
	}
	_, err := c.do(ctx, http.MethodPost, "/api/services/media_player/media_stop", map[string]string{"entity_id": id})
	return err
}
