package player

import (
	"errors"
	"slices"
	"sync"
	"testing"
)

type fakeBackend struct {
	mu    sync.Mutex
	plays []string
	fail  map[string]bool
	gen   uint64
	onEnd EndFunc
}

func (f *fakeBackend) Name() string { return "fake" }
func (f *fakeBackend) Caps() Caps   { return Caps{Seek: true, LiveVolume: true} }
func (f *fakeBackend) Play(t Track, gen uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail[t.ID] {
		return errors.New("boom")
	}
	f.plays = append(f.plays, t.ID)
	f.gen = gen
	return nil
}
func (f *fakeBackend) Pause() error        { return nil }
func (f *fakeBackend) Resume() error       { return nil }
func (f *fakeBackend) Stop() error         { return nil }
func (f *fakeBackend) Seek(float64) error  { return nil }
func (f *fakeBackend) SetVolume(int) error { return nil }
func (f *fakeBackend) Position() float64   { return 0 }
func (f *fakeBackend) Prefetch(Track)      {}
func (f *fakeBackend) Close()              {}
func (f *fakeBackend) finish(err error)    { f.onEnd(f.currentGen(), err) }
func (f *fakeBackend) currentGen() uint64  { f.mu.Lock(); defer f.mu.Unlock(); return f.gen }
func (f *fakeBackend) played() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.plays)
}

type nopReporter struct{}

func (nopReporter) Started(Track, int)                         {}
func (nopReporter) Progress(Track, float64, bool, int, string) {}
func (nopReporter) Stopped(Track, float64)                     {}

func newTestPlayer(t *testing.T) (*Player, *fakeBackend) {
	t.Helper()
	fb := &fakeBackend{fail: map[string]bool{}}
	p := New(func(onEnd EndFunc) (Backend, error) { fb.onEnd = onEnd; return fb, nil }, nopReporter{}, 50)
	t.Cleanup(p.Close)
	return p, fb
}

func tracks(ids ...string) []Track {
	out := make([]Track, len(ids))
	for i, id := range ids {
		out[i] = Track{ID: id, Name: id}
	}
	return out
}

func current(p *Player) string {
	if s := p.Status(0); s.Current != nil && s.State != Stopped {
		return s.Current.ID
	}
	return ""
}

func TestAdvanceAndEndOfQueue(t *testing.T) {
	p, fb := newTestPlayer(t)
	if err := p.Enqueue(tracks("a", "b"), ""); err != nil {
		t.Fatal(err)
	}
	fb.finish(nil)
	if got := current(p); got != "b" {
		t.Fatalf("after first track ends: current = %q, want b", got)
	}
	fb.finish(nil)
	if got := current(p); got != "" {
		t.Fatalf("after queue ends: current = %q, want nothing", got)
	}
	// Resume after the queue finished starts over.
	if err := p.Resume(); err != nil || current(p) != "a" {
		t.Fatalf("resume after end: err=%v current=%q", err, current(p))
	}
}

func TestStaleEndIgnored(t *testing.T) {
	p, fb := newTestPlayer(t)
	_ = p.Enqueue(tracks("a", "b", "c"), "")
	stale := fb.currentGen()
	_ = p.Next() // now on b
	fb.onEnd(stale, nil)
	if got := current(p); got != "b" {
		t.Fatalf("stale end moved the queue: current = %q, want b", got)
	}
}

func TestEnqueueModes(t *testing.T) {
	p, fb := newTestPlayer(t)
	_ = p.Enqueue(tracks("a", "b"), "")
	_ = p.Enqueue(tracks("x"), "next")
	_ = p.Enqueue(tracks("z"), "append")
	if got := current(p); got != "a" {
		t.Fatalf("next/append changed the current track to %q", got)
	}
	var order []string
	for range 4 {
		order = append(order, current(p))
		fb.finish(nil)
	}
	if want := []string{"a", "x", "b", "z"}; !slices.Equal(order, want) {
		t.Fatalf("play order = %v, want %v", order, want)
	}
	// With nothing playing, append starts the new tracks.
	_ = p.Enqueue(tracks("n"), "append")
	if got := current(p); got != "n" {
		t.Fatalf("append while idle: current = %q, want n", got)
	}
}

func TestRepeat(t *testing.T) {
	p, fb := newTestPlayer(t)
	_ = p.Enqueue(tracks("a", "b"), "")
	_ = p.SetRepeat(RepeatOne)
	fb.finish(nil)
	if got := current(p); got != "a" {
		t.Fatalf("repeat one: current = %q, want a", got)
	}
	_ = p.SetRepeat(RepeatAll)
	fb.finish(nil)
	fb.finish(nil)
	if got := current(p); got != "a" {
		t.Fatalf("repeat all: current = %q, want a (wrapped)", got)
	}
}

func TestFailuresSkipThenStop(t *testing.T) {
	p, fb := newTestPlayer(t)
	fb.fail["bad"] = true
	if err := p.Enqueue(tracks("bad", "good"), ""); err != nil {
		t.Fatalf("a failing first track should be skipped, got %v", err)
	}
	if got := current(p); got != "good" {
		t.Fatalf("current = %q, want good", got)
	}

	// Tracks that start but then fail mid-play stop playback after a few in a row.
	_ = p.Enqueue(tracks("1", "2", "3", "4", "5"), "")
	for range maxConsecutiveFailures {
		fb.finish(errors.New("stream died"))
	}
	if s := p.Status(0); s.State != Stopped || s.LastErr == "" {
		t.Fatalf("after %d failures: state=%s lastErr=%q, want stopped with an error", maxConsecutiveFailures, s.State, s.LastErr)
	}
	if got := fb.played(); got[len(got)-1] != "3" {
		t.Fatalf("played %v; expected to stop after track 3", got)
	}
}

func TestPreviousAndJump(t *testing.T) {
	p, _ := newTestPlayer(t)
	_ = p.Enqueue(tracks("a", "b", "c"), "")
	_ = p.Jump(2)
	if got := current(p); got != "c" {
		t.Fatalf("jump: current = %q, want c", got)
	}
	_ = p.Previous()
	if got := current(p); got != "b" {
		t.Fatalf("previous: current = %q, want b", got)
	}
	if err := p.Jump(5); err == nil {
		t.Fatal("jump out of range should fail")
	}
	if n := p.ClearUpcoming(); n != 1 {
		t.Fatalf("cleared %d, want 1", n)
	}
}
