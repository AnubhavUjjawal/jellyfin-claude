// Command jellyfin-mcp is an MCP server that plays music from a Jellyfin server
// on the local machine (via mpv, or afplay on macOS) under Claude's direction.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"jellyfin-claude/internal/config"
	"jellyfin-claude/internal/jellyfin"
	"jellyfin-claude/internal/player"
)

const instructions = `Plays music from the user's Jellyfin server on this computer's speakers (no Jellyfin app needed).
Start with status. If not connected: discover_servers, then connect with the found address. connect uses Quick Connect by default: tell the user the code and to approve it in Jellyfin (profile menu > Quick Connect), then call status to confirm.
To play: search (or browse) to get ids, then play them. play accepts tracks, albums, artists, playlists and genres; use instant_mix=true for "more like this". Use control for pause/skip/volume/seek.
Keep replies short and conversational; don't show item ids to the user.`

func main() {
	log.SetFlags(0)
	log.SetPrefix("jellyfin-mcp: ")
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println(jellyfin.Version)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	a := newApp(cfg)
	defer a.close()

	server := mcp.NewServer(&mcp.Implementation{Name: "jellyfin", Title: "Jellyfin music", Version: jellyfin.Version},
		&mcp.ServerOptions{Instructions: instructions})
	a.registerTools(server)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("server stopped: %v", err)
	}
}

type app struct {
	mu      sync.Mutex
	cfg     *config.Config
	client  *jellyfin.Client // nil until logged in
	pending *pendingLogin
	device  string

	playerOpts player.Options
	player     *player.Player
}

// pendingLogin is a Quick Connect request waiting for the user's approval.
type pendingLogin struct {
	server string
	code   string
	err    string
	cancel context.CancelFunc
}

func newApp(cfg *config.Config) *app {
	a := &app{cfg: cfg, device: deviceName()}

	switch {
	case os.Getenv("JELLYFIN_URL") != "" && os.Getenv("JELLYFIN_API_KEY") != "":
		// API key mode: the user is resolved on first use.
		base, err := jellyfin.NormalizeURL(os.Getenv("JELLYFIN_URL"))
		if err != nil {
			log.Printf("ignoring JELLYFIN_URL: %v", err)
			break
		}
		a.client = a.newClient(base)
		a.client.Token = os.Getenv("JELLYFIN_API_KEY")
		a.client.UserID = os.Getenv("JELLYFIN_USER_ID")
	case cfg.AccessToken != "":
		a.client = a.newClient(cfg.ServerURL)
		a.client.Token, a.client.UserID = cfg.AccessToken, cfg.UserID
	}

	volume := 100
	if v, err := parseInt(os.Getenv("JELLYFIN_VOLUME")); err == nil {
		volume = v
	}
	a.playerOpts = player.Options{
		Backend:   os.Getenv("JELLYFIN_PLAYER"),
		MPVPath:   os.Getenv("MPV_PATH"),
		StreamURL: a.streamURL,
		Fetch:     a.fetchForAfplay,
	}
	a.player = player.New(player.NewFactory(a.playerOpts), &reporter{a: a}, volume)
	return a
}

func (a *app) close() {
	a.player.Close()
	a.mu.Lock()
	if a.pending != nil {
		a.pending.cancel()
	}
	a.mu.Unlock()
}

func (a *app) newClient(base string) *jellyfin.Client {
	return &jellyfin.Client{BaseURL: base, DeviceID: a.cfg.DeviceID, Device: a.device}
}

// conn returns a snapshot of the logged-in client, resolving the user for API
// key logins, or explains how to connect.
func (a *app) conn(ctx context.Context) (*jellyfin.Client, error) {
	a.mu.Lock()
	if a.client == nil {
		defer a.mu.Unlock()
		if a.pending != nil && a.pending.err == "" {
			return nil, fmt.Errorf("still waiting for Quick Connect approval: ask the user to enter code %s in Jellyfin (profile menu > Quick Connect)", a.pending.code)
		}
		return nil, errors.New("not connected to a Jellyfin server: use discover_servers, then connect")
	}
	c := *a.client
	a.mu.Unlock()
	if c.UserID != "" {
		return &c, nil
	}

	users, err := c.Users(ctx)
	if err != nil {
		return nil, fmt.Errorf("looking up Jellyfin users for the API key: %w", err)
	}
	want := os.Getenv("JELLYFIN_USERNAME")
	for _, u := range users {
		if (want != "" && strings.EqualFold(u.Name, want)) || (want == "" && u.Policy.IsAdministrator) {
			c.UserID = u.ID
			break
		}
	}
	if c.UserID == "" {
		return nil, errors.New("couldn't pick a Jellyfin user for the API key: set JELLYFIN_USERNAME or JELLYFIN_USER_ID")
	}
	a.mu.Lock()
	if a.client != nil && a.client.Token == c.Token {
		a.client.UserID = c.UserID
	}
	a.mu.Unlock()
	return &c, nil
}

// connected returns the current client without network calls (nil if none);
// used by the player, which must not block.
func (a *app) connected() *jellyfin.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client == nil {
		return nil
	}
	c := *a.client
	return &c
}

