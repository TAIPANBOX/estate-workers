// Package glob matches slash-separated paths against patterns where "**"
// stands for any number of whole segments and every other segment follows
// path.Match.
package glob

import (
	"path"
	"strings"
)

// Match reports whether name matches pattern. An empty pattern or name, or
// a malformed segment, never matches.
func Match(pattern, name string) bool {
	if pattern == "" || name == "" {
		return false
	}
	return match(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func match(p, n []string) bool {
	for len(p) > 0 {
		if p[0] == "**" {
			if len(p) == 1 {
				return true
			}
			for i := 0; i <= len(n); i++ {
				if match(p[1:], n[i:]) {
					return true
				}
			}
			return false
		}
		if len(n) == 0 {
			return false
		}
		ok, err := path.Match(p[0], n[0])
		if err != nil || !ok {
			return false
		}
		p, n = p[1:], n[1:]
	}
	return len(n) == 0
}
