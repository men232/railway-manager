package railway_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
)

const liveDoc = "subscription Logs($id: String!) { deploymentLogs(deploymentId: $id) { message } }"

// seqTokens hands out tok-1, tok-2, … one per Get, as a refresh would.
type seqTokens struct {
	n       atomic.Int32
	missing string
}

func (s *seqTokens) Get(context.Context) railway.TokenStatus {
	if s.missing != "" {
		return railway.TokenStatus{Missing: s.missing}
	}
	return railway.TokenStatus{Token: fmt.Sprintf("tok-%d", s.n.Add(1)), Source: "config"}
}
func (s *seqTokens) Refresh(ctx context.Context) railway.TokenStatus { return s.Get(ctx) }
func (s *seqTokens) Peek() railway.TokenStatus {
	return railway.TokenStatus{Token: fmt.Sprintf("tok-%d", s.n.Load()), Source: "config"}
}

// sleeps records the backoff pauses and returns at once.
type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
	return ctx.Err()
}

func (s *sleeps) get() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.d...)
}

// newLive builds a Live on the fake with a short grace period; see newLiveT.
func newLive(t *testing.T, f *railwaytest.Fake, tokens railway.TokenSource, dial func(context.Context, string) (*websocket.Conn, error)) (*railway.Live, *sleeps) {
	t.Helper()
	return newLiveT(t, f, tokens, dial, railway.LiveTimings{Grace: 20 * time.Millisecond})
}

