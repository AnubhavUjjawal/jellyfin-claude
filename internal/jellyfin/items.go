package jellyfin

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Item types used by the music tools.
const (
	TypeAudio    = "Audio"
	TypeAlbum    = "MusicAlbum"
	TypeArtist   = "MusicArtist"
	TypePlaylist = "Playlist"
	TypeGenre    = "MusicGenre"
)

// maxTracks caps how many tracks expanding an artist or genre can queue.
const maxTracks = 1000

type Item struct {
	ID                string    `json:"Id"`
	Name              string    `json:"Name"`
	Type              string    `json:"Type"`
	Album             string    `json:"Album,omitempty"`
	AlbumID           string    `json:"AlbumId,omitempty"`
	AlbumArtist       string    `json:"AlbumArtist,omitempty"`
	Artists           []string  `json:"Artists,omitempty"`
	RunTimeTicks      int64     `json:"RunTimeTicks,omitempty"`
	ProductionYear    int       `json:"ProductionYear,omitempty"`
	IndexNumber       int       `json:"IndexNumber,omitempty"`
	ParentIndexNumber int       `json:"ParentIndexNumber,omitempty"`
	ChildCount        int       `json:"ChildCount,omitempty"`
	Genres            []string  `json:"Genres,omitempty"`
	Container         string    `json:"Container,omitempty"`
	UserData          *UserData `json:"UserData,omitempty"`
}

type UserData struct {
	PlayCount  int  `json:"PlayCount"`
	IsFavorite bool `json:"IsFavorite"`
}

// DurationSec is the track length in seconds (0 if unknown).
func (i Item) DurationSec() float64 { return float64(i.RunTimeTicks) / ticksPerSecond }

// Artist is the best single artist name to show for the item.
func (i Item) Artist() string {
	if len(i.Artists) > 0 {
		return strings.Join(i.Artists, ", ")
	}
	return i.AlbumArtist
}

type itemsResponse struct {
	Items            []Item `json:"Items"`
	TotalRecordCount int    `json:"TotalRecordCount"`
}

const itemFields = "Genres,ChildCount"

// Query runs GET path with the user and standard fields filled in.
func (c *Client) Query(ctx context.Context, path string, q url.Values) ([]Item, int, error) {
	if q == nil {
		q = url.Values{}
	}
	if c.UserID != "" {
		q.Set("userId", c.UserID)
	}
	if q.Get("fields") == "" {
		q.Set("fields", itemFields)
	}
	var res itemsResponse
	if err := c.do(ctx, http.MethodGet, path, q, nil, &res); err != nil {
		return nil, 0, err
	}
	return res.Items, res.TotalRecordCount, nil
}

// Items queries /Items, recursing through libraries.
func (c *Client) Items(ctx context.Context, q url.Values) ([]Item, int, error) {
	if q.Get("recursive") == "" {
		q.Set("recursive", "true")
	}
	return c.Query(ctx, "/Items", q)
}

// Search finds items of one type whose name matches term.
func (c *Client) Search(ctx context.Context, term, itemType string, limit int) ([]Item, error) {
	q := url.Values{"searchTerm": {term}, "limit": {strconv.Itoa(limit)}}
	switch itemType {
	case TypeArtist:
		items, _, err := c.Query(ctx, "/Artists", q)
		return items, err
	case TypeGenre:
		items, _, err := c.Query(ctx, "/MusicGenres", q)
		return items, err
	}
	q.Set("includeItemTypes", itemType)
	items, _, err := c.Items(ctx, q)
	return items, err
}

// Item fetches a single item by id.
func (c *Client) Item(ctx context.Context, id string) (*Item, error) {
	items, _, err := c.Items(ctx, url.Values{"ids": {id}})
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("no item with id %s", id)
	}
	return &items[0], nil
}

// Tracks expands an item to playable tracks in a sensible order: a track is
// itself, an album is its track list, an artist is their songs album by album,
// a playlist keeps its order, and a genre is its songs shuffled by the server.
func (c *Client) Tracks(ctx context.Context, it *Item) ([]Item, error) {
	audio := func(q url.Values) ([]Item, error) {
		q.Set("includeItemTypes", TypeAudio)
		q.Set("limit", strconv.Itoa(maxTracks))
		items, _, err := c.Items(ctx, q)
		return items, err
	}
	switch it.Type {
	case TypeAudio:
		return []Item{*it}, nil
	case TypeAlbum:
		return audio(url.Values{"parentId": {it.ID}, "sortBy": {"ParentIndexNumber,IndexNumber,SortName"}})
	case TypeArtist:
		return audio(url.Values{"artistIds": {it.ID}, "sortBy": {"ProductionYear,Album,ParentIndexNumber,IndexNumber"}})
	case TypeGenre:
		return audio(url.Values{"genreIds": {it.ID}, "sortBy": {"Random"}})
	case TypePlaylist:
		items, _, err := c.Query(ctx, "/Playlists/"+url.PathEscape(it.ID)+"/Items", url.Values{"limit": {strconv.Itoa(maxTracks)}})
		return items, err
	default:
		return nil, fmt.Errorf("%q is a %s, which isn't playable music", it.Name, it.Type)
	}
}

// InstantMix returns tracks similar to the seed item (track, album, artist,
// playlist or genre).
func (c *Client) InstantMix(ctx context.Context, id string, limit int) ([]Item, error) {
	items, _, err := c.Query(ctx, "/Items/"+url.PathEscape(id)+"/InstantMix", url.Values{"limit": {strconv.Itoa(limit)}})
	return items, err
}

// Genres lists music genres.
func (c *Client) Genres(ctx context.Context, limit int) ([]Item, error) {
	items, _, err := c.Query(ctx, "/MusicGenres", url.Values{"limit": {strconv.Itoa(limit)}, "sortBy": {"SortName"}})
	return items, err
}

func (c *Client) SetFavorite(ctx context.Context, id string, favorite bool) error {
	method := http.MethodPost
	if !favorite {
		method = http.MethodDelete
	}
	return c.do(ctx, method, "/UserFavoriteItems/"+url.PathEscape(id), url.Values{"userId": {c.UserID}}, nil, nil)
}
