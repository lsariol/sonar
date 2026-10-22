package containers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// maxAnswer is the largest answer the client reads from the proxy.
const maxAnswer = 4 << 20

// Client asks sonar-proxy for containers and stats over its socket.
type Client struct {
	base   string
	socket string
	http   *http.Client
}

// NewClient returns a client for the proxy listening on socket.
func NewClient(socket string) *Client {
	return &Client{
		base:   "http://sonar-proxy",
		socket: socket,
		http: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
			},
		},
	}
}

// Containers returns every container, stopped ones included, and the ones
// Docker couldn't inspect.
func (c *Client) Containers(ctx context.Context) (ContainersAnswer, error) {
	var answer ContainersAnswer
	err := c.get(ctx, "/v1/containers", &answer)
	return answer, err
}

// Stats returns every running container's stats.
func (c *Client) Stats(ctx context.Context) (StatsAnswer, error) {
	var answer StatsAnswer
	err := c.get(ctx, "/v1/stats", &answer)
	return answer, err
}

// Healthy returns nil when the proxy answers and can reach Docker.
func (c *Client) Healthy(ctx context.Context) error {
	return c.get(ctx, "/healthz", nil)
}

func (c *Client) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("proxy: %w: is sonar-proxy running, with its socket at %s?", err, c.socket)
	}
	defer resp.Body.Close()

	body := io.LimitReader(resp.Body, maxAnswer+1)
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("proxy: GET %s: %s", path, e.Error)
	}
	if v == nil {
		return nil
	}

	data, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("proxy: GET %s: %w", path, err)
	}
	if len(data) > maxAnswer {
		return fmt.Errorf("proxy: GET %s: answer over %d bytes, refused", path, maxAnswer)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("proxy: GET %s: unreadable answer: %w", path, err)
	}
	return nil
}
