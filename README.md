# voidgrid-soundscapes

I want to have rain, waves, or anything else I want to fall asleep to, and be able to mix them as I see fit, and I couldn't find anything outside of closed-source apps that let me do it. There are closed-source phone apps that do exactly this, but nothing I could run myself and tie into Home Assistant.

[Amniotic](https://github.com/fmtr/amniotic) is what got me started: it showed that you could mix ambient recordings into one stream and drive it from Home Assistant. But it doesn't do what I want. It has no web UI and no live mixing, and you can't see or build a whole mix at once; you pick one recording at a time. I started as a fork of it, ended up wanting to change almost everything, and rewrote it from scratch in Go. **No Amniotic code is in here.** If you want the original, mature, supported project, use Amniotic.

I wrote this using Claude. The design is mine (what a soundscape is, how folders work, how Home Assistant fits in, what V1 includes and leaves out); a lot of the code is Claude's. I'm also learning Go as I go, and I'll be reading all of the code, over time, after it has been written here. Until I have, don't take my word for it that the code is good: read it before you trust it, especially if it is going anywhere near a speaker in a room where someone is asleep. It is a one person plus AI project and I would rather be upfront about that than have you find out later.

It is also very early: this is a beta. The mixing, the web page and saving are tested, including against a real speaker in an earlier prototype. The Home Assistant side (the speaker control and the MQTT entities) is tested against a stub Home Assistant and a throwaway MQTT broker, **not yet against a real Home Assistant**, so expect rough edges there. Releases are tagged betas (see [Image tags](#image-tags)).

## What it is

- A **layer** is one audio file (rain, waves, fire, a fan). A **soundscape** is a named set of layers, each with its own level and an on/off switch.
- Each soundscape is served as one HTTP MP3 stream. Home Assistant only tells a media player to fetch the URL; the audio never passes through Home Assistant.
- Your audio folder is organised by type (the first-level folder is the category, e.g. `rain/`, `waves/`), and the UI groups layers that way. Any format ffmpeg can read works.
- You mix live in a built-in web page. A few MQTT entities (pick a soundscape, pick a player, play and stop) expose the basics to Home Assistant for automations, and the page can play a soundscape on any of your media players.
- Built to be deployed with Docker. The container runs as a non-root user with every capability dropped.

Not planned for V1: a Home Assistant custom card, YouTube downloads, and anything resembling auth inside the app (it is meant for your LAN, so put it behind your own reverse proxy if you need more). Those may come later.

## Requirements

- Docker (and Docker Compose v2) on the machine that will run it. The container is the way this is meant to be deployed. There is nothing else to install: ffmpeg is bundled in the image (it is run as a separate program for decoding and for encoding the stream).
- A folder of audio files. Short loops are fine; they are looped without a gap.
- To play on a speaker: a media player that can fetch an HTTP URL and can reach the container over your network. Controlling it from here also needs Home Assistant (see [Home Assistant](#home-assistant)); without Home Assistant you can still give any player the stream URL yourself.
- For the Home Assistant entities: an MQTT broker that Home Assistant uses.

## Image tags

The image is `ghcr.io/voidgrid/voidgrid-soundscapes` (linux/amd64):

```sh
IMAGE=ghcr.io/voidgrid/voidgrid-soundscapes:latest         # newest stable release (none yet)
IMAGE=ghcr.io/voidgrid/voidgrid-soundscapes:beta           # newest release of any kind, pre-releases included
IMAGE=ghcr.io/voidgrid/voidgrid-soundscapes:v0.1.0-beta.1  # one exact version, never moves
```

While this is in beta, use `:beta` to follow along or an exact version to stay put. A beta can change in ways that need a look at the release notes; your soundscapes live in the config folder, so keep a copy of it before moving to a newer image.

## Getting started

Docker Compose, from the files in [examples/](examples/):

```sh
mkdir voidgrid-soundscapes && cd voidgrid-soundscapes
curl -fsSLO https://raw.githubusercontent.com/voidgrid/voidgrid-soundscapes/main/examples/compose.yaml
curl -fsSL  https://raw.githubusercontent.com/voidgrid/voidgrid-soundscapes/main/examples/env.example -o .env
chmod 600 .env                       # it will hold a token and a password
mkdir -p audio config
chown 1000:1000 config               # the container runs as uid 1000 and writes soundscapes.json here
$EDITOR .env                         # at least PUBLIC_URL, and put some sounds in ./audio
docker compose up -d
curl http://localhost:8007/healthz   # prints "ok"
```

Then open `http://<host>:8007/`. The compose file runs the container the way it is built to run: as uid 1000, every capability dropped, `no-new-privileges`, a read-only root filesystem, and your audio folder mounted read-only. It has a health check (`docker compose ps` shows `healthy`). Plain `docker run` works too:

```sh
docker run -d --name voidgrid-soundscapes --restart unless-stopped \
  --user 1000:1000 --cap-drop ALL --security-opt no-new-privileges:true --read-only \
  -p 8007:8007 \
  -e VOIDGRID_SOUNDSCAPES_PUBLIC_URL=http://192.168.1.10:8007 \
  -v "$PWD/audio:/soundscapes/audio:ro" -v "$PWD/config:/soundscapes/config" \
  ghcr.io/voidgrid/voidgrid-soundscapes:beta
```

- **Secrets:** the Home Assistant token and the MQTT password can be given to the server alone, by a wrapper or secrets manager that starts it. The health check does not need them (it reads only `LISTEN`), so a container started that way still reports `healthy`.
- **Updating:** `docker compose pull && docker compose up -d`. Streams drop for a moment and players reconnect; soundscapes are kept in the config folder.
- **Logs:** `docker compose logs -f`. They list what was configured (secrets are shown only as `set`), the audio library changes, and every stream start and stop.
- **Memory:** every sound that is switched on in a saved soundscape is decoded into memory (about 5 MB per minute of audio). A library of a few hours of audio, mostly switched on, needs a few hundred MB. Sounds switched off are released after a couple of minutes.
- **Network:** a speaker fetches `http://<PUBLIC_URL>/stream/<id>`, so `PUBLIC_URL` must be an address the *speaker* can reach (not `localhost`, not a Docker-internal name), and the port must be open between the two. The web page and the stream have **no login**; keep them on a trusted network or put your own reverse proxy in front. A proxy must not buffer (`/api/events` is a long-lived event stream and `/stream/<id>` never ends), and must not time those requests out.
- **Storage:** the config folder holds one small file, `soundscapes.json`, written atomically. If it is damaged or was written by a newer version, the program refuses to start and names it rather than overwrite it.

Settings are environment variables:

| Variable | Default | Meaning |
|---|---|---|
| `VOIDGRID_SOUNDSCAPES_LISTEN` | `:8007` | Address the HTTP server binds to |
| `VOIDGRID_SOUNDSCAPES_AUDIO_DIR` | `/soundscapes/audio` | Root of your audio library |
| `VOIDGRID_SOUNDSCAPES_CONFIG_DIR` | `/soundscapes/config` | Where soundscapes and settings are stored |
| `VOIDGRID_SOUNDSCAPES_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `VOIDGRID_SOUNDSCAPES_SCAN_INTERVAL` | `10s` | How often the audio directory is rescanned for new, changed or removed files (at least `1s`) |
| `VOIDGRID_SOUNDSCAPES_PUBLIC_URL` | (unset) | How a speaker reaches this program, such as `http://192.168.1.117:8007` (a scheme and host, no path). Needed to play on a speaker, because the container cannot know its own LAN address |
| `VOIDGRID_SOUNDSCAPES_HA_URL` | (unset) | Home Assistant, such as `http://192.168.1.211:8123`. Set it together with `HA_TOKEN` to list and control media players |
| `VOIDGRID_SOUNDSCAPES_HA_TOKEN` | (unset) | A Home Assistant long-lived access token. A secret: it is never logged or saved |
| `VOIDGRID_SOUNDSCAPES_MQTT_URL` | (unset) | MQTT broker, such as `tcp://192.168.1.211:1883` (`ssl://` for TLS). Unset means no Home Assistant entities |
| `VOIDGRID_SOUNDSCAPES_MQTT_USER`, `..._MQTT_PASSWORD` | (unset) | Broker credentials. The password is a secret and is never logged |
| `VOIDGRID_SOUNDSCAPES_DEVICE_NAME` | `Voidgrid Soundscapes` | The device name in Home Assistant; the entity IDs, topics and MQTT client ID derive from it. Give a second instance, or one beside another tool, a different name |
| `VOIDGRID_SOUNDSCAPES_DISCOVERY_PREFIX` | `homeassistant` | Home Assistant's MQTT discovery prefix |
| `VOIDGRID_SOUNDSCAPES_LOOP_CROSSFADE` | `1500ms` | Length of the crossfade that joins the end of a layer's audio to its start when it loops (`0s` to `10s`; `0s` is a hard cut) |

### Your audio folder

Put audio files anywhere under the audio directory. Each file is a **layer**, identified by its path inside that directory (`rain/heavy.mp3`), so two files called `heavy.mp3` in different folders are two different layers. The first-level folder is the layer's category (`rain`), which the page's Add menu uses to group them; files directly in the root have no category.

- Recognised extensions: `mp3 wav flac ogg oga opus m4a aac wma aif aiff webm mp4`. Every file is checked with `ffprobe`; one that is not really audio is listed with an error and never mixed. `webm` and `mp4` may carry video: only the audio track is used, and a file with no audio track is reported as unusable. The tests build a tone in each of these formats and check it comes out of the stream at the right pitch, in the same Alpine ffmpeg the image ships.
- Hidden files and folders (names starting with `.`) and anything else are ignored. A symlink to a file counts; a symlink to a folder is not followed.
- New, changed and removed files are picked up by rescanning every `SCAN_INTERVAL`. A file is only read once it has been left alone for two seconds, so copying or downloading into the folder is safe.
- `GET /api/layers` returns what was found. If the folder becomes unreadable (an unmounted share, say) the last good list is kept rather than emptied.

### The web page

Open `http://<host>:8007/`. The landing page is a card for each saved soundscape; a card opens its mixer. A mixer shows only the sounds in that soundscape, each with an on/off switch, a level slider and a ✕ to take it out of the soundscape (the file is not touched). The **Add** menu at the bottom lists the sounds that are not in it yet. Changes are applied as you make them and saved automatically; there is no Save button. **Listen** plays the stream in that browser tab (a browser buffers a second or more, so it lags your slider more than the server does). Rename, Duplicate and Delete are on the mixer, and the stream URL is there to copy. Several tabs stay in sync. It is green on black, like voidgrid-backup, and works on a phone.

The page has no login (the plan is a trusted LAN or your own reverse proxy), and its scripts and styles are all served by the program itself.

### Home Assistant

Nothing here is needed to mix and stream. With `HA_URL`, `HA_TOKEN` and `PUBLIC_URL` set, the mixer page gets a speaker picker and a **Play on speaker** button: the program lists your `media_player` entities and calls `media_player.play_media` with the soundscape's stream URL (and `media_player.media_stop` to stop). The audio still never passes through Home Assistant; the speaker fetches the stream from this program. **Stop speaker** only stops a speaker that is playing this program's stream, never one playing something else. Whether a speaker is playing is read back from the speaker, not assumed. If the speaker is playing a soundscape and you choose another, it is pointed at the new one.

With `MQTT_URL` set as well, one device appears in Home Assistant through MQTT discovery (nothing to configure there), with these entities (the IDs shown are for the default device name):

| Entity | What it does |
|---|---|
| `select.voidgrid_soundscapes_soundscape` | Choose the soundscape (the options are your soundscapes' names) |
| `select.voidgrid_soundscapes_player` | Choose the speaker (the options are your media players) |
| `switch.voidgrid_soundscapes_playing` | On: play the chosen soundscape on the chosen speaker. Off: stop it. Shows what the speaker is really doing |
| `sensor.voidgrid_soundscapes_stream_url` | The chosen soundscape's stream URL |
| `binary_sensor.voidgrid_soundscapes_streamable` | On when a soundscape is chosen and `PUBLIC_URL` is set, so a speaker could be told to play it |
| `sensor.voidgrid_soundscapes_now_playing` | The soundscape being played on the speaker, if any |

The entities go unavailable if the program stops (the broker publishes a last will). The broker user needs read and write on `voidgrid-soundscapes/#` and `homeassistant/#`. Each entity's state is retained on the broker, and Home Assistant's own restart is noticed (it announces itself on `homeassistant/status`) and the discovery messages are sent again. The chosen soundscape and speaker are saved with the soundscapes. Mixing the layers is done on the web page; there is no per-layer entity.

### Soundscapes and streaming

A soundscape is a named mix of layers, each with a level from 0 to 1 and an on/off switch. Define one by sending the whole mix; sending it again is harmless, and you can send it while someone is listening to hear the change live:

```sh
curl -X PUT http://localhost:8007/api/soundscapes/night -d '{"layers": [
  {"id": "rain/heavy.mp3",  "level": 0.6, "enabled": true},
  {"id": "waves/gentle.flac", "level": 0.3, "enabled": true}
]}'
```

To change one layer without sending the whole mix (this is what the web page does, so two browsers adjusting different layers do not overwrite each other): `PATCH /api/soundscapes/night/layers` with `{"id": "rain/heavy.mp3", "level": 0.4}`, `{"id": "...", "enabled": true}` or `{"id": "...", "remove": true}`. A layer that is not yet in the soundscape is added (from the library only), at level 0.5 and off unless you say otherwise. `GET /api/events` is a server-sent-events feed of the whole state (layers and soundscapes), sent on connect and whenever it changes.

`GET /api/player` shows the chosen soundscape and speaker and whether it is playing; `PUT /api/player` (`{"soundscape": "night", "player": "media_player.bedroom"}`), `POST /api/player/play` and `POST /api/player/stop` are what the page uses.

A soundscape has a fixed ID (part of its stream URL) and a name you can change. `PUT` can also carry `"name"`; a field left out keeps its current value, so `{"name": "Sleep"}` renames without touching the mix. Other calls: `POST /api/soundscapes` with `{"name": "Night rain"}` (and optionally `"layers"`) creates one and picks the ID (`night-rain`); `POST /api/soundscapes/night/duplicate` (optionally `{"name": "..."}`) copies one; `DELETE /api/soundscapes/night` removes it and disconnects anyone listening.

The reply lists each layer's state: `ready`, `loading` (being decoded; silent until it is ready), `off`, `missing` (not in the library), `unusable` or `error`. One bad layer never stops the others. Then point any player at `http://<host>:8007/stream/night` (MP3, 128 kbit/s). `GET /api/soundscapes` and `GET /api/soundscapes/night` show the state, including how many listeners there are.

- **Shared work:** each file is decoded once, however many soundscapes use it, and each soundscape is mixed and encoded once however many players listen. The encoder runs only while somebody is listening and stops a few seconds after the last listener leaves.
- **Live changes:** level changes and switching layers on or off ramp over 50 ms, so they never click. In a test with real files and the real encoder, a change reached the stream's output about 200 ms after the request. A player then adds its own buffer, which this cannot control. The one exception is a layer that is not in memory yet: switching it on is silent until it has been decoded (a fraction of a second for short files, about a second for a long recording on a busy machine), and its state reads `loading` meanwhile. A layer you switch off is dropped from memory a couple of minutes later.
- **Loops:** every layer loops forever and independently. Silence at the start and end of a file is trimmed, and the end is crossfaded into the start. If the start and end of the audio are clearly alike (a hum, a fan, a drone), the join is moved to where they line up and the fade is linear, so they do not cancel or swell; for noise-like sounds such as rain it is an equal-power fade. Try `LOOP_CROSSFADE` if a join is audible on your recordings.
- **Limits:** a layer longer than 20 minutes is refused. Loud mixes are eased towards full scale instead of being clipped.
- **Saved:** soundscapes are kept in `soundscapes.json` in the config directory and come back after a restart. Changes are written about a second after they happen (a slider being dragged becomes one write), by writing a temporary file and renaming it, so a crash leaves the old or the new file, never half of one. A file the program cannot understand (damaged, or written by a newer version) is never overwritten: it refuses to start and names the file. A config directory that cannot be written to also stops it at startup.

The container has a health check (`voidgrid-soundscapes healthcheck`), and invalid settings make it exit with code 2 and say which one.

## Troubleshooting

- **The page shows no sounds:** the audio folder is empty, not mounted where you expect, or unreadable by uid 1000 (a file only readable by its owner, say). `docker compose logs` says what the scan found (including how many files are unusable), and `curl http://localhost:8007/api/layers` lists every file with the reason for any that cannot be used. A file that is not really audio never appears in the page's Add menu.
- **The container exits at once:** the log says why. Two common ones: the config folder is not writable by uid 1000 (`chown 1000:1000 config`), or a setting is invalid (the program exits with code 2 and names the variable).
- **Play on speaker does nothing or fails:** the page shows Home Assistant's own error. Check `PUBLIC_URL` (the speaker must be able to open it; try the stream URL from the page in VLC on another machine), `HA_URL`/`HA_TOKEN`, and that the speaker is a `media_player` that accepts a URL.
- **No Home Assistant entities:** the log says `connected to the MQTT broker` once it is in, and warns if the broker cannot be reached after 15 seconds; the MQTT library's own connection errors (a refused login, say) are logged too. The broker user needs read and write on `voidgrid-soundscapes/#` and `homeassistant/#`, and Home Assistant's MQTT discovery prefix must match `DISCOVERY_PREFIX`.
- **Two entities with the same name, or an old tool's entities:** each device name gets its own entities. Give this one its own `DEVICE_NAME`.

## Development

You need Go 1.22 or newer, or just Docker.

- `go vet ./... && go test -race ./...` builds, vets and tests everything.
- `gofmt -l cmd internal` must print nothing.
- `docker build -t voidgrid-soundscapes:dev .` builds the image (Go 1.27 builder, Alpine runtime with ffmpeg).
- `scripts/integration.sh <image>` runs the image against a throwaway Mosquitto broker and a stub Home Assistant API (`scripts/stubha`): discovery, commands, play and stop, restart, last will. It never touches a real broker or Home Assistant.
- `scripts/smoke.sh <image>` boots an image the way it is meant to run (uid 1000, all capabilities dropped, read-only root) and checks it is healthy and that ffmpeg has the MP3 encoder.
- The image build runs `gofmt`, `go vet` and `go test`, so a failing test fails the build. CI (on every push to `main` and every pull request) is just that build plus the smoke test.

### Releases

Pushing a `v*` tag builds the image, runs the smoke test and pushes it to `ghcr.io/voidgrid/voidgrid-soundscapes` for linux/amd64:

```sh
IMAGE=ghcr.io/voidgrid/voidgrid-soundscapes:latest         # newest stable release
IMAGE=ghcr.io/voidgrid/voidgrid-soundscapes:beta           # newest release of any kind, pre-releases included
IMAGE=ghcr.io/voidgrid/voidgrid-soundscapes:v0.1.0-beta.1  # one exact version, never moves
```

A hyphen in the tag marks a pre-release: `v0.1.0-beta.1` moves only `:beta`, while `v0.1.0` moves `:latest` and `:beta`. Versioned tags never move. The image is Alpine with ffmpeg; the Go binary is static and embeds the web UI.

## Acknowledgement

Inspired by [Amniotic](https://github.com/fmtr/amniotic) by Frontmatter AI. See [NOTICE.md](NOTICE.md).

## License

MIT, see [LICENSE](LICENSE). I use MIT for everything I put out and I have no intention of monetising any of it. ffmpeg keeps its own licence, which depends on how the build you run was compiled.
