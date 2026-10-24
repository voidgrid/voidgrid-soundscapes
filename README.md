# voidgrid-soundscapes

I want to have rain, waves, or anything else I want to fall asleep to, and be able to mix them as I see fit, and I couldn't find anything outside of closed-source apps that let me do it. There are closed-source phone apps that do exactly this, but nothing I could run myself and tie into Home Assistant.

[Amniotic](https://github.com/fmtr/amniotic) is what got me started: it showed that you could mix ambient recordings into one stream and drive it from Home Assistant. But it doesn't do what I want. It has no web UI and no live mixing, and you can't see or build a whole mix at once; you pick one recording at a time. I started as a fork of it, ended up wanting to change almost everything, and rewrote it from scratch in Go. **No Amniotic code is in here.** If you want the original, mature, supported project, use Amniotic.

I wrote this using Claude. The design is mine (what a soundscape is, how folders work, how Home Assistant fits in, what V1 includes and leaves out); a lot of the code is Claude's. I'm also learning Go as I go, and I'll be reading all of the code, over time, after it has been written here. Until I have, don't take my word for it that the code is good: read it before you trust it, especially if it is going anywhere near a speaker in a room where someone is asleep. It is a one person plus AI project and I would rather be upfront about that than have you find out later.

It is also very early. **Right now it does almost nothing.** The real program starts, logs, serves a health check and stops cleanly. The mixing has been proven in a throwaway prototype (a few files looped and mixed with live level changes, played on a real speaker through Home Assistant and Music Assistant), but that code is not in this repository yet. There is no release and no published image.

## What it is going to be

- A **layer** is one audio file (rain, waves, fire, a fan). A **soundscape** is a named set of layers, each with its own level and an on/off switch.
- Each soundscape is served as one HTTP MP3 stream. Home Assistant only tells a media player to fetch the URL; the audio never passes through Home Assistant.
- Your audio folder is organised by type (the first-level folder is the category, e.g. `rain/`, `waves/`), and the UI groups layers that way. Any format ffmpeg can read works.
- You mix live in a built-in web page. A few MQTT entities (pick a soundscape, pick a player, play and stop) expose the basics to Home Assistant for automations.
- Built to be deployed with Docker. The container runs as a non-root user with every capability dropped.

Not planned for V1: a Home Assistant custom card, YouTube downloads, and anything resembling auth inside the app (it is meant for your LAN, so put it behind your own reverse proxy if you need more). Those may come later.

## Requirements

- Docker (and Docker Compose v2) on the machine that will run it. The container is the way this is meant to be deployed.
- A folder of audio files. Short loops are fine; the engine is being built to loop them without a gap.
- To play on a speaker: Home Assistant and a media player it can control that is able to fetch an HTTP URL, on a network the player can reach the container on.
- To expose controls in Home Assistant (later): an MQTT broker.

ffmpeg is bundled in the image and is run as a separate program for decoding and for encoding the stream.

## Getting started

There is nothing to install yet. You can build the image and check that it runs:

```sh
docker build --build-arg VERSION=dev -t voidgrid-soundscapes:dev .
docker run -d --name soundscapes --user 1000:1000 --cap-drop ALL \
  --security-opt no-new-privileges:true --read-only -p 8007:8007 \
  -v "$PWD/audio:/soundscapes/audio" -v "$PWD/config:/soundscapes/config" \
  voidgrid-soundscapes:dev
curl http://localhost:8007/healthz   # prints "ok"
```

The `audio` and `config` directories need to be writable by uid 1000 (`chown 1000:1000`). Once the first release exists, this section will become a Compose example and a published image, `ghcr.io/voidgrid/voidgrid-soundscapes`.

Settings are environment variables:

| Variable | Default | Meaning |
|---|---|---|
| `VOIDGRID_SOUNDSCAPES_LISTEN` | `:8007` | Address the HTTP server binds to |
| `VOIDGRID_SOUNDSCAPES_AUDIO_DIR` | `/soundscapes/audio` | Root of your audio library |
| `VOIDGRID_SOUNDSCAPES_CONFIG_DIR` | `/soundscapes/config` | Where soundscapes and settings are stored |
| `VOIDGRID_SOUNDSCAPES_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

The container has a health check (`voidgrid-soundscapes healthcheck`), and invalid settings make it exit with code 2 and say which one.

## Development

You need Go 1.22 or newer, or just Docker.

- `go vet ./... && go test -race ./...` builds, vets and tests everything.
- `gofmt -l cmd internal` must print nothing.
- `docker build -t voidgrid-soundscapes:dev .` builds the image (Go 1.27 builder, Alpine runtime with ffmpeg).
- CI runs formatting, vet and the tests, and builds and pushes the image to `ghcr.io` from `main`.

## Acknowledgement

Inspired by [Amniotic](https://github.com/fmtr/amniotic) by Frontmatter AI. See [NOTICE.md](NOTICE.md).

## License

MIT, see [LICENSE](LICENSE). I use MIT for everything I put out and I have no intention of monetising any of it. ffmpeg keeps its own licence, which depends on how the build you run was compiled.
