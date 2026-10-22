package containers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{base: srv.URL, socket: "test.sock", http: srv.Client()}
}

func TestClientReadsAnswers(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/containers":
			io.WriteString(w, `{"containers":[{"id":"x","name":"plop","project":"plop","service":"plop","state":"running","health":"healthy"}],"failed":[]}`)
		case "/v1/stats":
			io.WriteString(w, `{"stats":[{"id":"x","cpu_ns":5}],"failed":[{"id":"y","reason":"timed out"}]}`)
		case "/healthz":
			io.WriteString(w, `{"ok":true}`)
		}
	})
	ctx := context.Background()

	list, err := c.Containers(ctx)
	if err != nil || len(list.Containers) != 1 || list.Containers[0].Health != "healthy" {
		t.Errorf("Containers = %+v, %v", list, err)
	}
	stats, err := c.Stats(ctx)
	if err != nil || len(stats.Stats) != 1 || stats.Stats[0].CPUNanos != 5 || stats.Failed[0].Reason != "timed out" {
		t.Errorf("Stats = %+v, %v", stats, err)
	}
	if err := c.Healthy(ctx); err != nil {
		t.Errorf("Healthy = %v", err)
	}
}

func TestClientErrors(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/containers":
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, `{"error":"Docker: connection refused"}`)
		case "/v1/stats":
			io.WriteString(w, `[`+strings.Repeat(" ", maxAnswer)+`]`)
		case "/healthz":
			io.WriteString(w, `not json`)
		}
	})
	ctx := context.Background()

	if _, err := c.Containers(ctx); err == nil || !strings.Contains(err.Error(), "Docker: connection refused") {
		t.Errorf("502: err = %v, want the proxy's error passed on", err)
	}
	if _, err := c.Stats(ctx); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("oversized: err = %v, want it refused", err)
	}
	if err := c.Healthy(ctx); err != nil {
		t.Errorf("Healthy only needs a 200, got %v", err)
	}
}

func TestClientUnreachable(t *testing.T) {
	err := NewClient("/nonexistent/sonar-test.sock").Healthy(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is sonar-proxy running") {
		t.Errorf("err = %v, want a hint to check sonar-proxy", err)
	}
}
