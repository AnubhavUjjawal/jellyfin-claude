package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"jellyfin-claude/internal/jellyfin"
	"jellyfin-claude/internal/player"
)

// Tool annotations let clients tell reads from actions: Claude Code's permission
// modes treat a read-only tool's calls as reads, which the player pane's
// once-a-second now_playing poll relies on.
var (
	closedWorld = new(bool)
	readOnly    = &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: closedWorld}
	acts        = &mcp.ToolAnnotations{DestructiveHint: closedWorld, OpenWorldHint: closedWorld}
)

func (a *app) registerTools(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "discover_servers",
		Annotations: readOnly,
		Description: "Find Jellyfin servers on the local network via Jellyfin's UDP auto-discovery broadcast.",
	}, a.discoverServers)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "connect",
		Annotations: acts,
		Description: "Log in to a Jellyfin server. Without username/password this starts Quick Connect and returns a code " +
			"the user must approve in a signed-in Jellyfin app (profile menu > Quick Connect); login then completes in the " +
			"background, so check with status. Prefer Quick Connect so the password never goes through the chat.",
	}, a.connect)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "status",
		Annotations: readOnly,
		Description: "Show the Jellyfin connection and what's playing: current track and position, volume, repeat mode and the upcoming queue.",
	}, a.status)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "search",
		Annotations: readOnly,
		Description: "Search the music library by name. Returns tracks, albums, artists, playlists and genres with ids to pass to play.",
	}, a.search)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "browse",
		Annotations: readOnly,
		Description: "List library music without a search term: favorites, recently_played, most_played, recently_added, random, albums, artists, playlists, genres, artist_albums (needs item_id) or tracks (the tracks of an album/artist/playlist/genre item_id).",
	}, a.browse)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "play",
		Annotations: acts,
		Description: "Play music on this computer's speakers. item_ids may be tracks, albums, artists, playlists or genres (expanded to their tracks). " +
			"query plays the best library match for a name instead. With instant_mix, plays music similar to the first item. " +
			"With only genre, plays random tracks from that genre; with nothing, shuffles the whole library.",
	}, a.play)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "control",
		Annotations: acts,
		Description: "Control playback: pause, resume, toggle, next, previous, stop, seek, volume, jump, shuffle (the upcoming queue), clear (the upcoming queue) or repeat.",
	}, a.control)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "favorite",
		Annotations: acts,
		Description: "Mark an item (default: the current track) as a favorite in Jellyfin, or remove it from favorites.",
	}, a.favorite)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "now_playing",
		Annotations: readOnly,
		Description: "Playback state as structured data (track, position, queue, volume). Used by the player pane; prefer status for answering the user.",
	}, a.nowPlaying)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "artwork",
		Annotations: readOnly,
		Description: "Cover art of an item (usually the current track's album_id) as a PNG image. Used by the player pane.",
	}, a.artwork)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "lyrics",
		Annotations: readOnly,
		Description: "A track's lyrics (default: the current track), each line with its start time when the lyrics are synced.",
	}, a.lyrics)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "logout",
		Annotations: acts,
		Description: "Stop playback, log out of the Jellyfin server and forget the saved login.",
	}, a.logoutTool)
}

func text(format string, args ...any) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}}}, nil, nil
}

// --- discover_servers ---

type discoverArgs struct {
	TimeoutSeconds float64 `json:"timeout_seconds,omitempty" jsonschema:"how long to wait for replies (default 2)"`
	Address        string  `json:"address,omitempty" jsonschema:"a host or IP to also probe directly, for networks that drop broadcasts"`
}

