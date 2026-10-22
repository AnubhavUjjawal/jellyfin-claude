// The Jellyfin player in Claude Code's own interface: a now-playing pane, a
// status line, a toast on each new track, and /jf commands that drive the
// player without a model turn. Everything goes through this plugin's own MCP
// server ($.mcp.call), so the pane and Claude always see the same player.
import { atom, read, update } from 'claude-code'
import type { EngineInterface, McpToolResult, Register } from 'claude-code'

import type { JellyfinLyrics, JellyfinNowPlaying } from '../types'
import { fit, lyricWindow, progressLine, statusText, timeLabel } from './layout'

type Engine = EngineInterface

const PANE = 'jellyfin'
const TITLE = 'Jellyfin'
// The pane's cover, in cells: terminal cells are about twice as tall as wide.
const ART_COLS = 14
const ART_ROWS = 7
// Below this many columns the cover is left out so the text keeps its room.
const ART_MIN_WIDTH = 44
// The clock ticks every TICK_MS; the player is polled every so many ticks.
const TICK_MS = 250
const POLL_TICKS_LYRICS = 2 // every 500 ms while synced lyrics are on screen
const POLL_TICKS_PLAYING = 4 // every second while music plays
const POLL_TICKS_IDLE = 20 // every 5 s otherwise
const PERMISSION_TICKS = 40 // every 10 s while the permission rule is missing
// A lyrics fetch that fails (Jellyfin slow or away) is tried this many times.
const LYRICS_TRIES = 3
// How the engine names this plugin's MCP server and its tools.
const SETUP_RULE = 'mcp__plugin_jellyfin_jellyfin'
const OWN_TOOLS = `${SETUP_RULE}__`
const SETUP_HINT = `The player pane needs a permission rule to update in the background: run /permissions and add an allow rule for ${SETUP_RULE}`

const np = atom({ plugin: 'jellyfin', key: 'np' } as const, null)
const art = atom({ plugin: 'jellyfin', key: 'art' } as const, null)
const hasAutoOpened = atom({ plugin: 'jellyfin', key: 'hasAutoOpened' } as const, false)
const needsSetup = atom({ plugin: 'jellyfin', key: 'needsSetup' } as const, false)
const lyrics = atom({ plugin: 'jellyfin', key: 'lyrics' } as const, null)
const showLyrics = atom({ plugin: 'jellyfin', key: 'showLyrics' } as const, true)

const HELP = [
  '/jf                 show or hide the player pane',
  '/jf <name>          play the best match (artist, album, playlist, track)',
  '/jf pause | resume | next | prev | stop',
  '/jf vol 40 | vol +10 | seek 1:30 | seek +30',
  '/jf mix             more like the current track',
  '/jf lyrics          print the lyrics; /jf lyrics on|off shows or hides them in the pane',
  '/jf fav | shuffle | clear | repeat off|all|one | status',
].join('\n')

function textOf(res: McpToolResult): string {
  return res.content
    .filter(b => b.type === 'text')
    .map(b => b.text ?? '')
    .join('\n')
    .trim()
}

// Claude Code already heads this plugin's command output and toasts with its
// name, so a message's own "jellyfin: " prefixes are dropped.
function errorText(err: unknown): string {
  return (err instanceof Error ? err.message : String(err)).replace(/^(jellyfin: )+/i, '')
}

/**
 * The base64 PNG of an image result: Claude Code hands MCP image blocks over as
 * `{ source: { data } }`; plain MCP has `{ data }`.
 */
function pngOf(res: McpToolResult): string | null {
  const img = res.content.find(b => b.type === 'image')
  if (!img) return null
  if (typeof img.data === 'string' && img.data) return img.data
  const source = img.source as { data?: unknown } | undefined
  return typeof source?.data === 'string' && source.data ? source.data : null
}

/** now_playing's structured result, or the same JSON from its text block. */
function nowPlayingOf(res: McpToolResult): JellyfinNowPlaying | null {
  if (res.structuredContent && typeof res.structuredContent === 'object') {
    return res.structuredContent as JellyfinNowPlaying
  }
  try {
    const parsed: unknown = JSON.parse(textOf(res))
    return parsed && typeof parsed === 'object' && 'state' in parsed ? (parsed as JellyfinNowPlaying) : null
  } catch {
    return null
  }
}

