package player

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

const mpvRequestTimeout = 5 * time.Second

// mpvLoadWait is how long Play waits for mpv to open the stream, so bad URLs
// or auth failures surface as Play errors instead of silent skips.
const mpvLoadWait = 10 * time.Second

// mpv drives a headless mpv process over its JSON IPC socket. It streams the
// original file straight from the server, so it can seek and change volume live.
type mpv struct {
	bin   string
	url   func(Track) (url, authHeader string)
	onEnd EndFunc

	// mu serialises commands and guards the process fields.
	mu      sync.Mutex
	cmd     *exec.Cmd
	conn    net.Conn
	sock    string
	exited  chan struct{}
	closing bool
	volume  int

	// reqMu guards request bookkeeping; the reader goroutine only takes reqMu
	// and smu, never mu, so a command waiting for its reply can't deadlock it.
	reqMu   sync.Mutex
	nextID  int64
	pending map[int64]chan mpvMsg

	// smu guards the state of the file being played.
	smu       sync.Mutex
	gen       uint64
	entry     int64 // mpv playlist_entry_id of the current file, -1 if unknown
	prevEntry int64
	playing   bool
	loaded    chan error // receives the outcome of the pending load, if any
}

type mpvMsg struct {
	Error           string          `json:"error"`
	Data            json.RawMessage `json:"data"`
	RequestID       int64           `json:"request_id"`
	Event           string          `json:"event"`
	Reason          string          `json:"reason"`
	FileError       string          `json:"file_error"`
	PlaylistEntryID int64           `json:"playlist_entry_id"`
}

func newMPV(bin string, url func(Track) (string, string), onEnd EndFunc) *mpv {
	return &mpv{bin: bin, url: url, onEnd: onEnd, volume: 100, entry: -1, prevEntry: -1}
}

func (m *mpv) Name() string { return "mpv" }
func (m *mpv) Caps() Caps   { return Caps{Seek: true, LiveVolume: true} }

func (m *mpv) running() bool {
	if m.cmd == nil {
		return false
	}
	select {
	case <-m.exited:
		return false
	default:
		return true
	}
}

