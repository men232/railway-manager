// Package logs is the Logs screen: the live tail of one service's running
// deployment. On Resume it finds the running deployment in the store, reads
// its newest 500 lines and then subscribes to the rest; Suspend stops the
// subscription. Lines are sanitized when they arrive, kept in a ring of the
// newest 2000 and drawn one per row, cut to the width and never wrapped.
//
// A generation counter, bumped by Resume and Suspend, drops reads of an
// earlier round; a live message counts only while the subscription that sent
// it is still held. A stream the server ends reads the states once (a
// redeploy shows up as a new running deployment, which is then followed) and
// is never made again for the same deployment: r reads it again by hand.
// Coming back on top (a modal closed) keeps the lines and the position and
// only makes the stream again.
package logs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
)

const (
	// loadLimit is how many lines are read on Resume, and the backlog the
	// subscription is asked for.
	loadLimit = 500
	// ringSize is how many lines are kept.
	ringSize = 2000
	// maxLineRunes bounds a stored line; the view cuts it to the width.
	maxLineRunes = 4096
	// stampWidth is "15:04:05 ".
	stampWidth = 9
)

type (
	loadedMsg struct {
		gen   uint64
		dep   string
		lines []railway.LogLine
		err   error
	}
	liveLinesMsg struct {
		id    uint64
		lines []railway.LogLine
	}
	liveFailMsg struct {
		id   uint64
		text string
		why  railway.LiveFail
	}
)

// entry is one stored line, sanitized; seq orders every line ever pushed.
type entry struct {
	seq uint64
	at  time.Time
	sev string
	msg string
}

// ring keeps the newest ringSize entries, oldest first.
type ring struct {
	buf   []entry
	start int
	seq   uint64
}

func (r *ring) push(e entry) {
	r.seq++
	e.seq = r.seq
	if len(r.buf) < ringSize {
		r.buf = append(r.buf, e)
		return
	}
	r.buf[r.start] = e
	r.start = (r.start + 1) % ringSize
}

func (r *ring) len() int { return len(r.buf) }

func (r *ring) at(i int) entry { return r.buf[(r.start+i)%len(r.buf)] }

// reset drops every line; seq goes on, so an old anchor never matches a new
// line by accident.
func (r *ring) reset() { r.buf, r.start = r.buf[:0], 0 }

type screen struct {
	place  domain.Place
	filter *components.Filter
	lines  ring
	follow bool   // the view shows the newest lines
	anchor uint64 // paused: the seq of the top line

	gen     uint64 // bumped by Resume and Suspend
	active  bool   // between Resume and Suspend
	dep     string // the deployment shown; "" = none (yet)
	waiting bool   // the states are being read to find dep
	loading bool
	loadErr string // sanitized

	subSeq  uint64 // the last subscription id
	sub     *liveSub
	ended   bool // the server ended dep's stream
	endRead bool // that end has read the states
	liveErr string
	// liveConnErr: liveErr is a connection failure; it is over once the
	// socket is open again
	liveConnErr bool
	// seen are the lines held when the stream was made, by time and
	// message: its backlog repeats them, and each live line matching one
	// uses it up.
	seen map[lineKey]int
}

type lineKey struct {
	at  int64 // Unix nanoseconds; that of the zero time when none
	msg string
}

func keyOf(e entry) lineKey { return lineKey{e.at.UnixNano(), e.msg} }

type liveSub struct {
	id   uint64
	stop func()
}

// New returns the logs screen of a service.
func New(p domain.Place) tui.Screen {
	return &screen{place: p, filter: components.NewFilter("logs:" + p.Key()), follow: true}
}

// Build is the registry constructor: the only argument is the domain.Place
// of the service. It returns nil for any other arguments.
func Build(args ...any) tui.Screen {
	if len(args) != 1 {
		return nil
	}
	p, ok := args[0].(domain.Place)
	if !ok {
		return nil
	}
	return New(p)
}

func (s *screen) Init(ctx *tui.Ctx) tea.Cmd { s.filter.Load(ctx); return s.Resume(ctx) }
func (s *screen) Title() string             { return "Logs" }

// Capturing reports whether the filter takes every key.
func (s *screen) Capturing() bool { return s.filter.Editing() }

