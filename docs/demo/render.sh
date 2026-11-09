#!/usr/bin/env bash
# Records each tape with vhs and saves its last frame as a PNG in docs/images.
set -euo pipefail
cd "$(dirname "$0")/../.."

for tape in docs/demo/*.tape; do
  name=$(basename "$tape" .tape)
  video=$(sed -n 's/^Output "\(.*\)"$/\1/p' "$tape")
  vhs "$tape" >/dev/null
  ffmpeg -loglevel error -y -sseof -0.2 -i "$video" -frames:v 1 -update 1 "docs/images/$name.png"
  echo "docs/images/$name.png"
done
