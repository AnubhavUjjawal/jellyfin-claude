---
name: jellyfin-dj
description: Play music from the user's Jellyfin server on this computer's speakers. Use when the user asks to play, queue, pause, skip or change the volume of music, asks what's playing, wants "something like this", or wants recommendations from their own music library.
---

# Jellyfin DJ

The `jellyfin` MCP tools play music from the user's Jellyfin server locally (through mpv, or afplay on macOS). No Jellyfin app needs to be open.

## Getting connected

1. Call `status`. If it says connected, skip to playing.
2. Not connected: call `discover_servers`. If one server answers, use it; if several, ask which. If none, ask the user for the address (e.g. `192.168.1.10` or `nas.local`; port 8096 is assumed).
3. Call `connect` with just the `url`. This uses Quick Connect and returns a code. Tell the user the code and to approve it in Jellyfin while signed in (web or app: profile menu > Quick Connect). When they say it's done, call `status` to confirm.
   - Only if Quick Connect is disabled, ask whether they want to give a username and password instead. Don't ask for a password otherwise.
4. The login is saved, so later sessions are connected straight away.

## Playing music

- Turn a request into ids with `search` (by name) or `browse` (favorites, recently_played, most_played, recently_added, random, albums, artists, playlists, genres, artist_albums, tracks).
- Then call `play`:
  - Specific song/album/artist/playlist: `item_ids` with that id. Albums and playlists keep their order; set `shuffle` for artists unless the user wants discography order.
  - "Something like X" / "more like this": `instant_mix: true` with X's id (use the current track id from `status` for "this").
  - A mood or genre ("some jazz", "chill 90s"): find matching genres (`search` with types `genres`, or `browse` genres) and either play the genre id with `shuffle`, or pick tracks/albums yourself using what the library contains.
  - "Just play something": `play` with no ids shuffles the library.
  - "Add this"/"queue that": `mode: append`. "Play this next": `mode: next`.
- When search is ambiguous (several albums or artists match), pick the most likely one and say what you chose rather than asking, unless it's genuinely unclear.
- For curated requests ("a playlist that winds down", "songs I haven't heard in a while"), browse/search to gather candidates, choose tracks yourself, and pass their ids in the order you want.

## Controlling playback

Use `control`: pause, resume, toggle, next, previous, stop, seek (`value` seconds, `m:ss`, or `+30`/`-10`), volume (`0`-`100` or `+10`/`-10`), jump (queue position from `status`), shuffle, clear (upcoming tracks), repeat (`off`/`all`/`one`). `favorite` hearts the current track.

If `status` shows the afplay player, seeking isn't possible and volume changes apply from the next track; suggest `brew install mpv` if the user wants those.

## The player pane and /jf

In Claude Code the plugin also shows a now-playing pane, a status line and `/jf` commands the user can run without you (`/jf next`, `/jf vol 40`, `/jf radiohead`). When the user just wants a quick transport action, you may mention `/jf` as the instant way. If the pane says it needs setup, tell the user to add the permission rule `mcp__plugin_jellyfin_jellyfin` via `/permissions`.

## Style

Keep replies to a line or two: what's playing now and anything notable ("Playing OK Computer by Radiohead, 12 tracks"). Never show item ids to the user. Music stops when this Claude session ends.
