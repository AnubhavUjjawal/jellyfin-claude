import { expect, mock, test } from 'claude-code/testing'
import type { On } from 'claude-code'

import type { JellyfinNowPlaying, JellyfinTrack } from '../types'
import { cellWidth, fit, lyricWindow, progressLine } from './layout'

const SURFACES = ['terminal', 'desktop', 'vscode', 'mobile'] as const
const WIDTHS = [24, 40, 60, 100]
// A 1x1 PNG.
const PNG = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=='

function track(id: string, name: string, artist = 'Coldplay'): JellyfinTrack {
  return { id, name, artist, album: 'A Rush Of Blood To The Head', album_id: 'album-1', duration_sec: 307, has_lyrics: true }
}

// Synced lyrics as the server sends them; the 100 s line is deliberately long
// and wide, to hold the pane to its width.
const LYRICS = [
  { start_sec: 20, text: 'The lights go out and I can’t be saved' },
  { start_sec: 90, text: 'Tides that I tried to swim against' },
  { start_sec: 100, text: 'Have brought me down upon my knees, oh 東京の夜に 🎸 I beg, I beg and plead, singing' },
  { start_sec: 110, text: '' },
  { start_sec: 115, text: 'Come out of things unsaid' },
]

function playing(over: Partial<JellyfinNowPlaying> = {}): JellyfinNowPlaying {
  return {
    connected: true,
    server: 'base01',
    user: 'me',
    pending_code: '',
    state: 'playing',
    backend: 'mpv',
    can_seek: true,
    track: track('t1', 'Clocks'),
    position_sec: 102,
    queue_position: 1,
    queue_length: 3,
    upcoming: [track('t2', 'My Universe'), track('t3', 'Yellow')],
    volume: 60,
    repeat: 'off',
    last_error: '',
    ...over,
  }
}

type Call = { tool: string; args: Record<string, unknown> }

/** Stands in for the engine: the jellyfin MCP server, panes, status and toasts. */
function fakeEngine(on: On, state: { np: JellyfinNowPlaying }, permission: 'allow' | 'ask' = 'allow') {
  const calls: Call[] = []
  const statuses: (string | undefined)[] = []
  const toasts: string[] = []
  on('tool.check', () => ({ decision: permission }))
  on('mcp.connect', () => ({ value: { isConnected: true, server: 'plugin:jellyfin:jellyfin' } }))
  on('mcp.call', (_, e) => {
    calls.push({ tool: e.tool, args: e.args })
    switch (e.tool) {
      case 'now_playing':
        // As Claude Code hands it over: the JSON text, no structuredContent.
        return { value: { content: [{ type: 'text', text: JSON.stringify(state.np) }], isError: false } }
      case 'artwork':
        // The shape Claude Code hands MCP image blocks over in.
        return {
          value: {
            content: [{ type: 'image', source: { type: 'base64', media_type: 'image/png', data: PNG } }],
            isError: false,
          },
        }
      case 'lyrics':
        return {
          value: {
            content: [{ type: 'text', text: JSON.stringify({ track_id: e.args.item_id, synced: true, lines: LYRICS }) }],
            isError: false,
          },
        }
      case 'control':
        if (e.args.action === 'next') {
          state.np = { ...state.np, track: track('t2', 'My Universe'), queue_position: 2, upcoming: [track('t3', 'Yellow')] }
        }
        if (e.args.action === 'volume' && e.args.value === 'loud') {
          return { value: { content: [{ type: 'text', text: 'volume needs a number' }], isError: true } }
        }
        return { value: { content: [{ type: 'text', text: 'Player: mpv.' }], isError: false } }
      default:
        return { value: { content: [{ type: 'text', text: 'ok' }], isError: false } }
    }
  })
  on('command.register', (_, e) => ({ value: { command: e.name } }))
  on('ui.open', () => ({ value: { isPlaced: true } }))
  on('ui.panes', () => ({ value: [] }))
  on('ui.status', (_, e) => {
    statuses.push(e.text)
    return { value: undefined }
  })
  on('ui.toast', (_, e) => {
    toasts.push(e.text)
    return { value: undefined }
  })
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  return { calls, statuses, toasts }
}

function pane(surface: (typeof SURFACES)[number], width: number) {
  return {
    plugin: 'jellyfin',
    component: 'Pane',
    requestId: 'jellyfin',
    surface,
    props: {
      title: 'Jellyfin',
      isFocused: false,
      bodyColumns: width,
      placement: 'dock',
      scroll: { offset: 0, bodyRows: 40 },
      view: {},
    },
  } as const
}

