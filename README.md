# Jellyfin for Claude

Talk to Claude, hear your music. This Claude Code plugin (and MCP server for Claude Desktop) finds Jellyfin servers on your network, logs in with Quick Connect, searches your library, and plays music **on this computer**: no Jellyfin app, browser tab or TV needs to be open.

> "Find my Jellyfin server and play some Beatles"
> "Something like this, but more upbeat"
> "Skip. Turn it down a bit. Queue Parachutes after this."
> "Make me a 30 minute playlist that winds down for the evening"

## How it works

```
Claude ──MCP──▶ jellyfin-mcp ──HTTP──▶ Jellyfin server (search, stream, play reporting)
                     │
                     └──▶ mpv (streams, seeks, live volume)  or  afplay (macOS fallback)
```

- **Discovery**: Jellyfin's UDP broadcast on port 7359, so Claude can find servers without you typing an address.
- **Login**: Quick Connect by default. You approve a 6-digit code in any signed-in Jellyfin app, so your password never goes through the chat. The token is saved in `~/.config/jellyfin-claude/config.json` (mode 0600).
- **Playback**: a headless `mpv` streams the original file. If mpv isn't installed, macOS's built-in `afplay` is used instead (it downloads each track first and can't seek or change volume mid-track).
- **Reporting**: plays are reported back to Jellyfin, so play counts, "recently played" and the dashboard stay accurate.

## Requirements

- [Go](https://go.dev/dl/) 1.26+ to build (the plugin builds itself on first start).
- [mpv](https://mpv.io) recommended: `brew install mpv`. Without it, macOS falls back to `afplay`.
- Jellyfin 10.9 or newer (tested on 10.11).

## Install

### Claude Code (plugin)

```sh
claude plugin marketplace add ~/Work/jellyfin-claude
claude plugin install jellyfin@jellyfin-claude
```

Restart Claude Code, then ask it to play something. The first start builds the server (a few seconds); run `make build` beforehand to skip that wait.

### Claude Desktop

```sh
make desktop-config
```

Merge the printed snippet into `~/Library/Application Support/Claude/claude_desktop_config.json` and restart Claude Desktop.

## Tools

| Tool | What it does |
| --- | --- |
| `discover_servers` | Broadcast for Jellyfin servers on the LAN |
| `connect` | Quick Connect login (or username/password) |
| `status` | Connection, now playing, position, queue |
| `search` | Find tracks, albums, artists, playlists, genres |
| `browse` | Favorites, recently played, most played, recently added, random, albums, artists, playlists, genres, an artist's albums, an item's tracks |
| `play` | Play/queue items (replace, next, append), shuffle, instant mix, genre radio, library shuffle |
| `control` | Pause, resume, next, previous, stop, seek, volume, jump, shuffle, clear, repeat |
| `favorite` | Favorite or unfavorite the current track or any item |
| `logout` | Log out and forget the saved login |

## Configuration (environment variables, all optional)

| Variable | Meaning |
| --- | --- |
| `JELLYFIN_PLAYER` | `auto` (default: mpv, else afplay), `mpv` or `afplay` |
| `MPV_PATH` | Path to mpv if it isn't on PATH or in a Homebrew location |
| `JELLYFIN_VOLUME` | Starting volume, 0-100 (default 100) |
| `JELLYFIN_URL` + `JELLYFIN_API_KEY` | Use an API key instead of logging in (`JELLYFIN_USERNAME` or `JELLYFIN_USER_ID` picks the user; default is the first admin) |
| `JELLYFIN_CLAUDE_CONFIG_DIR` | Where to keep the saved login |

## Troubleshooting

- **No servers found**: auto-discovery may be off (Jellyfin Dashboard > Networking > "Enable auto discovery"), the server may be on another subnet or VPN, or macOS may be blocking local network access. Check System Settings > Privacy & Security > Local Network and allow your terminal app (or Claude). You can always give Claude the address directly.
- **Quick Connect is disabled**: an admin can enable it in Dashboard > General, or connect with a username and password.
- **Music stops when I close Claude**: by design. The player lives inside the MCP server, which exits with the session.

## Development

```sh
make build        # bin/jellyfin-mcp
make test         # unit tests (backend tests use mpv/afplay when installed, at volume 0)
JELLYFIN_TEST_URL=nas.local JELLYFIN_TEST_USER=me JELLYFIN_TEST_PASS=... JELLYFIN_TEST_PLAYER=mpv make integration
```

The integration test drives the real binary over MCP against a live server at volume 0, and checks that Jellyfin sees the playback session.
