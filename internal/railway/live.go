package railway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// LiveEndpoint is Railway's websocket endpoint for subscriptions.
const LiveEndpoint = "wss://backboard.railway.com/graphql/v2"

const (
	liveProtocol     = "graphql-transport-ws"
	liveDialTimeout  = 15 * time.Second
	liveWriteTimeout = 10 * time.Second
	liveReadLimit    = 16 << 20
)

// liveTimings are the protocol's clocks; tests shorten them.
type liveTimings struct {
	ackWait   time.Duration // connection_init → connection_ack
	grace     time.Duration // the socket outlives the last stop this long
	ping      time.Duration // keepalive ping interval while open
	pongWait  time.Duration // a ping unanswered this long drops the connection
	stable    time.Duration // acked this long resets the backoff
	closeWait time.Duration // final flush + close handshake, together
}

var defaultLiveTimings = liveTimings{
	ackWait: 10 * time.Second, grace: 2 * time.Second, ping: 30 * time.Second,
	pongWait: 10 * time.Second, stable: 10 * time.Second, closeWait: 5 * time.Second,
}

// liveRetry are the pauses before reconnecting, one per failed attempt in a
// row; the last repeats.
var liveRetry = []time.Duration{1 * time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second}

// LiveStreamEnded is the failure text of a subscription the server completed.
const LiveStreamEnded = "stream ended"

// LiveFail says what a subscription's failure was, and so what follows it.
type LiveFail uint8

const (
	// LiveFailNext is an error in a "next" of the subscription: it goes on.
	LiveFailNext LiveFail = iota
	// LiveFailConn is a connection that could not be made or was lost: the
	// subscription is kept and sent again on reconnect, and the failure is
	// over once the socket is open again.
	LiveFailConn
	// LiveFailEnded is an "error" or "complete" from the server, or
	// variables that cannot be encoded: nothing more arrives, nothing is
	// resent.
	LiveFailEnded
)

// Fixed failure texts: the server's own reasons are never passed on.
const (
	liveNoAck       = "live: no ack"
	liveStreamEnded = LiveStreamEnded
	liveBadVars     = "live: cannot encode the subscription variables"
	liveNoPong      = "live: no pong"
	liveLost        = "live: connection lost"
)

// Live runs GraphQL subscriptions over one websocket in the
// graphql-transport-ws protocol. The socket opens with the first subscription
// and closes 2 s after the last stops (a Subscribe in between reuses it); the
// token goes in connection_init, never in the URL. A dropped connection comes
// back after a growing pause with a freshly resolved token, and every
// subscription is sent again. While open, a ping goes out every 30 s and a
// pong must come back within 10 s.
//
// All network work, token resolution included, runs on one background
// goroutine per session (plus a reader per connection); Subscribe and stop
// never block.
type Live struct {
	// Sleep pauses before a reconnect; it returns early with ctx's error when
	// the socket is no longer wanted. Tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error

	endpoint string
	tokens   TokenSource
	dial     func(ctx context.Context, url string) (*websocket.Conn, error)
	timing   liveTimings

	mu     sync.Mutex // guards everything below and the session fields
	subs   map[string]*liveSub
	nextID int
	sess   *liveSession
	grace  *time.Timer // running while the session outlives its last stop
	state  string
	wg     sync.WaitGroup // one per session goroutine
}

type liveSub struct {
	seq     int
	id      string
	payload json.RawMessage // the subscribe payload, encoded once
	next    func(json.RawMessage)
	fail    func(msg string, why LiveFail)
}

// liveSession is one wanted socket: from the first subscription to the last,
// across reconnects. Its fields are guarded by Live.mu.
type liveSession struct {
	ctx    context.Context
	cancel context.CancelFunc
	ready  bool        // acked: subscribe/complete go out as they happen
	outbox []liveFrame // frames for the run goroutine to send
	wake   chan struct{}
}

type liveFrame struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Payload any    `json:"payload,omitempty"`
}

type liveMessage struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// NewLive builds a subscription client for endpoint (LiveEndpoint in the
// app). dial nil dials with the graphql-transport-ws subprotocol.
func NewLive(endpoint string, tokens TokenSource, dial func(ctx context.Context, url string) (*websocket.Conn, error)) *Live {
	if dial == nil {
		dial = DialLive
	}
	return &Live{
		Sleep:    sleepCtx,
		endpoint: endpoint,
		tokens:   tokens,
		dial:     dial,
		timing:   defaultLiveTimings,
		subs:     map[string]*liveSub{},
		state:    "idle",
	}
}

// liveHTTP dials websockets without a connection pool: a dial cancelled while
// its TCP connect is still finishing would otherwise leave that connection
// (and two goroutines) idle in the pool for 90 s.
var liveHTTP = func() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableKeepAlives = true
	return &http.Client{Transport: t}
}()

