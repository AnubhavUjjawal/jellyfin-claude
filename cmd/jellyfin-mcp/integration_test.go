//go:build integration

// Integration test against a real Jellyfin server, driving the built binary over
// stdio like Claude does. Plays at volume 0.
//
//	JELLYFIN_TEST_URL=http://host:8096 JELLYFIN_TEST_USER=me JELLYFIN_TEST_PASS=secret \
//	JELLYFIN_TEST_PLAYER=mpv go test -tags integration -v ./cmd/jellyfin-mcp
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"jellyfin-claude/internal/jellyfin"
)

func TestIntegration(t *testing.T) {
	url, user := os.Getenv("JELLYFIN_TEST_URL"), os.Getenv("JELLYFIN_TEST_USER")
	if url == "" || user == "" {
		t.Skip("set JELLYFIN_TEST_URL, JELLYFIN_TEST_USER and JELLYFIN_TEST_PASS")
	}
	backend := os.Getenv("JELLYFIN_TEST_PLAYER")
	if backend == "" {
		backend = "auto"
	}

	bin := filepath.Join(t.TempDir(), "jellyfin-mcp")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"JELLYFIN_CLAUDE_CONFIG_DIR="+t.TempDir(),
		"JELLYFIN_PLAYER="+backend,
		"JELLYFIN_VOLUME=0",
		"JELLYFIN_URL=", "JELLYFIN_API_KEY=",
	)
	cmd.Stderr = os.Stderr

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "integration-test", Version: "0"}, nil)
	sess, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	call := func(name string, args map[string]any) string {
		t.Helper()
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		out := b.String()
		t.Logf("%s %v ->\n%s", name, args, out)
		if res.IsError {
			t.Fatalf("%s returned an error: %s", name, out)
		}
		return out
	}
	expectErr := func(name string, args map[string]any) {
		t.Helper()
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !res.IsError {
			t.Fatalf("%s %v: expected an error", name, args)
		}
	}
	idRe := regexp.MustCompile(`\| id: (\w+)`)
	firstID := func(out string) string {
		t.Helper()
		m := idRe.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("no id in:\n%s", out)
		}
		return m[1]
	}

	tools, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 9 {
		t.Errorf("got %d tools, want 9", len(tools.Tools))
	}

	if out := call("status", nil); !strings.Contains(out, "Not connected") {
		t.Errorf("expected not connected")
	}
	expectErr("search", map[string]any{"query": "a"})
	call("discover_servers", map[string]any{"timeout_seconds": 2})
	call("connect", map[string]any{"url": url, "username": user, "password": os.Getenv("JELLYFIN_TEST_PASS")})

	call("browse", map[string]any{"view": "genres", "limit": 5})
	call("browse", map[string]any{"view": "albums", "limit": 3})
	call("browse", map[string]any{"view": "recently_played", "limit": 3})

	artists := call("search", map[string]any{"query": "the", "types": []string{"artists"}, "limit": 2})
	artistID := firstID(artists)
	// An artist's first album is a real album, so "next" has somewhere to go.
	albumID := firstID(call("browse", map[string]any{"view": "artist_albums", "item_id": artistID}))
	call("browse", map[string]any{"view": "tracks", "item_id": albumID, "limit": 3})

	if out := call("play", map[string]any{"item_ids": []string{albumID}}); !strings.Contains(out, "Now playing") {
		t.Errorf("expected Now playing")
	}
	time.Sleep(3 * time.Second)
	st := call("status", nil)
	if !strings.Contains(st, "Playing:") {
		t.Fatalf("expected Playing in status")
	}
	playingID := regexp.MustCompile(`id (\w+)\)`).FindStringSubmatch(st)[1]
	checkServerSeesPlayback(ctx, t, url, user, os.Getenv("JELLYFIN_TEST_PASS"), playingID)
	call("control", map[string]any{"action": "pause"})
	if !strings.Contains(call("status", nil), "Paused:") {
		t.Errorf("expected Paused")
	}
	call("control", map[string]any{"action": "resume"})
	call("control", map[string]any{"action": "next"})
	if !strings.Contains(call("status", nil), "track 2 of") {
		t.Errorf("expected track 2 after next")
	}
	call("control", map[string]any{"action": "volume", "value": "+5"})
	if strings.Contains(backend, "afplay") {
		expectErr("control", map[string]any{"action": "seek", "value": "1:00"})
	} else {
		call("control", map[string]any{"action": "seek", "value": "0:30"})
		time.Sleep(time.Second)
		if !regexp.MustCompile(`\[0:3\d /`).MatchString(call("status", nil)) {
			t.Errorf("expected position ~0:30 after seek")
		}
		call("control", map[string]any{"action": "seek", "value": "+10"})
	}
	call("control", map[string]any{"action": "previous"})
	call("play", map[string]any{"item_ids": []string{artistID}, "mode": "next", "shuffle": true})
	call("control", map[string]any{"action": "repeat", "value": "all"})
	call("control", map[string]any{"action": "clear"})
	call("play", map[string]any{"item_ids": []string{albumID}, "instant_mix": true, "limit": 5})
	call("control", map[string]any{"action": "jump", "value": "3"})
	call("favorite", nil)
	call("favorite", map[string]any{"remove": true})
	call("play", map[string]any{"limit": 3})
	call("control", map[string]any{"action": "stop"})
	call("status", nil)
	call("logout", nil)
	expectErr("search", map[string]any{"query": "a"})
}

// checkServerSeesPlayback logs in as a separate device and checks that Jellyfin
// lists our session with a now-playing item, i.e. playback reporting works.
func checkServerSeesPlayback(ctx context.Context, t *testing.T, rawURL, user, pass, itemID string) {
	t.Helper()
	base, err := jellyfin.NormalizeURL(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	c := &jellyfin.Client{BaseURL: base, DeviceID: "integration-test-observer", Device: "integration test"}
	res, err := c.AuthenticateByName(ctx, user, pass)
	if err != nil {
		t.Fatal(err)
	}
	c.Token = res.AccessToken
	defer c.Logout(ctx)

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.URL("/Sessions", nil), nil)
	req.Header.Set("Authorization", c.AuthHeader())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sessions []struct {
		Client         string `json:"Client"`
		DeviceName     string `json:"DeviceName"`
		NowPlayingItem *struct {
			ID   string `json:"Id"`
			Name string `json:"Name"`
		} `json:"NowPlayingItem"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if s.Client == jellyfin.ClientName && s.NowPlayingItem != nil && s.NowPlayingItem.ID == itemID {
			t.Logf("server sees %q playing %q", s.DeviceName, s.NowPlayingItem.Name)
			return
		}
	}
	t.Errorf("Jellyfin doesn't list a %q session playing item %s", jellyfin.ClientName, itemID)
}