func (a *app) finishLogin(c *jellyfin.Client, serverName string, res *jellyfin.AuthResult) error {
	a.player.Stop()
	a.mu.Lock()
	defer a.mu.Unlock()
	c.Token, c.UserID = res.AccessToken, res.User.ID
	a.client = c
	a.pending = nil
	a.cfg.ServerURL, a.cfg.ServerName, a.cfg.ServerID = c.BaseURL, serverName, res.ServerID
	a.cfg.UserID, a.cfg.UserName, a.cfg.AccessToken = res.User.ID, res.User.Name, res.AccessToken
	return a.cfg.Save()
}

// startQuickConnect polls in the background until the user approves the code,
// then completes the login.
func (a *app) startQuickConnect(c *jellyfin.Client, serverName string, qc *jellyfin.QuickConnect) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	p := &pendingLogin{server: serverName, code: qc.Code, cancel: cancel}
	a.mu.Lock()
	if a.pending != nil {
		a.pending.cancel()
	}
	a.pending = p
	a.mu.Unlock()

	fail := func(msg string) {
		a.mu.Lock()
		if a.pending == p {
			p.err = msg
		}
		a.mu.Unlock()
	}
	go func() {
		defer cancel()
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					fail("Quick Connect code " + qc.Code + " expired; call connect again")
				}
				return
			case <-tick.C:
			}
			st, err := c.QuickConnectState(ctx, qc.Secret)
			if err != nil {
				if jellyfin.IsStatus(err, 404) || jellyfin.IsStatus(err, 401) {
					fail("Quick Connect request expired or was rejected; call connect again")
					return
				}
				continue // transient
			}
			if !st.Authenticated {
				continue
			}
			res, err := c.AuthenticateWithQuickConnect(ctx, qc.Secret)
			if err != nil {
				fail("Quick Connect was approved but login failed: " + err.Error())
				return
			}
			if err := a.finishLogin(c, serverName, res); err != nil {
				log.Printf("saving login: %v", err)
			}
			return
		}
	}()
}

func (a *app) logout(ctx context.Context) {
	a.player.Stop()
	a.mu.Lock()
	c := a.client
	a.client = nil
	if a.pending != nil {
		a.pending.cancel()
		a.pending = nil
	}
	a.cfg.ClearLogin()
	err := a.cfg.Save()
	a.mu.Unlock()
	if err != nil {
		log.Printf("saving config: %v", err)
	}
	if c != nil && os.Getenv("JELLYFIN_API_KEY") == "" {
		_ = c.Logout(ctx)
	}
}

// --- player plumbing ---

func (a *app) streamURL(t player.Track) (string, string) {
	c := a.connected()
	if c == nil {
		return "", ""
	}
	return c.DirectStreamURL(t.ID), c.AuthHeader()
}

// afplayFormats are containers afplay plays natively; anything else is
// transcoded to MP3 by the server.
var afplayFormats = map[string]bool{
	"mp3": true, "flac": true, "m4a": true, "mp4": true, "aac": true,
	"alac": true, "wav": true, "aiff": true, "aif": true, "caf": true,
}

func (a *app) fetchForAfplay(ctx context.Context, t player.Track, dir string) (string, error) {
	c := a.connected()
	if c == nil {
		return "", errors.New("not connected")
	}
	ext, u := "mp3", c.MP3StreamURL(t.ID)
	for _, f := range strings.Split(strings.ToLower(t.Container), ",") {
		if afplayFormats[f] {
			ext, u = f, c.DirectStreamURL(t.ID)
			break
		}
	}
	f, err := os.CreateTemp(dir, "track-*."+ext)
	if err != nil {
		return "", err
	}
	err = c.Download(ctx, u, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// reporter forwards playback events to Jellyfin so play counts, "recently
// played" and the dashboard stay accurate. Failures are only logged.
type reporter struct {
	a         *app
	sessionID string
}

func (r *reporter) send(name string, fn func(context.Context, *jellyfin.Client) error) {
	c := r.a.connected()
	if c == nil || c.UserID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := fn(ctx, c); err != nil {
			log.Printf("reporting %s: %v", name, err)
		}
	}()
}

func (r *reporter) Started(t player.Track, volume int) {
	r.sessionID = randomHex(8)
	rep := jellyfin.PlaybackReport{ItemID: t.ID, VolumeLevel: volume, PlaySessionID: r.sessionID}
	r.send("start", func(ctx context.Context, c *jellyfin.Client) error { return c.ReportStart(ctx, rep) })
}

func (r *reporter) Progress(t player.Track, pos float64, paused bool, volume int, event string) {
	rep := jellyfin.PlaybackReport{ItemID: t.ID, PositionSec: pos, IsPaused: paused, VolumeLevel: volume, EventName: event, PlaySessionID: r.sessionID}
	r.send("progress", func(ctx context.Context, c *jellyfin.Client) error { return c.ReportProgress(ctx, rep) })
}

func (r *reporter) Stopped(t player.Track, pos float64) {
	rep := jellyfin.PlaybackReport{ItemID: t.ID, PositionSec: pos, PlaySessionID: r.sessionID}
	r.send("stop", func(ctx context.Context, c *jellyfin.Client) error { return c.ReportStopped(ctx, rep) })
}

func deviceName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "Claude"
	}
	return "Claude on " + strings.TrimSuffix(h, ".local")
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
