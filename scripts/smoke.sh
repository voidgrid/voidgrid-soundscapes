#!/usr/bin/env bash
# Boots an image the way it is meant to run (uid 1000, no capabilities, read-only root), checks that it
# comes up healthy, lists the audio library, and streams a real mix. Needs docker, curl, awk and od on the
# host. Usage: scripts/smoke.sh <image>
set -euo pipefail
image=${1:?usage: scripts/smoke.sh <image>}
name=vgs-smoke-$$
dir=$(mktemp -d)
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; rm -rf "$dir"; }
trap cleanup EXIT
mkdir "$dir/audio" "$dir/config" && chmod 777 "$dir/audio" "$dir/config"

docker run -d --name "$name" --user 1000:1000 --cap-drop ALL -p 127.0.0.1::8007 \
  --security-opt no-new-privileges:true --read-only \
  -e VOIDGRID_SOUNDSCAPES_SCAN_INTERVAL=1s \
  -v "$dir/audio:/soundscapes/audio" -v "$dir/config:/soundscapes/config" "$image" >/dev/null

for _ in $(seq 1 20); do
  if docker exec "$name" voidgrid-soundscapes healthcheck 2>/dev/null; then
    # Capture first: `grep -q` on a pipe would SIGPIPE ffmpeg and fail under pipefail.
    encoders=$(docker exec "$name" ffmpeg -hide_banner -encoders 2>/dev/null)
    grep -q libmp3lame <<<"$encoders" || { echo "smoke: ffmpeg has no libmp3lame encoder"; exit 1; }

    # Library: make a real tone and a fake mp3 inside the container, then wait for the scan to list them.
    docker exec "$name" sh -c 'mkdir -p /soundscapes/audio/rain \
      && ffmpeg -v error -f lavfi -i sine=frequency=300:duration=1 /soundscapes/audio/rain/tone.flac \
      && echo "not audio" > /soundscapes/audio/fake.mp3'
    for _ in $(seq 1 30); do
      layers=$(docker exec "$name" wget -qO- http://127.0.0.1:8007/api/layers)
      grep -q '"id":"rain/tone.flac"' <<<"$layers" && grep -q '"id":"fake.mp3"' <<<"$layers" && break
      sleep 0.5
    done
    grep -q '"id":"rain/tone.flac"' <<<"$layers" || { echo "smoke: tone not listed: $layers"; exit 1; }
    grep -q '"category":"rain"' <<<"$layers" || { echo "smoke: category missing: $layers"; exit 1; }
    grep -q '"id":"fake.mp3".*"error"' <<<"$layers" || { echo "smoke: fake.mp3 not reported as unusable: $layers"; exit 1; }
    grep -q '/soundscapes/audio' <<<"$layers" && { echo "smoke: API leaks the host path: $layers"; exit 1; }

    # Streaming: mix the tone into a soundscape, capture the stream, and check it is real, loud MP3 audio;
    # then switch the layer off and check the stream goes silent without reconnecting anything.
    port=$(docker port "$name" 8007/tcp | head -1 | sed 's/.*://')
    url=http://127.0.0.1:$port
    mix() { # $1 = level, $2 = enabled
      curl -sf -X PUT "$url/api/soundscapes/smoke" \
        -d '{"layers":[{"id":"rain/tone.flac","level":'"$1"',"enabled":'"$2"'}]}' >/dev/null
    }
    loudness() { # reads MP3 on stdin, prints mean volume in dB
      docker exec -i "$name" ffmpeg -hide_banner -f mp3 -i - -af volumedetect -f null - 2>&1 \
        | sed -n 's/.*mean_volume: \(-\?[0-9.]*\) dB.*/\1/p'
    }
    mix 1 true || { echo "smoke: PUT soundscape failed"; exit 1; }
    for _ in $(seq 1 30); do
      curl -sf "$url/api/soundscapes/smoke" | grep -q '"state":"ready"' && break
      sleep 0.5
    done
    curl -sf "$url/api/soundscapes/smoke" | grep -q '"state":"ready"' \
      || { echo "smoke: layer never became ready: $(curl -s "$url/api/soundscapes/smoke")"; exit 1; }

    curl -s --max-time 3 -o "$dir/on.mp3" "$url/stream/smoke" || true
    [ "$(stat -c %s "$dir/on.mp3")" -gt 20000 ] || { echo "smoke: stream too short ($(stat -c %s "$dir/on.mp3") bytes)"; exit 1; }
    read -r b0 b1 < <(head -c 2 "$dir/on.mp3" | od -An -tu1)
    (( b0 == 255 && (b1 & 224) == 224 )) \
      || { echo "smoke: stream does not start with an MP3 frame header (first bytes: $b0 $b1)"; exit 1; }
    on=$(loudness <"$dir/on.mp3")
    awk -v v="$on" 'BEGIN { exit !(v != "" && v > -40) }' || { echo "smoke: stream is not audible (mean volume '$on' dB)"; exit 1; }

    mix 1 false
    sleep 1
    curl -s --max-time 2 -o "$dir/off.mp3" "$url/stream/smoke" || true
    off=$(loudness <"$dir/off.mp3")
    awk -v v="$off" 'BEGIN { exit !(v != "" && v < -60) }' || { echo "smoke: stream is not silent with the layer off (mean volume '$off' dB)"; exit 1; }
    echo "smoke: streamed ${on} dB with the layer on, ${off} dB with it off"

    # Persistence: rename it, add a second soundscape and delete a third, restart the container (a graceful
    # stop writes what is still waiting for its delay), and check the first two come back and the third does not.
    curl -sf -X PUT "$url/api/soundscapes/smoke" -d '{"name":"Smoke test"}' >/dev/null || { echo "smoke: rename failed"; exit 1; }
    curl -sf -X POST "$url/api/soundscapes/smoke/duplicate" >/dev/null || { echo "smoke: duplicate failed"; exit 1; }
    curl -sf -X PUT "$url/api/soundscapes/doomed" -d '{}' >/dev/null && curl -sf -X DELETE "$url/api/soundscapes/doomed" >/dev/null \
      || { echo "smoke: create/delete failed"; exit 1; }
    docker restart -t 10 "$name" >/dev/null
    for _ in $(seq 1 20); do docker exec "$name" voidgrid-soundscapes healthcheck 2>/dev/null && break; sleep 0.5; done
    url=http://127.0.0.1:$(docker port "$name" 8007/tcp | head -1 | sed 's/.*://')
    after=$(curl -sf "$url/api/soundscapes") || { echo "smoke: list after restart failed"; exit 1; }
    grep -q '"name":"Smoke test","id":"smoke"' <<<"$after" || { echo "smoke: renamed soundscape lost in restart: $after"; exit 1; }
    grep -q '"id":"smoke-test-copy"\|"id":"smoke-copy"' <<<"$after" || { echo "smoke: duplicate lost in restart: $after"; exit 1; }
    grep -q '"id":"doomed"' <<<"$after" && { echo "smoke: deleted soundscape came back: $after"; exit 1; }
    echo "smoke: soundscapes survived a restart"

    echo "smoke: ok"
    exit 0
  fi
  sleep 0.5
done
echo "smoke: not healthy"; docker logs "$name" 2>&1 | tail -20; exit 1