test('layout helpers never exceed their width', () => {
  for (const w of [0, 1, 2, 5, 13, 40]) {
    expect(cellWidth(fit('東京事変 - 群青日和 🎸🎸 (Live at Budokan)', w))).toBeLessThanOrEqual(w)
    expect(cellWidth(progressLine(102, 307, w))).toBeLessThanOrEqual(w)
  }
  expect(cellWidth('東京')).toBe(4)
  expect(fit('Clocks', 10)).toBe('Clocks')
})

test('the pane stays inside its width on every surface and size', async ($, on) => {
  const clock = mock.clock(on)
  const longTitle = '群青日和 (Gunjou Biyori) 🎸 Live at Nippon Budokan, Remastered Deluxe Edition'
  const state = { np: playing({ track: { ...track('t1', longTitle, '東京事変 Tokyo Jihen'), album: 'Golden Time Rendezvous' } }) }
  fakeEngine(on, state)
  mock.env(on, { TERM_PROGRAM: 'ghostty' })
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await clock.advance(1000)
  await clock.settle()

  for (const surface of SURFACES) {
    for (const width of WIDTHS) {
      const ui = await $.ui.mount(pane(surface, width))
      await ui.drawn() // rejects if the surface refuses the tree
      const shown = [...(await ui.findAll({ type: 'Text' })), ...(await ui.findAll({ type: 'Button' }))]
      expect(shown.length).toBeGreaterThan(5)
      for (const el of shown) {
        const label = typeof el.props.label === 'string' ? el.props.label : el.text
        if (cellWidth(label) > width) throw new Error(`${surface}@${width}: "${label}" is ${cellWidth(label)} cells`)
      }
      const image = await ui.find({ type: 'Image' })
      if (surface === 'terminal' && width >= 44) {
        expect(image?.props.columns).toBe(14)
      } else {
        expect(image).toBeUndefined()
      }
      await ui.unmount()
    }
  }
})

test('a terminal without pictures gives the cover columns to the text', async ($, on) => {
  const clock = mock.clock(on)
  fakeEngine(on, { np: playing() })
  mock.env(on, { TERM_PROGRAM: 'Apple_Terminal' })
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await clock.advance(1000)
  await clock.settle()

  const ui = await $.ui.mount(pane('terminal', 100))
  expect(await ui.find({ type: 'Image' })).toBeUndefined()
  const bar = await ui.find({ type: 'Text', text: /1:42 \/ 5:07/ })
  expect(cellWidth(bar?.text ?? '')).toBe(100)
  await ui.unmount()
})

test('buttons drive the player and the pane follows', async ($, on) => {
  const clock = mock.clock(on)
  const state = { np: playing() }
  const { calls, statuses, toasts } = fakeEngine(on, state)
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await clock.advance(1000)
  await clock.settle()
  expect(statuses.at(-1)).toBe('> Clocks - Coldplay  1:42 / 5:07')

  for (const surface of SURFACES) {
    state.np = playing()
    await $.command.run({ command: 'jf', args: 'status' })
    const ui = await $.ui.mount(pane(surface, 60))
    expect((await ui.find({ type: 'Text', text: /Clocks/ }))?.text).toBe('Clocks')
    await ui.press({ key: 'next' })
    expect(calls.some(c => c.tool === 'control' && c.args.action === 'next')).toBe(true)
    expect(await ui.find({ type: 'Text', text: /My Universe/ })).toBeDefined()
    await ui.press({ key: 'queue-3' })
    expect(calls).toContainEqual({ tool: 'control', args: { action: 'jump', value: '3' } })
    await ui.press({ key: 'mix' })
    expect(calls.some(c => c.tool === 'play' && c.args.instant_mix === true)).toBe(true)
    await ui.unmount()
  }
  expect(toasts.some(t => t === 'Now playing: My Universe - Coldplay')).toBe(true)
})

test('/jf commands run without the model', async ($, on) => {
  const clock = mock.clock(on)
  const state = { np: playing() }
  const { calls } = fakeEngine(on, state)
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await clock.settle()

  const vol = await $.command.run({ command: 'jf', args: 'vol 40' })
  expect(calls).toContainEqual({ tool: 'control', args: { action: 'volume', value: '40' } })
  expect(vol.text).toBe('Playing: Clocks - Coldplay (1:42 / 5:07)')

  await $.command.run({ command: 'jf', args: 'radiohead ok computer' })
  expect(calls.some(c => c.tool === 'play' && c.args.query === 'radiohead ok computer')).toBe(true)

  await $.command.run({ command: 'jf', args: 'n' })
  expect(calls.some(c => c.tool === 'control' && c.args.action === 'next')).toBe(true)

  const bad = await $.command.run({ command: 'jf', args: 'vol loud' })
  expect(bad.text).toBe('volume needs a number')

  const help = await $.command.run({ command: 'jf', args: 'help' })
  expect(help.text).toContain('/jf <name>')
})