// newLiveT builds a Live with timings and, when the test ends, closes it
// (failing if Close takes over 3 s) and checks that no goroutine it started
// is still running.
func newLiveT(t *testing.T, f *railwaytest.Fake, tokens railway.TokenSource, dial func(context.Context, string) (*websocket.Conn, error), timings railway.LiveTimings) (*railway.Live, *sleeps) {
	t.Helper()
	base := runtime.NumGoroutine()
	l := railway.NewLive(f.WSURL, tokens, dial)
	railway.SetLiveTimings(l, timings)
	s := &sleeps{}
	l.Sleep = s.sleep
	t.Cleanup(func() {
		// The client alone must close its sockets: no server-side drop here.
		closed := make(chan struct{})
		go func() { l.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(3 * time.Second):
			buf := make([]byte, 1<<20)
			t.Errorf("Close did not return within 3 s\n%s", buf[:runtime.Stack(buf, true)])
			return
		}
		deadline := time.Now().Add(3 * time.Second)
		for runtime.NumGoroutine() > base {
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<20)
				t.Errorf("goroutines: %d after, %d before\n%s", runtime.NumGoroutine(), base, buf[:runtime.Stack(buf, true)])
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	return l, s
}

// gateSleep blocks each backoff until the test releases it, so the test can
// act while the client is backing off.
type gateSleep struct {
	entered chan time.Duration
	release chan struct{}
}

func newGateSleep() *gateSleep {
	return &gateSleep{entered: make(chan time.Duration), release: make(chan struct{})}
}

func (g *gateSleep) sleep(ctx context.Context, d time.Duration) error {
	select {
	case g.entered <- d:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// waitBackoff waits until the client is in a backoff sleep.
func (g *gateSleep) waitBackoff(t *testing.T) time.Duration {
	t.Helper()
	return recv(t, g.entered)
}

// wake ends the current backoff sleep.
func (g *gateSleep) wake(t *testing.T) {
	t.Helper()
	select {
	case g.release <- struct{}{}:
	case <-time.After(5 * time.Second):
		t.Fatal("no backoff sleep to wake")
	}
}

// subscribedIDs are the ids of the subscribe messages the fake received.
func subscribedIDs(f *railwaytest.Fake) []string {
	var ids []string
	for _, s := range f.WSSubscribed() {
		ids = append(ids, s.ID)
	}
	return ids
}

// settled waits a little and checks the value is unchanged (nothing more is
// sent late).
func settled[T any](t *testing.T, what string, get func() T, want T) {
	t.Helper()
	waitFor(t, what, func() bool { return reflect.DeepEqual(get(), want) })
	time.Sleep(50 * time.Millisecond)
	if got := get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: %v, want %v", what, got, want)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a message")
		var zero T
		return zero
	}
}

func collect(ch chan json.RawMessage) func(json.RawMessage) {
	return func(d json.RawMessage) { ch <- d }
}

func noFail(t *testing.T) func(string, railway.LiveFail) {
	return func(msg string, _ railway.LiveFail) { t.Errorf("fail(%q)", msg) }
}

// onlyLost is noFail for tests that drop the connection: a lost connection
// is reported, nothing else is.
func onlyLost(t *testing.T) func(string, railway.LiveFail) {
	return func(msg string, _ railway.LiveFail) {
		if msg != "live: connection lost" {
			t.Errorf("fail(%q)", msg)
		}
	}
}

func TestLiveSubscribeReceivesNext(t *testing.T) {
	f := railwaytest.New(t)
	l, _ := newLive(t, f, &seqTokens{}, nil)
	if l.State() != "idle" {
		t.Fatalf("State before = %q, want idle", l.State())
	}
	got := make(chan json.RawMessage, 4)
	stop := l.Subscribe(liveDoc, map[string]any{"id": "dep-1"}, collect(got), noFail(t))
	defer stop()

	f.WSPush("1", map[string]any{"deploymentLogs": []any{map[string]any{"message": "hello"}}})
	if d := recv(t, got); string(d) != `{"deploymentLogs":[{"message":"hello"}]}` {
		t.Fatalf("next = %s", d)
	}
	if l.State() != "open" {
		t.Fatalf("State = %q, want open", l.State())
	}
	inits := f.WSInits()
	if len(inits) != 1 || inits[0]["Authorization"] != "Bearer tok-1" {
		t.Fatalf("connection_init payloads = %v", inits)
	}
	subs := f.WSSubscribed()
	want := []railwaytest.WSSub{{ID: "1", Query: liveDoc, Vars: map[string]any{"id": "dep-1"}}}
	if !reflect.DeepEqual(subs, want) {
		t.Fatalf("subscribed = %+v, want %+v", subs, want)
	}
	if reqs := f.WSRequests(); len(reqs) != 1 || reqs[0].Protocol != "graphql-transport-ws" {
		t.Fatalf("ws requests = %+v", reqs)
	}
}

func TestLiveStopLastClosesSocket(t *testing.T) {
	f := railwaytest.New(t)
	l, _ := newLive(t, f, &seqTokens{}, nil)
	got1, got2 := make(chan json.RawMessage, 4), make(chan json.RawMessage, 4)
	stop1 := l.Subscribe(liveDoc, nil, collect(got1), noFail(t))
	f.WSPush("1", 1)
	recv(t, got1)
	stop2 := l.Subscribe(liveDoc, map[string]any{"id": "b"}, collect(got2), noFail(t))
	f.WSPush("2", 2)
	if d := recv(t, got2); string(d) != "2" {
		t.Fatalf("second subscription got %s", d)
	}

	stop1()
	stop1() // idempotent
	waitFor(t, "complete of 1", func() bool { return reflect.DeepEqual(f.WSCompletes(), []string{"1"}) })
	if f.WSOpen() != 1 || l.State() != "open" {
		t.Fatalf("after first stop: open sockets %d, state %q", f.WSOpen(), l.State())
	}

	stop2()
	waitFor(t, "socket closed after the grace period", func() bool { return f.WSOpen() == 0 && l.State() == "idle" })
	if c := f.WSCompletes(); !reflect.DeepEqual(c, []string{"1", "2"}) {
		t.Fatalf("completes = %v", c)
	}
	if n := len(f.WSRequests()); n != 1 {
		t.Fatalf("sockets opened = %d, want 1", n)
	}
}

func TestLiveReconnectsWithFreshToken(t *testing.T) {
	f := railwaytest.New(t)
	l, sl := newLive(t, f, &seqTokens{}, nil)
	got := make(chan json.RawMessage, 4)
	stop := l.Subscribe(liveDoc, map[string]any{"id": "x"}, collect(got), onlyLost(t))
	defer stop()
	f.WSPush("1", "a")
	recv(t, got)

	f.WSDrop()
	waitFor(t, "second connection_init", func() bool { return len(f.WSInits()) == 2 })
	if a := f.WSInits()[1]["Authorization"]; a != "Bearer tok-2" {
		t.Fatalf("second connection_init Authorization = %v, want the refreshed token", a)
	}
	f.WSPush("1", "b")
	if d := recv(t, got); string(d) != `"b"` {
		t.Fatalf("after reconnect next = %s", d)
	}
	if s := sl.get(); !reflect.DeepEqual(s, []time.Duration{time.Second}) {
		t.Fatalf("sleeps = %v, want [1s]", s)
	}
	if subs := f.WSSubscribed(); len(subs) != 2 || subs[1].ID != "1" || subs[1].Vars["id"] != "x" {
		t.Fatalf("resubscribe = %+v", subs)
	}
}

func TestLiveBackoffSequence(t *testing.T) {
	f := railwaytest.New(t)
	var calls atomic.Int32
	dial := func(ctx context.Context, url string) (*websocket.Conn, error) {
		if n := calls.Add(1); n >= 2 && n <= 6 {
			return nil, errors.New("connection refused")
		}
		return railway.DialLive(ctx, url)
	}
	l, sl := newLive(t, f, &seqTokens{}, dial)
	got := make(chan json.RawMessage, 4)
	var fails []string
	var mu sync.Mutex
	stop := l.Subscribe(liveDoc, nil, collect(got), func(msg string, _ railway.LiveFail) { mu.Lock(); fails = append(fails, msg); mu.Unlock() })
	defer stop()
	f.WSPush("1", 1)
	recv(t, got)

	f.WSDrop() // then five refused dials
	f.WSPush("1", 2)
	recv(t, got)
	s := sl.get()
	want := []time.Duration{1 * time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, 30 * time.Second}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("sleeps = %v, want %v", s, want)
	}
	mu.Lock()
	if len(fails) != 6 || fails[0] != "live: connection lost" || !strings.Contains(fails[1], "connection refused") {
		t.Errorf("fails = %q, want the lost connection, then five connect failures", fails)
	}
	mu.Unlock()

	// An ack alone does not reset the attempt counter (the connection has
	// not stayed up 10 s): the next drop still waits 30 s.
	f.WSDrop()
	f.WSPush("1", 3)
	recv(t, got)
	if s := sl.get(); len(s) != 7 || s[6] != 30*time.Second {
		t.Fatalf("sleeps after a short-lived connection = %v", s)
	}
}

func TestLiveTokenNeverInURL(t *testing.T) {
	f := railwaytest.New(t)
	var mu sync.Mutex
	var urls []string
	dial := func(ctx context.Context, url string) (*websocket.Conn, error) {
		mu.Lock()
		urls = append(urls, url)
		mu.Unlock()
		return railway.DialLive(ctx, url)
	}
	l, _ := newLive(t, f, &seqTokens{}, dial)
	got := make(chan json.RawMessage, 4)
	stop := l.Subscribe(liveDoc, nil, collect(got), noFail(t))
	defer stop()
	f.WSPush("1", 1)
	recv(t, got)

	mu.Lock()
	defer mu.Unlock()
	if len(urls) != 1 || urls[0] != f.WSURL {
		t.Fatalf("dialled %v, want %s", urls, f.WSURL)
	}
	for _, r := range f.WSRequests() {
		if strings.Contains(r.URI, "tok-") {
			t.Errorf("token in request URI %q", r.URI)
		}
		for name, vs := range r.Header {
			for _, v := range vs {
				if strings.Contains(v, "tok-") {
					t.Errorf("token in header %s", name)
				}
			}
		}
	}
	if a := f.WSInits()[0]["Authorization"]; a != "Bearer tok-1" {
		t.Fatalf("the token belongs in connection_init, got %v", a)
	}
}

func TestLiveConnectFailureHidesToken(t *testing.T) {
	f := railwaytest.New(t)
	dial := func(context.Context, string) (*websocket.Conn, error) {
		return nil, errors.New("dial tcp: no route to host")
	}
	l, sl := newLive(t, f, &seqTokens{}, dial)
	fails := make(chan string, 16)
	stop := l.Subscribe(liveDoc, nil, func(json.RawMessage) {}, func(msg string, _ railway.LiveFail) {
		select {
		case fails <- msg:
		default:
		}
	})
	msg := recv(t, fails)
	if !strings.Contains(msg, "no route to host") || strings.Contains(msg, "tok-") {
		t.Fatalf("fail message = %q", msg)
	}
	waitFor(t, "backoff after a failed connect", func() bool { return len(sl.get()) > 0 })
	stop()
}

func TestLiveMissingToken(t *testing.T) {
	f := railwaytest.New(t)
	l, _ := newLive(t, f, &seqTokens{missing: "not logged in: run railway login"}, nil)
	fails := make(chan string, 16)
	stop := l.Subscribe(liveDoc, nil, func(json.RawMessage) {}, func(msg string, _ railway.LiveFail) {
		select {
		case fails <- msg:
		default:
		}
	})
	if msg := recv(t, fails); msg != "not logged in: run railway login" {
		t.Fatalf("fail = %q", msg)
	}
	stop()
	if n := len(f.WSInits()); n != 0 {
		t.Fatalf("connection_init sent without a token: %d", n)
	}
}

func TestLiveAnswersPing(t *testing.T) {
	f := railwaytest.New(t)
	l, _ := newLive(t, f, &seqTokens{}, nil)
	got := make(chan json.RawMessage, 4)
	stop := l.Subscribe(liveDoc, nil, collect(got), noFail(t))
	defer stop()
	f.WSPush("1", 1)
	recv(t, got)
	f.WSPing()
	waitFor(t, "pong", func() bool { return f.WSPongs() == 1 })
}

func TestLiveErrorEndsSubscription(t *testing.T) {
	f := railwaytest.New(t)
	l, _ := newLive(t, f, &seqTokens{}, nil)
	got := make(chan json.RawMessage, 4)
	fails := make(chan string, 4)
	stop := l.Subscribe(liveDoc, nil, collect(got), func(msg string, _ railway.LiveFail) { fails <- msg })
	defer stop()
	f.WSPush("1", 1)
	recv(t, got)

	f.WSPushErrors("1", "rate limited") // next with errors: reported, subscription stays
	if msg := recv(t, fails); msg != "rate limited" {
		t.Fatalf("fail = %q", msg)
	}
	f.WSError("1", "Deployment not found", "try again")
	if msg := recv(t, fails); msg != "Deployment not found; try again" {
		t.Fatalf("fail = %q", msg)
	}
	// The error ended the only subscription: the socket closes.
	waitFor(t, "socket closed", func() bool { return f.WSOpen() == 0 && l.State() == "idle" })
	if c := f.WSCompletes(); len(c) != 0 {
		t.Fatalf("a subscription the server ended is not completed again: %v", c)
	}
}

func TestLiveResubscribeAfterIdle(t *testing.T) {
	f := railwaytest.New(t)
	l, _ := newLive(t, f, &seqTokens{}, nil)
	for i := range 3 {
		got := make(chan json.RawMessage, 4)
		stop := l.Subscribe(liveDoc, nil, collect(got), noFail(t))
		id := fmt.Sprint(i + 1)
		f.WSPush(id, i)
		recv(t, got)
		stop()
		waitFor(t, "idle", func() bool { return l.State() == "idle" && f.WSOpen() == 0 })
	}
	waitFor(t, "sockets closed", func() bool { return f.WSOpen() == 0 })
	if n := len(f.WSRequests()); n != 3 {
		t.Fatalf("sockets opened = %d, want 3", n)
	}
}

func TestLiveBackoffResetsAfterStableConnection(t *testing.T) {
	f := railwaytest.New(t)
	l, sl := newLiveT(t, f, &seqTokens{}, nil, railway.LiveTimings{Grace: 20 * time.Millisecond, Stable: 100 * time.Millisecond})
	got := make(chan json.RawMessage, 4)
	stop := l.Subscribe(liveDoc, nil, collect(got), onlyLost(t))
	defer stop()
	f.WSPush("1", 1)
	recv(t, got)
	f.WSDrop() // up only briefly before: 1 s
	f.WSPush("1", 2)
	recv(t, got)
	f.WSDrop() // that one was acked but not stable: 2 s
	f.WSPush("1", 3)
	recv(t, got)
	time.Sleep(200 * time.Millisecond)
	f.WSDrop() // stable for longer than Stable: back to 1 s
	f.WSPush("1", 4)
	recv(t, got)
	if s := sl.get(); !reflect.DeepEqual(s, []time.Duration{time.Second, 2 * time.Second, time.Second}) {
		t.Fatalf("sleeps = %v, want [1s 2s 1s]", s)
	}
}

func TestLiveGraceReusesSocket(t *testing.T) {
	f := railwaytest.New(t)
	l, _ := newLiveT(t, f, &seqTokens{}, nil, railway.LiveTimings{Grace: 10 * time.Second})
	got1, got2 := make(chan json.RawMessage, 4), make(chan json.RawMessage, 4)
	stop1 := l.Subscribe(liveDoc, nil, collect(got1), noFail(t))
	f.WSPush("1", 1)
	recv(t, got1)
	stop1()
	if l.State() != "open" || f.WSOpen() != 1 {
		t.Fatalf("in the grace period: state %q, open sockets %d", l.State(), f.WSOpen())
	}
	stop2 := l.Subscribe(liveDoc, nil, collect(got2), noFail(t))
	defer stop2()
	f.WSPush("2", 2)
	recv(t, got2)
	if n := len(f.WSRequests()); n != 1 {
		t.Fatalf("sockets opened = %d, want the one reused", n)
	}
	if ids := subscribedIDs(f); !reflect.DeepEqual(ids, []string{"1", "2"}) {
		t.Fatalf("subscribed = %v", ids)
	}
}

func TestLiveKeepalive(t *testing.T) {
	f := railwaytest.New(t)
	l, sl := newLiveT(t, f, &seqTokens{}, nil, railway.LiveTimings{Grace: 20 * time.Millisecond, Ping: 20 * time.Millisecond, PongWait: 40 * time.Millisecond})
	type failure struct {
		msg string
		why railway.LiveFail
	}
	got := make(chan json.RawMessage, 4)
	fails := make(chan failure, 4)
	stop := l.Subscribe(liveDoc, nil, collect(got), func(msg string, why railway.LiveFail) { fails <- failure{msg, why} })
	defer stop()
	f.WSPush("1", 1)
	recv(t, got)
	waitFor(t, "pings", func() bool { return f.WSPings() >= 3 })
	if n := len(f.WSInits()); n != 1 {
		t.Fatalf("answered pings must keep the connection: %d connects", n)
	}
	select {
	case got := <-fails:
		t.Fatalf("answered pings reported a failure: %+v", got)
	default:
	}

	f.WSWithholdPongs(1)
	// The stall is reported like any other lost connection (LiveFailConn), so
	// the subscription is kept and sent again.
	if got := recv(t, fails); got != (failure{"live: no pong", railway.LiveFailConn}) {
		t.Fatalf("unanswered ping: %+v", got)
	}
	waitFor(t, "reconnect after an unanswered ping", func() bool { return len(f.WSInits()) == 2 })
	// the resubscribe follows the ack: wait for it, not just the init
	waitFor(t, "the subscription resent", func() bool { return reflect.DeepEqual(subscribedIDs(f), []string{"1", "1"}) })
	f.WSPush("1", 2)
	recv(t, got)
	if s := sl.get(); !reflect.DeepEqual(s, []time.Duration{time.Second}) {
		t.Fatalf("sleeps = %v, want [1s]: the normal backoff", s)
	}
}

func TestLiveServerCompleteEndsStream(t *testing.T) {
	f := railwaytest.New(t)
	l, _ := newLive(t, f, &seqTokens{}, nil)
	got := make(chan json.RawMessage, 4)
	fails := make(chan string, 4)
	stop := l.Subscribe(liveDoc, nil, collect(got), func(msg string, _ railway.LiveFail) { fails <- msg })
	defer stop()
	f.WSPush("1", 1)
	recv(t, got)
	f.WSComplete("1")
	if msg := recv(t, fails); msg != "stream ended" {
		t.Fatalf("fail = %q, want stream ended", msg)
	}
	waitFor(t, "socket closed", func() bool { return f.WSOpen() == 0 && l.State() == "idle" })
	if c := f.WSCompletes(); len(c) != 0 {
		t.Fatalf("completes sent = %v", c)
	}
}

func TestLiveConnectionRejected(t *testing.T) {
	f := railwaytest.New(t)
	f.WSRejectInits(4403, "Forbidden: token tok-1 revoked")
	l, _ := newLive(t, f, &seqTokens{}, nil)
	fails := make(chan string, 16)
	stop := l.Subscribe(liveDoc, nil, func(json.RawMessage) {}, func(msg string, _ railway.LiveFail) {
		select {
		case fails <- msg:
		default:
		}
	})
	defer stop()
	if msg := recv(t, fails); msg != "live: connection rejected (code 4403)" {
		t.Fatalf("fail = %q", msg)
	}
}

func TestLiveNoAck(t *testing.T) {
	f := railwaytest.New(t)
	f.WSHoldAcks()
	l, _ := newLiveT(t, f, &seqTokens{}, nil, railway.LiveTimings{Grace: 20 * time.Millisecond, AckWait: 30 * time.Millisecond})
	fails := make(chan string, 16)
	stop := l.Subscribe(liveDoc, nil, func(json.RawMessage) {}, func(msg string, _ railway.LiveFail) {
		select {
		case fails <- msg:
		default:
		}
	})
	msg := recv(t, fails)
	stop()
	f.WSReleaseAcks()
	if msg != "live: no ack" {
		t.Fatalf("fail = %q", msg)
	}
}

func TestLiveStopLastDuringBackoff(t *testing.T) {
	f := railwaytest.New(t)
	l, _ := newLive(t, f, &seqTokens{}, nil)
	g := newGateSleep()
	l.Sleep = g.sleep
	got := make(chan json.RawMessage, 4)
	stop := l.Subscribe(liveDoc, nil, collect(got), onlyLost(t))
	f.WSPush("1", 1)
	recv(t, got)
	f.WSDrop()
	g.waitBackoff(t)
	stop()
	if l.State() != "idle" {
		t.Fatalf("State = %q, want idle at once: no socket to keep", l.State())
	}
	settled(t, "sockets opened", func() int { return len(f.WSRequests()) }, 1)
}

func TestLiveStopDuringBackoffNotResubscribed(t *testing.T) {
	f := railwaytest.New(t)
	l, _ := newLive(t, f, &seqTokens{}, nil)
	g := newGateSleep()
	l.Sleep = g.sleep
	got := make(chan json.RawMessage, 4)
	stop1 := l.Subscribe(liveDoc, nil, collect(got), onlyLost(t))
	stop2 := l.Subscribe(liveDoc, nil, collect(got), onlyLost(t))
	defer stop2()
	waitFor(t, "both subscribed", func() bool { return len(f.WSSubscribed()) == 2 })
	f.WSDrop()
	g.waitBackoff(t)
	stop1()
	g.wake(t)
	settled(t, "subscribed", func() []string { return subscribedIDs(f) }, []string{"1", "2", "2"})
}

func TestLiveStopLastWhileConnecting(t *testing.T) {
	f := railwaytest.New(t)
	entered := make(chan struct{})
	dial := func(ctx context.Context, url string) (*websocket.Conn, error) {
		close(entered)
		<-ctx.Done() // a dial that hangs until it is no longer wanted
		return nil, ctx.Err()
	}
	l, sl := newLive(t, f, &seqTokens{}, dial)
	stop := l.Subscribe(liveDoc, nil, func(json.RawMessage) {}, noFail(t))
	recv(t, entered)
	stop()
	if l.State() != "idle" {
		t.Fatalf("State = %q, want idle", l.State())
	}
	time.Sleep(50 * time.Millisecond)
	if s := sl.get(); len(s) != 0 {
		t.Fatalf("a cancelled session must not back off: sleeps %v", s)
	}
}

func TestLiveSubscribeDuringBackoff(t *testing.T) {
	f := railwaytest.New(t)
	l, _ := newLive(t, f, &seqTokens{}, nil)
	g := newGateSleep()
	l.Sleep = g.sleep
	got := make(chan json.RawMessage, 4)
	stop1 := l.Subscribe(liveDoc, nil, collect(got), onlyLost(t))
	defer stop1()
	f.WSPush("1", 1)
	recv(t, got)
	f.WSDrop()
	g.waitBackoff(t)
	stop2 := l.Subscribe(liveDoc, nil, collect(got), onlyLost(t))
	defer stop2()
	g.wake(t)
	settled(t, "subscribed", func() []string { return subscribedIDs(f) }, []string{"1", "1", "2"})
}

func TestLiveSubscribeBeforeAck(t *testing.T) {
	f := railwaytest.New(t)
	l, _ := newLive(t, f, &seqTokens{}, nil)
	got := make(chan json.RawMessage, 4)
	stop1 := l.Subscribe(liveDoc, nil, collect(got), onlyLost(t))
	defer stop1()
	f.WSPush("1", 1)
	recv(t, got)
	f.WSHoldAcks()
	f.WSDrop()
	waitFor(t, "second connection_init", func() bool { return len(f.WSInits()) == 2 })
	stop2 := l.Subscribe(liveDoc, nil, collect(got), onlyLost(t))
	defer stop2()
	f.WSReleaseAcks()
	f.WSPush("2", 2)
	recv(t, got)
	settled(t, "subscribed", func() []string { return subscribedIDs(f) }, []string{"1", "1", "2"})
}

func TestLiveCallbackReentry(t *testing.T) {
	f := railwaytest.New(t)
	l, _ := newLive(t, f, &seqTokens{}, nil)
	got2 := make(chan json.RawMessage, 4)
	var stop1, stop2 func()
	ready := make(chan struct{})
	stop1 = l.Subscribe(liveDoc, nil, func(json.RawMessage) {
		<-ready
		stop1() // stop and Subscribe from the live goroutine
		stop2 = l.Subscribe(liveDoc, nil, collect(got2), func(string, railway.LiveFail) { stop2() })
	}, noFail(t))
	close(ready)
	f.WSPush("1", 1)
	f.WSPush("2", 2)
	recv(t, got2)
	f.WSError("2", "gone") // its fail calls its own stop
	waitFor(t, "socket closed", func() bool { return f.WSOpen() == 0 && l.State() == "idle" })
}

func TestLiveSubscribeStopStress(t *testing.T) {
	f := railwaytest.New(t)
	l, _ := newLive(t, f, &seqTokens{}, nil)
	done := make(chan struct{})
	var dropper sync.WaitGroup
	dropper.Add(1)
	go func() {
		defer dropper.Done()
		for {
			select {
			case <-done:
				return
			case <-time.After(3 * time.Millisecond):
				f.WSDrop()
			}
		}
	}()
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 25 {
				stop := l.Subscribe(liveDoc, map[string]any{"w": w, "i": i}, func(json.RawMessage) {}, func(string, railway.LiveFail) {})
				time.Sleep(time.Duration(i%4) * time.Millisecond)
				stop()
				if i%3 == 0 {
					stop()
				}
			}
		}()
	}
	wg.Wait()
	close(done)
	dropper.Wait()

	got := make(chan json.RawMessage, 4)
	stop := l.Subscribe(liveDoc, nil, collect(got), onlyLost(t))
	defer stop()
	f.WSPush("201", "after")
	if d := recv(t, got); string(d) != `"after"` {
		t.Fatalf("after the stress run next = %s", d)
	}
}

