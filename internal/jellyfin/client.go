// Package jellyfin is a small client for the parts of the Jellyfin REST API a
// music player needs: login, library search/browse, streaming and play reporting.
package jellyfin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	ClientName = "Claude Jellyfin"
	Version    = "0.1.0"
)

// Client talks to one Jellyfin server. Token and UserID are empty until login.
type Client struct {
	BaseURL  string
	Token    string
	UserID   string
	DeviceID string
	Device   string
	HTTP     *http.Client
}

type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("jellyfin %s %s: HTTP %d", e.Method, e.Path, e.Status)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// IsStatus reports whether err is an APIError with the given HTTP status.
func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

// NormalizeURL accepts "192.168.1.5", "nas.local:8096" or a full URL and returns
// a base URL without a trailing slash. Jellyfin's default port 8096 is assumed
// only when neither a scheme nor a port was given.
func NormalizeURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("empty server URL")
	}
	bare := !strings.Contains(s, "://")
	if bare {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid server URL %q", raw)
	}
	if bare && u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), "8096")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

// AuthHeader is the MediaBrowser authorization header Jellyfin expects on every
// request, carrying the client identity and, once logged in, the access token.
func (c *Client) AuthHeader() string {
	h := fmt.Sprintf(`MediaBrowser Client="%s", Device="%s", DeviceId="%s", Version="%s"`,
		ClientName, headerSafe(c.Device), headerSafe(c.DeviceID), Version)
	if c.Token != "" {
		h += fmt.Sprintf(`, Token="%s"`, headerSafe(c.Token))
	}
	return h
}

func headerSafe(s string) string {
	return strings.NewReplacer(`"`, "", "\r", "", "\n", "", ",", " ").Replace(s)
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// URL builds an absolute server URL for path and query.
func (c *Client) URL(path string, q url.Values) string {
	u := c.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL(path, q), rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.AuthHeader())
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// --- server info and login ---

type PublicInfo struct {
	ServerName string `json:"ServerName"`
	Version    string `json:"Version"`
	ID         string `json:"Id"`
}

func (c *Client) PublicInfo(ctx context.Context) (*PublicInfo, error) {
	var info PublicInfo
	if err := c.do(ctx, http.MethodGet, "/System/Info/Public", nil, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

type User struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Policy struct {
		IsAdministrator bool `json:"IsAdministrator"`
	} `json:"Policy"`
}

type AuthResult struct {
	User        User   `json:"User"`
	AccessToken string `json:"AccessToken"`
	ServerID    string `json:"ServerId"`
}

func (c *Client) AuthenticateByName(ctx context.Context, username, password string) (*AuthResult, error) {
	var res AuthResult
	err := c.do(ctx, http.MethodPost, "/Users/AuthenticateByName", nil,
		map[string]string{"Username": username, "Pw": password}, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

type QuickConnect struct {
	Secret        string `json:"Secret"`
	Code          string `json:"Code"`
	Authenticated bool   `json:"Authenticated"`
}

func (c *Client) QuickConnectEnabled(ctx context.Context) (bool, error) {
	var enabled bool
	err := c.do(ctx, http.MethodGet, "/QuickConnect/Enabled", nil, nil, &enabled)
	return enabled, err
}

// QuickConnectInitiate starts a Quick Connect request; the user approves Code from
// any signed-in Jellyfin client. Servers before 10.9 only accept GET here.
func (c *Client) QuickConnectInitiate(ctx context.Context) (*QuickConnect, error) {
	var qc QuickConnect
	err := c.do(ctx, http.MethodPost, "/QuickConnect/Initiate", nil, nil, &qc)
	if IsStatus(err, http.StatusMethodNotAllowed) || IsStatus(err, http.StatusNotFound) {
		err = c.do(ctx, http.MethodGet, "/QuickConnect/Initiate", nil, nil, &qc)
	}
	if err != nil {
		return nil, err
	}
	return &qc, nil
}

func (c *Client) QuickConnectState(ctx context.Context, secret string) (*QuickConnect, error) {
	var qc QuickConnect
	if err := c.do(ctx, http.MethodGet, "/QuickConnect/Connect", url.Values{"secret": {secret}}, nil, &qc); err != nil {
		return nil, err
	}
	return &qc, nil
}

func (c *Client) AuthenticateWithQuickConnect(ctx context.Context, secret string) (*AuthResult, error) {
	var res AuthResult
	err := c.do(ctx, http.MethodPost, "/Users/AuthenticateWithQuickConnect", nil, map[string]string{"Secret": secret}, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// Users lists users; with an API key this needs no user context.
func (c *Client) Users(ctx context.Context) ([]User, error) {
	var users []User
	err := c.do(ctx, http.MethodGet, "/Users", nil, nil, &users)
	return users, err
}

func (c *Client) Logout(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/Sessions/Logout", nil, nil, nil)
}

// --- streaming ---

// DirectStreamURL serves the original file untouched (for players like mpv that
// decode anything).
func (c *Client) DirectStreamURL(itemID string) string {
	return c.URL("/Audio/"+url.PathEscape(itemID)+"/stream", url.Values{"static": {"true"}})
}

// MP3StreamURL asks the server for an MP3, transcoding if needed (for players
// with limited codec support such as afplay).
func (c *Client) MP3StreamURL(itemID string) string {
	return c.URL("/Audio/"+url.PathEscape(itemID)+"/stream.mp3", url.Values{
		"audioCodec":   {"mp3"},
		"audioBitRate": {"256000"},
	})
}

// Download fetches rawURL (a stream URL from this server) with auth into w.
func (c *Client) Download(ctx context.Context, rawURL string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.AuthHeader())
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return &APIError{Method: http.MethodGet, Path: req.URL.Path, Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// --- playback reporting, so play counts and "recently played" stay accurate ---

const ticksPerSecond = 10_000_000

type PlaybackReport struct {
	ItemID        string
	PositionSec   float64
	IsPaused      bool
	VolumeLevel   int
	EventName     string
	PlaySessionID string
}

func (r PlaybackReport) body() map[string]any {
	b := map[string]any{
		"ItemId":        r.ItemID,
		"PositionTicks": int64(r.PositionSec * ticksPerSecond),
		"IsPaused":      r.IsPaused,
		"CanSeek":       true,
		"PlayMethod":    "DirectPlay",
		"PlaySessionId": r.PlaySessionID,
		"VolumeLevel":   r.VolumeLevel,
	}
	if r.EventName != "" {
		b["EventName"] = r.EventName
	}
	return b
}

func (c *Client) ReportStart(ctx context.Context, r PlaybackReport) error {
	return c.do(ctx, http.MethodPost, "/Sessions/Playing", nil, r.body(), nil)
}

func (c *Client) ReportProgress(ctx context.Context, r PlaybackReport) error {
	return c.do(ctx, http.MethodPost, "/Sessions/Playing/Progress", nil, r.body(), nil)
}

func (c *Client) ReportStopped(ctx context.Context, r PlaybackReport) error {
	return c.do(ctx, http.MethodPost, "/Sessions/Playing/Stopped", nil, r.body(), nil)
}