// DialLive is NewLive's default dial: the graphql-transport-ws subprotocol,
// no pooled HTTP connections.
func DialLive(ctx context.Context, url string) (*websocket.Conn, error) {
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: liveHTTP, Subprotocols: []string{liveProtocol}})
	return c, err
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// State is "idle" (no socket wanted), "connecting", "open" or "retrying".
func (l *Live) State() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state
}

// Subscribe starts a subscription. next receives each payload's data and fail
// each error: GraphQL errors of this subscription, why the connection cannot
// be made or was lost, or "stream ended". Both run on the live goroutine, so
// callers forward them (program.Send); they may call stop or Subscribe.
//
// fail also says what the failure was (LiveFail): an error in a "next", a
// connection that failed or was lost, or the subscription's end. It is
// decided where the failure happens, so a caller handling it later never
// needs to read State to tell a connection failure from the others.
//
// stop ends the subscription and is idempotent. A next or fail already under
// way may still run after stop returns: callers drop stale messages (by a
// generation or id they keep).
//
// vars are encoded here, once. If they cannot be encoded, fail is called on
// a goroutine of its own (never on the caller's; Close waits for it) and stop
// does nothing.
func (l *Live) Subscribe(document string, vars map[string]any, next func(data json.RawMessage), fail func(msg string, why LiveFail)) (stop func()) {
	if vars == nil {
		vars = map[string]any{}
	}
	payload, err := json.Marshal(struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}{document, vars})
	if err != nil {
		if fail != nil {
			// Its own goroutine: the caller may hold what fail needs. Tracked,
			// so Close does not return while it runs.
			l.wg.Add(1)
			go func() {
				defer l.wg.Done()
				fail(liveBadVars, LiveFailEnded)
			}()
		}
		return func() {}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextID++
	sub := &liveSub{seq: l.nextID, id: strconv.Itoa(l.nextID), payload: payload, next: next, fail: fail}
	l.subs[sub.id] = sub
	l.stopGraceLocked()
	switch {
	case l.sess == nil:
		ctx, cancel := context.WithCancel(context.Background())
		s := &liveSession{ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1)}
		l.sess, l.state = s, "connecting"
		l.wg.Add(1)
		go l.run(s)
	case l.sess.ready:
		l.sess.send(subscribeFrame(sub))
	}
	// Not ready: the subscription goes out with the others on connection_ack.
	return func() { l.end(sub.id, true) }
}

// Close ends every subscription at once (no grace period) and waits for the
// socket's goroutines to finish. It is for shutdown: not from a next/fail
// callback (which runs on the goroutine it waits for) and not concurrently
// with Subscribe.
func (l *Live) Close() {
	l.mu.Lock()
	clear(l.subs)
	l.dropSessionLocked()
	l.mu.Unlock()
	l.wg.Wait()
}

// end removes a subscription; complete says whether to tell the server. With
// the last one gone, an open socket is kept for the grace period and any
// other session ends at once.
func (l *Live) end(id string, complete bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.subs[id]; !ok {
		return
	}
	delete(l.subs, id)
	s := l.sess
	if s == nil {
		return
	}
	if complete && s.ready {
		s.send(liveFrame{ID: id, Type: "complete"})
	}
	if len(l.subs) > 0 {
		return
	}
	if !s.ready {
		l.dropSessionLocked()
		return
	}
	l.stopGraceLocked()
	l.grace = time.AfterFunc(l.timing.grace, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.sess == s && len(l.subs) == 0 {
			l.dropSessionLocked()
		}
	})
}

func (l *Live) stopGraceLocked() {
	if l.grace != nil {
		l.grace.Stop()
		l.grace = nil
	}
}

// dropSessionLocked ends the current session: its goroutine sends what is
// queued, closes the socket and exits.
func (l *Live) dropSessionLocked() {
	l.stopGraceLocked()
	if l.sess != nil {
		l.sess.cancel()
		l.sess = nil
	}
	l.state = "idle"
}

// wanted reports whether session s should keep (re)connecting; a session
// left without subscriptions while not connected ends here.
func (l *Live) wanted(s *liveSession) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sess != s || s.ctx.Err() != nil {
		return false
	}
	if len(l.subs) == 0 {
		l.dropSessionLocked()
		return false
	}
	return true
}

