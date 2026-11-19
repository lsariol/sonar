package report

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	ok := Report{StartedAt: time.Now(), Counters: map[string]uint64{"games_completed": 1},
		Checks: []Check{{Name: "twitch_chat", OK: true, Message: "connected"}}}
	if err := Validate(ok); err != nil {
		t.Errorf("a good report: %v", err)
	}
	bad := map[string]Report{
		"started_at":    {},
		"snake_case":    {StartedAt: time.Now(), Counters: map[string]uint64{"Games Completed": 1}},
		"appears twice": {StartedAt: time.Now(), Checks: []Check{{Name: "a"}, {Name: "a"}}},
		"message":       {StartedAt: time.Now(), Checks: []Check{{Name: "a", Message: strings.Repeat("x", 201)}}},
	}
	for want, r := range bad {
		if err := Validate(r); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate = %v, want an error mentioning %q", err, want)
		}
	}
	many := Report{StartedAt: time.Now(), Counters: map[string]uint64{}}
	for i := range MaxCounters + 1 {
		many.Counters["c"+strconv.Itoa(i)] = 1
	}
	if err := Validate(many); err == nil {
		t.Error("too many counters: no error")
	}
}

func TestParsePort(t *testing.T) {
	for _, bad := range []string{"", "http", "0", "65536", "-1", "80x"} {
		if _, err := ParsePort(bad); err == nil {
			t.Errorf("ParsePort(%q): no error", bad)
		}
	}
	if p, err := ParsePort("8080"); err != nil || p != 8080 {
		t.Errorf("ParsePort(8080) = %d, %v", p, err)
	}
}

func TestFetch(t *testing.T) {
	answers := map[string]string{
		Path: `{"started_at":"2026-11-18T10:00:00Z","counters":{"games_completed":7},"checks":[{"name":"db","ok":true,"message":""}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect" + Path:
			http.Redirect(w, r, Path, http.StatusFound)
		case "/big" + Path:
			io.WriteString(w, strings.Repeat(" ", maxBody+10))
		default:
			io.WriteString(w, answers[r.URL.Path])
		}
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	c := NewClient()
	r, err := c.Fetch(context.Background(), host, port)
	if err != nil || r.Counters["games_completed"] != 7 || len(r.Checks) != 1 {
		t.Fatalf("Fetch = %+v, %v", r, err)
	}
	if _, err := c.Fetch(context.Background(), host, 1); err == nil || !strings.Contains(err.Error(), "GET http://") {
		t.Errorf("nothing listening: %v", err)
	}
}