// Resume starts a new round. When the deployment shown still runs and its
// lines were read, they stay (with the follow state and position) and only
// the stream is made again, unless the server ended it; otherwise it is a
// fresh start.
func (s *screen) Resume(ctx *tui.Ctx) tea.Cmd {
	if ctx.Store != nil && s.dep != "" && s.lines.len() > 0 && s.loadErr == "" && !s.loading {
		if st, _ := ctx.Store.State(s.place.EnvID, s.place.ServiceID); st.ActiveID == s.dep {
			s.gen++
			s.active = true
			s.stop()
			s.liveErr, s.liveConnErr = "", false
			if !s.ended {
				s.subscribe(ctx)
			}
			return nil
		}
	}
	return s.restart(ctx)
}

// restart finds the running deployment again and reads it from scratch.
func (s *screen) restart(ctx *tui.Ctx) tea.Cmd {
	s.gen++
	s.active = true
	s.stop()
	s.liveErr, s.liveConnErr, s.loadErr = "", false, ""
	if ctx.Store == nil {
		return nil
	}
	if !ctx.Store.StatesRead(s.place.EnvID) {
		s.dep, s.waiting = "", true
		s.lines.reset()
		return s.readStates(ctx)
	}
	st, _ := ctx.Store.State(s.place.EnvID, s.place.ServiceID)
	if st.ActiveID == "" {
		s.dep = ""
		s.lines.reset()
		return nil
	}
	return s.start(ctx, st.ActiveID)
}

// Suspend ends the round: the stream stops and late messages are dropped.
func (s *screen) Suspend(*tui.Ctx) {
	s.gen++
	s.active = false
	s.stop()
	s.liveErr, s.liveConnErr = "", false
}

func (s *screen) stop() {
	if s.sub != nil {
		s.sub.stop()
		s.sub = nil
	}
}

// readStates reads the deployment states of the service's environment.
func (s *screen) readStates(ctx *tui.Ctx) tea.Cmd {
	if ctx.API == nil {
		s.waiting = false
		return nil
	}
	s.waiting = true
	// its own load key: a failure here is not the Services screen's
	return loaders.PlaceStates(ctx.API, s.place, ctx.Clock())
}

// start shows deployment dep: its lines are read, then streamed.
func (s *screen) start(ctx *tui.Ctx, dep string) tea.Cmd {
	s.stop()
	s.dep, s.waiting = dep, false
	s.lines.reset()
	s.loadErr, s.liveErr, s.liveConnErr = "", "", false
	s.ended, s.endRead = false, false
	s.seen = nil
	if ctx.API == nil {
		s.loading = false
		return nil
	}
	s.loading = true
	api, gen := ctx.API, s.gen
	return func() tea.Msg {
		lines, err := api.DeploymentLogs(context.Background(), dep, loadLimit)
		return loadedMsg{gen: gen, dep: dep, lines: lines, err: err}
	}
}

// subscribe streams dep's lines, forwarded through ctx.Send tagged with a
// fresh id (a callback can still run after stop).
func (s *screen) subscribe(ctx *tui.Ctx) {
	if ctx.Live == nil || ctx.Send == nil || s.dep == "" {
		return
	}
	// the backlog repeats up to loadLimit of the newest lines held
	s.seen = map[lineKey]int{}
	for i := max(0, s.lines.len()-loadLimit); i < s.lines.len(); i++ {
		s.seen[keyOf(s.lines.at(i))]++
	}
	s.subSeq++
	id, send := s.subSeq, ctx.Send
	stop := ctx.Live.Subscribe(railway.DeploymentLogsSubscription,
		map[string]any{"deploymentId": s.dep, "limit": loadLimit},
		func(data json.RawMessage) {
			lines, err := railway.DecodeDeploymentLogs(data)
			if err != nil {
				send(liveFailMsg{id: id, text: "malformed log lines"})
				return
			}
			send(liveLinesMsg{id: id, lines: lines})
		},
		func(msg string, why railway.LiveFail) { send(liveFailMsg{id: id, text: msg, why: why}) })
	s.sub = &liveSub{id: id, stop: stop}
}

// held reports whether a live message comes from the subscription held.
func (s *screen) held(id uint64) bool { return s.active && s.sub != nil && s.sub.id == id }

// clean is a line as stored: sanitized and bounded.
func clean(l railway.LogLine) entry {
	msg := text.Sanitize(l.Message)
	if rs := []rune(msg); len(rs) > maxLineRunes {
		msg = string(rs[:maxLineRunes-1]) + "…"
	}
	return entry{at: l.Timestamp, sev: strings.ToLower(text.Sanitize(l.Severity)), msg: msg}
}

// pushLive adds a live batch, minus the lines already held: a line matching
// one held when the stream was made uses that match up.
func (s *screen) pushLive(lines []railway.LogLine) {
	for _, l := range lines {
		e := clean(l)
		if k := keyOf(e); s.seen[k] > 0 {
			s.seen[k]--
			continue
		}
		s.lines.push(e)
	}
}

