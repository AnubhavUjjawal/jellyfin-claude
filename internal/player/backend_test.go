package player

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// writeSilence writes a short 16-bit mono WAV of silence.
func writeSilence(t *testing.T, seconds float64) string {
	t.Helper()
	const rate = 8000
	n := int(seconds * rate)
	path := filepath.Join(t.TempDir(), "silence.wav")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	le := binary.LittleEndian
	hdr := []any{
		[4]byte{'R', 'I', 'F', 'F'}, uint32(36 + 2*n), [4]byte{'W', 'A', 'V', 'E'},
		[4]byte{'f', 'm', 't', ' '}, uint32(16), uint16(1), uint16(1), uint32(rate), uint32(2 * rate), uint16(2), uint16(16),
		[4]byte{'d', 'a', 't', 'a'}, uint32(2 * n),
	}
	for _, v := range hdr {
		if err := binary.Write(f, le, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.Write(make([]byte, 2*n)); err != nil {
		t.Fatal(err)
	}
	return path
}

// exerciseBackend plays a short file and checks the backend reports its natural
// end with the right generation.
func exerciseBackend(t *testing.T, newBackend func(EndFunc) Backend) {
	t.Helper()
	ended := make(chan uint64, 4)
	errs := make(chan error, 4)
	b := newBackend(func(gen uint64, err error) {
		if err != nil {
			errs <- err
			return
		}
		ended <- gen
	})
	defer b.Close()
	_ = b.SetVolume(0)

	if err := b.Play(Track{ID: "one", Name: "silence", Container: "wav"}, 7); err != nil {
		t.Fatalf("play: %v", err)
	}
	if err := b.Pause(); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := b.Resume(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	select {
	case gen := <-ended:
		if gen != 7 {
			t.Fatalf("ended gen = %d, want 7", gen)
		}
	case err := <-errs:
		t.Fatalf("track failed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no end-of-track event")
	}

	// A stopped track must not report an end.
	if err := b.Play(Track{ID: "two", Name: "silence", Container: "wav"}, 8); err != nil {
		t.Fatalf("play: %v", err)
	}
	_ = b.Stop()
	select {
	case gen := <-ended:
		t.Fatalf("stopped track reported an end (gen %d)", gen)
	case <-time.After(1500 * time.Millisecond):
	}
}

func TestAfplayBackend(t *testing.T) {
	if !afplayAvailable() {
		t.Skip("afplay is macOS-only")
	}
	src := writeSilence(t, 0.5)
	fetch := func(_ context.Context, _ Track, dir string) (string, error) {
		b, err := os.ReadFile(src)
		if err != nil {
			return "", err
		}
		f, err := os.CreateTemp(dir, "*.wav")
		if err != nil {
			return "", err
		}
		defer f.Close()
		_, err = f.Write(b)
		return f.Name(), err
	}
	exerciseBackend(t, func(onEnd EndFunc) Backend {
		b, err := newAfplay(fetch, onEnd)
		if err != nil {
			t.Fatal(err)
		}
		return b
	})
}

func TestMPVBackend(t *testing.T) {
	bin := FindMPV(os.Getenv("MPV_PATH"))
	if bin == "" {
		t.Skip("mpv not installed")
	}
	src := writeSilence(t, 0.5)
	exerciseBackend(t, func(onEnd EndFunc) Backend {
		return newMPV(bin, func(Track) (string, string) { return src, "MediaBrowser Token=\"x\"" }, onEnd)
	})
}

func TestMPVBadStreamFailsPlay(t *testing.T) {
	bin := FindMPV(os.Getenv("MPV_PATH"))
	if bin == "" || runtime.GOOS == "windows" {
		t.Skip("mpv not installed")
	}
	m := newMPV(bin, func(Track) (string, string) { return filepath.Join(t.TempDir(), "missing.flac"), "" }, func(uint64, error) {})
	defer m.Close()
	if err := m.Play(Track{ID: "x"}, 1); err == nil {
		t.Fatal("playing a missing file should fail")
	}
}
