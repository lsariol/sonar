package checks

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	ok := []string{
		"https://plop.mobasity.com",
		"https://mobasity.com/status",
		"https://danshardwood.com:443/",
		" https://Plop.Mobasity.com ",
	}
	for _, raw := range ok {
		if _, err := Validate(raw); err != nil {
			t.Errorf("Validate(%q) = %v, want ok", raw, err)
		}
	}

	refused := map[string]string{
		"http://plop.mobasity.com":          "https://",
		"plop.mobasity.com":                 "https://",
		"https://user:pw@plop.mobasity.com": "user or password",
		"https://plop.mobasity.com:8443":    "port 443",
		"https://10.0.0.159":                "not an IP",
		"https://[::1]/":                    "not an IP",
		"https://cove":                      "internal name",
		"https://sparkdb:5432":              "port 443",
		"https://spark.local":               "internal name",
		"https://printer.lan/":              "internal name",
		"https:///nohost":                   "no hostname",
		"https://%zz":                       "isn't a URL",
	}
	for raw, want := range refused {
		_, err := Validate(raw)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate(%q) = %v, want an error mentioning %q", raw, err, want)
		}
	}
}

func TestPublic(t *testing.T) {
	public := []string{"104.21.32.1", "172.67.1.1", "8.8.8.8", "2606:4700::6810:1", "::ffff:104.21.32.1"}
	for _, s := range public {
		if !Public(netip.MustParseAddr(s)) {
			t.Errorf("Public(%s) = false, want true", s)
		}
	}
	private := []string{
		"10.0.0.159", "172.18.0.9", "192.168.1.1", "127.0.0.1", "0.0.0.0", "169.254.169.254",
		"100.64.0.1", "198.18.0.1", "192.0.2.1", "255.255.255.255", "224.0.0.1",
		"::1", "::", "fe80::1", "fd00::1", "64:ff9b::a00:1", "2001:db8::1", "::ffff:10.0.0.1",
	}
	for _, s := range private {
		if Public(netip.MustParseAddr(s)) {
			t.Errorf("Public(%s) = true, want false", s)
		}
	}
}

// fakeDNS answers every lookup with addrs.
type fakeDNS struct{ addrs []netip.Addr }

func (f fakeDNS) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	if f.addrs == nil {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return f.addrs, nil
}

// testChecker checks srv as if it were https://plop.mobasity.com. allowLocal
// lets it reach the test server on 127.0.0.1; without it the real rule holds.
func testChecker(t *testing.T, srv *httptest.Server, dns fakeDNS, allowLocal bool) *Checker {
	t.Helper()
	allowed := Public
	if allowLocal {
		allowed = func(a netip.Addr) bool { return a.IsLoopback() || Public(a) }
	}
	pool := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	c := newChecker("v0.1.0", dns, allowed, &tls.Config{RootCAs: pool, ServerName: "example.com"})
	_, c.port, _ = net.SplitHostPort(srv.Listener.Addr().String())
	return c
}

var local = fakeDNS{addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}