function nowLine(s: JellyfinNowPlaying | null): string {
  if (!s) return 'The Jellyfin player is not running.'
  if (!s.connected) return 'Not connected to Jellyfin.'
  if (!s.track.id || s.state === 'stopped') return 'Stopped.'
  const who = s.track.artist ? `${s.track.name} - ${s.track.artist}` : s.track.name
  const state = s.state === 'paused' ? 'Paused' : 'Playing'
  return `${state}: ${who} (${timeLabel(s.position_sec, s.track.duration_sec)})`
}

// Per-load bookkeeping: reset on a reload. What the drawing reads is in $.state,
// which a resumed or switched session starts empty while these stay, so what is
// already loaded is told from $.state; here are only the fetches in flight.
const live: {
  server?: string
  isPolling: boolean
  ticks: number
  lastTrackId?: string
  artLoading?: string
  lyricsKey?: string
  lyricsLoading?: string
  lyricsTries: number
  isLyricsRetryDue: boolean
  canShowArt: boolean
  isAllowed: boolean
} = {
  isPolling: false,
  ticks: 0,
  lyricsTries: 0,
  isLyricsRetryDue: false,
  canShowArt: false,
  isAllowed: false,
}

/**
 * Whether this terminal draws pictures: kitty, Ghostty, WezTerm and iTerm2 do
 * (tmux passes none through). Elsewhere the cover would only leave a blank box,
 * so the pane gives its columns to the text instead.
 */
async function detectArt($: Engine): Promise<boolean> {
  if (await $.env.get('TMUX')) return false
  const program = ((await $.env.get('TERM_PROGRAM')) ?? '').toLowerCase()
  const term = ((await $.env.get('TERM')) ?? '').toLowerCase()
  return (
    ['ghostty', 'wezterm', 'iterm.app'].includes(program) ||
    term.includes('kitty') ||
    term.includes('ghostty') ||
    Boolean(await $.env.get('KITTY_WINDOW_ID'))
  )
}


async function serverName($: Engine): Promise<string> {
  if (live.server) return live.server
  const conn = await $.mcp.connect('jellyfin')
  if (!conn.isConnected) throw new Error(conn.message)
  live.server = conn.server
  return live.server
}

async function call($: Engine, tool: string, args?: Record<string, unknown>): Promise<McpToolResult> {
  let res: McpToolResult
  try {
    res = await $.mcp.call(await serverName($), tool, args)
  } catch (err) {
    live.server = undefined // reconnect next time, in case the server restarted
    throw err
  }
  if (res.isError) throw new Error(textOf(res) || `${tool} failed`)
  return res
}

async function loadArt($: Engine, key: string): Promise<void> {
  let png: string | null = null
  try {
    const res = await call($, 'artwork', { item_id: key, size: 256 })
    png = pngOf(res)
  } catch {
    // No cover: the pane draws without one.
  }
  if (live.artLoading !== key) return // the track moved on to another cover
  try {
    await update($, art, () => ({ key, png }))
  } finally {
    live.artLoading = undefined
  }
}

/** The lyrics tool's JSON (Claude Code passes it as text), or null. */
function lyricsOf(res: McpToolResult): JellyfinLyrics | null {
  try {
    const parsed: unknown = JSON.parse(textOf(res))
    return parsed && typeof parsed === 'object' && 'lines' in parsed ? (parsed as JellyfinLyrics) : null
  } catch {
    return null
  }
}

async function loadLyrics($: Engine, trackId: string): Promise<void> {
  let ly: JellyfinLyrics = { track_id: trackId, synced: false, lines: [] }
  try {
    ly = lyricsOf(await call($, 'lyrics', { item_id: trackId })) ?? ly
  } catch {
    // A track without lyrics answers with no lines, so this is a failed fetch:
    // try again at the next poll rather than settle on none for the track.
    if (live.lyricsKey === trackId && ++live.lyricsTries < LYRICS_TRIES) {
      live.isLyricsRetryDue = true
      return
    }
  }
  if (live.lyricsKey === trackId) await update($, lyrics, () => ly)
}

