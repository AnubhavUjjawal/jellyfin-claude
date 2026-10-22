//go:build unix

package main

import (
	"net"
	"sync"
	"testing"
	"time"

	"jellyfin-claude/internal/config"
	"jellyfin-claude/internal/jellyfin"
	"jellyfin-claude/internal/player"
)

type silentBackend struct{}

func (silentBackend) Name() string                    { return "silent" }
func (silentBackend) Caps() player.Caps               { return player.Caps{Seek: true, LiveVolume: true} }
func (silentBackend) Play(player.Track, uint64) error { return nil }
func (silentBackend) Pause() error                    { return nil }
func (silentBackend) Resume() error                   { return nil }
func (silentBackend) Stop() error                     { return nil }
func (silentBackend) Seek(float64) error              { return nil }
func (silentBackend) SetVolume(int) error             { return nil }
func (silentBackend) Position() float64               { return 0 }
func (silentBackend) Prefetch(player.Track)           {}
func (silentBackend) Close()                          {}

// startTestDaemon serves a silent player on this test's daemon socket.
func startTestDaemon(t *testing.T, grace time.Duration) (*app, <-chan struct{}) {
	t.Helper()
	t.Setenv("JELLYFIN_CLAUDE_CONFIG_DIR", t.TempDir())
	sock, err := daemonSocket()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	a := &app{cfg: &config.Config{}}
	p := player.New(func(player.EndFunc) (player.Backend, error) { return silentBackend{}, nil }, &reporter{a: a}, 50)
	a.player = p
	t.Cleanup(p.Close)

	done := make(chan struct{})
	stopped := make(chan struct{})
	var once sync.Once
	quit := func() { once.Do(func() { close(done) }) }
	go func() {
		serveDaemon(ln, &playerService{a: a, p: p, quit: quit}, done, grace)
		close(stopped)
	}()
	t.Cleanup(quit)
	return a, stopped
}

// A session that moves to a new process gets a new MCP server; it must see and
// control the music the old server started.
func TestSharedPlayerAcrossServers(t *testing.T) {
	daemon, _ := startTestDaemon(t, daemonIdleGrace)

	oldServer := &app{cfg: &config.Config{}, client: &jellyfin.Client{BaseURL: "http://jf:8096", Token: "tok", UserID: "u1"}}
	oldServer.player = &remotePlayer{a: oldServer}
	newServer := &app{cfg: &config.Config{}}
	newServer.player = &remotePlayer{a: newServer}
	defer newServer.player.Close()

	queue := []player.Track{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}, {ID: "c", Name: "C"}}
	if err := oldServer.player.Enqueue(queue, "replace"); err != nil {
		t.Fatal(err)
	}
	if c := daemon.connected(); c == nil || c.Token != "tok" || c.UserID != "u1" {
		t.Fatalf("daemon login = %+v, want the enqueuer's", c)
	}

	s := newServer.player.Status(5)
	if s.LastErr != "" || s.State != player.Playing || s.Current == nil || s.Current.ID != "a" || len(s.Upcoming) != 2 {
		t.Fatalf("new server sees %+v, want A playing with 2 upcoming", s)
	}

	oldServer.player.Close() // the old process lets go; the music plays on
	if err := newServer.player.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := newServer.player.SetVolume(70); err != nil {
		t.Fatal(err)
	}
	s = newServer.player.Status(5)
	if s.Current == nil || s.Current.ID != "b" || s.Volume != 70 || newServer.player.Volume() != 70 {
		t.Fatalf("after next and volume: %+v", s)
	}
	if n := newServer.player.ClearUpcoming(); n != 1 {
		t.Fatalf("cleared %d upcoming, want 1", n)
	}
	if err := newServer.player.SetRepeat("sideways"); err == nil {
		t.Fatal("a bad repeat mode should fail through the daemon")
	}
}

func TestDaemonQuitsWhenIdle(t *testing.T) {
	const grace = 100 * time.Millisecond
	_, stopped := startTestDaemon(t, grace)

	server := &app{cfg: &config.Config{}}
	r := &remotePlayer{a: server}
	if s := r.Status(0); s.LastErr != "" {
		t.Fatal(s.LastErr)
	}
	time.Sleep(3 * grace)
	select {
	case <-stopped:
		t.Fatal("the daemon quit while a server was connected")
	default:
	}

	r.Close()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the daemon kept running with no server connected")
	}
}
