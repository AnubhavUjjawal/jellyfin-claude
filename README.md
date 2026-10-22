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

#### One-time setup: let the player pane update

The plugin adds a now-playing pane, a status line and `/jf` commands to Claude Code (see below). They talk to the plugin's own server in the background, and Claude Code never lets a plugin approve its own calls, so add one permission rule. Run `/permissions` and add an allow rule for:

```
mcp__plugin_jellyfin_jellyfin
```

or add it to `~/.claude/settings.json` under `permissions.allow`. Until then the pane shows this hint instead of prompting you every second.

### Claude Desktop

```sh
make desktop-config
```

Merge the printed snippet into `~/Library/Application Support/Claude/claude_desktop_config.json` and restart Claude Desktop.

## In Claude Code: pane, status line and /jf

```
╭────────────────────────────────────────────────────────────────────────────────✕─╮
│ Yellow                                                                           │
│ Coldplay - Parachutes                                                            │
│ ━━────────────────────────────────────────────────────────────────── 0:09 / 4:29 │
│ playing  vol 0  repeat off  1/1                                                  │
│                                                                                  │
│ [ prev ] [ pause ] [ next ] [ -10s ] [ +30s ] [ vol - ] [ vol + ] [ mix ]        │
│ [ fav ] [ repeat off ]                                                           │
╰──────────────────────────────────────────────────────────────────────────────────╯
  ⚠ jellyfin: > Yellow - Coldplay  0:09 / 4:29
```

- **The pane** opens by itself when music starts on a wide terminal (docked beside the chat), or with `/jf` at any width (above the prompt). Buttons work with the mouse, or with Tab and Enter once the pane has focus. Pressing an up-next row jumps to it.
- **Album art** shows in terminals that draw pictures (kitty, Ghostty, WezTerm, iTerm2 3.5+, not inside tmux); elsewhere the text gets the room.
- **Lyrics** stream in the pane, line by line, for tracks with synced lyrics (from `.lrc` files beside your music or a lyrics plugin such as LrcLib; Jellyfin 10.9+): the line being sung in bold between the one before and the one after. The pane polls twice a second while they show. `hide lyrics` (or `/jf lyrics off`) turns them off; `/jf lyrics` prints the whole text, which also works for unsynced lyrics.
- **The status line** under the prompt shows the current track; a toast announces each new one.
- **`/jf` commands** act instantly, without a model turn:

| Command | Does |
| --- | --- |
| `/jf` | show or hide the pane |
| `/jf <name>` | play the best match for an artist, album, playlist or track |
| `/jf pause` `resume` `next` `prev` `stop` | transport |
| `/jf vol 40` / `vol +10` | volume |
| `/jf seek 1:30` / `seek +30` | seek (mpv) |
| `/jf mix` | more like the current track |
| `/jf lyrics` / `lyrics on` / `lyrics off` | print the lyrics / show or hide them in the pane |
| `/jf fav`, `shuffle`, `clear`, `repeat all` | the rest |

Everything is laid out against the pane's measured width and only single-width characters are drawn, so the frame stays intact at any terminal size; long or wide (CJK, emoji) titles are cut with `…`.

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
| `now_playing` | Playback state as structured data (used by the pane) |
| `artwork` | An item's cover as a PNG (used by the pane) |
| `lyrics` | A track's lyrics, with each line's start time when synced |
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
claude plugin test .       # the pane/status/commands mod (hooks/), on every surface and width
claude plugin validate .
JELLYFIN_TEST_URL=nas.local JELLYFIN_TEST_USER=me JELLYFIN_TEST_PASS=... JELLYFIN_TEST_PLAYER=mpv make integration
```

The integration test drives the real binary over MCP against a live server at volume 0, and checks that Jellyfin sees the playback session.