test('the lyric window follows the position', () => {
  expect(lyricWindow(LYRICS, 5)).toEqual({ previous: '', current: '...', next: LYRICS[0].text })
  expect(lyricWindow(LYRICS, 19.8).current).toBe(LYRICS[0].text) // shown just before its time
  expect(lyricWindow(LYRICS, 95)).toEqual({ previous: LYRICS[0].text, current: LYRICS[1].text, next: LYRICS[2].text })
  expect(lyricWindow(LYRICS, 112).current).toBe('') // an instrumental gap
  expect(lyricWindow(LYRICS, 400)).toEqual({ previous: '', current: LYRICS[4].text, next: '' })
  expect(lyricWindow([], 10)).toEqual({ previous: '', current: '...', next: '' })
})

test('synced lyrics stream in the pane, faster polling while they show', async ($, on) => {
  const clock = mock.clock(on)
  const state = { np: playing({ position_sec: 95 }) }
  const { calls } = fakeEngine(on, state)
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await clock.advance(1000)
  await clock.settle()

  for (const surface of SURFACES) {
    const ui = await $.ui.mount(pane(surface, 40))
    expect((await ui.find({ type: 'Text', text: /^Tides/ }))?.props.bold).toBe(true)
    await ui.unmount()
  }

  // The song moves on: the pane follows at the next poll.
  state.np = playing({ position_sec: 101 })
  const before = calls.filter(c => c.tool === 'now_playing').length
  await clock.advance(2000)
  await clock.settle()
  const polls = calls.filter(c => c.tool === 'now_playing').length - before
  expect(polls).toBeGreaterThanOrEqual(4) // every 500 ms while lyrics show

  const ui = await $.ui.mount(pane('terminal', 40))
  const current = await ui.find({ type: 'Text', text: /^Have brought/ })
  expect(current?.props.bold).toBe(true)
  expect(cellWidth(current?.text ?? '')).toBeLessThanOrEqual(40)

  // Hidden: no lyric rows, and polling drops back to once a second.
  await ui.press({ key: 'lyrics' })
  expect(await ui.find({ type: 'Text', text: /^Have brought/ })).toBeUndefined()
  const hidden = calls.filter(c => c.tool === 'now_playing').length
  await clock.advance(2000)
  await clock.settle()
  expect(calls.filter(c => c.tool === 'now_playing').length - hidden).toBeLessThanOrEqual(2)
  await ui.unmount()

  await $.command.run({ command: 'jf', args: 'lyrics on' })
  const printed = await $.command.run({ command: 'jf', args: 'lyrics' })
  expect(printed.text).toContain('Lyrics: Clocks - Coldplay')
  expect(printed.text).toContain('Come out of things unsaid')
})

test('without the permission rule it does not poll and says how to set it up', async ($, on) => {
  const clock = mock.clock(on)
  const { calls } = fakeEngine(on, { np: playing() }, 'ask')
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await clock.advance(30_000)
  await clock.settle()
  expect(calls.filter(c => c.tool === 'now_playing')).toHaveLength(0)

  for (const surface of SURFACES) {
    const ui = await $.ui.mount(pane(surface, 40))
    await ui.drawn()
    expect((await ui.find({ type: 'Text', text: /permissions/ }))?.text).toContain('mcp__plugin_jellyfin_jellyfin')
    await ui.unmount()
  }
})

test('not connected and Quick Connect states', async ($, on) => {
  const clock = mock.clock(on)
  const state = { np: playing({ connected: false, pending_code: '123456', track: track('', '') }) }
  const { statuses } = fakeEngine(on, state)
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await clock.advance(1000)
  await clock.settle()

  for (const surface of SURFACES) {
    const ui = await $.ui.mount(pane(surface, 40))
    expect((await ui.find({ type: 'Text', text: /123456/ }))?.text.length).toBeLessThanOrEqual(40)
    await ui.unmount()
  }
  expect(statuses.at(-1)).toBeUndefined()
})