// send queues a frame and wakes the run goroutine. Caller holds Live.mu.
func (s *liveSession) send(f liveFrame) {
	s.outbox = append(s.outbox, f)
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func subscribeFrame(sub *liveSub) liveFrame {
	return liveFrame{ID: sub.id, Type: "subscribe", Payload: sub.payload}
}

func (l *Live) setState(s *liveSession, state string) {
	l.mu.Lock()
	if l.sess == s {
		l.state = state
	}
	l.mu.Unlock()
}

// run is the live goroutine of one session: connect, serve, and on a drop
// back off and connect again, until the session ends. The attempt counter
// resets only after a connection stayed acked for timing.stable.
func (l *Live) run(s *liveSession) {
	defer l.wg.Done()
	attempt := 0
	for {
		if up := l.connect(s); up >= l.timing.stable {
			attempt = 0
		}
		if !l.wanted(s) {
			return
		}
		d := liveRetry[min(attempt, len(liveRetry)-1)]
		attempt++
		l.setState(s, "retrying")
		if err := l.Sleep(s.ctx, d); err != nil || !l.wanted(s) {
			return
		}
	}
}

// failAll reports msg to every subscription as a connection failure
// (outside the lock), unless the session has ended.
func (l *Live) failAll(s *liveSession, msg string) {
	l.mu.Lock()
	if l.sess != s || s.ctx.Err() != nil {
		l.mu.Unlock()
		return
	}
	fails := make([]func(string, LiveFail), 0, len(l.subs))
	for _, sub := range l.subs {
		if sub.fail != nil {
			fails = append(fails, sub.fail)
		}
	}
	l.mu.Unlock()
	for _, f := range fails {
		f(msg, LiveFailConn)
	}
}

// connect runs one connection until it drops or the session ends, and
// returns how long it stayed acked (0: never acked).
func (l *Live) connect(s *liveSession) time.Duration {
	l.setState(s, "connecting")
	dctx, cancel := context.WithTimeout(s.ctx, liveDialTimeout)
	conn, err := l.dial(dctx, l.endpoint)
	cancel()
	if err != nil {
		l.failAll(s, "live: connect: "+err.Error())
		return 0
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(liveReadLimit)

	st := l.tokens.Get(s.ctx)
	if s.ctx.Err() != nil {
		return 0
	}
	if st.Missing != "" {
		l.failAll(s, st.Missing)
		return 0
	}

	// The reader hands frames over until the connection fails or this
	// function returns; it exits before connect does. Cancelling readCtx
	// forces the socket shut (the library closes it when a read's context
	// ends).
	readCtx, cancelRead := context.WithCancel(context.Background())
	defer cancelRead()
	incoming := make(chan []byte)
	readErr := make(chan error, 1)
	done := make(chan struct{})
	var reader sync.WaitGroup
	reader.Add(1)
	go func() {
		defer reader.Done()
		for {
			_, data, err := conn.Read(readCtx)
			if err != nil {
				readErr <- err
				return
			}
			select {
			case incoming <- data:
			case <-done:
				return
			}
		}
	}()
	defer func() {
		_ = conn.CloseNow()
		close(done)
		reader.Wait()
	}()
	defer l.notReady(s)

	init := liveFrame{Type: "connection_init", Payload: map[string]string{"Authorization": "Bearer " + st.Token}}
	if writeFrame(s.ctx, conn, init) != nil {
		l.failAll(s, liveNoAck)
		return 0
	}

	// Writes are bounded by their own timeout, not by the session: frames
	// queued by the last stop (complete) still go out before the close.
	wctx := context.WithoutCancel(s.ctx)
	var ackedAt time.Time
	upFor := func() time.Duration {
		if ackedAt.IsZero() {
			return 0
		}
		return time.Since(ackedAt)
	}
	// lost reports a dropped connection to every subscription, the same way
	// whatever dropped it: not ended (the subscriptions are kept and sent
	// again), and with the socket no longer open, so a screen can tell the
	// failure is over once it is open again.
	lost := func(msg string) time.Duration {
		if ackedAt.IsZero() {
			msg = liveNoAck
		}
		l.notReady(s)
		l.setState(s, "retrying")
		l.failAll(s, msg)
		return upFor()
	}
	ackTimer := time.NewTimer(l.timing.ackWait)
	defer ackTimer.Stop()
	pinger := time.NewTicker(l.timing.ping)
	pinger.Stop() // started on connection_ack
	defer pinger.Stop()
	var pingC, pongC <-chan time.Time
	var pongTimer *time.Timer
	defer func() {
		if pongTimer != nil {
			pongTimer.Stop()
		}
	}()

	for {
		select {
		case <-s.ctx.Done():
			l.closeConn(s, conn, incoming, cancelRead)
			return upFor()
		case err := <-readErr:
			if ackedAt.IsZero() {
				if code := websocket.CloseStatus(err); code != -1 {
					l.failAll(s, fmt.Sprintf("live: connection rejected (code %d)", code))
				} else {
					l.failAll(s, liveNoAck)
				}
				return 0
			}
			return lost(liveLost)
		case <-ackTimer.C:
			if ackedAt.IsZero() {
				l.failAll(s, liveNoAck)
				return 0
			}
		case <-s.wake:
			if !l.flush(wctx, s, conn) {
				return lost(liveLost)
			}
		case <-pingC:
			if pongC == nil {
				if writeFrame(wctx, conn, liveFrame{Type: "ping"}) != nil {
					return lost(liveLost)
				}
				pongTimer = time.NewTimer(l.timing.pongWait)
				pongC = pongTimer.C
			}
		case <-pongC:
			// The server stopped answering: live updates may have been
			// missing for a while, so say so, then reconnect.
			return lost(liveNoPong)
		case data := <-incoming:
			var m liveMessage
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			switch m.Type {
			case "connection_ack":
				if !ackedAt.IsZero() {
					continue
				}
				ackedAt = time.Now()
				ackTimer.Stop()
				pinger.Reset(l.timing.ping)
				pingC = pinger.C
				l.ack(s)
				if !l.flush(wctx, s, conn) {
					return lost(liveLost)
				}
			case "ping":
				if writeFrame(wctx, conn, liveFrame{Type: "pong"}) != nil {
					return lost(liveLost)
				}
			case "pong":
				if pongTimer != nil {
					pongTimer.Stop()
					pongTimer, pongC = nil, nil
				}
			case "next", "error", "complete":
				if !ackedAt.IsZero() {
					l.route(s, m)
				}
			}
		}
	}
}

// closeConn ends a connection the session no longer wants: the queued frames
// (complete) and the close handshake share one timing.closeWait deadline,
// after which the socket is shut regardless.
func (l *Live) closeConn(s *liveSession, conn *websocket.Conn, incoming <-chan []byte, cancelRead context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), l.timing.closeWait)
	defer cancel()
	l.flush(ctx, s, conn)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()
	deadline := ctx.Done()
	for {
		select {
		case <-closed:
			return
		case <-incoming: // keep the reader reading so the peer's close frame is seen
		case <-deadline:
			deadline = nil
			cancelRead()
		}
	}
}

