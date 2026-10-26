package railway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	defaultRetryDelay = 2 * time.Second
	defaultTimeout    = 300 * time.Second
	maxAttempts       = 2
)

var authMessage = regexp.MustCompile(`(?i)not authorized|unauthorized|unauthenticated`)

// AuthError: the token is missing or rejected. Never retried as is.
type AuthError struct{ Msg string }

func (e *AuthError) Error() string { return e.Msg }

// GraphQLError carries the errors of a response; with partial data it is
// returned alongside the decoded data.
type GraphQLError struct {
	Errors []struct {
		Message string `json:"message"`
		Path    []any  `json:"path"`
	}
}

func (e *GraphQLError) Error() string {
	msgs := make([]string, len(e.Errors))
	for i, x := range e.Errors {
		msgs[i] = x.Message
	}
	return "railway: " + strings.Join(msgs, "; ")
}

// Transport sends GraphQL operations to Railway.
type Transport struct {
	Endpoint   string
	HTTP       *http.Client
	Tokens     TokenSource
	RetryDelay time.Duration // before the single retry; default 2 s
	Timeout    time.Duration // per HTTP attempt; default 300 s
}

// transientError marks a failure worth one retry: network error, 5xx, 429.
type transientError struct{ msg string }

func (e *transientError) Error() string { return e.msg }

type noRetryKey struct{}

// WithoutRetry marks ctx so that Do sends the operation at most once: a
// transient failure (network error, 5xx, 429) is returned, not retried.
// Mutations use it, because Railway may have processed a request whose
// answer was lost, and a second send would act twice. The retry after a
// token refresh still happens: a rejected token means nothing was done.
func WithoutRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, noRetryKey{}, true)
}

func retryAllowed(ctx context.Context) bool {
	no, _ := ctx.Value(noRetryKey{}).(bool)
	return !no
}

// Do runs one operation and decodes the response `data` into out (may be nil).
// A response with data and errors decodes the data and returns *GraphQLError.
func (t *Transport) Do(ctx context.Context, op, document string, vars map[string]any, out any) error {
	st := t.Tokens.Get(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if st.Missing != "" {
		return &AuthError{Msg: st.Missing}
	}
	err := t.attempt(ctx, st.Token, op, document, vars, out)
	var ae *AuthError
	// A config token Railway rejects is usually just expired: refresh, retry once.
	if errors.As(err, &ae) && st.Source == "config" {
		if st.RefreshErr == "" { // Get did not already try (and fail) to refresh
			st = t.Tokens.Refresh(ctx)
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			if st.Missing != "" {
				return &AuthError{Msg: st.Missing}
			}
		}
		if st.RefreshErr != "" {
			return &AuthError{Msg: "token refresh failed: " + st.RefreshErr + ": run railway login"}
		}
		err = t.attempt(ctx, st.Token, op, document, vars, out)
	}
	return err
}

// attempt sends the request, retrying once on transient failures unless ctx
// carries WithoutRetry.
func (t *Transport) attempt(ctx context.Context, token, op, document string, vars map[string]any, out any) error {
	delay := t.RetryDelay
	if delay <= 0 {
		delay = defaultRetryDelay
	}
	attempts := maxAttempts
	if !retryAllowed(ctx) {
		attempts = 1
	}
	var err error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		err = t.once(ctx, token, op, document, vars, out)
		var te *transientError
		if !errors.As(err, &te) {
			return err
		}
	}
	return err
}

func (t *Transport) once(ctx context.Context, token, op, document string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": document, "variables": vars, "operationName": op})
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, http.MethodPost, t.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "railway-manager")

	client := t.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err() // a caller's cancellation is final
		}
		// url.Error text contains the URL only, never headers.
		return &transientError{msg: "railway request failed: " + err.Error()}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return &AuthError{Msg: "Railway rejected the token (HTTP 401): run railway login"}
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return &transientError{msg: fmt.Sprintf("Railway answered HTTP %d", resp.StatusCode)}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return fmt.Errorf("Railway answered HTTP %d", resp.StatusCode)
	}

	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
			Path    []any  `json:"path"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("decode Railway response: %w", err)
	}
	for _, e := range env.Errors {
		if authMessage.MatchString(e.Message) {
			return &AuthError{Msg: "Railway: " + e.Message}
		}
	}
	if out != nil && len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("decode Railway data: %w", err)
		}
	}
	if len(env.Errors) > 0 {
		return &GraphQLError{Errors: env.Errors}
	}
	return nil
}
