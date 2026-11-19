// Package report is the contract between Sonar and the apps it watches: an
// app serves its usage counters and feature checks at Path on its report
// port (the com.mobasity.report-port label), on the spark network, without a
// token, and Sonar reads them once a minute.
//
//	GET http://<container>:<port>/report/v1
//	{
//	  "started_at": "2026-11-18T10:00:00Z",
//	  "counters": {"games_completed": 1234},
//	  "checks": [{"name": "twitch_chat", "ok": true, "message": "connected to 3 channels"}]
//	}
//
// Counters are running totals since started_at (normally when the process
// started): they only grow, and start over from zero with a new started_at.
// Sonar keeps how much each grew per minute and per day. A check says
// whether one of the app's own features works; Sonar keeps when it changed.
// Nothing in a report may be a secret or someone's data: it's served
// without a token.
package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

// Path is where an app serves its report.
const Path = "/report/v1"

// Limits on a report, so one app can't flood Sonar's history.
const (
	MaxCounters = 50
	MaxChecks   = 20
	MaxMessage  = 200
	maxBody     = 64 << 10
	timeout     = 5 * time.Second
)

// namePattern is a counter's or check's name: lowercase snake_case.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Report is what an app serves.
type Report struct {
	StartedAt time.Time         `json:"started_at"`
	Counters  map[string]uint64 `json:"counters"`
	Checks    []Check           `json:"checks"`
}

// Check is one of the app's features and whether it works.
type Check struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// Validate says what's wrong with r, nil when nothing is.
func Validate(r Report) error {
	var errs []error
	if r.StartedAt.IsZero() {
		errs = append(errs, errors.New("started_at is missing"))
	}
	if len(r.Counters) > MaxCounters {
		errs = append(errs, fmt.Errorf("%d counters, at most %d", len(r.Counters), MaxCounters))
	}
	for name := range r.Counters {
		if !namePattern.MatchString(name) {
			errs = append(errs, fmt.Errorf("counter %q isn't a lowercase snake_case name", name))
		}
	}
	if len(r.Checks) > MaxChecks {
		errs = append(errs, fmt.Errorf("%d checks, at most %d", len(r.Checks), MaxChecks))
	}
	seen := make(map[string]bool)
	for _, c := range r.Checks {
		switch {
		case !namePattern.MatchString(c.Name):
			errs = append(errs, fmt.Errorf("check %q isn't a lowercase snake_case name", c.Name))
		case seen[c.Name]:
			errs = append(errs, fmt.Errorf("check %q appears twice", c.Name))
		case len(c.Message) > MaxMessage:
			errs = append(errs, fmt.Errorf("check %q's message is %d bytes, at most %d", c.Name, len(c.Message), MaxMessage))
		}
		seen[c.Name] = true
	}
	return errors.Join(errs...)
}

// ParsePort reads a report-port label: a port number.
func ParsePort(label string) (int, error) {
	port, err := strconv.Atoi(label)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("isn't a port number from 1 to 65535")
	}
	return port, nil
}

// Client fetches reports.
type Client struct {
	http *http.Client
}

// NewClient returns a client that never follows redirects.
func NewClient() *Client {
	return &Client{http: &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Fetch reads the report of the container named container on port.
func (c *Client) Fetch(ctx context.Context, container string, port int) (Report, error) {
	addr := "http://" + net.JoinHostPort(container, strconv.Itoa(port)) + Path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return Report{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Report{}, fmt.Errorf("GET %s: %w", addr, unwrap(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Report{}, fmt.Errorf("GET %s: %s", addr, http.StatusText(resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return Report{}, fmt.Errorf("GET %s: %w", addr, err)
	}
	if len(data) > maxBody {
		return Report{}, fmt.Errorf("GET %s: the report is over %d KB", addr, maxBody>>10)
	}
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return Report{}, fmt.Errorf("GET %s: not a report: %w", addr, err)
	}
	if err := Validate(r); err != nil {
		return Report{}, fmt.Errorf("GET %s: %w", addr, err)
	}
	return r, nil
}

// unwrap drops the method and URL from a client error; the caller names them.
func unwrap(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
