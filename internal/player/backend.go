package player

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

type Options struct {
	// Backend is "auto" (mpv, else afplay on macOS), "mpv" or "afplay".
	Backend string
	// MPVPath overrides where to find mpv.
	MPVPath string
	// StreamURL returns the original-file URL for t and the auth header to send.
	StreamURL func(t Track) (url, authHeader string)
	// Fetch downloads t into dir in a format afplay can play, returning the path.
	Fetch func(ctx context.Context, t Track, dir string) (string, error)
}

var ErrNoBackend = errors.New("no audio player found: install mpv (on macOS: brew install mpv), or set MPV_PATH to its location")

// mpvCandidates are checked after PATH, since GUI apps like Claude Desktop don't
// inherit the shell's PATH.
var mpvCandidates = []string{
	"/opt/homebrew/bin/mpv",
	"/usr/local/bin/mpv",
	"/opt/local/bin/mpv",
	"/Applications/mpv.app/Contents/MacOS/mpv",
	"/usr/bin/mpv",
	"/snap/bin/mpv",
}

// FindMPV returns the path to an mpv binary, or "" if none is installed.
func FindMPV(override string) string {
	if override != "" {
		if isExecutable(override) {
			return override
		}
		return ""
	}
	if p, err := exec.LookPath("mpv"); err == nil {
		return p
	}
	for _, p := range mpvCandidates {
		if isExecutable(p) {
			return p
		}
	}
	return ""
}

func afplayAvailable() bool {
	return runtime.GOOS == "darwin" && isExecutable(afplayBin)
}

func isExecutable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Mode()&0o111 != 0
}

// Describe says which backend Options would pick, for status output.
func (o Options) Describe() string {
	mpvPath := FindMPV(o.MPVPath)
	switch {
	case o.Backend != "afplay" && mpvPath != "":
		return "mpv (" + mpvPath + ")"
	case o.Backend == "mpv":
		return "mpv (not found)"
	case afplayAvailable():
		return "afplay (basic: no seeking or live volume; install mpv for full controls)"
	default:
		return "none (install mpv)"
	}
}

// NewFactory returns a Factory that picks the backend per Options.
func NewFactory(o Options) Factory {
	return func(onEnd EndFunc) (Backend, error) {
		mpvPath := FindMPV(o.MPVPath)
		switch o.Backend {
		case "", "auto":
			if mpvPath != "" {
				return newMPV(mpvPath, o.StreamURL, onEnd), nil
			}
			if afplayAvailable() {
				return newAfplay(o.Fetch, onEnd)
			}
			return nil, ErrNoBackend
		case "mpv":
			if mpvPath == "" {
				return nil, ErrNoBackend
			}
			return newMPV(mpvPath, o.StreamURL, onEnd), nil
		case "afplay":
			if !afplayAvailable() {
				return nil, errors.New("afplay is only available on macOS")
			}
			return newAfplay(o.Fetch, onEnd)
		default:
			return nil, fmt.Errorf("unknown JELLYFIN_PLAYER %q (use auto, mpv or afplay)", o.Backend)
		}
	}
}
