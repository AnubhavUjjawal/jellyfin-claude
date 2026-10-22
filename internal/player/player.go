// Package player keeps a play queue and drives a local audio backend (mpv or
// macOS afplay) through it.
package player

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"time"
)

type Track struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Artist    string  `json:"artist,omitempty"`
	Album     string  `json:"album,omitempty"`
	AlbumID   string  `json:"album_id,omitempty"`
	Container string  `json:"-"`
	Duration  float64 `json:"duration_sec,omitempty"`
}

func (t Track) String() string {
	s := t.Name
	if t.Artist != "" {
		s += " - " + t.Artist
	}
	if t.Album != "" {
		s += " (" + t.Album + ")"
	}
	return s
}

// Caps describes what a backend can do mid-track.
type Caps struct {
	Seek       bool
	LiveVolume bool
}

// Backend plays one track at a time. Play replaces whatever is playing. When a
// track ends on its own the backend calls its EndFunc with the gen it was given,
// from its own goroutine.
type Backend interface {
	Name() string
	Caps() Caps
	Play(t Track, gen uint64) error
	Pause() error
	Resume() error
	Stop() error
	Seek(sec float64) error
	SetVolume(v int) error
	Position() float64
	// Prefetch hints that t is likely next; backends may ignore it.
	Prefetch(t Track)
	Close()
}

// EndFunc reports that the track started with gen finished; err is set if it
// failed rather than reaching its end.
type EndFunc func(gen uint64, err error)

// Factory creates the backend lazily, on first playback.
type Factory func(onEnd EndFunc) (Backend, error)

// Reporter is told about playback so the server can track plays. Calls are made
// with the player locked and must not block.
type Reporter interface {
	Started(t Track, volume int)
	Progress(t Track, pos float64, paused bool, volume int, event string)
	Stopped(t Track, pos float64)
}

type State string

const (
	Stopped State = "stopped"
	Playing State = "playing"
	Paused  State = "paused"
)

// Repeat modes.
const (
	RepeatOff = "off"
	RepeatAll = "all"
	RepeatOne = "one"
)

// maxConsecutiveFailures stops playback instead of skipping through the whole
// queue when every track fails (e.g. the server went away).
const maxConsecutiveFailures = 3

var ErrNothingPlaying = errors.New("nothing is playing")

type Player struct {
	mu       sync.Mutex
	factory  Factory
	backend  Backend
	reporter Reporter

	queue    []Track
	index    int
	state    State
	gen      uint64
	volume   int
	repeat   string
	failures int
	lastErr  string

	stop chan struct{}
}

func New(factory Factory, reporter Reporter, volume int) *Player {
	p := &Player{
		factory:  factory,
		reporter: reporter,
		state:    Stopped,
		volume:   clampVolume(volume),
		repeat:   RepeatOff,
		stop:     make(chan struct{}),
	}
	go p.reportProgress()
	return p
}

func (p *Player) backendLocked() (Backend, error) {
	if p.backend == nil {
		b, err := p.factory(p.onEnd)
		if err != nil {
			return nil, err
		}
		_ = b.SetVolume(p.volume)
		p.backend = b
	}
	return p.backend, nil
}

func (p *Player) current() (Track, bool) {
	if p.index < 0 || p.index >= len(p.queue) {
		return Track{}, false
	}
	return p.queue[p.index], true
}

// Enqueue adds tracks. mode "replace" (default) swaps the queue and plays now,
// "next" inserts after the current track and "append" adds to the end; with
// nothing playing, "next" and "append" start playing the new tracks.
func (p *Player) Enqueue(tracks []Track, mode string) error {
	if len(tracks) == 0 {
		return errors.New("no tracks to play")
	}
	tracks = slices.Clone(tracks)
	p.mu.Lock()
	defer p.mu.Unlock()

	_, hasCurrent := p.current()
	idle := p.state == Stopped || !hasCurrent
	switch mode {
	case "", "replace":
		p.stopLocked()
		p.queue, p.index = tracks, 0
		return p.startLocked()
	case "next", "append":
		if idle {
			p.stopLocked()
			p.index = len(p.queue)
			p.queue = append(p.queue, tracks...)
			return p.startLocked()
		}
		at := len(p.queue)
		if mode == "next" {
			at = p.index + 1
		}
		p.queue = slices.Insert(p.queue, at, tracks...)
		if at == p.index+1 && p.backend != nil {
			p.backend.Prefetch(p.queue[at])
		}
		return nil
	default:
		return fmt.Errorf("unknown mode %q (use replace, next or append)", mode)
	}
}

