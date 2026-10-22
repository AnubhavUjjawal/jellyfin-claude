//go:build unix

package main

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"jellyfin-claude/internal/config"
)

// newPlayer returns the shared player, or a player of this server's own when
// JELLYFIN_SHARED_PLAYER=0.
func (a *app) newPlayer() playerAPI {
	if os.Getenv("JELLYFIN_SHARED_PLAYER") == "0" {
		return a.localPlayer()
	}
	return &remotePlayer{a: a}
}

// spawnDaemon starts this binary as the player daemon in a session of its own,
// so it outlives this server and isn't killed with its process group. Its log
// is player.log in the config dir.
func spawnDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, daemonFlag)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if dir, err := config.Dir(); err == nil && os.MkdirAll(dir, 0o700) == nil {
		logPath := filepath.Join(dir, "player.log")
		if st, err := os.Stat(logPath); err == nil && st.Size() > daemonLogMax {
			_ = os.Remove(logPath)
		}
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			defer f.Close()
			cmd.Stderr = f
		}
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // reap it if it exits while this server runs
	return nil
}

// runPlayerDaemon is the --player-daemon process: it owns the one player and
// serves it until it is idle or told to quit. A second daemon that loses the
// race for the lock exits quietly.
func runPlayerDaemon() error {
	sock, err := daemonSocket()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(sock+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if !flockWithin(lock, 3*time.Second) {
		return nil // another daemon is running
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	a := newApp(cfg)
	local := a.localPlayer()
	a.player = local
	defer a.close()

	_ = os.Remove(sock) // left by a daemon that crashed: the lock says none runs
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer os.Remove(sock)
	defer ln.Close()
	if err := os.Chmod(sock, 0o600); err != nil {
		return err
	}

	done := make(chan struct{})
	var once sync.Once
	quit := func() { once.Do(func() { close(done) }) }
	signal.Ignore(syscall.SIGHUP)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		quit()
	}()
	serveDaemon(ln, &playerService{a: a, p: local, quit: quit}, done, daemonIdleGrace)
	return nil
}

// flockWithin takes an exclusive lock on f, giving a daemon that is shutting
// down a moment to let go of it.
func flockWithin(f *os.File, wait time.Duration) bool {
	for deadline := time.Now().Add(wait); ; time.Sleep(100 * time.Millisecond) {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return true
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			return false
		}
	}
}
