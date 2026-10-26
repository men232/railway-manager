// Package railway talks to the Railway GraphQL API: where the token comes from
// and how a request is sent, retried and re-authenticated.
package railway

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"
)

// expiryMargin: a config token this close to (or past) its expiry is
// refreshed before it is used.
const expiryMargin = 30 * time.Second

// TokenStatus is the outcome of resolving a token. A non-empty Missing means
// no token is available and says why.
type TokenStatus struct {
	Token     string
	Source    string // "env:RAILWAY_API_TOKEN", "env:RAILWAY_TOKEN" or "config"
	ExpiresAt time.Time
	Missing   string
	// RefreshErr is why the last refresh failed (never token content); the
	// Token is then the unchanged, possibly stale one.
	RefreshErr string
}

// TokenSource resolves the Railway token.
type TokenSource interface {
	// Get returns the current token, refreshing a config token that is about
	// to expire.
	Get(ctx context.Context) TokenStatus
	// Refresh asks the Railway CLI to renew its token and re-reads the
	// config. Concurrent calls share one run. Env tokens are returned as is.
	Refresh(ctx context.Context) TokenStatus
	// Peek returns the current token without refreshing it and without
	// running anything: the config is re-read only when its mtime changed.
	// It is safe on the UI goroutine.
	Peek() TokenStatus
}

type configFile struct {
	User struct {
		Token          string `json:"token"`
		AccessToken    string `json:"accessToken"`
		TokenExpiresAt *int64 `json:"tokenExpiresAt"`
	} `json:"user"`
}

type fileStamp struct {
	mtime time.Time
	size  int64
	ok    bool
}

type tokenSource struct {
	getenv     func(string) string
	configPath string
	refresh    func(context.Context) error
	now        func() time.Time

	mu     sync.Mutex // guards cache fields and inflight
	cached *TokenStatus
	stamp  fileStamp

	inflight *refreshCall
	onJoin   func() // test hook: a caller joined an in-flight refresh
}

type refreshCall struct {
	done   chan struct{}
	status TokenStatus
}

// NewTokenSource builds the default token source. Token order: env
// RAILWAY_API_TOKEN, env RAILWAY_TOKEN, then the Railway CLI config file
// (re-read only when its mtime changes). refresh runs the Railway CLI to renew
// the config token; it may be nil.
func NewTokenSource(getenv func(string) string, configPath string, refresh func(context.Context) error, now func() time.Time) TokenSource {
	if getenv == nil {
		getenv = os.Getenv
	}
	if now == nil {
		now = time.Now
	}
	return &tokenSource{getenv: getenv, configPath: configPath, refresh: refresh, now: now}
}

func (s *tokenSource) envStatus() (TokenStatus, bool) {
	for _, name := range []string{"RAILWAY_API_TOKEN", "RAILWAY_TOKEN"} {
		if v := strings.TrimSpace(s.getenv(name)); v != "" {
			return TokenStatus{Token: v, Source: "env:" + name}, true
		}
	}
	return TokenStatus{}, false
}

func (s *tokenSource) stat() fileStamp {
	fi, err := os.Stat(s.configPath)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{mtime: fi.ModTime(), size: fi.Size(), ok: true}
}

// readConfig always reads the file and updates the cache.
func (s *tokenSource) readConfig() TokenStatus {
	stamp := s.stat()
	st := s.parseConfig()
	s.mu.Lock()
	s.cached, s.stamp = &st, stamp
	s.mu.Unlock()
	return st
}

func (s *tokenSource) parseConfig() TokenStatus {
	data, err := os.ReadFile(s.configPath)
	if errors.Is(err, fs.ErrNotExist) {
		return TokenStatus{Missing: "no ~/.railway/config.json: run railway login"}
	}
	if err != nil {
		return TokenStatus{Missing: "cannot read ~/.railway/config.json: run railway login"}
	}
	var cfg configFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return TokenStatus{Missing: "~/.railway/config.json is not valid JSON"}
	}
	token := cfg.User.Token
	if token == "" {
		token = cfg.User.AccessToken
	}
	if token == "" {
		return TokenStatus{Missing: "not logged in: run railway login"}
	}
	st := TokenStatus{Token: token, Source: "config"}
	if cfg.User.TokenExpiresAt != nil {
		st.ExpiresAt = time.Unix(*cfg.User.TokenExpiresAt, 0)
	}
	return st
}

func (s *tokenSource) cachedConfig() TokenStatus {
	stamp := s.stat()
	s.mu.Lock()
	if s.cached != nil && s.stamp == stamp {
		st := *s.cached
		s.mu.Unlock()
		return st
	}
	s.mu.Unlock()
	return s.readConfig()
}

func (s *tokenSource) Get(ctx context.Context) TokenStatus {
	if st, ok := s.envStatus(); ok {
		return st
	}
	st := s.cachedConfig()
	if st.Missing == "" && !st.ExpiresAt.IsZero() && st.ExpiresAt.Sub(s.now()) < expiryMargin {
		return s.Refresh(ctx)
	}
	return st
}

func (s *tokenSource) Peek() TokenStatus {
	if st, ok := s.envStatus(); ok {
		return st
	}
	return s.cachedConfig()
}

func (s *tokenSource) Refresh(ctx context.Context) TokenStatus {
	if st, ok := s.envStatus(); ok {
		return st
	}
	s.mu.Lock()
	if call := s.inflight; call != nil {
		s.mu.Unlock()
		if s.onJoin != nil {
			s.onJoin()
		}
		select {
		case <-call.done:
			return call.status
		case <-ctx.Done():
			return TokenStatus{Missing: "token refresh cancelled: " + ctx.Err().Error()}
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	s.inflight = call
	s.mu.Unlock()

	// The shared run must not die with one waiter's context.
	runCtx := context.WithoutCancel(ctx)
	var refreshErr error
	if s.refresh != nil {
		refreshErr = s.refresh(runCtx)
	}
	call.status = s.readConfig()
	if refreshErr != nil {
		call.status.RefreshErr = refreshErr.Error()
	}

	s.mu.Lock()
	s.inflight = nil
	s.mu.Unlock()
	close(call.done)
	return call.status
}