// ack marks the session ready and queues every live subscription, in the
// order they were made.
func (l *Live) ack(s *liveSession) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sess != s {
		return
	}
	s.ready = true
	l.state = "open"
	subs := make([]*liveSub, 0, len(l.subs))
	for _, sub := range l.subs {
		subs = append(subs, sub)
	}
	slices.SortFunc(subs, func(a, b *liveSub) int { return a.seq - b.seq })
	s.outbox = s.outbox[:0]
	for _, sub := range subs {
		s.outbox = append(s.outbox, subscribeFrame(sub))
	}
}

// notReady forgets the dead connection's queue: the next ack resubscribes.
func (l *Live) notReady(s *liveSession) {
	l.mu.Lock()
	s.ready = false
	s.outbox = nil
	l.mu.Unlock()
}

// flush sends the queued frames; false means the connection failed.
func (l *Live) flush(ctx context.Context, s *liveSession, conn *websocket.Conn) bool {
	l.mu.Lock()
	out := s.outbox
	s.outbox = nil
	l.mu.Unlock()
	for _, f := range out {
		if writeFrame(ctx, conn, f) != nil {
			return false
		}
	}
	return true
}

func writeFrame(ctx context.Context, conn *websocket.Conn, f liveFrame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, liveWriteTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, data)
}

type gqlMessage struct {
	Message string `json:"message"`
}

func joinMessages(errs []gqlMessage) string {
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Message)
	}
	return strings.Join(msgs, "; ")
}

// route delivers next/error/complete to its subscription. error and complete
// end the subscription on the server's side.
func (l *Live) route(s *liveSession, m liveMessage) {
	l.mu.Lock()
	sub := l.subs[m.ID]
	if l.sess != s {
		sub = nil
	}
	l.mu.Unlock()
	if sub == nil {
		return
	}
	switch m.Type {
	case "next":
		var p struct {
			Data   json.RawMessage `json:"data"`
			Errors []gqlMessage    `json:"errors"`
		}
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			callFail(sub, "live: malformed next message", LiveFailNext)
			return
		}
		if len(p.Errors) > 0 {
			callFail(sub, joinMessages(p.Errors), LiveFailNext)
			return
		}
		if sub.next != nil {
			sub.next(p.Data)
		}
	case "error":
		l.end(m.ID, false)
		var errs []gqlMessage
		msg := "live: subscription failed"
		if json.Unmarshal(m.Payload, &errs) == nil && len(errs) > 0 {
			msg = joinMessages(errs)
		}
		callFail(sub, msg, LiveFailEnded)
	case "complete":
		l.end(m.ID, false)
		callFail(sub, liveStreamEnded, LiveFailEnded)
	}
}

func callFail(sub *liveSub, msg string, why LiveFail) {
	if sub.fail != nil {
		sub.fail(msg, why)
	}
}
