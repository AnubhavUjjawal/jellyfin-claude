#!/bin/sh
# Starts the Jellyfin MCP server, building it first when the binary is missing or
# older than the source. Build output goes to stderr: stdout is the MCP channel.
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
BIN="$ROOT/bin/jellyfin-mcp"

find_go() {
	if command -v go >/dev/null 2>&1; then
		command -v go
		return 0
	fi
	# GUI-launched processes often lack the shell's PATH.
	for g in /usr/local/go/bin/go /opt/homebrew/bin/go /usr/local/bin/go /usr/lib/go/bin/go; do
		if [ -x "$g" ]; then
			echo "$g"
			return 0
		fi
	done
	return 1
}

stale() {
	[ ! -x "$BIN" ] && return 0
	[ -n "$(find "$ROOT/cmd" "$ROOT/internal" "$ROOT/go.mod" "$ROOT/go.sum" -newer "$BIN" 2>/dev/null | head -n 1)" ]
}

if stale; then
	if ! GO=$(find_go); then
		echo "jellyfin: Go is needed to build the server on first run (macOS: brew install go)" >&2
		exit 1
	fi
	echo "jellyfin: building the MCP server..." >&2
	(cd "$ROOT" && "$GO" build -o "$BIN" ./cmd/jellyfin-mcp) >&2
fi

exec "$BIN" "$@"