func TestCheckOutcomes(t *testing.T) {
	var redirectedTo atomic.Bool
	var userAgent atomic.Value
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userAgent.Store(r.UserAgent())
		switch r.URL.Path {
		case "/":
			w.Write([]byte(strings.Repeat("x", 200<<10)))
		case "/moved":
			http.Redirect(w, r, "/elsewhere", http.StatusMovedPermanently)
		case "/elsewhere":
			redirectedTo.Store(true)
		case "/broken":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/missing":
			http.NotFound(w, r)
		case "/slow":
			time.Sleep(150 * time.Millisecond)
		}
	}))
	defer srv.Close()

	c := testChecker(t, srv, local, true)
	c.slowAfter = 100 * time.Millisecond
	ctx := context.Background()

	r := c.Check(ctx, "https://plop.mobasity.com/")
	if r.Outcome != OK || r.Status != 200 || r.Latency <= 0 || r.Error != "" {
		t.Errorf("ok: %+v", r)
	}
	if ua, _ := userAgent.Load().(string); !strings.HasPrefix(ua, "Sonar/v0.1.0 ") {
		t.Errorf("User-Agent = %q", ua)
	}

	r = c.Check(ctx, "https://plop.mobasity.com/moved")
	if r.Outcome != OK || r.Status != 301 || redirectedTo.Load() {
		t.Errorf("redirect: %+v, followed = %v; want ok, not followed", r, redirectedTo.Load())
	}

	r = c.Check(ctx, "https://plop.mobasity.com/broken")
	if r.Outcome != Failed || r.Status != 500 || !strings.Contains(r.Error, "500") {
		t.Errorf("500: %+v, want failed", r)
	}
	if r := c.Check(ctx, "https://plop.mobasity.com/missing"); r.Outcome != Failed || r.Status != 404 {
		t.Errorf("404: %+v, want failed", r)
	}

	r = c.Check(ctx, "https://plop.mobasity.com/slow")
	if r.Outcome != Slow || r.Status != 200 {
		t.Errorf("slow: %+v", r)
	}
}

// TestHostileStatusLine: a site's status line can carry terminal control
// codes; the check's error uses Go's wording instead.
func TestHostileStatusLine(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		buf.WriteString("HTTP/1.1 500 \x1b[2J\x1b]0;owned\x07Totally fine\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		buf.Flush()
	}))
	defer srv.Close()

	r := testChecker(t, srv, local, true).Check(context.Background(), "https://plop.mobasity.com/")
	if r.Outcome != Failed || r.Status != 500 || r.Error != "HTTP 500 Internal Server Error" {
		t.Errorf("result = %+v, want Go's own wording for a 500", r)
	}
}

func TestCheckTimeout(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r := testChecker(t, srv, local, true).Check(ctx, "https://plop.mobasity.com/")
	if r.Outcome != Failed || r.Status != 0 || !strings.Contains(r.Error, "no answer within") {
		t.Errorf("timeout: %+v", r)
	}
}

// TestGuard checks that addresses which aren't public are never contacted.
func TestGuard(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	ctx := context.Background()

	cases := map[string]fakeDNS{
		"loopback":            local,
		"home network":        {addrs: []netip.Addr{netip.MustParseAddr("10.0.0.159")}},
		"docker network":      {addrs: []netip.Addr{netip.MustParseAddr("172.18.0.9")}},
		"public and private":  {addrs: []netip.Addr{netip.MustParseAddr("104.21.32.1"), netip.MustParseAddr("127.0.0.1")}},
		"cloud metadata":      {addrs: []netip.Addr{netip.MustParseAddr("169.254.169.254")}},
		"IPv4-mapped private": {addrs: []netip.Addr{netip.MustParseAddr("::ffff:127.0.0.1")}},
	}
	for name, dns := range cases {
		r := testChecker(t, srv, dns, false).Check(ctx, "https://plop.mobasity.com/")
		if r.Outcome != Failed || !r.Refused || !strings.Contains(r.Error, "isn't a public address") {
			t.Errorf("%s: %+v, want refused", name, r)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the server was contacted %d times; refused addresses must never be", n)
	}

	r := testChecker(t, srv, local, false).Check(ctx, "https://cove/")
	if !r.Refused || !strings.Contains(r.Error, "label must be a public hostname") {
		t.Errorf("internal name: %+v, want refused before any lookup", r)
	}
}

func TestDNSFailure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	r := testChecker(t, srv, fakeDNS{}, true).Check(context.Background(), "https://gone.mobasity.com/")
	if r.Outcome != Failed || r.Refused || !strings.Contains(r.Error, "DNS:") {
		t.Errorf("DNS failure: %+v", r)
	}
}

func TestDescribe(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "https://x", Err: errors.New("connection refused")}
	if got := describe(err); got != "connection refused" {
		t.Errorf("describe = %q", got)
	}
}
