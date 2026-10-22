package player

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const afplayBin = "/usr/bin/afplay"

// afplay is the no-install fallback on macOS. afplay only plays local files, so
// each track is downloaded first (the next one is prefetched). Pause/resume
// suspend the process; seeking and live volume aren't possible.
type afplay struct {
	fetch func(ctx context.Context, t Track, dir string) (path string, err error)
	onEnd EndFunc
	dir   string

	mu        sync.Mutex
	volume    int
	cmd       *exec.Cmd
	path      string
	started   time.Time
	paused    bool
	pausedAt  time.Time
	pausedFor time.Duration
	downloads map[string]*download
}

type download struct {
	done chan struct{}
	path string
	err  error
}

func newAfplay(fetch func(context.Context, Track, string) (string, error), onEnd EndFunc) (*afplay, error) {
	dir, err := os.MkdirTemp("", "jellyfin-claude-")
	if err != nil {
		return nil, err
	}
	return &afplay{fetch: fetch, onEnd: onEnd, dir: dir, volume: 100, downloads: map[string]*download{}}, nil
}

func (a *afplay) Name() string { return "afplay" }
func (a *afplay) Caps() Caps   { return Caps{} }

// downloadLocked starts (or joins) the download of t.
func (a *afplay) downloadLocked(t Track) *download {
	if d, ok := a.downloads[t.ID]; ok {
		return d
	}
	d := &download{done: make(chan struct{})}
	a.downloads[t.ID] = d
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		d.path, d.err = a.fetch(ctx, t, a.dir)
		close(d.done)
	}()
	return d
}

func (a *afplay) Play(t Track, gen uint64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.killLocked()

	d := a.downloadLocked(t)
	<-d.done
	if d.err != nil {
		delete(a.downloads, t.ID)
		return d.err
	}

	cmd := exec.Command(afplayBin, "-v", fmt.Sprintf("%.2f", float64(a.volume)/100), d.path)
	if err := cmd.Start(); err != nil {
		return err
	}
	a.cmd, a.path = cmd, d.path
	a.started, a.paused, a.pausedFor = time.Now(), false, 0

	go func() {
		err := cmd.Wait()
		a.mu.Lock()
		mine := a.cmd == cmd
		if mine {
			a.cmd = nil
			a.forgetLocked(t.ID, d.path)
		}
		a.mu.Unlock()
		if !mine {
			return // stopped or replaced by us
		}
		if err != nil {
			err = fmt.Errorf("afplay: %w", err)
		}
		a.onEnd(gen, err)
	}()
	return nil
}

func (a *afplay) forgetLocked(id, path string) {
	delete(a.downloads, id)
	_ = os.Remove(path)
}

// killLocked stops the current afplay process without reporting an end.
func (a *afplay) killLocked() {
	if a.cmd == nil {
		return
	}
	cmd := a.cmd
	a.cmd = nil
	if a.paused {
		_ = cmd.Process.Signal(syscall.SIGCONT)
	}
	_ = cmd.Process.Kill()
	for id, d := range a.downloads {
		if d.path == a.path {
			a.forgetLocked(id, a.path)
		}
	}
}

func (a *afplay) Pause() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cmd == nil {
		return ErrNothingPlaying
	}
	if a.paused {
		return nil
	}
	if err := a.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		return err
	}
	a.paused, a.pausedAt = true, time.Now()
	return nil
}

func (a *afplay) Resume() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cmd == nil {
		return ErrNothingPlaying
	}
	if !a.paused {
		return nil
	}
	if err := a.cmd.Process.Signal(syscall.SIGCONT); err != nil {
		return err
	}
	a.paused = false
	a.pausedFor += time.Since(a.pausedAt)
	return nil
}

func (a *afplay) Stop() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.killLocked()
	return nil
}

func (a *afplay) Seek(float64) error {
	return errors.New("afplay can't seek")
}

func (a *afplay) SetVolume(v int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.volume = v
	return nil
}

func (a *afplay) Position() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cmd == nil {
		return 0
	}
	elapsed := time.Since(a.started) - a.pausedFor
	if a.paused {
		elapsed -= time.Since(a.pausedAt)
	}
	return elapsed.Seconds()
}

func (a *afplay) Prefetch(t Track) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.downloadLocked(t)
}

func (a *afplay) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.killLocked()
	_ = os.RemoveAll(a.dir)
}