func (a *app) discoverServers(ctx context.Context, _ *mcp.CallToolRequest, in discoverArgs) (*mcp.CallToolResult, any, error) {
	timeout := 2 * time.Second
	if in.TimeoutSeconds > 0 {
		timeout = time.Duration(min(in.TimeoutSeconds, 15) * float64(time.Second))
	}
	var extra []string
	if in.Address != "" {
		extra = append(extra, in.Address)
	}
	found, err := jellyfin.Discover(ctx, timeout, extra...)
	if err != nil {
		return nil, nil, fmt.Errorf("discovery failed: %w", err)
	}
	if len(found) == 0 {
		return text("No Jellyfin servers answered. Possible causes: auto-discovery is off on the server " +
			"(Dashboard > Networking), the server is on another subnet or VPN, or macOS hasn't granted Local Network " +
			"access to this app (System Settings > Privacy & Security > Local Network). Ask the user for the server " +
			"address and call connect with it.")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d Jellyfin server(s):\n", len(found))
	for _, s := range found {
		fmt.Fprintf(&b, "- %s at %s\n", s.Name, s.Address)
	}
	return text("%s", b.String())
}

// --- connect / logout / status ---

type connectArgs struct {
	URL      string `json:"url" jsonschema:"server address, e.g. http://192.168.1.10:8096 or nas.local"`
	Username string `json:"username,omitempty" jsonschema:"only for password login; omit to use Quick Connect"`
	Password string `json:"password,omitempty" jsonschema:"only for password login; omit to use Quick Connect"`
}

func (a *app) connect(ctx context.Context, _ *mcp.CallToolRequest, in connectArgs) (*mcp.CallToolResult, any, error) {
	base, err := jellyfin.NormalizeURL(in.URL)
	if err != nil {
		return nil, nil, err
	}
	c := a.newClient(base)
	info, err := c.PublicInfo(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("couldn't reach a Jellyfin server at %s: %w", base, err)
	}

	if in.Username != "" {
		res, err := c.AuthenticateByName(ctx, in.Username, in.Password)
		if jellyfin.IsStatus(err, 401) {
			return nil, nil, errors.New("Jellyfin rejected that username or password")
		}
		if err != nil {
			return nil, nil, err
		}
		if err := a.finishLogin(c, info.ServerName, res); err != nil {
			return nil, nil, fmt.Errorf("logged in but couldn't save the login: %w", err)
		}
		return text("Connected to %s (Jellyfin %s) as %s.", info.ServerName, info.Version, res.User.Name)
	}

	enabled, err := c.QuickConnectEnabled(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !enabled {
		return nil, nil, fmt.Errorf("Quick Connect is turned off on %s (an admin can enable it in Dashboard > General); "+
			"otherwise call connect with a username and password", info.ServerName)
	}
	qc, err := c.QuickConnectInitiate(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("starting Quick Connect: %w", err)
	}
	a.startQuickConnect(c, info.ServerName, qc)
	return text("Quick Connect code for %s: %s\n"+
		"Ask the user to open Jellyfin while signed in (web or app), go to their profile menu > Quick Connect and enter this code. "+
		"Login finishes automatically once approved (the code is valid for 5 minutes); call status to confirm.",
		info.ServerName, qc.Code)
}

func (a *app) logoutTool(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	a.logout(ctx)
	return text("Logged out and forgot the saved login.")
}

func (a *app) status(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	var b strings.Builder
	a.mu.Lock()
	switch {
	case a.client != nil && a.cfg.AccessToken != "":
		fmt.Fprintf(&b, "Connected to %s (%s) as %s.\n", a.cfg.ServerName, a.cfg.ServerURL, a.cfg.UserName)
	case a.client != nil:
		fmt.Fprintf(&b, "Connected to %s with an API key.\n", a.client.BaseURL)
	case a.pending != nil && a.pending.err != "":
		fmt.Fprintf(&b, "Not connected: %s\n", a.pending.err)
	case a.pending != nil:
		fmt.Fprintf(&b, "Waiting for the user to approve Quick Connect code %s for %s.\n", a.pending.code, a.pending.server)
	default:
		b.WriteString("Not connected to a Jellyfin server (use discover_servers, then connect).\n")
	}
	a.mu.Unlock()
	b.WriteString(describePlayer(a.player.Status(10), a.playerOpts))
	return text("%s", b.String())
}

func describePlayer(s player.Status, opts player.Options) string {
	var b strings.Builder
	backend := s.Backend
	if backend == "" {
		backend = opts.Describe()
	}
	fmt.Fprintf(&b, "Player: %s. Volume %d, repeat %s.\n", backend, s.Volume, s.Repeat)
	switch {
	case s.Current != nil && s.State != player.Stopped:
		length := "?:??"
		if s.Current.Duration > 0 {
			length = fmtDur(s.Current.Duration)
		}
		fmt.Fprintf(&b, "%s: %s [%s / %s] (track %d of %d, id %s)\n", titleCase(string(s.State)), s.Current,
			fmtDur(s.Position), length, s.Index+1, s.Total, s.Current.ID)
	case s.Current != nil:
		fmt.Fprintf(&b, "Stopped at: %s (track %d of %d)\n", s.Current, s.Index+1, s.Total)
	case s.Total > 0:
		fmt.Fprintf(&b, "Stopped: finished the queue (%d tracks).\n", s.Total)
	default:
		b.WriteString("Nothing playing; the queue is empty.\n")
	}
	if len(s.Upcoming) > 0 {
		fmt.Fprintf(&b, "Up next (%d more queued):\n", s.Total-s.Index-1)
		for i, t := range s.Upcoming {
			fmt.Fprintf(&b, "  %d. %s\n", s.Index+2+i, t)
		}
	}
	if s.LastErr != "" {
		fmt.Fprintf(&b, "Last error: %s\n", s.LastErr)
	}
	return b.String()
}

// --- search / browse ---

var searchTypes = map[string]string{
	"tracks":    jellyfin.TypeAudio,
	"albums":    jellyfin.TypeAlbum,
	"artists":   jellyfin.TypeArtist,
	"playlists": jellyfin.TypePlaylist,
	"genres":    jellyfin.TypeGenre,
}

type searchArgs struct {
	Query string   `json:"query" jsonschema:"text to match against names"`
	Types []string `json:"types,omitempty" jsonschema:"any of tracks, albums, artists, playlists, genres (default: all)"`
	Limit int      `json:"limit,omitempty" jsonschema:"max results per type (default 8)"`
}

func (a *app) search(ctx context.Context, _ *mcp.CallToolRequest, in searchArgs) (*mcp.CallToolResult, any, error) {
	c, err := a.conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(in.Query) == "" {
		return nil, nil, errors.New("query is empty; use browse to list music without a search term")
	}
	types := in.Types
	if len(types) == 0 {
		types = []string{"artists", "albums", "playlists", "tracks", "genres"}
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 8
	}
	var b strings.Builder
	total := 0
	for _, name := range types {
		typ, ok := searchTypes[strings.ToLower(name)]
		if !ok {
			return nil, nil, fmt.Errorf("unknown type %q (use tracks, albums, artists, playlists or genres)", name)
		}
		items, err := c.Search(ctx, in.Query, typ, limit)
		if err != nil {
			return nil, nil, err
		}
		if len(items) == 0 {
			continue
		}
		total += len(items)
		fmt.Fprintf(&b, "%s:\n", titleCase(name))
		writeItems(&b, items)
	}
	if total == 0 {
		return text("Nothing in the library matches %q.", in.Query)
	}
	return text("%s", b.String())
}

type browseArgs struct {
	View   string `json:"view" jsonschema:"favorites, recently_played, most_played, recently_added, random, albums, artists, playlists, genres, artist_albums or tracks"`
	ItemID string `json:"item_id,omitempty" jsonschema:"for artist_albums: an artist id; for tracks: an album, artist, playlist or genre id"`
	Genre  string `json:"genre,omitempty" jsonschema:"for random and albums: only this genre (name)"`
	Kind   string `json:"kind,omitempty" jsonschema:"for favorites: tracks (default), albums or artists"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max results (default 25)"`
}

func (a *app) browse(ctx context.Context, _ *mcp.CallToolRequest, in browseArgs) (*mcp.CallToolResult, any, error) {
	c, err := a.conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 25
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	list := func(itemType, sortBy, order string) ([]jellyfin.Item, int, error) {
		q.Set("includeItemTypes", itemType)
		q.Set("sortBy", sortBy)
		if order != "" {
			q.Set("sortOrder", order)
		}
		if in.Genre != "" {
			q.Set("genres", in.Genre)
		}
		return c.Items(ctx, q)
	}

	var items []jellyfin.Item
	var total int
	switch in.View {
	case "favorites":
		q.Set("filters", "IsFavorite")
		switch in.Kind {
		case "", "tracks":
			items, total, err = list(jellyfin.TypeAudio, "SortName", "")
		case "albums":
			items, total, err = list(jellyfin.TypeAlbum, "SortName", "")
		case "artists":
			q.Set("isFavorite", "true")
			items, total, err = c.Query(ctx, "/Artists", q)
		default:
			return nil, nil, fmt.Errorf("unknown kind %q (use tracks, albums or artists)", in.Kind)
		}
	case "recently_played":
		q.Set("filters", "IsPlayed")
		items, total, err = list(jellyfin.TypeAudio, "DatePlayed", "Descending")
	case "most_played":
		q.Set("filters", "IsPlayed")
		items, total, err = list(jellyfin.TypeAudio, "PlayCount", "Descending")
	case "recently_added":
		items, total, err = list(jellyfin.TypeAlbum, "DateCreated", "Descending")
	case "random":
		items, total, err = list(jellyfin.TypeAudio, "Random", "")
	case "albums":
		items, total, err = list(jellyfin.TypeAlbum, "AlbumArtist,ProductionYear,SortName", "")
	case "artists":
		q.Set("sortBy", "SortName")
		items, total, err = c.Query(ctx, "/Artists", q)
	case "playlists":
		items, total, err = list(jellyfin.TypePlaylist, "SortName", "")
	case "genres":
		items, total, err = c.Query(ctx, "/MusicGenres", url.Values{"limit": {strconv.Itoa(limit)}, "sortBy": {"SortName"}})
	case "artist_albums":
		if in.ItemID == "" {
			return nil, nil, errors.New("artist_albums needs item_id (an artist id from search)")
		}
		q.Set("artistIds", in.ItemID)
		items, total, err = list(jellyfin.TypeAlbum, "ProductionYear,SortName", "")
	case "tracks":
		if in.ItemID == "" {
			return nil, nil, errors.New("tracks needs item_id (an album, artist, playlist or genre id)")
		}
		it, err := c.Item(ctx, in.ItemID)
		if err != nil {
			return nil, nil, err
		}
		all, err := c.Tracks(ctx, it)
		if err != nil {
			return nil, nil, err
		}
		total = len(all)
		items = all[:min(len(all), limit)]
	default:
		return nil, nil, fmt.Errorf("unknown view %q", in.View)
	}
	if err != nil {
		return nil, nil, err
	}
	if len(items) == 0 {
		return text("Nothing to show for %s.", in.View)
	}
	var b strings.Builder
	if total > len(items) {
		fmt.Fprintf(&b, "Showing %d of %d:\n", len(items), total)
	}
	writeItems(&b, items)
	return text("%s", b.String())
}

// --- play / control / favorite ---

type playArgs struct {
	ItemIDs    []string `json:"item_ids,omitempty" jsonschema:"ids from search or browse: tracks, albums, artists, playlists or genres, played in the order given"`
	Query      string   `json:"query,omitempty" jsonschema:"instead of item_ids: play the best library match for this artist, album, playlist or track name (artists are shuffled)"`
	Mode       string   `json:"mode,omitempty" jsonschema:"replace (default: play now, replacing the queue), next (play after the current track) or append (add to the end of the queue)"`
	Shuffle    bool     `json:"shuffle,omitempty" jsonschema:"shuffle the tracks being added"`
	InstantMix bool     `json:"instant_mix,omitempty" jsonschema:"play a mix of similar music seeded from the first item_id instead of the items themselves"`
	Genre      string   `json:"genre,omitempty" jsonschema:"with no item_ids: play random tracks from this genre (name)"`
	Limit      int      `json:"limit,omitempty" jsonschema:"how many tracks for instant_mix, genre or whole-library shuffle (default 50)"`
}

func (a *app) play(ctx context.Context, _ *mcp.CallToolRequest, in playArgs) (*mcp.CallToolResult, any, error) {
	c, err := a.conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}

	if in.Query != "" && len(in.ItemIDs) == 0 {
		match, err := bestMatch(ctx, c, in.Query)
		if err != nil {
			return nil, nil, err
		}
		in.ItemIDs = []string{match.ID}
		if match.Type == jellyfin.TypeArtist {
			in.Shuffle = true
		}
	}

	var items []jellyfin.Item
	var what string
	switch {
	case in.InstantMix:
		if len(in.ItemIDs) == 0 {
			return nil, nil, errors.New("instant_mix needs an item_id to seed the mix")
		}
		items, err = c.InstantMix(ctx, in.ItemIDs[0], limit)
		what = "an instant mix"
	case len(in.ItemIDs) > 0:
		for _, id := range in.ItemIDs {
			it, ierr := c.Item(ctx, id)
			if ierr != nil {
				return nil, nil, ierr
			}
			tracks, terr := c.Tracks(ctx, it)
			if terr != nil {
				return nil, nil, terr
			}
			items = append(items, tracks...)
		}
	default:
		q := url.Values{"includeItemTypes": {jellyfin.TypeAudio}, "sortBy": {"Random"}, "limit": {strconv.Itoa(limit)}}
		what = "a shuffle of the whole library"
		if in.Genre != "" {
			q.Set("genres", in.Genre)
			what = "random " + in.Genre + " tracks"
		}
		items, _, err = c.Items(ctx, q)
	}
	if err != nil {
		return nil, nil, err
	}
	if len(items) == 0 {
		return nil, nil, errors.New("found no tracks to play")
	}

	tracks := make([]player.Track, len(items))
	for i, it := range items {
		tracks[i] = player.Track{ID: it.ID, Name: it.Name, Artist: it.Artist(), Album: it.Album, AlbumID: it.AlbumID, HasLyrics: it.HasLyrics, Container: it.Container, Duration: it.DurationSec()}
	}
	if in.Shuffle {
		rand.Shuffle(len(tracks), func(i, j int) { tracks[i], tracks[j] = tracks[j], tracks[i] })
	}
	if err := a.player.Enqueue(tracks, in.Mode); err != nil {
		return nil, nil, err
	}

	s := a.player.Status(0)
	var head string
	switch in.Mode {
	case "next":
		head = fmt.Sprintf("Queued %d track(s) to play next.", len(tracks))
	case "append":
		head = fmt.Sprintf("Added %d track(s) to the end of the queue.", len(tracks))
	default:
		head = fmt.Sprintf("Playing %d track(s)", len(tracks))
		if what != "" {
			head += " from " + what
		}
		head += "."
	}
	if s.Current != nil && s.State == player.Playing && (in.Mode == "" || in.Mode == "replace" || s.Total == len(tracks)) {
		head += fmt.Sprintf(" Now playing: %s.", s.Current)
	}
	return text("%s", head)
}

type controlArgs struct {
	Action string `json:"action" jsonschema:"pause, resume, toggle, next, previous, stop, seek, volume, jump, shuffle, clear or repeat"`
	Value  string `json:"value,omitempty" jsonschema:"seek: seconds or m:ss, or +N/-N to skip relative; volume: 0-100, or +N/-N; jump: queue position (1-based, as shown by status); repeat: off, all or one"`
}

func (a *app) control(ctx context.Context, _ *mcp.CallToolRequest, in controlArgs) (*mcp.CallToolResult, any, error) {
	p := a.player
	var err error
	var note string
	switch in.Action {
	case "pause":
		err = p.Pause()
	case "resume", "play":
		err = p.Resume()
	case "toggle":
		err = p.Toggle()
	case "next", "skip":
		err = p.Next()
	case "previous", "prev", "back":
		err = p.Previous()
	case "stop":
		err = p.Stop()
	case "seek":
		v := strings.TrimSpace(in.Value)
		relative := strings.HasPrefix(v, "+") || strings.HasPrefix(v, "-")
		sec, perr := parseTime(strings.TrimPrefix(v, "+"))
		if perr != nil {
			return nil, nil, perr
		}
		err = p.Seek(sec, relative)
	case "volume":
		v := strings.TrimSpace(in.Value)
		n, perr := parseInt(v)
		if perr != nil {
			return nil, nil, fmt.Errorf("volume needs a number 0-100 or +N/-N, got %q", in.Value)
		}
		if strings.HasPrefix(v, "+") || strings.HasPrefix(v, "-") {
			n += p.Volume()
		}
		note, err = p.SetVolume(n)
	case "jump":
		n, perr := parseInt(in.Value)
		if perr != nil {
			return nil, nil, fmt.Errorf("jump needs a queue position, got %q", in.Value)
		}
		err = p.Jump(n - 1)
	case "shuffle":
		p.ShuffleUpcoming()
	case "clear":
		note = fmt.Sprintf("Removed %d upcoming track(s).", p.ClearUpcoming())
	case "repeat":
		err = p.SetRepeat(strings.ToLower(strings.TrimSpace(in.Value)))
	default:
		return nil, nil, fmt.Errorf("unknown action %q", in.Action)
	}
	if err != nil {
		return nil, nil, err
	}
	out := describePlayer(p.Status(3), a.playerOpts)
	if note != "" {
		out = note + "\n" + out
	}
	return text("%s", out)
}

type favoriteArgs struct {
	ItemID string `json:"item_id,omitempty" jsonschema:"item to favorite; defaults to the current track"`
	Remove bool   `json:"remove,omitempty" jsonschema:"remove from favorites instead"`
}

func (a *app) favorite(ctx context.Context, _ *mcp.CallToolRequest, in favoriteArgs) (*mcp.CallToolResult, any, error) {
	c, err := a.conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	id, name := in.ItemID, ""
	if id == "" {
		s := a.player.Status(0)
		if s.Current == nil {
			return nil, nil, errors.New("nothing is playing; pass item_id")
		}
		id, name = s.Current.ID, s.Current.String()
	} else if it, err := c.Item(ctx, id); err == nil {
		name = it.Name
	}
	if err := c.SetFavorite(ctx, id, !in.Remove); err != nil {
		return nil, nil, err
	}
	if in.Remove {
		return text("Removed %s from favorites.", name)
	}
	return text("Added %s to favorites.", name)
}

// bestMatch picks what a bare name most likely means: an exact name match wins
// (artists, then albums, playlists, tracks), else the first artist, album,
// playlist or track that the search returns.
func bestMatch(ctx context.Context, c *jellyfin.Client, query string) (*jellyfin.Item, error) {
	var first *jellyfin.Item
	for _, typ := range []string{jellyfin.TypeArtist, jellyfin.TypeAlbum, jellyfin.TypePlaylist, jellyfin.TypeAudio} {
		items, err := c.Search(ctx, query, typ, 5)
		if err != nil {
			return nil, err
		}
		for i := range items {
			if strings.EqualFold(strings.TrimSpace(items[i].Name), strings.TrimSpace(query)) {
				return &items[i], nil
			}
		}
		if first == nil && len(items) > 0 {
			first = &items[0]
		}
	}
	if first == nil {
		return nil, fmt.Errorf("nothing in the library matches %q", query)
	}
	return first, nil
}

// --- player pane ---

type npTrack struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Artist    string  `json:"artist"`
	Album     string  `json:"album"`
	AlbumID   string  `json:"album_id"`
	Duration  float64 `json:"duration_sec"`
	HasLyrics bool    `json:"has_lyrics"`
}

