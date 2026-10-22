// Package config persists the Jellyfin connection (server, user, access token)
// between sessions in a user-only readable JSON file.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

type Config struct {
	ServerURL   string `json:"server_url,omitempty"`
	ServerName  string `json:"server_name,omitempty"`
	ServerID    string `json:"server_id,omitempty"`
	UserID      string `json:"user_id,omitempty"`
	UserName    string `json:"user_name,omitempty"`
	AccessToken string `json:"access_token,omitempty"`
	// DeviceID identifies this install to Jellyfin; it is generated once and kept
	// so re-logins replace the previous token instead of piling up devices.
	DeviceID string `json:"device_id"`
}

// Dir is $JELLYFIN_CLAUDE_CONFIG_DIR, else $XDG_CONFIG_HOME/jellyfin-claude,
// else ~/.config/jellyfin-claude.
func Dir() (string, error) {
	if d := os.Getenv("JELLYFIN_CLAUDE_CONFIG_DIR"); d != "" {
		return d, nil
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "jellyfin-claude"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "jellyfin-claude"), nil
}

func path() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.json"), nil
}

// Load reads the config, returning a fresh one (with a new DeviceID) if none exists.
func Load() (*Config, error) {
	p, err := path()
	if err != nil {
		return nil, err
	}
	c := &Config{}
	b, err := os.ReadFile(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, c); err != nil {
			return nil, err
		}
	}
	if c.DeviceID == "" {
		c.DeviceID = randomHex(16)
		if err := c.Save(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Save writes the config atomically with 0600 permissions.
func (c *Config) Save() error {
	p, err := path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// ClearLogin forgets the server credentials but keeps the DeviceID.
func (c *Config) ClearLogin() {
	*c = Config{DeviceID: c.DeviceID}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
