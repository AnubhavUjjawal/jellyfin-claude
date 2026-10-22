// What the jellyfin MCP server's now_playing tool returns, and the values the
// player pane keeps in $.state.

export type JellyfinTrack = {
  id: string
  name: string
  artist: string
  album: string
  album_id: string
  duration_sec: number
  has_lyrics: boolean
}

/** One lyric line; start_sec is -1 when the lyrics are not synced. */
export type JellyfinLyricLine = { start_sec: number; text: string }

/** What the lyrics tool returns: lines is empty when the track has none. */
export type JellyfinLyrics = { track_id: string; synced: boolean; lines: JellyfinLyricLine[] }

export type JellyfinNowPlaying = {
  connected: boolean
  server: string
  user: string
  pending_code: string
  state: 'playing' | 'paused' | 'stopped'
  backend: string
  can_seek: boolean
  track: JellyfinTrack
  position_sec: number
  queue_position: number
  queue_length: number
  upcoming: JellyfinTrack[]
  volume: number
  repeat: string
  last_error: string
}

/** Cover art for one album (or track), cached by key; png is null when the server has none. */
export type JellyfinArt = { key: string; png: string | null }

declare module 'claude-code' {
  interface PluginState {
    jellyfin: {
      np: JellyfinNowPlaying | null
      art: JellyfinArt | null
      hasAutoOpened: boolean
      needsSetup: boolean
      lyrics: JellyfinLyrics | null
      showLyrics: boolean
    }
  }
}