// nowPlayingOut is the pane's view of the world. Every field is always present
// so the client never has to guess at absent keys.
type nowPlayingOut struct {
	Connected   bool      `json:"connected"`
	Server      string    `json:"server"`
	User        string    `json:"user"`
	PendingCode string    `json:"pending_code"`
	State       string    `json:"state" jsonschema:"playing, paused or stopped"`
	Backend     string    `json:"backend"`
	CanSeek     bool      `json:"can_seek"`
	Track       npTrack   `json:"track" jsonschema:"the current track; id is empty when there is none"`
	Position    float64   `json:"position_sec"`
	QueuePos    int       `json:"queue_position" jsonschema:"1-based position of track in the queue, 0 when none"`
	QueueLength int       `json:"queue_length"`
	Upcoming    []npTrack `json:"upcoming"`
	Volume      int       `json:"volume"`
	Repeat      string    `json:"repeat"`
	LastError   string    `json:"last_error"`
}

func toNP(t player.Track) npTrack {
	return npTrack{ID: t.ID, Name: t.Name, Artist: t.Artist, Album: t.Album, AlbumID: t.AlbumID, Duration: t.Duration, HasLyrics: t.HasLyrics}
}

func (a *app) nowPlaying(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, nowPlayingOut, error) {
	s := a.player.Status(5)
	out := nowPlayingOut{
		State:       string(s.State),
		Backend:     s.Backend,
		CanSeek:     s.Caps.Seek,
		Position:    s.Position,
		QueueLength: s.Total,
		Upcoming:    []npTrack{},
		Volume:      s.Volume,
		Repeat:      s.Repeat,
		LastError:   s.LastErr,
	}
	if s.Current != nil {
		out.Track = toNP(*s.Current)
		out.QueuePos = s.Index + 1
	}
	for _, t := range s.Upcoming {
		out.Upcoming = append(out.Upcoming, toNP(t))
	}
	a.mu.Lock()
	out.Connected = a.client != nil
	if out.Connected {
		out.Server, out.User = a.cfg.ServerName, a.cfg.UserName
		if out.Server == "" {
			out.Server = a.client.BaseURL
		}
	} else if a.pending != nil && a.pending.err == "" {
		out.PendingCode = a.pending.code
	}
	a.mu.Unlock()
	return nil, out, nil
}

