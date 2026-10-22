// Package api is sonar-api: it serves Sonar's data to authenticated
// consumers on the spark network. Every answer is built from a
// strict projection; nothing internal is served as is.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lsariol/sonar/internal/snapshot"
	"github.com/lsariol/sonar/internal/store"
)

const (
	// Port is where the API listens, inside its container on spark.
	Port = 2500

	statusCacheFor  = 10 * time.Second
	historyCacheFor = 30 * time.Second
	queryTimeout    = 3 * time.Second
	requestTimeout  = 5 * time.Second
	maxDays         = 380
	connectRetry    = 5 * time.Second
)

// Reader is the database, as the API reads it (sonar_reader).
type Reader interface {
	ProjectDays(ctx context.Context, from, to time.Time) ([]store.DayCounts, error)
	ResponseTimes(ctx context.Context, since time.Time) (map[string]int, error)
	HostHistory(ctx context.Context, from, to time.Time, bucket time.Duration) ([]store.HostBucket, error)
	CheckSchemaVersion(ctx context.Context) error
	Ping(ctx context.Context) error
}

// Snapshots is where the agent's latest snapshot comes from.
type Snapshots interface {
	Load() (snapshot.Snapshot, error)
}

// Options are the server's parts; tests swap them for fakes.
type Options struct {
	Version   string
	Getenv    func(string) string // where tokens are read from
	Snapshots Snapshots
	Log       *slog.Logger
	Now       func() time.Time // time.Now unless set

	// OpenReader connects to the database; the server retries until it works.
	OpenReader func(ctx context.Context) (Reader, error)
}

// Server answers the API.
type Server struct {
	opts  Options
	creds []credential

	mu    sync.Mutex
	db    Reader
	dbErr error

	status  *cache[int]
	history *cache[string]
	refused refusals
}

// New returns a server, or an error when a consumer's token is missing.
func New(opts Options) (*Server, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	creds, err := loadCredentials(opts.Getenv, consumers)
	if err != nil {
		return nil, err
	}
	return &Server{opts: opts, creds: creds, dbErr: errors.New("not connected yet"),
		status: newCache[int](statusCacheFor), history: newCache[string](historyCacheFor)}, nil
}

// Handler routes the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /v1/status", s.require(statusRead, s.statusHandler))
	mux.HandleFunc("GET /v1/status/history", s.require(statusRead, s.historyHandler))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/v1/status", "/v1/status/history":
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "only GET is supported")
		default:
			writeError(w, http.StatusNotFound, "no such endpoint: the API serves /healthz, /v1/status and /v1/status/history")
		}
	})
	return http.TimeoutHandler(securityHeaders(mux), requestTimeout, `{"error":"the request took too long"}`)
}

// Serve listens on addr until ctx is cancelled, connecting to the database
// in the background.
func (s *Server) Serve(ctx context.Context, addr string) error {
	go s.connect(ctx)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("api: listening on %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	s.opts.Log.Info("API listening", "addr", addr, "consumers", len(s.creds))
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("api: %w", err)
	}
	return nil
}

// connect opens the database, retrying until it answers with the schema this
// build needs (the agent applies migrations).
func (s *Server) connect(ctx context.Context) {
	for {
		db, err := s.opts.OpenReader(ctx)
		if err == nil {
			cctx, cancel := context.WithTimeout(ctx, queryTimeout)
			err = db.CheckSchemaVersion(cctx)
			cancel()
		}
		s.mu.Lock()
		if err == nil {
			s.db, s.dbErr = db, nil
		} else if s.dbErr == nil || s.dbErr.Error() != err.Error() {
			s.opts.Log.Warn("waiting for the database", "err", err)
			s.dbErr = err
		} else {
			s.dbErr = err
		}
		s.mu.Unlock()
		if err == nil {
			s.opts.Log.Info("database ready")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(connectRetry):
		}
	}
}

func (s *Server) reader() (Reader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, s.dbErr
	}
	return s.db, nil
}

// healthz is unauthenticated, so it says only whether the API works.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	db, err := s.reader()
	if err == nil {
		err = db.Ping(r.Context())
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "version": s.opts.Version})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.opts.Version})
}