func TestLiveCloseDeadline(t *testing.T) {
	f := railwaytest.New(t)
	l, _ := newLiveT(t, f, &seqTokens{}, nil, railway.LiveTimings{Grace: 10 * time.Millisecond, CloseWait: 200 * time.Millisecond})
	got := make(chan json.RawMessage, 4)
	stop := l.Subscribe(liveDoc, nil, collect(got), noFail(t))
	f.WSPush("1", 1)
	recv(t, got)
	f.WSStallReads() // the server will not answer the close frame
	start := time.Now()
	stop()
	l.Close()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("closing took %v: the flush and close handshake share one deadline", d)
	}
	f.WSResumeReads()
}

func TestLiveUnencodableVars(t *testing.T) {
	f := railwaytest.New(t)
	l, _ := newLive(t, f, &seqTokens{}, nil)
	fails := make(chan string, 1)
	stop := l.Subscribe(liveDoc, map[string]any{"bad": make(chan int)}, func(json.RawMessage) {}, func(msg string, _ railway.LiveFail) { fails <- msg })
	stop()
	if msg := recv(t, fails); msg != "live: cannot encode the subscription variables" {
		t.Fatalf("fail = %q", msg)
	}
	if l.State() != "idle" || len(f.WSRequests()) != 0 {
		t.Fatalf("an unencodable subscription opened a socket: state %q", l.State())
	}
}

