// Package cloudflare reads traffic per hostname from Cloudflare's GraphQL
// Analytics API: requests, 5xx answers and bytes, by hour. It only reads;
// the token needs Zone Read and Analytics Read, nothing else.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// API is Cloudflare's API.
	API = "https://api.cloudflare.com/client/v4"

	maxAnswer = 8 << 20
	maxGroups = 5000 // per query: hostnames × hours
)

// Client calls Cloudflare's API with one token.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New returns a client for the API at base with token.
func New(base, token string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second}}
}

// Zone is one zone the token can read.
type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Hour is one hostname's traffic in one hour.
type Hour struct {
	Host     string
	Hour     time.Time
	Requests int64
	Errors   int64 // answers with a 5xx status
	Bytes    int64 // sent to visitors
}

// Zones lists every zone the token can read.
func (c *Client) Zones(ctx context.Context) ([]Zone, error) {
	var zones []Zone
	for page := 1; ; page++ {
		var answer struct {
			Success    bool       `json:"success"`
			Errors     []apiError `json:"errors"`
			Result     []Zone     `json:"result"`
			ResultInfo struct {
				TotalPages int `json:"total_pages"`
			} `json:"result_info"`
		}
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/zones?per_page=50&page=%d", page), nil, &answer); err != nil {
			return nil, err
		}
		if !answer.Success {
			return nil, fmt.Errorf("Cloudflare: listing zones: %s", messages(answer.Errors))
		}
		zones = append(zones, answer.Result...)
		if page >= answer.ResultInfo.TotalPages || len(answer.Result) == 0 {
			return zones, nil
		}
	}
}

const trafficQuery = `query Traffic($zone: string!, $from: Time!, $to: Time!, $limit: uint64!) {
  viewer {
    zones(filter: {zoneTag: $zone}) {
      all: httpRequestsAdaptiveGroups(limit: $limit, filter: {datetime_geq: $from, datetime_lt: $to, requestSource: "eyeball"}) {
        count
        sum { edgeResponseBytes }
        dimensions { datetimeHour clientRequestHTTPHost }
      }
      errors: httpRequestsAdaptiveGroups(limit: $limit, filter: {datetime_geq: $from, datetime_lt: $to, requestSource: "eyeball", edgeResponseStatus_geq: 500, edgeResponseStatus_lt: 600}) {
        count
        dimensions { datetimeHour clientRequestHTTPHost }
      }
    }
  }
}`

type group struct {
	Count int64 `json:"count"`
	Sum   struct {
		EdgeResponseBytes int64 `json:"edgeResponseBytes"`
	} `json:"sum"`
	Dimensions struct {
		DatetimeHour time.Time `json:"datetimeHour"`
		Host         string    `json:"clientRequestHTTPHost"`
	} `json:"dimensions"`
}

// Traffic returns each hostname's traffic in zone, hour by hour, from from to
// to (whole hours).
func (c *Client) Traffic(ctx context.Context, zone string, from, to time.Time) ([]Hour, error) {
	body, err := json.Marshal(map[string]any{
		"query": trafficQuery,
		"variables": map[string]any{
			"zone": zone, "from": from.UTC().Format(time.RFC3339), "to": to.UTC().Format(time.RFC3339), "limit": maxGroups,
		},
	})
	if err != nil {
		return nil, err
	}
	var answer struct {
		Data struct {
			Viewer struct {
				Zones []struct {
					All    []group `json:"all"`
					Errors []group `json:"errors"`
				} `json:"zones"`
			} `json:"viewer"`
		} `json:"data"`
		Errors []apiError `json:"errors"`
	}
	if err := c.do(ctx, http.MethodPost, "/graphql", body, &answer); err != nil {
		return nil, err
	}
	if len(answer.Errors) > 0 {
		return nil, fmt.Errorf("Cloudflare: reading traffic: %s", messages(answer.Errors))
	}
	type key struct {
		host string
		hour time.Time
	}
	byKey := make(map[key]*Hour)
	var out []Hour
	for _, z := range answer.Data.Viewer.Zones {
		for _, g := range z.All {
			k := key{strings.ToLower(g.Dimensions.Host), g.Dimensions.DatetimeHour.UTC()}
			out = append(out, Hour{Host: k.host, Hour: k.hour, Requests: g.Count, Bytes: g.Sum.EdgeResponseBytes})
		}
	}
	for i := range out {
		byKey[key{out[i].Host, out[i].Hour}] = &out[i]
	}
	for _, z := range answer.Data.Viewer.Zones {
		for _, g := range z.Errors {
			if h := byKey[key{strings.ToLower(g.Dimensions.Host), g.Dimensions.DatetimeHour.UTC()}]; h != nil {
				h.Errors = g.Count
			}
		}
	}
	return out, nil
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func messages(errs []apiError) string {
	if len(errs) == 0 {
		return "no reason given"
	}
	var parts []string
	for _, e := range errs {
		parts = append(parts, e.Message)
	}
	return strings.Join(parts, "; ")
}

// do sends one request and decodes the JSON answer into v. The token is
// never part of an error.
func (c *Client) do(ctx context.Context, method, path string, body []byte, v any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // without the URL, which the message names already
		}
		return fmt.Errorf("Cloudflare: %s %s: %w", method, route(path), err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return fmt.Errorf("Cloudflare: %s %s: %w", method, route(path), err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("Cloudflare: %s %s: %s: SONAR_CLOUDFLARE_TOKEN needs Zone Read and Analytics Read on every zone", method, route(path), resp.Status)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("Cloudflare: %s %s: %s", method, route(path), resp.Status)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("Cloudflare: %s %s: unreadable answer: %w", method, route(path), err)
	}
	return nil
}

func route(path string) string {
	r, _, _ := strings.Cut(path, "?")
	return r
}