// require lets a request through only with a token whose consumer has scope.
func (s *Server) require(sc scope, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearer(r.Header.Get("Authorization"))
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sonar"`)
			writeError(w, http.StatusUnauthorized, "a token is required: Authorization: Bearer <token>")
			return
		}
		c, ok := identify(s.creds, token)
		if !ok {
			s.refused.note(s.opts.Log, s.opts.Now(), "an unknown token", r)
			w.Header().Set("WWW-Authenticate", `Bearer realm="sonar", error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, "unknown token")
			return
		}
		if !c.may(sc) {
			s.refused.note(s.opts.Log, s.opts.Now(), fmt.Sprintf("%s's token, which doesn't have %s", c.Name, sc), r)
			writeError(w, http.StatusForbidden, fmt.Sprintf("this token doesn't have %s", sc))
			return
		}
		next(w, r)
	}
}

// refusals logs refused requests: the first at once, then at most one summary
// a minute, so a flood of bad requests can't flood the log (and the disk).
type refusals struct {
	mu      sync.Mutex
	logged  time.Time
	pending int
}

func (f *refusals) note(log *slog.Logger, now time.Time, why string, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending++
	if now.Sub(f.logged) < time.Minute {
		return
	}
	log.Warn("refused requests", "count", f.pending, "since", f.logged.UTC().Format(time.RFC3339),
		"latest", why, "path", r.URL.Path, "from", r.RemoteAddr)
	f.logged, f.pending = now, 0
}

func bearer(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

func (s *Server) statusHandler(w http.ResponseWriter, r *http.Request) {
	days := 90
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxDays {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("days must be a number from 1 to %d", maxDays))
			return
		}
		days = n
	}
	s.answer(w, r, func(db Reader, now time.Time) ([]byte, error) {
		return s.status.get(days, now, func(ctx context.Context) (any, error) {
			return s.buildStatus(ctx, db, now, days)
		})
	})
}

func (s *Server) historyHandler(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("range")
	if name == "" {
		name = "24h"
	}
	hr, ok := historyRanges[name]
	if !ok {
		writeError(w, http.StatusBadRequest, "range must be 1h, 24h, 7d or 30d")
		return
	}
	s.answer(w, r, func(db Reader, now time.Time) ([]byte, error) {
		return s.history.get(name, now, func(ctx context.Context) (any, error) {
			from, to := historySpan(now, hr)
			buckets, err := db.HostHistory(ctx, from, to, hr.bucket)
			if err != nil {
				return nil, err
			}
			return buildHistory(now, name, hr, buckets), nil
		})
	})
}

func (s *Server) buildStatus(ctx context.Context, db Reader, now time.Time, days int) (statusAnswer, error) {
	var snap *snapshot.Snapshot
	if sn, err := s.opts.Snapshots.Load(); err == nil {
		snap = &sn
	} else {
		s.opts.Log.Warn("no snapshot to serve", "err", err)
	}
	from := windowOf(now, max(days, 90)).first
	rows, err := db.ProjectDays(ctx, from, now)
	if err != nil {
		return statusAnswer{}, err
	}
	response, err := db.ResponseTimes(ctx, now.Add(-time.Hour))
	if err != nil {
		return statusAnswer{}, err
	}
	return buildStatus(now, snap, days, rows, response), nil
}

// answer serves what get builds, or 503 while the database can't answer.
func (s *Server) answer(w http.ResponseWriter, r *http.Request, get func(db Reader, now time.Time) ([]byte, error)) {
	db, err := s.reader()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "the database isn't ready yet; try again shortly")
		return
	}
	body, err := get(db, s.opts.Now())
	if err != nil {
		s.opts.Log.Error("building an answer failed", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusServiceUnavailable, "the database didn't answer; try again shortly")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// cache keeps built answers for a while, by key.
type cache[K comparable] struct {
	ttl time.Duration

	mu      sync.Mutex
	entries map[K]cached
}

type cached struct {
	body    []byte
	expires time.Time
}

func newCache[K comparable](ttl time.Duration) *cache[K] {
	return &cache[K]{ttl: ttl, entries: make(map[K]cached)}
}

// get returns key's answer while it's fresh, otherwise builds it. The lock is
// held while building, so a burst of requests costs one query; the build
// isn't tied to one request, so a caller hanging up doesn't fail the others.
func (c *cache[K]) get(key K, now time.Time, build func(ctx context.Context) (any, error)) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok && now.Before(e.expires) {
		return e.body, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	v, err := build(ctx)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	c.entries[key] = cached{body: append(body, '\n'), expires: now.Add(c.ttl)}
	return c.entries[key].body, nil
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