// startLocked plays queue[index], skipping forward past tracks that fail to start.
func (p *Player) startLocked() error {
	b, err := p.backendLocked()
	if err != nil {
		p.state = Stopped
		return err
	}
	var lastErr error
	for tries := 0; tries < maxConsecutiveFailures && p.index < len(p.queue); tries++ {
		t := p.queue[p.index]
		p.gen++
		if err := b.Play(t, p.gen); err != nil {
			lastErr = fmt.Errorf("couldn't play %q: %w", t.Name, err)
			p.lastErr = lastErr.Error()
			p.index++
			continue
		}
		p.state = Playing
		p.lastErr = ""
		p.reporter.Started(t, p.volume)
		if p.index+1 < len(p.queue) {
			b.Prefetch(p.queue[p.index+1])
		}
		return nil
	}
	p.state = Stopped
	if lastErr == nil {
		lastErr = errors.New("reached the end of the queue")
	}
	return lastErr
}

// stopLocked stops the current track (if any) and reports how far it got.
func (p *Player) stopLocked() {
	if p.state == Stopped || p.backend == nil {
		p.state = Stopped
		return
	}
	if t, ok := p.current(); ok {
		p.reporter.Stopped(t, p.backend.Position())
	}
	p.gen++ // ignore any end event racing with this stop
	_ = p.backend.Stop()
	p.state = Stopped
}

func (p *Player) onEnd(gen uint64, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if gen != p.gen || p.state == Stopped {
		return
	}
	t, _ := p.current()
	if err != nil {
		p.failures++
		p.lastErr = fmt.Sprintf("%q failed: %v", t.Name, err)
		p.reporter.Stopped(t, p.backend.Position())
	} else {
		p.failures = 0
		p.reporter.Stopped(t, t.Duration)
	}
	if p.failures >= maxConsecutiveFailures {
		p.state = Stopped
		p.lastErr += fmt.Sprintf(" (stopped after %d failures in a row)", p.failures)
		return
	}
	switch {
	case p.repeat == RepeatOne && err == nil:
	case p.index+1 < len(p.queue):
		p.index++
	case p.repeat == RepeatAll:
		p.index = 0
	default:
		p.state = Stopped
		p.index = len(p.queue)
		return
	}
	_ = p.startLocked()
}

func (p *Player) Pause() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != Playing {
		return ErrNothingPlaying
	}
	if err := p.backend.Pause(); err != nil {
		return err
	}
	p.state = Paused
	t, _ := p.current()
	p.reporter.Progress(t, p.backend.Position(), true, p.volume, "Pause")
	return nil
}

// Resume continues a paused track, or restarts the queue from the current
// position if playback was stopped.
func (p *Player) Resume() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch p.state {
	case Paused:
		if err := p.backend.Resume(); err != nil {
			return err
		}
		p.state = Playing
		t, _ := p.current()
		p.reporter.Progress(t, p.backend.Position(), false, p.volume, "Unpause")
		return nil
	case Stopped:
		if len(p.queue) == 0 {
			return errors.New("the queue is empty")
		}
		if p.index >= len(p.queue) {
			p.index = 0
		}
		p.failures = 0
		return p.startLocked()
	default:
		return nil
	}
}

func (p *Player) Toggle() error {
	p.mu.Lock()
	playing := p.state == Playing
	p.mu.Unlock()
	if playing {
		return p.Pause()
	}
	return p.Resume()
}

func (p *Player) Next() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.index+1 < len(p.queue):
		p.stopLocked()
		p.index++
	case p.repeat == RepeatAll && len(p.queue) > 0:
		p.stopLocked()
		p.index = 0
	default:
		p.stopLocked()
		p.index = len(p.queue)
		return errors.New("that was the last track in the queue")
	}
	p.failures = 0
	return p.startLocked()
}

// Previous restarts the current track if more than a few seconds in, otherwise
// goes back one track.
func (p *Player) Previous() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) == 0 {
		return errors.New("the queue is empty")
	}
	if p.state != Stopped && p.backend.Position() > 3 && p.backend.Caps().Seek {
		return p.backend.Seek(0)
	}
	p.stopLocked()
	if p.index > 0 {
		p.index--
	}
	if p.index >= len(p.queue) {
		p.index = len(p.queue) - 1
	}
	p.failures = 0
	return p.startLocked()
}

