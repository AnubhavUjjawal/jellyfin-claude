// Width-safe text helpers for the player pane. Everything drawn is measured in
// terminal cells and cut to fit before it reaches the surface, so a long or
// wide (CJK, emoji) title can never push past the pane's frame.

const ZERO_WIDTH: [number, number][] = [
  [0x0300, 0x036f], // combining diacritics
  [0x200b, 0x200f], // zero-width space, joiners, marks
  [0x20d0, 0x20ff], // combining marks for symbols
  [0xfe00, 0xfe0f], // variation selectors
  [0xe0100, 0xe01ef],
]

// Wide (two-cell) ranges: East Asian wide/fullwidth and emoji. Symbols whose
// width terminals disagree on are counted as wide, so text is cut a little
// early rather than overflowing.
const WIDE: [number, number][] = [
  [0x1100, 0x115f],
  [0x231a, 0x231b],
  [0x2329, 0x232a],
  [0x23e9, 0x23fa],
  [0x25fd, 0x25fe],
  [0x2600, 0x27bf],
  [0x2b1b, 0x2b1c],
  [0x2b50, 0x2b55],
  [0x2e80, 0x303e],
  [0x3041, 0x33ff],
  [0x3400, 0x4dbf],
  [0x4e00, 0x9fff],
  [0xa000, 0xa4cf],
  [0xa960, 0xa97f],
  [0xac00, 0xd7a3],
  [0xf900, 0xfaff],
  [0xfe10, 0xfe19],
  [0xfe30, 0xfe6f],
  [0xff00, 0xff60],
  [0xffe0, 0xffe6],
  [0x1f000, 0x1faff],
  [0x20000, 0x3fffd],
]

function inRanges(cp: number, ranges: [number, number][]): boolean {
  for (const [lo, hi] of ranges) {
    if (cp >= lo && cp <= hi) return true
  }
  return false
}

export function charWidth(cp: number): number {
  if (cp < 0x20 || (cp >= 0x7f && cp < 0xa0)) return 0
  if (inRanges(cp, ZERO_WIDTH)) return 0
  return inRanges(cp, WIDE) ? 2 : 1
}

export function cellWidth(s: string): number {
  let w = 0
  for (const ch of s) w += charWidth(ch.codePointAt(0) ?? 0)
  return w
}

/** Cuts s to at most cols cells, ending in an ellipsis when it was cut. */
export function fit(s: string, cols: number): string {
  const clean = s.replace(/[\u0000-\u001f\u007f-\u009f]/g, ' ')
  if (cols <= 0) return ''
  if (cellWidth(clean) <= cols) return clean
  let out = ''
  let w = 0
  for (const ch of clean) {
    const cw = charWidth(ch.codePointAt(0) ?? 0)
    if (w + cw > cols - 1) break
    out += ch
    w += cw
  }
  return out + '…'
}

export function fmtTime(sec: number): string {
  const s = Math.max(0, Math.round(sec))
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const ss = String(s % 60).padStart(2, '0')
  return h > 0 ? `${h}:${String(m).padStart(2, '0')}:${ss}` : `${m}:${ss}`
}

/** A bar exactly cols cells wide, filled in proportion to pos / dur. */
export function progressBar(pos: number, dur: number, cols: number): string {
  if (cols <= 0) return ''
  const ratio = dur > 0 ? Math.min(1, Math.max(0, pos / dur)) : 0
  const filled = Math.round(ratio * cols)
  return '━'.repeat(filled) + '─'.repeat(cols - filled)
}

/** "1:42 / 5:07", or just the position when the length is unknown. */
export function timeLabel(pos: number, dur: number): string {
  return dur > 0 ? `${fmtTime(pos)} / ${fmtTime(dur)}` : fmtTime(pos)
}

/** The progress line: bar plus time, never wider than cols. */
export function progressLine(pos: number, dur: number, cols: number): string {
  const time = timeLabel(pos, dur)
  const barCols = cols - cellWidth(time) - 1
  if (barCols < 4) return fit(time, cols)
  return `${progressBar(pos, dur, barCols)} ${time}`
}

/** The one-line status pinned under the prompt. */
export function statusText(np: { state: string; track: { id: string; name: string; artist: string; duration_sec: number }; position_sec: number }): string | undefined {
  if (np.state === 'stopped' || !np.track.id) return undefined
  const mark = np.state === 'paused' ? '||' : '>'
  const who = np.track.artist ? `${np.track.name} - ${np.track.artist}` : np.track.name
  return `${mark} ${fit(who, 60)}  ${timeLabel(np.position_sec, np.track.duration_sec)}`
}