type artworkArgs struct {
	ItemID string `json:"item_id" jsonschema:"the item whose cover to fetch, usually an album id"`
	Size   int    `json:"size,omitempty" jsonschema:"longest side in pixels (default 256, at most 1024)"`
}

func (a *app) artwork(ctx context.Context, _ *mcp.CallToolRequest, in artworkArgs) (*mcp.CallToolResult, any, error) {
	c, err := a.conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	size := in.Size
	if size <= 0 {
		size = 256
	}
	png, err := c.PrimaryImagePNG(ctx, in.ItemID, min(size, 1024))
	if jellyfin.IsStatus(err, 404) {
		return text("No artwork for this item.")
	}
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: png, MIMEType: "image/png"}}}, nil, nil
}

type lyricsArgs struct {
	ItemID string `json:"item_id,omitempty" jsonschema:"the track; defaults to the one playing"`
}

type lyricLine struct {
	Start float64 `json:"start_sec" jsonschema:"seconds into the track, or -1 when the lyrics are not synced"`
	Text  string  `json:"text"`
}

type lyricsOut struct {
	TrackID string      `json:"track_id"`
	Synced  bool        `json:"synced"`
	Lines   []lyricLine `json:"lines" jsonschema:"empty when the track has no lyrics"`
}

func (a *app) lyrics(ctx context.Context, _ *mcp.CallToolRequest, in lyricsArgs) (*mcp.CallToolResult, lyricsOut, error) {
	c, err := a.conn(ctx)
	if err != nil {
		return nil, lyricsOut{}, err
	}
	id := in.ItemID
	if id == "" {
		s := a.player.Status(0)
		if s.Current == nil {
			return nil, lyricsOut{}, errors.New("nothing is playing; pass item_id")
		}
		id = s.Current.ID
	}
	out := lyricsOut{TrackID: id, Lines: []lyricLine{}}
	lines, err := c.Lyrics(ctx, id)
	if jellyfin.IsStatus(err, 404) {
		return nil, out, nil
	}
	if err != nil {
		return nil, lyricsOut{}, err
	}
	out.Synced = len(lines) > 0
	for _, l := range lines {
		start := l.StartSec()
		if start < 0 {
			out.Synced = false
		}
		out.Lines = append(out.Lines, lyricLine{Start: start, Text: l.Text})
	}
	return nil, out, nil
}