async function refresh($: Engine): Promise<JellyfinNowPlaying | null> {
  const s = nowPlayingOf(await call($, 'now_playing'))
  if (!s) throw new Error('now_playing returned no player state')
  await update($, np, () => s)
  $.ui.status(statusText(s))

  const id = s.track.id
  const isPlaying = s.state === 'playing'
  // No toast for the first look after a start or reload: only real changes.
  if (live.lastTrackId !== undefined && id && id !== live.lastTrackId && isPlaying) {
    const who = s.track.artist ? `${s.track.name} - ${s.track.artist}` : s.track.name
    $.ui.toast(`Now playing: ${fit(who, 80)}`)
  }
  live.lastTrackId = id
  if (isPlaying && !(await read($, hasAutoOpened))) {
    await update($, hasAutoOpened, () => true)
    void $.ui.open({ id: PANE, title: TITLE })
  }

  const key = s.track.album_id || s.track.id
  if (key && key !== live.artLoading && (await read($, art))?.key !== key) {
    live.artLoading = key
    void loadArt($, key).catch(() => {}) // a reload may have unloaded the state
  }
  const isLyricsMissing = id !== live.lyricsLoading && (await read($, lyrics))?.track_id !== id
  if (id && (id !== live.lyricsKey || live.isLyricsRetryDue || isLyricsMissing)) {
    if (id !== live.lyricsKey) live.lyricsTries = 0
    live.lyricsKey = id
    live.isLyricsRetryDue = false
    if (s.track.has_lyrics) {
      live.lyricsLoading = id
      void loadLyrics($, id)
        .catch(() => {})
        .finally(() => {
          if (live.lyricsLoading === id) live.lyricsLoading = undefined
        })
    } else {
      await update($, lyrics, () => ({ track_id: id, synced: false, lines: [] }))
    }
  }
  return s
}

/**
 * Whether the session lets this plugin call its server without asking: only a
 * permission rule does (no permission mode lets a plugin's background calls
 * through on its own). A dry-run check: nothing runs and no dialog opens.
 */
async function isAllowed($: Engine): Promise<boolean> {
  try {
    const verdict = await $.tool.check({ tool: `${OWN_TOOLS}now_playing`, input: {} })
    return verdict.decision === 'allow'
  } catch {
    return false
  }
}

async function tick($: Engine): Promise<void> {
  const n = live.ticks++
  if (live.isPolling) return
  if (!live.isAllowed) {
    // Without the rule every poll would raise a permission prompt (or be
    // refused in auto mode), so wait for it, looking again now and then.
    if (n % PERMISSION_TICKS !== 0) return
    live.isAllowed = await isAllowed($)
    await update($, needsSetup, () => !live.isAllowed)
    if (!live.isAllowed) return
  }
  const s = await read($, np)
  let every = POLL_TICKS_IDLE
  if (s?.state === 'playing') {
    const ly = await read($, lyrics)
    const isFollowing = ly?.synced && ly.track_id === s.track.id && (await read($, showLyrics))
    every = isFollowing ? POLL_TICKS_LYRICS : POLL_TICKS_PLAYING
  }
  if (n % every !== 0) return
  live.isPolling = true
  try {
    await refresh($)
  } catch {
    // The server is starting or went away; the next tick tries again.
  } finally {
    live.isPolling = false
  }
}

/** Runs a player tool, then refreshes; resolves the line to show, or throws. */
async function act($: Engine, tool: string, args: Record<string, unknown>): Promise<string> {
  const res = await call($, tool, args)
  const s = await refresh($).catch(() => null)
  // A note from the server (say, volume applying from the next track) leads.
  const note = tool === 'control' ? textOf(res).split('\n')[0] : ''
  const line = nowLine(s)
  return note && !note.startsWith('Player:') ? `${note}\n${line}` : line
}

/** The lyrics Button's press: show or hide the lyrics section. */
function toggleLyrics($: Engine): () => void {
  return () => {
    void update($, showLyrics, on => !on).catch(() => {})
  }
}

