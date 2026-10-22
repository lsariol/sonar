package api

import (
	"strings"
	"testing"
)

// FuzzBearer: no header crashes the parser, and a token it returns is
// trimmed and non-empty.
func FuzzBearer(f *testing.F) {
	for _, s := range []string{"Bearer abc", "bearer  abc ", "Basic abc", "Bearer", "", " Bearer x y"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, header string) {
		token, ok := bearer(header)
		if ok && (token == "" || token != strings.TrimSpace(token)) {
			t.Fatalf("bearer(%q) = %q", header, token)
		}
	})
}