// --- formatting ---

func writeItems(b *strings.Builder, items []jellyfin.Item) {
	for _, it := range items {
		b.WriteString("- ")
		b.WriteString(it.Name)
		switch it.Type {
		case jellyfin.TypeAudio:
			if ar := it.Artist(); ar != "" {
				b.WriteString(" - " + ar)
			}
			if it.Album != "" {
				b.WriteString(" (" + it.Album + ")")
			}
			if it.RunTimeTicks > 0 {
				b.WriteString(" [" + fmtDur(it.DurationSec()) + "]")
			}
		case jellyfin.TypeAlbum:
			if it.AlbumArtist != "" {
				b.WriteString(" - " + it.AlbumArtist)
			}
			if it.ProductionYear > 0 {
				fmt.Fprintf(b, " (%d)", it.ProductionYear)
			}
		case jellyfin.TypePlaylist:
			if it.ChildCount > 0 {
				fmt.Fprintf(b, " (%d items)", it.ChildCount)
			}
		}
		if it.UserData != nil && it.UserData.IsFavorite {
			b.WriteString(" [favorite]")
		}
		if it.UserData != nil && it.UserData.PlayCount > 0 {
			fmt.Fprintf(b, " [played %dx]", it.UserData.PlayCount)
		}
		b.WriteString(" | id: " + it.ID + "\n")
	}
}

func fmtDur(sec float64) string {
	d := int(max(sec, 0) + 0.5)
	if d >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", d/3600, d/60%60, d%60)
	}
	return fmt.Sprintf("%d:%02d", d/60, d%60)
}

// parseTime accepts "90", "1:30" or "1:02:03".
func parseTime(s string) (float64, error) {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var total float64
	for _, part := range strings.Split(s, ":") {
		n, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, fmt.Errorf("couldn't read %q as a time (use seconds or m:ss)", s)
		}
		total = total*60 + n
	}
	if neg {
		total = -total
	}
	return total, nil
}

func parseInt(s string) (int, error) {
	return strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(s), "+"))
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