func (s *screen) Update(ctx *tui.Ctx, msg tea.Msg) (tui.Screen, tea.Cmd) {
	switch m := msg.(type) {
	case loadedMsg:
		if m.gen != s.gen || !s.active || m.dep != s.dep {
			return s, nil
		}
		s.loading = false
		if m.err != nil {
			s.loadErr = text.Sanitize(m.err.Error())
			return s, nil
		}
		for _, l := range m.lines {
			s.lines.push(clean(l))
		}
		s.subscribe(ctx)
		return s, nil
	case liveLinesMsg:
		if !s.held(m.id) {
			return s, nil
		}
		s.pushLive(m.lines)
		return s, nil
	case liveFailMsg:
		if !s.held(m.id) {
			return s, nil
		}
		if m.why != railway.LiveFailEnded {
			s.liveErr = text.Sanitize(m.text)
			s.liveConnErr = m.why == railway.LiveFailConn
			return s, nil
		}
		// over at Live; never made again for this deployment
		s.stop()
		s.ended = true
		if m.text != railway.LiveStreamEnded {
			s.liveErr, s.liveConnErr = text.Sanitize(m.text), false
		}
		if s.endRead || ctx.Store == nil {
			return s, nil
		}
		// a redeploy ends the stream: read what runs now, once
		s.endRead = true
		return s, s.readStates(ctx)
	case loaders.StatesMsg:
		// the root has applied it to the store
		if !s.active || ctx.Store == nil || m.ProjectID != s.place.ProjectID {
			return s, nil
		}
		s.waiting = false
		st, _ := ctx.Store.State(s.place.EnvID, s.place.ServiceID)
		if st.ActiveID != "" && st.ActiveID != s.dep {
			return s, s.start(ctx, st.ActiveID)
		}
		// only this screen's own read says it failed; a partial answer
		// (another environment failed) is no failure here
		var partial *railway.PartialError
		if s.dep == "" && m.Key == store.PlaceStatesKey(s.place) && m.Err != nil && !errors.As(m.Err, &partial) {
			s.loadErr = text.Sanitize(m.Err.Error())
		}
		return s, nil
	case tea.KeyMsg:
		if h, cmd := s.filter.Update(ctx, m); h {
			return s, cmd
		}
		return s, s.key(ctx, m)
	}
	return s, nil
}

// bodyHeight is the rows left for lines in a body of the given height.
func (s *screen) bodyHeight(height int) int {
	h := height - 1 // the header
	if s.filter.Active() || s.filter.Editing() {
		h--
	}
	return max(1, h)
}

// shown are the entries matching the filter, oldest first.
func (s *screen) shown() []entry {
	out := make([]entry, 0, s.lines.len())
	for i := range s.lines.len() {
		if e := s.lines.at(i); s.filter.Match(e.msg) {
			out = append(out, e)
		}
	}
	return out
}

// top is the index of the first row drawn.
func (s *screen) top(rows []entry, h int) int {
	last := max(0, len(rows)-h)
	if s.follow {
		return last
	}
	for i, e := range rows {
		if e.seq >= s.anchor {
			return min(i, last)
		}
	}
	return last
}

func (s *screen) key(ctx *tui.Ctx, k tea.KeyMsg) tea.Cmd {
	h := s.bodyHeight(ctx.Height - tui.ChromeRows)
	rows := s.shown()
	scroll := func(d int) {
		if len(rows) == 0 {
			return
		}
		t := max(0, min(s.top(rows, h)+d, max(0, len(rows)-h)))
		s.follow, s.anchor = false, rows[t].seq
	}
	switch k.Type {
	case tea.KeyUp:
		scroll(-1)
	case tea.KeyDown:
		scroll(1)
	case tea.KeyPgUp:
		scroll(-h)
	case tea.KeyPgDown:
		scroll(h)
	case tea.KeyHome:
		scroll(-len(rows))
	case tea.KeyEnd:
		s.follow = true
	case tea.KeyRunes:
		switch string(k.Runes) {
		case "k":
			scroll(-1)
		case "j":
			scroll(1)
		case "f":
			if s.follow {
				scroll(0)
			} else {
				s.follow = true
			}
		case "r":
			return s.restart(ctx)
		}
	}
	return nil
}

