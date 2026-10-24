#!/usr/bin/env bash
# Boots an image the way it is meant to run (uid 1000, no capabilities, read-only root) and checks that
# it comes up healthy, the way the deploy does. Usage: scripts/smoke.sh <image>
set -euo pipefail
image=${1:?usage: scripts/smoke.sh <image>}
name=vgs-smoke-$$
dir=$(mktemp -d)
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; rm -rf "$dir"; }
trap cleanup EXIT
mkdir "$dir/audio" "$dir/config" && chmod 777 "$dir/audio" "$dir/config"

docker run -d --name "$name" --user 1000:1000 --cap-drop ALL \
  --security-opt no-new-privileges:true --read-only \
  -v "$dir/audio:/soundscapes/audio" -v "$dir/config:/soundscapes/config" "$image" >/dev/null

for _ in $(seq 1 20); do
  if docker exec "$name" voidgrid-soundscapes healthcheck 2>/dev/null; then
    # Capture first: `grep -q` on a pipe would SIGPIPE ffmpeg and fail under pipefail.
    encoders=$(docker exec "$name" ffmpeg -hide_banner -encoders 2>/dev/null)
    grep -q libmp3lame <<<"$encoders" || { echo "smoke: ffmpeg has no libmp3lame encoder"; exit 1; }
    echo "smoke: ok"
    exit 0
  fi
  sleep 0.5
done
echo "smoke: not healthy"; docker logs "$name" 2>&1 | tail -20; exit 1