/** /jf lyrics: the current track's lyrics in full, for the transcript. */
async function printLyrics($: Engine) {
  try {
    const s = await read($, np)
    if (!s?.track.id) return { text: 'Nothing is playing.' }
    const ly = lyricsOf(await call($, 'lyrics', { item_id: s.track.id }))
    if (!ly || ly.lines.length === 0) return { text: `No lyrics for ${s.track.name}.` }
    const who = s.track.artist ? `${s.track.name} - ${s.track.artist}` : s.track.name
    return { text: [`Lyrics: ${who}`, '', ...ly.lines.map(l => l.text)].join('\n') }
  } catch (err) {
    return { text: errorText(err) }
  }
}

/** A Button's press: act, and surface a failure as a toast. */
function press($: Engine, tool: string, args: Record<string, unknown> = {}): () => void {
  return () => {
    void act($, tool, args).catch(err => $.ui.toast(fit(errorText(err), 200)))
  }
}

async function run($: Engine, tool: string, args: Record<string, unknown>) {
  try {
    return { text: await act($, tool, args) }
  } catch (err) {
    const hint = (await read($, needsSetup)) ? `\n${SETUP_HINT}` : ''
    return { text: errorText(err) + hint }
  }
}

async function togglePane($: Engine) {
  const panes = await $.ui.panes()
  // An auto-opened pane may be waiting undrawn on a narrow terminal: that is
  // not "showing", and the person's /jf is what places it.
  if (panes.some(p => p.id === PANE && p.isShown && p.isPlaced)) {
    await $.ui.close({ id: PANE })
    return {}
  }
  const opened = await $.ui.open({ id: PANE, title: TITLE })
  // Without the permission rule the pane shows how to add it; refreshing here
  // would only raise a prompt.
  if (live.isAllowed) await refresh($).catch(() => null)
  return opened.isPlaced ? {} : { text: `The player pane can't be shown here: ${opened.reason}` }
}

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    await $.command.register({
      name: 'jf',
      description: 'Jellyfin player: show the pane, or play, pause, skip and set volume without asking Claude',
      argumentHint: '[name | pause | next | prev | vol N | seek t | mix | fav | help]',
      immediate: true,
    })
    live.canShowArt = await detectArt($).catch(() => false)
    $.clock.every(TICK_MS, () => void tick($))
    return next(e)
  })

  // When Claude drives the player, show the result right away rather than at
  // the next poll. The pane's own calls go through $.mcp.call, not here.
  on('tool.call', async ($, e, next) => {
    const ran = await next(e)
    const tool = String(e.tool)
    const isRead = ['__now_playing', '__artwork', '__lyrics'].some(suffix => tool.endsWith(suffix))
    if (tool.startsWith(OWN_TOOLS) && !isRead) {
      void refresh($).catch(() => null)
    }
    return ran
  }).catch(($, e, next) => next(e))

  on('command.run', { command: 'jf' }, async ($, e) => {
    const args = e.args.trim()
    const [word = '', ...rest] = args.split(/\s+/)
    const value = rest.join(' ')
    switch (word.toLowerCase()) {
      case '':
        return togglePane($)
      case 'help':
        return { text: HELP }
      case 'debug': {
        // What the server answers, for troubleshooting the pane.
        try {
          const res = await call($, 'now_playing')
          const shape = res.structuredContent === undefined ? 'no structuredContent' : 'structuredContent'
          const lines = [`now_playing: ${shape}; blocks ${res.content.map(b => b.type).join(',')}; ${textOf(res).slice(0, 400)}`]
          const s = nowPlayingOf(res)
          const key = s?.track.album_id || s?.track.id
          if (key) {
            const artRes = await call($, 'artwork', { item_id: key, size: 256 })
            const block = artRes.content[0]
            const fields = block ? Object.keys(block).map(k => `${k}:${typeof block[k]}`).join(' ') : 'none'
            lines.push(`artwork: ${fields}; png ${pngOf(artRes)?.length ?? 0} base64 chars`)
          }
          lines.push(`terminal art: ${live.canShowArt ? 'yes' : 'no'}`)
          return { text: lines.join('\n') }
        } catch (err) {
          return { text: `now_playing failed: ${errorText(err)}` }
        }
      }
      case 'status':
      case 'np':
        return { text: nowLine(await refresh($).catch(() => null)) }
      case 'play':
        return value ? run($, 'play', { query: value }) : run($, 'control', { action: 'resume' })
      case 'p':
      case 'toggle':
        return run($, 'control', { action: 'toggle' })
      case 'pause':
      case 'resume':
      case 'next':
      case 'stop':
      case 'shuffle':
      case 'clear':
        return run($, 'control', { action: word.toLowerCase() })
      case 'n':
      case 'skip':
        return run($, 'control', { action: 'next' })
      case 'b':
      case 'prev':
      case 'previous':
      case 'back':
        return run($, 'control', { action: 'previous' })
      case 'vol':
      case 'volume':
        return run($, 'control', { action: 'volume', value })
      case 'seek':
        return run($, 'control', { action: 'seek', value })
      case 'repeat':
        return run($, 'control', { action: 'repeat', value: value || 'all' })
      case 'fav':
      case 'favorite':
        return run($, 'favorite', {})
      case 'lyrics':
        if (value === 'on' || value === 'off') {
          await update($, showLyrics, () => value === 'on')
          return { text: `Lyrics in the pane: ${value}.` }
        }
        return printLyrics($)
      case 'mix': {
        const s = await read($, np)
        if (!s?.track.id) return { text: 'Nothing is playing to make a mix from.' }
        return run($, 'play', { item_ids: [s.track.id], instant_mix: true })
      }
      default:
        return run($, 'play', { query: args })
    }
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const { Box, Text, Button } = $.ui.resolve(e)
    const s = await read($, np)
    const cover = await read($, art)
    const width = Math.max(1, e.props.bodyColumns)

    if (await read($, needsSetup)) {
      // Wrapped rather than cut, so the rule's name is always there to read.
      return (
        <Box flexDirection="column" width={width} overflow="hidden">
          <Text bold>{fit('One-time setup', width)}</Text>
          <Text>{SETUP_HINT}</Text>
        </Box>
      )
    }

    if (!s) {
      return (
        <Box width={width} overflow="hidden">
          <Text dimColor>{fit('Starting the player...', width)}</Text>
        </Box>
      )
    }

    if (!s.connected) {
      const hint = s.pending_code
        ? `Approve Quick Connect code ${s.pending_code} in Jellyfin (profile > Quick Connect).`
        : 'Ask Claude to connect to your Jellyfin server.'
      return (
        <Box flexDirection="column" width={width} overflow="hidden">
          <Text bold>{fit('Not connected', width)}</Text>
          <Text dimColor>{fit(hint, width)}</Text>
        </Box>
      )
    }

    if (!s.track.id) {
      return (
        <Box flexDirection="column" gap={1} width={width} overflow="hidden">
          <Box flexDirection="column">
            <Text bold>{fit('Nothing playing', width)}</Text>
            <Text dimColor>{fit(`Connected to ${s.server}. Try /jf <artist or album>.`, width)}</Text>
          </Box>
          <Box>
            <Button key="shuffle-all" label="shuffle library" hotkey="s" onPress={press($, 'play', {})} />
          </Box>
        </Box>
      )
    }

    const t = s.track
    const coverKey = t.album_id || t.id
    const pngOnTerminal =
      e.surface === 'terminal' && live.canShowArt && width >= ART_MIN_WIDTH && cover?.key === coverKey ? cover.png : null
    const textCols = pngOnTerminal ? width - ART_COLS - 2 : width

    let picture = null
    if (pngOnTerminal && e.surface === 'terminal') {
      const { Image } = $.ui.resolve(e)
      picture = <Image key="cover" source={{ png: pngOnTerminal }} columns={ART_COLS} rows={ART_ROWS} alt=" " />
    }

    const state = s.state === 'playing' ? 'playing' : s.state === 'paused' ? 'paused' : 'stopped'
    const details = `${state}  vol ${s.volume}  repeat ${s.repeat}  ${s.queue_position}/${s.queue_length}`
    const byline = [t.artist, t.album].filter(Boolean).join(' - ')
    const nextRepeat = s.repeat === 'off' ? 'all' : s.repeat === 'all' ? 'one' : 'off'

    // Lyrics: three rows (last line, current line, next line) cut to the width,
    // so the pane keeps one height as they move and nothing wraps past a frame.
    const isLyricsOn = await read($, showLyrics)
    const ly = await read($, lyrics)
    let lyricsView = null
    if (isLyricsOn && t.has_lyrics) {
      if (!ly || ly.track_id !== t.id) {
        lyricsView = <Text dimColor>{fit('Loading lyrics...', width)}</Text>
      } else if (ly.lines.length > 0 && !ly.synced) {
        lyricsView = <Text dimColor>{fit('These lyrics are not synced: /jf lyrics prints them.', width)}</Text>
      } else if (ly.lines.length > 0) {
        const w = lyricWindow(ly.lines, s.position_sec)
        lyricsView = (
          <Box flexDirection="column" width={width}>
            <Text dimColor wrap="truncate-end">{fit(w.previous, width) || ' '}</Text>
            <Text bold wrap="truncate-end">{fit(w.current, width) || ' '}</Text>
            <Text dimColor wrap="truncate-end">{fit(w.next, width) || ' '}</Text>
          </Box>
        )
      }
    }

    return (
      <Box flexDirection="column" gap={1} width={width} overflow="hidden">
        <Box flexDirection="row" gap={2}>
          {picture}
          <Box flexDirection="column" width={textCols}>
            <Text bold wrap="truncate-end">{fit(t.name, textCols)}</Text>
            <Text dimColor wrap="truncate-end">{fit(byline, textCols)}</Text>
            <Text wrap="truncate-end">{progressLine(s.position_sec, t.duration_sec, textCols)}</Text>
            <Text dimColor wrap="truncate-end">{fit(details, textCols)}</Text>
          </Box>
        </Box>

        {lyricsView}

        <Box flexDirection="row" flexWrap="wrap" columnGap={1} width={width}>
          <Button key="prev" label="prev" hotkey="b" onPress={press($, 'control', { action: 'previous' })} />
          <Button
            key="toggle"
            label={s.state === 'playing' ? 'pause' : 'play'}
            hotkey="p"
            variant="primary"
            onPress={press($, 'control', { action: 'toggle' })}
          />
          <Button key="next" label="next" hotkey="n" onPress={press($, 'control', { action: 'next' })} />
          {s.can_seek && <Button key="back10" label="-10s" onPress={press($, 'control', { action: 'seek', value: '-10' })} />}
          {s.can_seek && <Button key="fwd30" label="+30s" onPress={press($, 'control', { action: 'seek', value: '+30' })} />}
          <Button key="voldown" label="vol -" hotkey="d" onPress={press($, 'control', { action: 'volume', value: '-10' })} />
          <Button key="volup" label="vol +" hotkey="u" onPress={press($, 'control', { action: 'volume', value: '+10' })} />
          <Button key="mix" label="mix" hotkey="m" onPress={press($, 'play', { item_ids: [t.id], instant_mix: true })} />
          <Button key="fav" label="fav" hotkey="f" onPress={press($, 'favorite', {})} />
          <Button key="repeat" label={`repeat ${s.repeat}`} hotkey="r" onPress={press($, 'control', { action: 'repeat', value: nextRepeat })} />
          {t.has_lyrics && (
            <Button key="lyrics" label={isLyricsOn ? 'hide lyrics' : 'lyrics'} hotkey="l" onPress={toggleLyrics($)} />
          )}
        </Box>

        {s.upcoming.length > 0 && (
          <Box flexDirection="column" width={width}>
            <Text dimColor>{fit(`Up next (${s.queue_length - s.queue_position} more)`, width)}</Text>
            {s.upcoming.map((u, i) => {
              const pos = s.queue_position + 1 + i
              const who = u.artist ? `${u.name} - ${u.artist}` : u.name
              return (
                <Button key={`queue-${pos}`} plain onPress={press($, 'control', { action: 'jump', value: String(pos) })}>
                  {fit(`${pos}. ${who}`, width)}
                </Button>
              )
            })}
          </Box>
        )}

        {s.last_error !== '' && <Text dimColor wrap="truncate-end">{fit(`! ${s.last_error}`, width)}</Text>}
      </Box>
    )
  })
}
