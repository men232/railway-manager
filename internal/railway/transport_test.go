package railway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type staticTokens struct{ st TokenStatus }

func (s staticTokens) Get(context.Context) TokenStatus     { return s.st }
func (s staticTokens) Refresh(context.Context) TokenStatus { return s.st }
func (s staticTokens) Peek() TokenStatus                   { return s.st }

func newTransport(url string, ts TokenSource) *Transport {
	return &Transport{Endpoint: url, HTTP: http.DefaultClient, Tokens: ts, RetryDelay: 5 * time.Millisecond}
}

func envTokens(tok string) TokenSource {
	return staticTokens{TokenStatus{Token: tok, Source: "env:RAILWAY_TOKEN"}}
}

func TestTransportSendsBearerAndBody(t *testing.T) {
	var auth string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"data":{"me":{"id":"u1"}}}`))
	}))
	defer srv.Close()

	var out struct{ Me struct{ ID string } }
	err := newTransport(srv.URL, envTokens("tok")).Do(context.Background(), "Op", "query Op { me { id } }", map[string]any{"a": 1.0}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok" {
		t.Errorf("Authorization = %q", auth)
	}
	if body["query"] != "query Op { me { id } }" || body["operationName"] != "Op" {
		t.Errorf("body = %v", body)
	}
	if v, _ := body["variables"].(map[string]any); v["a"] != 1.0 {
		t.Errorf("variables = %v", body["variables"])
	}
	if out.Me.ID != "u1" {
		t.Errorf("out = %+v", out)
	}
}

func TestTransportAuthError(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"http 401": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) },
		"graphql": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"errors":[{"message":"Not Authorized"}]}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			err := newTransport(srv.URL, envTokens("secret-tok")).Do(context.Background(), "Op", "q", nil, nil)
			var ae *AuthError
			if !errors.As(err, &ae) {
				t.Fatalf("err = %v, want *AuthError", err)
			}
			if strings.Contains(err.Error(), "secret-tok") {
				t.Error("error leaks token")
			}
		})
	}
}

func TestTransportRefreshesRejectedConfigToken(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	write := func(tok string) {
		if err := os.WriteFile(cfg, []byte(`{"user":{"token":"`+tok+`"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("old")
	var refreshes int32
	ts := NewTokenSource(func(string) string { return "" }, cfg, func(context.Context) error {
		atomic.AddInt32(&refreshes, 1)
		write("new")
		return nil
	}, time.Now)

	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := r.Header.Get("Authorization")
		seen = append(seen, a)
		if a == "Bearer old" {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	}))
	defer srv.Close()

	var out struct{ OK bool }
	if err := newTransport(srv.URL, ts).Do(context.Background(), "Op", "q", nil, &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || len(seen) != 2 || seen[1] != "Bearer new" || refreshes != 1 {
		t.Errorf("seen=%v refreshes=%d out=%+v", seen, refreshes, out)
	}
}

func TestTransportEnvTokenRejectedNotRefreshed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	err := newTransport(srv.URL, envTokens("t")).Do(context.Background(), "Op", "q", nil, nil)
	var ae *AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v", err)
	}
}

func TestTransportMissingToken(t *testing.T) {
	ts := staticTokens{TokenStatus{Missing: "not logged in: run railway login"}}
	err := newTransport("http://127.0.0.1:1", ts).Do(context.Background(), "Op", "q", nil, nil)
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Msg != "not logged in: run railway login" {
		t.Fatalf("err = %v", err)
	}
}

func TestTransportRetriesOnceOn5xx(t *testing.T) {
	t.Run("502 then 200", func(t *testing.T) {
		var n int32
		var first, second time.Time
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&n, 1) == 1 {
				first = time.Now()
				w.WriteHeader(502)
				return
			}
			second = time.Now()
			_, _ = w.Write([]byte(`{"data":{}}`))
		}))
		defer srv.Close()
		tr := newTransport(srv.URL, envTokens("t"))
		tr.RetryDelay = 40 * time.Millisecond
		if err := tr.Do(context.Background(), "Op", "q", nil, nil); err != nil {
			t.Fatal(err)
		}
		if n != 2 || second.Sub(first) < 40*time.Millisecond {
			t.Errorf("requests=%d gap=%v", n, second.Sub(first))
		}
	})
	t.Run("502 twice", func(t *testing.T) {
		var n int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&n, 1)
			w.WriteHeader(502)
		}))
		defer srv.Close()
		if err := newTransport(srv.URL, envTokens("t")).Do(context.Background(), "Op", "q", nil, nil); err == nil || n != 2 {
			t.Errorf("err=%v requests=%d", err, n)
		}
	})
	t.Run("400 no retry", func(t *testing.T) {
		var n int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&n, 1)
			w.WriteHeader(400)
		}))
		defer srv.Close()
		if err := newTransport(srv.URL, envTokens("t")).Do(context.Background(), "Op", "q", nil, nil); err == nil || n != 1 {
			t.Errorf("err=%v requests=%d", err, n)
		}
	})
	t.Run("429 retried", func(t *testing.T) {
		var n int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&n, 1) == 1 {
				w.WriteHeader(429)
				return
			}
			_, _ = w.Write([]byte(`{"data":{}}`))
		}))
		defer srv.Close()
		if err := newTransport(srv.URL, envTokens("t")).Do(context.Background(), "Op", "q", nil, nil); err != nil || n != 2 {
			t.Errorf("err=%v requests=%d", err, n)
		}
	})
}

