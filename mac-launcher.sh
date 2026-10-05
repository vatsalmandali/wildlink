#!/bin/sh
# Wild Link launcher: starts the background service, or brings its window up if it's already running.
DIR="$(cd "$(dirname "$0")" && pwd)"
APP="$(cd "$DIR/../.." && pwd)"
# Clear the download flag so macOS doesn't silently block the helper binaries.
xattr -dr com.apple.quarantine "$APP" 2>/dev/null
if [ "$(uname -m)" = "arm64" ]; then BIN="$DIR/wildlink-arm64"; else BIN="$DIR/wildlink-amd64"; fi
nohup "$BIN" "$@" >/dev/null 2>&1 &
exit 0