func (s *screen) Keys() []tui.KeyHint {
	if s.filter.Editing() {
		return []tui.KeyHint{
			{Key: "enter", Label: "keep filter", Always: true},
			{Key: "esc", Label: "clear", Always: true},
		}
	}
	follow := "follow"
	if s.follow {
		follow = "pause"
	}
	return []tui.KeyHint{
		{Key: "f", Label: follow, Rank: 1},
		{Key: "r", Label: "read again", Rank: 2},
		{Key: "/", Label: "filter", Rank: 3, Optional: true},
		{Key: "esc", Label: "back", Always: true},
	}
}

// Note is the line count and whether the view follows.
func (s *screen) Note(*tui.Ctx) string {
	if s.dep == "" {
		return ""
	}
	state := "following"
	if !s.follow {
		state = "paused"
	}
	return fmt.Sprintf("%d lines · %s", s.lines.len(), state)
}

// Notice says why lines may not be arriving.
func (s *screen) Notice(ctx *tui.Ctx) tui.FlashMsg {
	switch {
	case s.sub != nil && ctx.Live != nil && ctx.Live.State() == "retrying":
		return tui.FlashMsg{Kind: '✗', Text: "live updates paused · retrying"}
	case s.liveErr != "" && !(s.liveConnErr && ctx.Live != nil && ctx.Live.State() == "open"):
		return tui.FlashMsg{Kind: '!', Text: "live: " + strings.TrimPrefix(s.liveErr, "live: ")}
	case s.ended:
		return tui.FlashMsg{Kind: '!', Text: "stream ended · r reads again"}
	}
	return tui.FlashMsg{}
}

func (s *screen) View(ctx *tui.Ctx, width, height int) string {
	if ctx.Store == nil {
		return ""
	}
	th := ctx.Theme
	header := th.Accent.Render(text.Sanitize(ctx.EnvName(s.place.EnvID) + " · " + ctx.Store.ServiceName(s.place.ServiceID)))
	if s.dep != "" {
		header += th.Muted.Render(" · " + text.Sanitize(s.dep))
	}
	out := []string{ansi.Truncate(header, width, "…")}
	rows := s.shown()
	if f := s.filter.View(width, len(rows), s.lines.len()); f != "" {
		out = append(out, f)
	}
	if line, ok := s.state(th, len(rows)); ok {
		out = append(out, ansi.Truncate(line, width, "…"))
	} else {
		h := s.bodyHeight(height)
		from := s.top(rows, h)
		loc := ctx.Clock()().Location()
		for _, e := range rows[from:min(len(rows), from+h)] {
			out = append(out, s.line(th, e, loc, width))
		}
	}
	if len(out) > height {
		out = out[:max(height, 0)]
	}
	return strings.Join(out, "\n")
}

// state is the line shown instead of the log lines, if any.
func (s *screen) state(th tui.Theme, shown int) (string, bool) {
	switch {
	case s.dep == "" && s.waiting:
		return th.Muted.Render("reading the deployment"), true
	case s.dep == "" && s.loadErr != "":
		return th.Danger.Render("✗ cannot read the deployment: " + s.loadErr + " · r reads again"), true
	case s.dep == "":
		return th.Warn.Render("! nothing running"), true
	case s.loading && s.lines.len() == 0:
		return th.Muted.Render("reading logs"), true
	case s.loadErr != "":
		return th.Danger.Render("✗ cannot read the logs: " + s.loadErr + " · r reads again"), true
	case s.lines.len() == 0:
		return th.Muted.Render("no log lines yet"), true
	case shown == 0:
		return th.Muted.Render("no line matches the filter"), true
	}
	return "", false
}

// tone is a severity's style; nil for none.
func tone(th tui.Theme, sev string) *lipgloss.Style {
	switch {
	case strings.HasPrefix(sev, "err"), strings.HasPrefix(sev, "fatal"),
		strings.HasPrefix(sev, "crit"), strings.HasPrefix(sev, "panic"):
		return &th.Danger
	case strings.HasPrefix(sev, "warn"):
		return &th.Warn
	}
	return nil
}

// line draws one entry: its time, muted, then the message cut to the width.
func (s *screen) line(th tui.Theme, e entry, loc *time.Location, width int) string {
	stamp := strings.Repeat(" ", stampWidth-1)
	if !e.at.IsZero() {
		stamp = e.at.In(loc).Format("15:04:05")
	}
	if width <= stampWidth {
		return th.Muted.Render(ansi.Truncate(stamp, width, ""))
	}
	msg := ansi.Truncate(e.msg, width-stampWidth, "…")
	if st := tone(th, e.sev); st != nil {
		msg = st.Render(msg)
	}
	return th.Muted.Render(stamp) + " " + msg
}