func (p *Player) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()
	return nil
}

// Seek moves within the current track; relative seeks are offsets from the
// current position.
func (p *Player) Seek(sec float64, relative bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == Stopped {
		return ErrNothingPlaying
	}
	if !p.backend.Caps().Seek {
		return fmt.Errorf("the %s player can't seek; install mpv (brew install mpv) for seeking", p.backend.Name())
	}
	if relative {
		sec += p.backend.Position()
	}
	return p.backend.Seek(max(sec, 0))
}

// SetVolume sets the volume (0-100). The returned note explains caveats, such as
// a backend that only applies it from the next track.
func (p *Player) SetVolume(v int) (note string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.volume = clampVolume(v)
	if p.backend == nil {
		return "", nil
	}
	if err := p.backend.SetVolume(p.volume); err != nil {
		return "", err
	}
	if !p.backend.Caps().LiveVolume && p.state != Stopped {
		return fmt.Sprintf("the %s player applies volume changes from the next track; install mpv for live volume", p.backend.Name()), nil
	}
	return "", nil
}

func (p *Player) Volume() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.volume
}

// Jump plays the queue entry at index (0-based).
func (p *Player) Jump(index int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if index < 0 || index >= len(p.queue) {
		return fmt.Errorf("queue position %d is out of range (queue has %d tracks)", index+1, len(p.queue))
	}
	p.stopLocked()
	p.index = index
	p.failures = 0
	return p.startLocked()
}

// ClearUpcoming drops everything queued after the current track.
func (p *Player) ClearUpcoming() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	keep := min(p.index+1, len(p.queue))
	removed := len(p.queue) - keep
	p.queue = p.queue[:keep]
	return removed
}

// ShuffleUpcoming shuffles the tracks after the current one.
func (p *Player) ShuffleUpcoming() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.index+1 >= len(p.queue) {
		return
	}
	rest := p.queue[p.index+1:]
	rand.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
	if p.backend != nil {
		p.backend.Prefetch(rest[0])
	}
}

func (p *Player) SetRepeat(mode string) error {
	switch mode {
	case RepeatOff, RepeatAll, RepeatOne:
	default:
		return fmt.Errorf("unknown repeat mode %q (use off, all or one)", mode)
	}
	p.mu.Lock()
	p.repeat = mode
	p.mu.Unlock()
	return nil
}

type Status struct {
	State    State   `json:"state"`
	Backend  string  `json:"backend,omitempty"`
	Caps     Caps    `json:"-"`
	Current  *Track  `json:"current,omitempty"`
	Position float64 `json:"position_sec,omitempty"`
	// Index is the 0-based queue position of Current.
	Index    int     `json:"index"`
	Upcoming []Track `json:"upcoming,omitempty"`
	Total    int     `json:"queue_length"`
	Volume   int     `json:"volume"`
	Repeat   string  `json:"repeat"`
	LastErr  string  `json:"last_error,omitempty"`
}

// Status snapshots playback, listing up to maxUpcoming queued tracks.
func (p *Player) Status(maxUpcoming int) Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Status{State: p.state, Index: p.index, Total: len(p.queue), Volume: p.volume, Repeat: p.repeat, LastErr: p.lastErr}
	if p.backend != nil {
		s.Backend, s.Caps = p.backend.Name(), p.backend.Caps()
	}
	if t, ok := p.current(); ok {
		s.Current = &t
		if p.state != Stopped {
			s.Position = p.backend.Position()
		}
	}
	if p.index+1 < len(p.queue) {
		up := p.queue[p.index+1:]
		s.Upcoming = slices.Clone(up[:min(len(up), maxUpcoming)])
	}
	return s
}

// reportProgress sends periodic position updates while a track plays.
func (p *Player) reportProgress() {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-tick.C:
			p.mu.Lock()
			if t, ok := p.current(); ok && p.state == Playing {
				p.reporter.Progress(t, p.backend.Position(), false, p.volume, "TimeUpdate")
			}
			p.mu.Unlock()
		}
	}
}

// Close stops playback and shuts the backend down.
func (p *Player) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.stop:
		return
	default:
		close(p.stop)
	}
	p.stopLocked()
	if p.backend != nil {
		p.backend.Close()
		p.backend = nil
	}
}

func clampVolume(v int) int { return min(max(v, 0), 100) }