func TestTransportNoRetryOnCancel(t *testing.T) {
	var n int32
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		_, _ = io.Copy(io.Discard, r.Body) // lets the server notice the disconnect
		cancel()
		<-r.Context().Done()
	}))
	defer srv.Close()
	err := newTransport(srv.URL, envTokens("t")).Do(ctx, "Op", "q", nil, nil)
	if !errors.Is(err, context.Canceled) || n != 1 {
		t.Errorf("err=%v requests=%d", err, n)
	}
}

func TestTransportPartialErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"p0":{"id":"x"},"p1":null},"errors":[{"message":"Project not found","path":["p1"]}]}`))
	}))
	defer srv.Close()
	var out struct{ P0 struct{ ID string } }
	err := newTransport(srv.URL, envTokens("t")).Do(context.Background(), "Op", "q", nil, &out)
	var ge *GraphQLError
	if !errors.As(err, &ge) {
		t.Fatalf("err = %v, want *GraphQLError", err)
	}
	if out.P0.ID != "x" {
		t.Errorf("out not decoded: %+v", out)
	}
	if len(ge.Errors) != 1 || ge.Errors[0].Message != "Project not found" || len(ge.Errors[0].Path) != 1 {
		t.Errorf("errors = %+v", ge.Errors)
	}
}

func TestTransport403IsPlainError(t *testing.T) {
	var refreshes int32
	cfg := filepath.Join(t.TempDir(), "config.json")
	_ = os.WriteFile(cfg, []byte(`{"user":{"token":"t"}}`), 0o600)
	ts := NewTokenSource(func(string) string { return "" }, cfg, func(context.Context) error {
		atomic.AddInt32(&refreshes, 1)
		return nil
	}, time.Now)
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(403)
	}))
	defer srv.Close()
	err := newTransport(srv.URL, ts).Do(context.Background(), "Op", "q", nil, nil)
	var ae *AuthError
	if err == nil || errors.As(err, &ae) || strings.Contains(err.Error(), "railway login") || refreshes != 0 || n != 1 {
		t.Errorf("err=%v refreshes=%d requests=%d", err, refreshes, n)
	}
}

func TestTransportRefreshFailure(t *testing.T) {
	for name, cfgBody := range map[string]string{
		"after 401":   `{"user":{"token":"old"}}`,
		"near expiry": `{"user":{"token":"old","tokenExpiresAt":1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := filepath.Join(t.TempDir(), "config.json")
			_ = os.WriteFile(cfg, []byte(cfgBody), 0o600)
			var refreshes, requests int32
			ts := NewTokenSource(func(string) string { return "" }, cfg, func(context.Context) error {
				atomic.AddInt32(&refreshes, 1)
				return errors.New("exec: railway: not found")
			}, time.Now)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&requests, 1)
				w.WriteHeader(401)
			}))
			defer srv.Close()
			err := newTransport(srv.URL, ts).Do(context.Background(), "Op", "q", nil, nil)
			var ae *AuthError
			if !errors.As(err, &ae) || !strings.Contains(ae.Msg, "token refresh failed: exec: railway: not found") ||
				strings.Contains(ae.Msg, "old") || refreshes != 1 || requests != 1 {
				t.Errorf("err=%v refreshes=%d requests=%d", err, refreshes, requests)
			}
		})
	}
}

func TestTransportCancelledWhileWaitingOnRefresh(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	_ = os.WriteFile(cfg, []byte(`{"user":{"token":"old","tokenExpiresAt":1}}`), 0o600)
	entered := make(chan struct{})
	release := make(chan struct{})
	ts := NewTokenSource(func(string) string { return "" }, cfg, func(context.Context) error {
		close(entered)
		<-release
		return nil
	}, time.Now).(*tokenSource)
	joined := make(chan struct{})
	ts.onJoin = func() { close(joined) }

	go ts.Refresh(context.Background()) // leader
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- newTransport("http://127.0.0.1:1", ts).Do(ctx, "Op", "q", nil, nil) }()
	<-joined
	cancel()
	err := <-done
	close(release)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// A mutation Railway may have processed without answering is never sent a
// second time by the transport.
func TestMutationsNotRetriedOn5xx(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(502)
	}))
	defer srv.Close()
	c := &Client{T: newTransport(srv.URL, envTokens("t")), WriteAllowed: func(string) bool { return true }}
	ctx := context.Background()
	calls := map[string]func() error{
		"CommitPatch": func() error {
			_, err := c.CommitPatch(ctx, "e1", map[string]any{"services": map[string]any{}}, "m")
			return err
		},
		"Redeploy":          func() error { return c.Redeploy(ctx, "e1", "s1") },
		"RestartDeployment": func() error { return c.RestartDeployment(ctx, "e1", "d1") },
	}
	for name, call := range calls {
		atomic.StoreInt32(&n, 0)
		if err := call(); err == nil {
			t.Errorf("%s: no error on 502", name)
		}
		if got := atomic.LoadInt32(&n); got != 1 {
			t.Errorf("%s sent %d times, want 1", name, got)
		}
	}
	// Reads still get their one retry.
	atomic.StoreInt32(&n, 0)
	if _, err := c.ReadVariables(ctx, "p", "e1", "s1", true); err == nil {
		t.Error("ReadVariables: no error on 502")
	}
	if got := atomic.LoadInt32(&n); got != 2 {
		t.Errorf("ReadVariables sent %d times, want 2", got)
	}
}