// Subscribe tells a subscription-ending failure (server error or
// complete) from one Live recovers from (errors in a next, a lost connection).
func TestLiveSubscribeReportsEnded(t *testing.T) {
	type failure struct {
		msg string
		why railway.LiveFail
	}
	f := railwaytest.New(t)
	l, _ := newLive(t, f, &seqTokens{}, nil)
	got := make(chan json.RawMessage, 4)
	fails := make(chan failure, 4)
	stop := l.Subscribe(liveDoc, nil, collect(got), func(msg string, why railway.LiveFail) { fails <- failure{msg, why} })
	defer stop()
	f.WSPush("1", 1)
	recv(t, got)
	f.WSPushErrors("1", "rate limited")
	if got := recv(t, fails); got != (failure{"rate limited", railway.LiveFailNext}) {
		t.Fatalf("next errors: %+v", got)
	}
	f.WSComplete("1")
	if got := recv(t, fails); got != (failure{railway.LiveStreamEnded, railway.LiveFailEnded}) {
		t.Fatalf("complete: %+v", got)
	}

	stop2 := l.Subscribe(liveDoc, nil, collect(got), func(msg string, why railway.LiveFail) { fails <- failure{msg, why} })
	defer stop2()
	f.WSError("2", "gone")
	if got := recv(t, fails); got != (failure{"gone", railway.LiveFailEnded}) {
		t.Fatalf("error: %+v", got)
	}

	waitFor(t, "socket closed", func() bool { return f.WSOpen() == 0 && l.State() == "idle" })
	f.WSRejectInits(4403, "nope")
	stop3 := l.Subscribe(liveDoc, nil, collect(got), func(msg string, why railway.LiveFail) {
		select {
		case fails <- failure{msg, why}:
		default:
		}
	})
	defer stop3()
	if got := recv(t, fails); got != (failure{"live: connection rejected (code 4403)", railway.LiveFailConn}) {
		t.Fatalf("connection: %+v", got)
	}
}