func (m *mpv) start() error {
	if m.running() {
		return nil
	}
	m.sock = filepath.Join(os.TempDir(), fmt.Sprintf("jellyfin-claude-mpv-%d.sock", os.Getpid()))
	_ = os.Remove(m.sock)
	cmd := exec.Command(m.bin,
		"--idle=yes",
		"--no-video",
		"--no-terminal",
		"--force-window=no",
		"--audio-display=no",
		"--ytdl=no",
		// Override user config that would stop end-of-file from advancing the queue.
		"--keep-open=no",
		"--loop-file=no",
		"--loop-playlist=no",
		"--input-ipc-server="+m.sock,
		fmt.Sprintf("--volume=%d", m.volume),
	)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting mpv: %w", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	var conn net.Conn
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial("unix", m.sock)
		if err == nil {
			conn = c
			break
		}
		select {
		case <-exited:
			return errors.New("mpv exited during startup")
		default:
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			return fmt.Errorf("connecting to mpv: %w", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	m.cmd, m.conn, m.exited = cmd, conn, exited
	m.reqMu.Lock()
	m.pending = map[int64]chan mpvMsg{}
	m.reqMu.Unlock()
	go m.read(conn)
	go m.watch(cmd, exited)
	return nil
}

// watch reports an unexpected mpv exit mid-track as a track failure, so the
// player moves on (and restarts mpv for the next track).
func (m *mpv) watch(cmd *exec.Cmd, exited chan struct{}) {
	<-exited
	m.mu.Lock()
	closing := m.closing
	m.mu.Unlock()
	m.smu.Lock()
	wasPlaying, gen := m.playing, m.gen
	m.playing = false
	m.smu.Unlock()
	if wasPlaying && !closing {
		m.onEnd(gen, errors.New("mpv exited unexpectedly"))
	}
}

func (m *mpv) read(conn net.Conn) {
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var msg mpvMsg
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		if msg.Event != "" {
			m.handleEvent(msg)
			continue
		}
		m.reqMu.Lock()
		ch := m.pending[msg.RequestID]
		delete(m.pending, msg.RequestID)
		m.reqMu.Unlock()
		if ch != nil {
			ch <- msg
		}
	}
	// Connection gone: fail anything still waiting.
	m.reqMu.Lock()
	for id, ch := range m.pending {
		ch <- mpvMsg{Error: "mpv connection closed", RequestID: id}
		delete(m.pending, id)
	}
	m.reqMu.Unlock()
}

func (m *mpv) handleEvent(ev mpvMsg) {
	m.smu.Lock()
	defer m.smu.Unlock()
	switch ev.Event {
	case "file-loaded":
		if m.loaded != nil {
			m.loaded <- nil
			m.loaded = nil
		}
	case "end-file":
		if ev.PlaylistEntryID != 0 && (ev.PlaylistEntryID == m.prevEntry || (m.entry != -1 && ev.PlaylistEntryID != m.entry)) {
			return // a previous file being replaced
		}
		var err error
		switch ev.Reason {
		case "eof":
		case "error":
			err = fmt.Errorf("mpv couldn't play the stream: %s", ev.FileError)
		default:
			return // stop/quit/redirect are our own doing
		}
		if m.loaded != nil {
			if err == nil {
				err = errors.New("stream ended before it started")
			}
			m.loaded <- err
			m.loaded = nil
			m.playing = false
			return
		}
		if m.playing {
			m.playing = false
			go m.onEnd(m.gen, err)
		}
	}
}

// request sends one IPC command and waits for its reply. Callers hold m.mu.
func (m *mpv) request(args ...any) (json.RawMessage, error) {
	if !m.running() {
		return nil, errors.New("mpv is not running")
	}
	ch := make(chan mpvMsg, 1)
	m.reqMu.Lock()
	m.nextID++
	id := m.nextID
	m.pending[id] = ch
	m.reqMu.Unlock()

	b, err := json.Marshal(map[string]any{"command": args, "request_id": id})
	if err != nil {
		return nil, err
	}
	_ = m.conn.SetWriteDeadline(time.Now().Add(mpvRequestTimeout))
	if _, err := m.conn.Write(append(b, '\n')); err != nil {
		m.reqMu.Lock()
		delete(m.pending, id)
		m.reqMu.Unlock()
		return nil, err
	}
	select {
	case msg := <-ch:
		if msg.Error != "success" {
			return nil, fmt.Errorf("mpv %v: %s", args[0], msg.Error)
		}
		return msg.Data, nil
	case <-time.After(mpvRequestTimeout):
		m.reqMu.Lock()
		delete(m.pending, id)
		m.reqMu.Unlock()
		return nil, fmt.Errorf("mpv %v: timed out", args[0])
	}
}

func (m *mpv) Play(t Track, gen uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.start(); err != nil {
		return err
	}
	u, auth := m.url(t)
	if _, err := m.request("set_property", "http-header-fields", []string{"Authorization: " + auth}); err != nil {
		return err
	}

	loaded := make(chan error, 1)
	m.smu.Lock()
	m.gen, m.prevEntry, m.entry = gen, m.entry, -1
	m.playing, m.loaded = true, loaded
	m.smu.Unlock()

	data, err := m.request("loadfile", u, "replace")
	if err != nil {
		m.smu.Lock()
		m.playing, m.loaded = false, nil
		m.smu.Unlock()
		return err
	}
	var res struct {
		PlaylistEntryID int64 `json:"playlist_entry_id"`
	}
	if json.Unmarshal(data, &res) == nil && res.PlaylistEntryID != 0 {
		m.smu.Lock()
		m.entry = res.PlaylistEntryID
		m.smu.Unlock()
	}
	if _, err := m.request("set_property", "pause", false); err != nil {
		return err
	}

	select {
	case err := <-loaded:
		return err
	case <-time.After(mpvLoadWait):
		// Slow server; keep going and let a later failure surface via onEnd.
		m.smu.Lock()
		m.loaded = nil
		m.smu.Unlock()
		return nil
	}
}

func (m *mpv) setPause(p bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := m.request("set_property", "pause", p)
	return err
}

func (m *mpv) Pause() error  { return m.setPause(true) }
func (m *mpv) Resume() error { return m.setPause(false) }

func (m *mpv) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.smu.Lock()
	m.playing = false
	m.smu.Unlock()
	if !m.running() {
		return nil
	}
	_, err := m.request("stop")
	return err
}

func (m *mpv) Seek(sec float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := m.request("seek", sec, "absolute")
	return err
}

func (m *mpv) SetVolume(v int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.volume = v
	if !m.running() {
		return nil
	}
	_, err := m.request("set_property", "volume", v)
	return err
}

func (m *mpv) Position() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running() {
		return 0
	}
	data, err := m.request("get_property", "time-pos")
	if err != nil {
		return 0
	}
	var pos float64
	_ = json.Unmarshal(data, &pos)
	return pos
}

func (m *mpv) Prefetch(Track) {}

func (m *mpv) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closing = true
	if !m.running() {
		return
	}
	_, _ = m.request("quit")
	select {
	case <-m.exited:
	case <-time.After(2 * time.Second):
		_ = m.cmd.Process.Kill()
	}
	_ = m.conn.Close()
	_ = os.Remove(m.sock)
}
