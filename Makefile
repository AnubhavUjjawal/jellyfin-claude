BIN := bin/jellyfin-mcp

.PHONY: build test integration desktop-config clean

build:
	go build -o $(BIN) ./cmd/jellyfin-mcp

test:
	go test -race ./...

# Needs JELLYFIN_TEST_URL, JELLYFIN_TEST_USER and JELLYFIN_TEST_PASS; optional
# JELLYFIN_TEST_PLAYER=mpv|afplay. Plays at volume 0.
integration:
	go test -tags integration -count=1 -v ./cmd/jellyfin-mcp

# Prints the snippet to add to Claude Desktop's claude_desktop_config.json.
desktop-config: build
	@printf '{\n  "mcpServers": {\n    "jellyfin": { "command": "%s" }\n  }\n}\n' "$(abspath $(BIN))"

clean:
	rm -rf bin