// Close waits for the fail of an unencodable subscription, which runs on a
// goroutine of its own.
func TestLiveCloseWaitsForUnencodableFail(t *testing.T) {
	f := railwaytest.New(t)
	l, _ := newLive(t, f, &seqTokens{}, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	l.Subscribe(liveDoc, map[string]any{"bad": make(chan int)}, nil, func(string, railway.LiveFail) {
		close(entered)
		<-release
	})
	recv(t, entered)
	closed := make(chan struct{})
	go func() { l.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned while a fail callback was still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	recv(t, closed)
}

// A connection lost after its ack is reported like an unanswered ping: to
// every subscription, as LiveFailConn, while the socket is no longer open; the
// subscriptions are kept and sent again.
func TestLiveDropAfterAckReportsEverySubscription(t *testing.T) {
	type failure struct {
		msg   string
		why   railway.LiveFail
		state string
	}
	f := railwaytest.New(t)
	l, _ := newLive(t, f, &seqTokens{}, nil)
	got := make(chan json.RawMessage, 8)
	fails := make(chan failure, 8)
	onFail := func(msg string, why railway.LiveFail) { fails <- failure{msg, why, l.State()} }
	stop1 := l.Subscribe(liveDoc, nil, collect(got), onFail)
	defer stop1()
	stop2 := l.Subscribe(liveDoc, map[string]any{"id": "b"}, collect(got), onFail)
	defer stop2()
	f.WSPush("1", 1)
	recv(t, got)
	f.WSPush("2", 2)
	recv(t, got)

	f.WSDrop()
	for range 2 {
		got := recv(t, fails)
		if got.msg != "live: connection lost" || got.why != railway.LiveFailConn || got.state == "open" {
			t.Fatalf("drop: %+v", got)
		}
	}
	waitFor(t, "reconnect", func() bool { return len(f.WSInits()) == 2 && l.State() == "open" })
	waitFor(t, "resubscribed", func() bool { return reflect.DeepEqual(subscribedIDs(f), []string{"1", "2", "1", "2"}) })
	f.WSPush("1", 3)
	if d := recv(t, got); string(d) != "3" {
		t.Fatalf("after reconnect next = %s", d)
	}
	select {
	case got := <-fails:
		t.Fatalf("extra failure: %+v", got)
	default:
	}
}
