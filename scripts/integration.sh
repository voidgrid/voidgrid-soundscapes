#!/usr/bin/env bash
# End-to-end test of the Home Assistant side, on throwaway services: a private Mosquitto broker and a stub
# Home Assistant REST API (scripts/stubha) on this host. It never touches the real broker or Home Assistant.
# Needs docker, curl and go; the eclipse-mosquitto:2 image must be available. Usage: scripts/integration.sh <image>
set -euo pipefail
image=${1:?usage: scripts/integration.sh <image>}
here=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
mqtt=vgs-it-mqtt app=vgs-it-app
mport=18883 aport=18008 hport=18123
stubpid=""
cleanup() {
  docker rm -f "$app" "$mqtt" >/dev/null 2>&1 || true
  [ -n "$stubpid" ] && kill "$stubpid" 2>/dev/null || true
  rm -rf "$tmp"
}
trap cleanup EXIT
fail() { echo "integration: $*"; echo "--- app log ---"; docker logs "$app" 2>&1 | tail -30; exit 1; }

# wait_for <what> <command...>: run the command until its output is non-empty (prints it), up to ~20 s.
wait_for() {
  local what=$1; shift
  for _ in $(seq 1 40); do
    out=$("$@" 2>/dev/null || true)
    [ -n "$out" ] && { printf '%s' "$out"; return 0; }
    sleep 0.5
  done
  fail "timed out waiting for $what"
}
retained() { docker exec "$mqtt" mosquitto_sub -h 127.0.0.1 -t "$1" -C 1 -W 2; } # a retained message, or nothing
topic_is() { # topic expected: succeeds when the retained message equals expected
  local v; v=$(retained "$1" 2>/dev/null || true); [ "$v" = "$2" ] && echo "$v"
}
pub() { docker exec "$mqtt" mosquitto_pub -h 127.0.0.1 -t "$1" -m "$2"; }
calls() { curl -sf "http://127.0.0.1:$hport/_calls"; }
base=voidgrid-soundscapes/integration_test

# 1. throwaway services
printf 'listener 1883\nallow_anonymous true\n' >"$tmp/mosquitto.conf"
docker run -d --name "$mqtt" -p 127.0.0.1:$mport:1883 -v "$tmp/mosquitto.conf:/mosquitto/config/mosquitto.conf:ro" eclipse-mosquitto:2 >/dev/null
(cd "$here" && go build -o "$tmp/stubha" ./scripts/stubha)
"$tmp/stubha" -listen 127.0.0.1:$hport >"$tmp/stubha.log" 2>&1 &
stubpid=$!
mkdir "$tmp/audio" "$tmp/config" && chmod 777 "$tmp/audio" "$tmp/config"
wait_for "mosquitto" docker exec "$mqtt" mosquitto_sub -h 127.0.0.1 -t '$SYS/broker/version' -C 1 -W 1 >/dev/null
wait_for "the stub Home Assistant" curl -sf -o /dev/null -w up "http://127.0.0.1:$hport/_calls" >/dev/null

run_app() {
  docker rm -f "$app" >/dev/null 2>&1 || true
  docker run -d --name "$app" --network host --user 1000:1000 --cap-drop ALL --security-opt no-new-privileges:true --read-only \
    -e VOIDGRID_SOUNDSCAPES_LISTEN=127.0.0.1:$aport -e VOIDGRID_SOUNDSCAPES_PUBLIC_URL=http://127.0.0.1:$aport \
    -e VOIDGRID_SOUNDSCAPES_MQTT_URL=tcp://127.0.0.1:$mport -e VOIDGRID_SOUNDSCAPES_HA_URL=http://127.0.0.1:$hport \
    -e VOIDGRID_SOUNDSCAPES_HA_TOKEN=test-token -e "VOIDGRID_SOUNDSCAPES_DEVICE_NAME=Integration Test" \
    -v "$tmp/audio:/soundscapes/audio" -v "$tmp/config:/soundscapes/config" "$image" >/dev/null
}
run_app
wait_for "the app to answer" curl -sf "http://127.0.0.1:$aport/healthz" >/dev/null

# 2. it announces itself, with the soundscapes and the players as options
wait_for "status online" topic_is "$base/status" online >/dev/null
curl -sf -X POST "http://127.0.0.1:$aport/api/soundscapes" -d '{"name":"Smoke"}' >/dev/null || fail "creating a soundscape failed"
cfg=homeassistant/select/integration_test/soundscape/config
for _ in $(seq 1 20); do c=$(retained "$cfg" || true); grep -q '"Smoke"' <<<"$c" && break; sleep 0.5; done
grep -q '"Smoke"' <<<"$c" || fail "the soundscape select does not offer Smoke: $c"
grep -q '"unique_id":"integration_test_soundscape"' <<<"$c" || fail "unique_id: $c"
grep -q '"name":"Integration Test"' <<<"$c" || fail "device name: $c"
p=$(retained homeassistant/select/integration_test/player/config)
grep -q '"Bedroom"' <<<"$p" && grep -q '"Kitchen"' <<<"$p" || fail "the player select does not offer both players: $p"
for e in switch/integration_test/playing sensor/integration_test/stream_url binary_sensor/integration_test/streamable sensor/integration_test/now_playing; do
  [ -n "$(retained "homeassistant/$e/config")" ] || fail "no discovery message for $e"
done
echo "integration: discovery ok"

# 3. commands from Home Assistant start playback on the stub, and the switch follows the player
pub "$base/soundscape/set" Smoke
pub "$base/player/set" Bedroom
wait_for "the soundscape state" topic_is "$base/soundscape/state" Smoke >/dev/null
wait_for "the stream URL" topic_is "$base/stream_url/state" "http://127.0.0.1:$aport/stream/smoke" >/dev/null
pub "$base/playing/set" ON
c=$(wait_for "play_media" calls)
grep -q "play_media media_player.bedroom http://127.0.0.1:$aport/stream/smoke" <<<"$c" || fail "unexpected calls: $c"
wait_for "the switch to turn on" topic_is "$base/playing/state" ON >/dev/null
wait_for "now playing" topic_is "$base/now_playing/state" Smoke >/dev/null
echo "integration: play ok"
pub "$base/playing/set" OFF
for _ in $(seq 1 20); do c=$(calls); grep -q media_stop <<<"$c" && break; sleep 0.5; done
grep -q "media_stop media_player.bedroom" <<<"$c" || fail "no media_stop: $c"
wait_for "the switch to turn off" topic_is "$base/playing/state" OFF >/dev/null
echo "integration: stop ok"

# 4. the choices survive a restart
run_app
wait_for "the app to answer again" curl -sf "http://127.0.0.1:$aport/healthz" >/dev/null
wait_for "the chosen soundscape after a restart" topic_is "$base/soundscape/state" Smoke >/dev/null
wait_for "the chosen player after a restart" topic_is "$base/player/state" Bedroom >/dev/null
echo "integration: restart ok"

# 5. if the program dies, the broker's last will marks the device unavailable
docker kill "$app" >/dev/null
wait_for "the last will" topic_is "$base/status" offline >/dev/null
echo "integration: last will ok"

# 6. nothing secret is in the log
logs=$(docker logs "$app" 2>&1)
grep -q "test-token" <<<"$logs" && fail "the Home Assistant token is in the log"
echo "integration: ok"
