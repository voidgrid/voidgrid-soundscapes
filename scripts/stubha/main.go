// Command stubha is a stand-in for the Home Assistant REST API, for scripts/integration.sh. It knows two
// media players, remembers what it was told to play, and lists the commands it received at GET /_calls.
// It is a test fixture and is not part of the image.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"strings"
	"sync"
)

type player struct {
	Name, State, Media string
}

func main() {
	addr := flag.String("listen", "127.0.0.1:18123", "address to listen on")
	token := flag.String("token", "test-token", "the access token it accepts")
	flag.Parse()

	var mu sync.Mutex
	players := map[string]*player{
		"media_player.bedroom": {Name: "Bedroom", State: "idle"},
		"media_player.kitchen": {Name: "Kitchen", State: "off"},
	}
	var calls []string

	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+*token {
				http.Error(w, `{"message":"401: Unauthorized"}`, http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	entity := func(id string, p *player) map[string]any {
		return map[string]any{"entity_id": id, "state": p.State, "attributes": map[string]any{"friendly_name": p.Name, "media_content_id": p.Media}}
	}
	mux.HandleFunc("GET /api/states", auth(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		out := []map[string]any{{"entity_id": "light.hall", "state": "off", "attributes": map[string]any{}}}
		for id, p := range players {
			out = append(out, entity(id, p))
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	mux.HandleFunc("GET /api/states/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		p := players[r.PathValue("id")]
		if p == nil {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(entity(r.PathValue("id"), p))
	}))
	service := func(name string, apply func(p *player, body map[string]string)) {
		mux.HandleFunc("POST /api/services/media_player/"+name, auth(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			defer mu.Unlock()
			p := players[body["entity_id"]]
			if p == nil {
				http.Error(w, `{"message":"unknown entity"}`, http.StatusBadRequest)
				return
			}
			calls = append(calls, name+" "+body["entity_id"]+" "+body["media_content_id"])
			apply(p, body)
			_, _ = w.Write([]byte("[]"))
		}))
	}
	// Like Music Assistant, report what is playing wrapped in builtin://radio/.
	service("play_media", func(p *player, b map[string]string) {
		p.State, p.Media = "playing", "builtin://radio/"+b["media_content_id"]
	})
	service("media_stop", func(p *player, b map[string]string) { p.State, p.Media = "idle", "" })
	mux.HandleFunc("GET /_calls", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = w.Write([]byte(strings.Join(calls, "\n") + "\n"))
	})
	log.Printf("stub Home Assistant on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
