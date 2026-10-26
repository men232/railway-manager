package railway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestTokenOrder(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	writeConfig(t, cfg, `{"user":{"token":"cfg-tok","accessToken":"cfg-access"}}`)
	now := func() time.Time { return time.Unix(1000, 0) }
	noRefresh := func(context.Context) error { t.Error("refresh must not be called"); return nil }

	tests := []struct {
		name       string
		env        map[string]string
		config     string
		wantToken  string
		wantSource string
	}{
		{"api token wins", map[string]string{"RAILWAY_API_TOKEN": "a", "RAILWAY_TOKEN": "b"}, "", "a", "env:RAILWAY_API_TOKEN"},
		{"railway token next", map[string]string{"RAILWAY_TOKEN": "b"}, "", "b", "env:RAILWAY_TOKEN"},
		{"config token", nil, "", "cfg-tok", "config"},
		{"config accessToken fallback", nil, `{"user":{"token":"","accessToken":"acc"}}`, "acc", "config"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.config != "" {
				writeConfig(t, cfg, tc.config)
			}
			st := NewTokenSource(envOf(tc.env), cfg, noRefresh, now).Get(context.Background())
			if st.Missing != "" || st.Token != tc.wantToken || st.Source != tc.wantSource {
				t.Errorf("got Source=%q Missing=%q (token match=%v), want source %q", st.Source, st.Missing, st.Token == tc.wantToken, tc.wantSource)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		st := NewTokenSource(envOf(nil), filepath.Join(dir, "nope.json"), nil, now).Get(context.Background())
		if !strings.Contains(st.Missing, "railway login") || st.Token != "" {
			t.Errorf("Missing = %q", st.Missing)
		}
	})
	t.Run("not logged in", func(t *testing.T) {
		p := filepath.Join(dir, "empty.json")
		writeConfig(t, p, `{"user":{}}`)
		st := NewTokenSource(envOf(nil), p, nil, now).Get(context.Background())
		if !strings.Contains(st.Missing, "not logged in") {
			t.Errorf("Missing = %q", st.Missing)
		}
	})
}

func TestTokenRefreshNearExpiry(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	base := time.Unix(1_000_000, 0)
	now := func() time.Time { return base }
	writeConfig(t, cfg, `{"user":{"token":"old","tokenExpiresAt":1000010}}`)

	var calls int32
	refresh := func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		writeConfig(t, cfg, `{"user":{"token":"fresh","tokenExpiresAt":1003600}}`)
		return nil
	}
	ts := NewTokenSource(envOf(nil), cfg, refresh, now)
	st := ts.Get(context.Background())
	if st.Token != "fresh" || atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("token fresh=%v calls=%d", st.Token == "fresh", calls)
	}
	if !st.ExpiresAt.Equal(time.Unix(1003600, 0)) {
		t.Errorf("ExpiresAt = %v", st.ExpiresAt)
	}
	ts.Get(context.Background())
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("second Get refreshed again: %d", calls)
	}

	t.Run("env never refreshes", func(t *testing.T) {
		ts := NewTokenSource(envOf(map[string]string{"RAILWAY_TOKEN": "e"}), cfg,
			func(context.Context) error { t.Error("refresh called"); return nil }, now)
		ts.Get(context.Background())
		if st := ts.Refresh(context.Background()); st.Token != "e" {
			t.Errorf("Refresh changed env token")
		}
	})
}

func TestTokenRefreshShared(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, cfg, `{"user":{"token":"old"}}`)
	const waiters = 7
	var calls int32
	entered := make(chan struct{})
	release := make(chan struct{})
	refresh := func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		close(entered)
		<-release
		writeConfig(t, cfg, `{"user":{"token":"new"}}`)
		return nil
	}
	ts := NewTokenSource(envOf(nil), cfg, refresh, time.Now).(*tokenSource)
	var joined sync.WaitGroup
	joined.Add(waiters)
	ts.onJoin = joined.Done

	var wg sync.WaitGroup
	results := make([]string, waiters+1)
	run := func(i int) {
		wg.Add(1)
		go func() { defer wg.Done(); results[i] = ts.Refresh(context.Background()).Token }()
	}
	run(0)
	<-entered // leader is inside refresh
	for i := 1; i <= waiters; i++ {
		run(i)
	}
	joined.Wait() // every other caller is waiting on the shared run
	close(release)
	wg.Wait()
	if calls != 1 {
		t.Errorf("refresh calls = %d, want 1", calls)
	}
	for _, r := range results {
		if r != "new" {
			t.Errorf("result %q", r)
		}
	}
}

func TestTokenReloadsOnMtimeChange(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, cfg, `{"user":{"token":"one"}}`)
	ts := NewTokenSource(envOf(nil), cfg, nil, time.Now)
	if ts.Get(context.Background()).Token != "one" {
		t.Fatal("first read")
	}
	writeConfig(t, cfg, `{"user":{"token":"two"}}`)
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(cfg, future, future); err != nil {
		t.Fatal(err)
	}
	if got := ts.Get(context.Background()).Token; got != "two" {
		t.Errorf("did not re-read changed config")
	}
}

func TestPeekNeverRefreshes(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	now := time.Unix(1_000_000, 0)
	// Expired: Get would refresh; Peek must not.
	writeConfig(t, cfg, `{"user":{"token":"one","tokenExpiresAt":999990}}`)
	ts := NewTokenSource(envOf(nil), cfg, func(context.Context) error {
		t.Error("Peek ran a refresh")
		return nil
	}, func() time.Time { return now })
	st := ts.Peek()
	if st.Token != "one" || st.Source != "config" || !st.ExpiresAt.Equal(time.Unix(999990, 0)) {
		t.Errorf("Peek = %+v", st)
	}
	writeConfig(t, cfg, `{"user":{"token":"two"}}`)
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(cfg, future, future); err != nil {
		t.Fatal(err)
	}
	if got := ts.Peek().Token; got != "two" {
		t.Errorf("Peek after mtime change = %q, want two", got)
	}
	env := NewTokenSource(envOf(map[string]string{"RAILWAY_TOKEN": "e"}), cfg, nil, nil)
	if st := env.Peek(); st.Source != "env:RAILWAY_TOKEN" {
		t.Errorf("env Peek = %+v", st)
	}
}
