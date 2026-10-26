package railwaytest

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// WSRequest is one websocket upgrade the fake accepted.
type WSRequest struct {
	URI      string
	Header   http.Header
	Protocol string // the negotiated subprotocol
}

// WSSub is one subscribe message the fake received.
type WSSub struct {
	ID    string
	Query string
	Vars  map[string]any
}

type wsConn struct {
	c    *websocket.Conn
	subs map[string]bool
}

type wsState struct {
	mu         sync.Mutex
	conns      map[*wsConn]bool
	requests   []WSRequest
	inits      []map[string]any
	subscribed []WSSub
	completes  []string
	pongs      int
	pings      int

	ackGate      chan struct{} // non-nil: connection_ack waits for it to close
	rejectCode   int           // non-zero: connection_init is answered by this close
	rejectReason string
	noPong       int           // pings left to leave unanswered
	stall        chan struct{} // non-nil: the handler stops reading until it closes
}

func isUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// serveWS speaks the server side of graphql-transport-ws: it acks every
// connection_init, tracks subscriptions per connection and answers ping.
func (f *Fake) serveWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"graphql-transport-ws"}})
	if err != nil {
		return
	}
	s := &f.ws
	wc := &wsConn{c: c, subs: map[string]bool{}}
	s.mu.Lock()
	s.requests = append(s.requests, WSRequest{URI: r.RequestURI, Header: r.Header.Clone(), Protocol: c.Subprotocol()})
	s.conns[wc] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, wc)
		s.mu.Unlock()
		_ = c.CloseNow()
	}()
	if c.Subprotocol() != "graphql-transport-ws" {
		_ = c.Close(4406, "Subprotocol not acceptable")
		return
	}
	ctx := context.Background()
	for {
		s.mu.Lock()
		stall := s.stall
		s.mu.Unlock()
		if stall != nil {
			<-stall
		}
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var m struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(data, &m) != nil {
			_ = c.Close(4400, "Invalid message")
			return
		}
		switch m.Type {
		case "connection_init":
			var p map[string]any
			_ = json.Unmarshal(m.Payload, &p)
			s.mu.Lock()
			s.inits = append(s.inits, p)
			gate, code, reason := s.ackGate, s.rejectCode, s.rejectReason
			s.mu.Unlock()
			if code != 0 {
				_ = c.Close(websocket.StatusCode(code), reason)
				return
			}
			if gate != nil {
				<-gate
			}
			writeWS(c, map[string]any{"type": "connection_ack"})
		case "subscribe":
			var p struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			_ = json.Unmarshal(m.Payload, &p)
			s.mu.Lock()
			wc.subs[m.ID] = true
			s.subscribed = append(s.subscribed, WSSub{ID: m.ID, Query: p.Query, Vars: p.Variables})
			s.mu.Unlock()
		case "complete":
			s.mu.Lock()
			delete(wc.subs, m.ID)
			s.completes = append(s.completes, m.ID)
			s.mu.Unlock()
		case "ping":
			s.mu.Lock()
			s.pings++
			answer := s.noPong == 0
			if !answer {
				s.noPong--
			}
			s.mu.Unlock()
			if answer {
				writeWS(c, map[string]any{"type": "pong"})
			}
		case "pong":
			s.mu.Lock()
			s.pongs++
			s.mu.Unlock()
		}
	}
}

