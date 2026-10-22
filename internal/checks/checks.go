// Package checks checks public addresses from outside, as a visitor would:
// an HTTPS GET through the internet. It only ever connects to
// addresses it has just verified are public, so a label can't point it at
// anything inside the network.
package checks

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	timeout   = 5 * time.Second
	slowAfter = 2 * time.Second
	maxBody   = 64 << 10
)

// Outcome is how one check went.
type Outcome string

const (
	OK     Outcome = "ok"     // answered 2xx or 3xx in time
	Slow   Outcome = "slow"   // answered correctly, but slower than 2 s
	Failed Outcome = "failed" // 4xx, 5xx, timeout, DNS, connection or TLS error
)

// Result is one check of one address.
type Result struct {
	At      time.Time
	Outcome Outcome
	Status  int           // the HTTP status; 0 without an answer
	Latency time.Duration // until the answer was read; 0 without one
	Error   string
	Refused bool // never contacted: the address isn't a public one
}

// Validate checks a public-url label's value and says why it can't be
// checked: https on port 443, and a public hostname (not an IP, not an
// internal name).
func Validate(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	switch {
	case err != nil:
		return nil, errors.New("isn't a URL")
	case u.Scheme != "https":
		return nil, errors.New("must start with https://")
	case u.User != nil:
		return nil, errors.New("mustn't contain a user or password")
	case u.Port() != "" && u.Port() != "443":
		return nil, errors.New("may only use port 443")
	case u.Hostname() == "":
		return nil, errors.New("has no hostname")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if _, err := netip.ParseAddr(host); err == nil {
		return nil, errors.New("must use a hostname, not an IP address")
	}
	if !strings.Contains(host, ".") || internalName(host) {
		return nil, errors.New("must be a public hostname such as plop.mobasity.com, not an internal name")
	}
	return u, nil
}

func internalName(host string) bool {
	for _, suffix := range []string{".local", ".internal", ".lan", ".home", ".localhost", ".localdomain"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// nonPublic are ranges netip's own tests don't cover: shared address space
// (CGNAT), benchmarking, documentation, reserved, and NAT64.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// Public reports whether a is an address on the public internet.
func Public(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// Resolver looks up a hostname's addresses (net.DefaultResolver).
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Checker runs outside checks.
type Checker struct {
	client    *http.Client
	userAgent string
	resolver  Resolver
	allowed   func(netip.Addr) bool
	slowAfter time.Duration
	port      string // tests only: dial this port instead of the URL's
}

// New returns a checker that identifies itself with Sonar's version.
func New(version string) *Checker {
	return newChecker(version, net.DefaultResolver, Public, nil)
}

func newChecker(version string, r Resolver, allowed func(netip.Addr) bool, tlsConfig *tls.Config) *Checker {
	c := &Checker{
		userAgent: "Sonar/" + version + " (status check; +https://mobasity.com/status)",
		resolver:  r,
		allowed:   allowed,
		slowAfter: slowAfter,
	}
	c.client = &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            c.dial,
			TLSClientConfig:        tlsConfig,
			TLSHandshakeTimeout:    timeout,
			DisableKeepAlives:      true,
			MaxResponseHeaderBytes: 64 << 10,
			ForceAttemptHTTP2:      true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c
}

// refusedError is a hostname that resolved to an address that isn't public.
type refusedError struct {
	host string
	addr netip.Addr
}

func (e refusedError) Error() string {
	return fmt.Sprintf("%s resolves to %s, which isn't a public address, so it wasn't contacted", e.host, e.addr)
}

// dial resolves the host, refuses it unless every address is public, and
// connects to a verified address, so DNS can't change between check and use.
func (c *Checker) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if c.port != "" {
		port = c.port
	}
	portNum, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("port %q: %w", port, err)
	}

	addrs, err := c.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("DNS: %w", err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("DNS: %s has no addresses", host)
	}
	for _, a := range addrs {
		if !c.allowed(a) {
			return nil, refusedError{host: host, addr: a}
		}
	}

	var d net.Dialer
	var lastErr error
	for _, a := range addrs {
		conn, err := d.DialContext(ctx, "tcp", netip.AddrPortFrom(a.Unmap(), uint16(portNum)).String())
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// Check fetches raw once, as a visitor would, without following redirects.
func (c *Checker) Check(ctx context.Context, raw string) Result {
	start := time.Now()
	r := Result{At: start.UTC(), Outcome: Failed}

	u, err := Validate(raw)
	if err != nil {
		r.Error, r.Refused = "label "+err.Error(), true
		return r
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.client.Do(req)
	if err != nil {
		var refused refusedError
		r.Refused = errors.As(err, &refused)
		r.Error = describe(err)
		return r
	}
	_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	resp.Body.Close()
	r.Latency = time.Since(start)
	r.Status = resp.StatusCode

	switch {
	case resp.StatusCode < 200 || resp.StatusCode >= 400:
		// Go's wording, not the site's: a status line can carry anything.
		r.Error = fmt.Sprintf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	case readErr != nil:
		r.Error = "reading the answer: " + describe(readErr)
	case r.Latency > c.slowAfter:
		r.Outcome = Slow
	default:
		r.Outcome = OK
	}
	return r
}

// describe is err without Go's "Get <url>:" prefix, and timeouts said plainly.
func describe(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return fmt.Sprintf("no answer within %s", timeout)
		}
		err = ue.Err
	}
	return err.Error()
}
