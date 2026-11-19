package cloudflare

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const token = "cf-token-SECRET-MARKER"

func fake(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`, http.StatusForbidden)
			return
		}
		handle(w, r)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, token)
}

func TestZones(t *testing.T) {
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			io.WriteString(w, `{"success":true,"result":[{"id":"z1","name":"mobasity.com"}],"result_info":{"total_pages":2}}`)
		case "2":
			io.WriteString(w, `{"success":true,"result":[{"id":"z2","name":"lucassariol.com"}],"result_info":{"total_pages":2}}`)
		}
	})
	zones, err := c.Zones(context.Background())
	if err != nil || len(zones) != 2 || zones[1].ID != "z2" {
		t.Errorf("zones = %+v, %v", zones, err)
	}

	bad := New(c.base, "wrong")
	_, err = bad.Zones(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Zone Read and Analytics Read") || strings.Contains(err.Error(), "wrong") {
		t.Errorf("a refused token: %v", err)
	}
}

func TestTraffic(t *testing.T) {
	hour := time.Date(2026, 11, 18, 14, 0, 0, 0, time.UTC)
	var got map[string]any
	c := fake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"data":{"viewer":{"zones":[{
			"all":[
				{"count":120,"sum":{"edgeResponseBytes":50000},"dimensions":{"datetimeHour":"2026-11-18T14:00:00Z","clientRequestHTTPHost":"Plop.Mobasity.com"}},
				{"count":7,"sum":{"edgeResponseBytes":900},"dimensions":{"datetimeHour":"2026-11-18T15:00:00Z","clientRequestHTTPHost":"dibs.mobasity.com"}}],
			"errors":[
				{"count":3,"dimensions":{"datetimeHour":"2026-11-18T14:00:00Z","clientRequestHTTPHost":"plop.mobasity.com"}}]
		}]}},"errors":null}`)
	})
	hours, err := c.Traffic(context.Background(), "z1", hour, hour.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(hours) != 2 || hours[0] != (Hour{Host: "plop.mobasity.com", Hour: hour, Requests: 120, Errors: 3, Bytes: 50000}) ||
		hours[1].Host != "dibs.mobasity.com" || hours[1].Errors != 0 {
		t.Errorf("hours = %+v", hours)
	}
	vars := got["variables"].(map[string]any)
	if vars["zone"] != "z1" || vars["from"] != "2026-11-18T14:00:00Z" || vars["to"] != "2026-11-18T16:00:00Z" {
		t.Errorf("variables = %v", vars)
	}

	failing := fake(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":null,"errors":[{"message":"zone 'z1' does not have access to the path"}]}`)
	})
	if _, err := failing.Traffic(context.Background(), "z1", hour, hour.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "does not have access") {
		t.Errorf("a GraphQL error: %v", err)
	}
	broken := fake(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "<html>") })
	if _, err := broken.Traffic(context.Background(), "z1", hour, hour.Add(time.Hour)); err == nil || strings.Contains(err.Error(), token) {
		t.Errorf("an unreadable answer: %v", err)
	}
}