func writeWS(c *websocket.Conn, msg any) {
	data, _ := json.Marshal(msg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Write(ctx, websocket.MessageText, data)
}

// subscribers waits (up to 5 s) until a connection has subscription id and
// returns those connections; the subscription may be ended by the server.
func (f *Fake) subscribers(id string, end bool) []*wsConn {
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.ws.mu.Lock()
		var out []*wsConn
		for wc := range f.ws.conns {
			if wc.subs[id] {
				out = append(out, wc)
				if end {
					delete(wc.subs, id)
				}
			}
		}
		f.ws.mu.Unlock()
		if len(out) > 0 {
			return out
		}
		if time.Now().After(deadline) {
			f.t.Errorf("railwaytest: no connection subscribed %q", id)
			return nil
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// WSPush sends a next message with data to subscription subID, waiting (up to
// 5 s) for a connection to have subscribed it.
func (f *Fake) WSPush(subID string, data any) {
	for _, wc := range f.subscribers(subID, false) {
		writeWS(wc.c, map[string]any{"id": subID, "type": "next", "payload": map[string]any{"data": data}})
	}
}

// WSPushErrors sends a next message carrying GraphQL errors (no data).
func (f *Fake) WSPushErrors(subID string, msgs ...string) {
	for _, wc := range f.subscribers(subID, false) {
		writeWS(wc.c, map[string]any{"id": subID, "type": "next", "payload": map[string]any{"data": nil, "errors": gqlErrors(msgs)}})
	}
}

// WSError sends an error message, which ends subscription subID.
func (f *Fake) WSError(subID string, msgs ...string) {
	for _, wc := range f.subscribers(subID, true) {
		writeWS(wc.c, map[string]any{"id": subID, "type": "error", "payload": gqlErrors(msgs)})
	}
}

// WSComplete sends complete, which ends subscription subID.
func (f *Fake) WSComplete(subID string) {
	for _, wc := range f.subscribers(subID, true) {
		writeWS(wc.c, map[string]any{"id": subID, "type": "complete"})
	}
}

func gqlErrors(msgs []string) []gqlError {
	out := make([]gqlError, len(msgs))
	for i, m := range msgs {
		out[i] = gqlError{Message: m}
	}
	return out
}

func (f *Fake) openConns() []*wsConn {
	f.ws.mu.Lock()
	defer f.ws.mu.Unlock()
	out := make([]*wsConn, 0, len(f.ws.conns))
	for wc := range f.ws.conns {
		out = append(out, wc)
	}
	return out
}

// WSPing sends a ping on every open connection.
func (f *Fake) WSPing() {
	for _, wc := range f.openConns() {
		writeWS(wc.c, map[string]any{"type": "ping"})
	}
}

// WSDrop cuts every open connection without a close handshake. They stop
// counting as open (and as subscribed) at once.
func (f *Fake) WSDrop() {
	f.ws.mu.Lock()
	conns := make([]*wsConn, 0, len(f.ws.conns))
	for wc := range f.ws.conns {
		conns = append(conns, wc)
		delete(f.ws.conns, wc)
	}
	f.ws.mu.Unlock()
	for _, wc := range conns {
		_ = wc.c.CloseNow()
	}
}

// WSOpen is the number of open websocket connections.
func (f *Fake) WSOpen() int {
	f.ws.mu.Lock()
	defer f.ws.mu.Unlock()
	return len(f.ws.conns)
}

// WSRequests are the websocket upgrades accepted, in order.
func (f *Fake) WSRequests() []WSRequest {
	f.ws.mu.Lock()
	defer f.ws.mu.Unlock()
	return append([]WSRequest(nil), f.ws.requests...)
}

// WSInits are the connection_init payloads received, in order.
func (f *Fake) WSInits() []map[string]any {
	f.ws.mu.Lock()
	defer f.ws.mu.Unlock()
	return append([]map[string]any(nil), f.ws.inits...)
}

// WSSubscribed are the subscribe messages received, in order.
func (f *Fake) WSSubscribed() []WSSub {
	f.ws.mu.Lock()
	defer f.ws.mu.Unlock()
	return append([]WSSub(nil), f.ws.subscribed...)
}

// WSCompletes are the ids of the complete messages received, in order.
func (f *Fake) WSCompletes() []string {
	f.ws.mu.Lock()
	defer f.ws.mu.Unlock()
	return append([]string(nil), f.ws.completes...)
}

// WSPongs is the number of pong messages received.
func (f *Fake) WSPongs() int {
	f.ws.mu.Lock()
	defer f.ws.mu.Unlock()
	return f.ws.pongs
}

// WSHoldAcks makes connection_ack wait until WSReleaseAcks.
func (f *Fake) WSHoldAcks() {
	f.ws.mu.Lock()
	f.ws.ackGate = make(chan struct{})
	f.ws.mu.Unlock()
}

// WSReleaseAcks sends the held acks and acks at once from now on.
func (f *Fake) WSReleaseAcks() {
	f.ws.mu.Lock()
	if f.ws.ackGate != nil {
		close(f.ws.ackGate)
		f.ws.ackGate = nil
	}
	f.ws.mu.Unlock()
}

// WSRejectInits answers every connection_init by closing with code and
// reason (code 0 acks again).
func (f *Fake) WSRejectInits(code int, reason string) {
	f.ws.mu.Lock()
	f.ws.rejectCode, f.ws.rejectReason = code, reason
	f.ws.mu.Unlock()
}

// WSWithholdPongs leaves the client's next n pings unanswered.
func (f *Fake) WSWithholdPongs(n int) {
	f.ws.mu.Lock()
	f.ws.noPong = n
	f.ws.mu.Unlock()
}

// WSPings is the number of ping messages received.
func (f *Fake) WSPings() int {
	f.ws.mu.Lock()
	defer f.ws.mu.Unlock()
	return f.ws.pings
}

// WSStallReads makes every handler stop reading after the message it is
// reading now (so a close frame goes unanswered), until WSResumeReads.
func (f *Fake) WSStallReads() {
	f.ws.mu.Lock()
	f.ws.stall = make(chan struct{})
	f.ws.mu.Unlock()
}

// WSResumeReads lets stalled handlers read again.
func (f *Fake) WSResumeReads() {
	f.ws.mu.Lock()
	if f.ws.stall != nil {
		close(f.ws.stall)
		f.ws.stall = nil
	}
	f.ws.mu.Unlock()
}
