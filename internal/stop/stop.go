// Package stop is the emergency brake. Two files stop every worker: a
// manual one a person writes as root, and an automatic one the orchestrator
// writes when it trips a cap or sees something it must not push. Either file
// existing means stopped; a file that cannot be checked also means stopped.
package stop

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Files struct {
	Manual string
	Auto   string
}

// Stopped reports whether work must not start or continue, and why.
func (f Files) Stopped() (bool, string) {
	for _, p := range []string{f.Manual, f.Auto} {
		if p == "" {
			continue
		}
		data, err := os.ReadFile(p)
		switch {
		case err == nil:
			reason := strings.TrimSpace(string(data))
			if reason == "" {
				reason = "no reason given"
			}
			return true, fmt.Sprintf("%s: %s", p, firstLine(reason))
		case errors.Is(err, fs.ErrNotExist):
			continue
		default:
			return true, fmt.Sprintf("%s cannot be checked (%v), treated as stopped", p, err)
		}
	}
	return false, ""
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	if len(line) > 200 {
		line = line[:200]
	}
	return line
}

// Write records a stop with its reason and time.
func Write(path, reason string, now time.Time) error {
	if path == "" {
		return errors.New("stop: no path")
	}
	body := fmt.Sprintf("%s at %s\n", strings.TrimSpace(reason), now.UTC().Format(time.RFC3339))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o640)
}

// Clear removes the given stop files; a missing file is not an error.
func Clear(paths ...string) error {
	var errs []error
	for _, p := range paths {
		if p == "" {
			continue
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
