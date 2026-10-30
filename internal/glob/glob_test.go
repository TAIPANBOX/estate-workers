package glob

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"services/qryx.md", "services/qryx.md", true},
		{"services/qryx.md", "services/qryx.mdx", false},
		{"services/*.md", "services/qryx.md", true},
		{"services/*.md", "services/sub/qryx.md", false},
		{".github/**", ".github/workflows/ci.yml", true},
		{".github/**", ".github", true},
		{".github/**", "src/.github/x", false},
		{"**/.git/**", "vendor/x/.git/config", true},
		{"**/.git/**", ".git/config", true},
		{"**/*.pem", "a/b/c/key.pem", true},
		{"**/*.pem", "key.pem", true},
		{"**/*.pem", "key.pem.txt", false},
		{"a/**/z", "a/z", true},
		{"a/**/z", "a/b/c/z", true},
		{"a/**/z", "a/b/c/y", false},
		{"README.md", "readme.md", false},
		{"", "x", false},
		{"x", "", false},
		{"[", "[", false},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.name); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}
