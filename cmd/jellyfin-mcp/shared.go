package main

// The player is shared by every jellyfin-mcp on the machine. Claude Code can
// move a session to a new process (and with it start a new MCP server) while
// music plays; a server with its own player would then neither see nor control
// what the old one is playing. So the first server that needs the player starts
// a detached player daemon (this binary with --player-daemon) and every server
// drives it over a Unix socket. The daemon exits once no server has been
// connected to it for daemonIdleGrace.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"sync"
	"time"

	"jellyfin-claude/internal/config"
	"jellyfin-claude/internal/player"
)

const (
	daemonFlag = "--player-daemon"
	// daemonProto changes whenever the RPC types below do; a server replaces an
	// older daemon and refuses a newer one.
	daemonProto = 1
	// daemonIdleGrace covers a session moving to a new process: the old server
	// may let go of the daemon a little before the new one first calls it.
	daemonIdleGrace = 15 * time.Second
	daemonStartWait = 5 * time.Second
	daemonLogMax    = 1 << 20
)

// playerAPI is what the tools need from the player, local or shared.
type playerAPI interface {
	Enqueue(tracks []player.Track, mode string) error
	Status(maxUpcoming int) player.Status
	Pause() error
	Resume() error
	Toggle() error
	Next() error
	Previous() error
	Stop() error
	Seek(sec float64, relative bool) error
	SetVolume(v int) (note string, err error)
	Volume() int
	Jump(index int) error
	ClearUpcoming() int
	ShuffleUpcoming()
	SetRepeat(mode string) error
	Close()
}

// daemonSocket is player.sock in the config dir, so a separate config dir (as
// in tests) gets its own player. Socket paths are limited to about 104 bytes,
// so a long config dir falls back to a short path keyed by it.
func daemonSocket() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "player.sock")
	if len(p) < 100 {
		return p, nil
	}
	sum := sha256.Sum256([]byte(dir))
	return filepath.Join(os.TempDir(), "jellyfin-claude-"+hex.EncodeToString(sum[:6])+".sock"), nil
}

// --- RPC: the daemon's side ---

// The RPC types are exported because net/rpc skips methods with unexported
// argument types.

type RPCLogin struct {
	BaseURL, Token, UserID string
}

type RPCEnqueue struct {
	Login  RPCLogin
	Tracks []player.Track
	Mode   string
}

type RPCControl struct {
	Action   string
	Num      float64
	Relative bool
	Text     string
}

type RPCReply struct {
	Num  int
	Note string
}

type playerService struct {
	a    *app
	p    *player.Player
	quit func()
}

func (s *playerService) Hello(_ int, out *RPCReply) error {
	out.Num = daemonProto
	return nil
}

// Enqueue carries the caller's login: the daemon streams and reports plays with
// the credentials of whoever last queued music.
func (s *playerService) Enqueue(in RPCEnqueue, _ *RPCReply) error {
	s.a.useLogin(in.Login)
	return s.p.Enqueue(in.Tracks, in.Mode)
}

func (s *playerService) Status(maxUpcoming int, out *player.Status) error {
	*out = s.p.Status(maxUpcoming)
	return nil
}

func (s *playerService) Control(in RPCControl, out *RPCReply) error {
	var err error
	switch in.Action {
	case "pause":
		err = s.p.Pause()
	case "resume":
		err = s.p.Resume()
	case "toggle":
		err = s.p.Toggle()
	case "next":
		err = s.p.Next()
	case "previous":
		err = s.p.Previous()
	case "stop":
		err = s.p.Stop()
	case "seek":
		err = s.p.Seek(in.Num, in.Relative)
	case "volume":
		out.Note, err = s.p.SetVolume(int(in.Num))
	case "get-volume":
		out.Num = s.p.Volume()
	case "jump":
		err = s.p.Jump(int(in.Num))
	case "clear":
		out.Num = s.p.ClearUpcoming()
	case "shuffle":
		s.p.ShuffleUpcoming()
	case "repeat":
		err = s.p.SetRepeat(in.Text)
	case "quit":
		go s.quit()
	default:
		err = fmt.Errorf("unknown player action %q", in.Action)
	}
	return err
}

// useLogin points the daemon's client at the caller's login, if it differs.
func (a *app) useLogin(l RPCLogin) {
	if l.Token == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil && a.client.BaseURL == l.BaseURL && a.client.Token == l.Token && a.client.UserID == l.UserID {
		return
	}
	c := a.newClient(l.BaseURL)
	c.Token, c.UserID = l.Token, l.UserID
	a.client = c
}

// serveDaemon answers RPCs on ln until done closes. It calls svc.quit once no
// server has been connected for grace.
func serveDaemon(ln net.Listener, svc *playerService, done <-chan struct{}, grace time.Duration) {
	srv := rpc.NewServer()
	if err := srv.RegisterName("Player", svc); err != nil {
		panic(err) // the service's method set is fixed: a programming error
	}
	var mu sync.Mutex
	conns := 0
	idle := time.AfterFunc(grace, svc.quit)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				svc.quit()
				return
			}
			mu.Lock()
			conns++
			idle.Stop()
			mu.Unlock()
			go func() {
				srv.ServeConn(c)
				mu.Lock()
				conns--
				if conns == 0 {
					idle.Reset(grace)
				}
				mu.Unlock()
			}()
		}
	}()
	<-done
	idle.Stop()
}

// --- RPC: a server's side ---

