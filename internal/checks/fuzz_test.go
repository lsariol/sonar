package checks

import (
	"net/netip"
	"strings"
	"testing"
)

// FuzzValidate: no label crashes Validate, and whatever it accepts is https
// on port 443 to a public-looking hostname.
func FuzzValidate(f *testing.F) {
	for _, s := range []string{"https://plop.mobasity.com", "https://10.0.0.1", "https://cove", "http://x.y",
		"https://u:p@x.y", "https://[::1]:443/", "https://x.y:8443", "https://plop.mobasity.com./a?b#c", "https://%zz"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		u, err := Validate(raw)
		if err != nil {
			return
		}
		host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
		if u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") || !strings.Contains(host, ".") {
			t.Fatalf("accepted %q: %+v", raw, u)
		}
		if _, err := netip.ParseAddr(host); err == nil {
			t.Fatalf("accepted an IP address: %q", raw)
		}
	})
}

// FuzzPublic: an address and its IPv4-mapped form always get the same answer.
func FuzzPublic(f *testing.F) {
	f.Add([]byte{10, 0, 0, 1})
	f.Add([]byte{104, 21, 32, 1})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) != 4 {
			return
		}
		v4 := netip.AddrFrom4([4]byte(b))
		mapped := netip.AddrFrom16(v4.As16())
		if Public(v4) != Public(mapped) {
			t.Fatalf("%s and %s disagree", v4, mapped)
		}
	})
}