// remotePlayer drives the shared daemon, starting it on first use. Connecting
// lazily keeps MCP servers that never touch the player (such as ones Claude
// Code starts ahead of time) from holding the daemon open.
type remotePlayer struct {
	a      *app
	mu     sync.Mutex
	client *rpc.Client
}

func (r *remotePlayer) conn() (*rpc.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client == nil {
		c, err := dialDaemon()
		if err != nil {
			return nil, err
		}
		r.client = c
	}
	return r.client, nil
}

func (r *remotePlayer) drop(c *rpc.Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client == c {
		_ = c.Close()
		r.client = nil
	}
}

// call runs one RPC, retrying once on a fresh daemon if the connection broke
// (say, the daemon exited between two calls).
func (r *remotePlayer) call(method string, args, reply any) error {
	for attempt := 0; ; attempt++ {
		c, err := r.conn()
		if err != nil {
			return err
		}
		err = c.Call("Player."+method, args, reply)
		var remote rpc.ServerError
		if err == nil || errors.As(err, &remote) {
			return err
		}
		r.drop(c)
		if attempt > 0 {
			return fmt.Errorf("lost the player: %w", err)
		}
	}
}

func (r *remotePlayer) control(in RPCControl) (RPCReply, error) {
	var out RPCReply
	err := r.call("Control", in, &out)
	return out, err
}

func (r *remotePlayer) act(action string) error {
	_, err := r.control(RPCControl{Action: action})
	return err
}

func (r *remotePlayer) Enqueue(tracks []player.Track, mode string) error {
	in := RPCEnqueue{Tracks: tracks, Mode: mode}
	if c := r.a.connected(); c != nil {
		in.Login = RPCLogin{BaseURL: c.BaseURL, Token: c.Token, UserID: c.UserID}
	}
	return r.call("Enqueue", in, &RPCReply{})
}

// Status reports an unreachable player as stopped, with the reason as its error.
func (r *remotePlayer) Status(maxUpcoming int) player.Status {
	var s player.Status
	if err := r.call("Status", maxUpcoming, &s); err != nil {
		return player.Status{State: player.Stopped, Repeat: player.RepeatOff, LastErr: err.Error()}
	}
	return s
}

func (r *remotePlayer) Pause() error     { return r.act("pause") }
func (r *remotePlayer) Resume() error    { return r.act("resume") }
func (r *remotePlayer) Toggle() error    { return r.act("toggle") }
func (r *remotePlayer) Next() error      { return r.act("next") }
func (r *remotePlayer) Previous() error  { return r.act("previous") }
func (r *remotePlayer) Stop() error      { return r.act("stop") }
func (r *remotePlayer) ShuffleUpcoming() { _ = r.act("shuffle") }

func (r *remotePlayer) Seek(sec float64, relative bool) error {
	_, err := r.control(RPCControl{Action: "seek", Num: sec, Relative: relative})
	return err
}

func (r *remotePlayer) SetVolume(v int) (string, error) {
	out, err := r.control(RPCControl{Action: "volume", Num: float64(v)})
	return out.Note, err
}

func (r *remotePlayer) Volume() int {
	out, _ := r.control(RPCControl{Action: "get-volume"})
	return out.Num
}

func (r *remotePlayer) Jump(index int) error {
	_, err := r.control(RPCControl{Action: "jump", Num: float64(index)})
	return err
}

func (r *remotePlayer) ClearUpcoming() int {
	out, _ := r.control(RPCControl{Action: "clear"})
	return out.Num
}

func (r *remotePlayer) SetRepeat(mode string) error {
	_, err := r.control(RPCControl{Action: "repeat", Text: mode})
	return err
}

// Close lets go of the daemon; the music plays on while other servers use it.
func (r *remotePlayer) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client != nil {
		_ = r.client.Close()
		r.client = nil
	}
}

// dialDaemon connects to the shared player, starting it if none answers, and
// replaces a daemon left running by an older version of this plugin.
func dialDaemon() (*rpc.Client, error) {
	sock, err := daemonSocket()
	if err != nil {
		return nil, err
	}
	for replaced := false; ; replaced = true {
		conn, err := net.Dial("unix", sock)
		if err != nil {
			if conn, err = startDaemon(sock); err != nil {
				return nil, err
			}
		}
		c := rpc.NewClient(conn)
		var hello RPCReply
		err = c.Call("Player.Hello", 0, &hello)
		switch {
		case err == nil && hello.Num == daemonProto:
			return c, nil
		case err == nil && hello.Num > daemonProto:
			_ = c.Close()
			return nil, errors.New("a newer version of this plugin is running the player: restart this Claude session to update")
		case replaced:
			_ = c.Close()
			return nil, fmt.Errorf("the player daemon didn't answer: %v", err)
		}
		// Older (or broken) daemon: ask it to quit, wait for it to go, start ours.
		_ = c.Call("Player.Control", RPCControl{Action: "quit"}, &RPCReply{})
		_ = c.Close()
		for deadline := time.Now().Add(daemonStartWait); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if _, err := os.Stat(sock); err != nil {
				break
			}
		}
	}
}

// startDaemon launches the player daemon and waits for its socket.
func startDaemon(sock string) (net.Conn, error) {
	if err := spawnDaemon(); err != nil {
		return nil, fmt.Errorf("starting the player: %w", err)
	}
	var err error
	for deadline := time.Now().Add(daemonStartWait); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		var conn net.Conn
		if conn, err = net.Dial("unix", sock); err == nil {
			return conn, nil
		}
	}
	return nil, fmt.Errorf("the player didn't start: %w", err)
}
