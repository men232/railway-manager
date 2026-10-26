// Package services is the Services screen: a live monitor of every service
// of the read environments — status, one-line CPU, memory and traffic charts
// and the pending change count, with a panel under the table for the service
// under the cursor (panel.go). It opens a service's variables, domains and
// logs, queues a redeploy, a limits change or a disk resize (edit.go), and
// restarts a service at once.
//
// While on top it reads the deployment states every 10 s and the metrics
// every 30 s, and subscribes to Railway's events on every running deployment
// and every deployment in progress; any event reads the states again after
// 250 ms (coalesced, at most 2 s after the first). A generation counter,
// bumped by Resume and Suspend, makes every timer of an earlier round a
// no-op; a live message counts only while the subscription that sent it is
// still held. The panel reads its service's detail and charts 300 ms after
// the cursor rests there, then the charts every 30 s; its own counters drop
// timers for another place or level. A chart level is a time per point
// (1m, 5m, 15m, 1h); the panel reads two points a chart cell, so the span
// follows the chart width, and reads again when a resize changes the cells,
// once the resizing rests.
package services

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/monitor"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
	"github.com/men232/railway-manager/internal/view"
)

// Subscription documents (old useMonitor.ts): the running deployment's
// instances, and a deployment in progress.
const (
	instancesDoc = `subscription MonitorInstances($input: DeploymentInstanceExecutionInput!) {
  deploymentInstanceExecutions(input: $input) { id status }
}`
	deploymentDoc = `subscription MonitorDeployment($id: String!) { deployment(id: $id) { id status } }`
)

// tick schedules a message; tests replace it.
var tick = tea.Tick

// liveMaxWait bounds the live debounce: a steady stream of events closer than
// loaders.LiveDebounce still reads the states this long after the first.
const liveMaxWait = 2 * time.Second

// Messages of the poll loop, tagged with the generation that started it, and
// of the live subscriptions, tagged with the subscription that sent it.
type (
	statesTickMsg  struct{ gen uint64 }
	metricsTickMsg struct{ gen uint64 }
	// debounceMsg ends a burst of live events: the newest event's timer
	// (seq) or the burst's max-wait timer (maxWait).
	debounceMsg struct {
		gen, burst, seq uint64
		maxWait         bool
	}
	liveEventMsg struct {
		key string
		id  uint64
	}
	liveFailMsg struct {
		key  string
		id   uint64
		text string
		why  railway.LiveFail
	}
)

// liveSub is one subscription the screen holds; id tells its messages from
// those of an earlier subscription under the same key.
type liveSub struct {
	id   uint64
	stop func()
}

type screen struct {
	list    components.List
	filter  *components.Filter
	confirm *components.Confirm // non-nil while a restart awaits an answer
	input   *editInput          // non-nil while a limits or size input is open
	panel   panelState
	target  view.MonitorRow // the row the confirm is about

	gen     uint64 // bumped by Resume and Suspend
	active  bool   // between Resume and Suspend
	debSeq  uint64 // the newest live event's debounce
	burst   uint64 // the current burst of live events
	pending bool   // a burst awaits its states read
	subSeq  uint64 // the last subscription id
	subs    map[string]*liveSub
	// ended are the keys (each names its deployment) whose stream the
	// server completed since the last poll tick: not made again before the
	// next, so each end reads the states at most once a poll interval.
	ended   map[string]bool
	liveErr string // the last live failure, sanitized
	// liveConnErr: liveErr is a connection failure; it is over once the
	// socket is open again
	liveConnErr bool
}

// New returns the Services screen.
func New() tui.Screen {
	return &screen{
		filter: components.NewFilter("services"), subs: map[string]*liveSub{},
		ended: map[string]bool{},
	}
}

// Build is the registry constructor. The screen takes no arguments: it
// returns nil when any is given.
func Build(args ...any) tui.Screen {
	if len(args) != 0 {
		return nil
	}
	return New()
}

func (s *screen) Init(ctx *tui.Ctx) tea.Cmd { s.filter.Load(ctx); return s.Resume(ctx) }
func (s *screen) Title() string             { return "Services" }

// Resume starts a new round: both reads at once, a timer per cadence and the
// subscriptions for what the store already knows. Timers of earlier rounds
// are ignored from now on.
func (s *screen) Resume(ctx *tui.Ctx) tea.Cmd {
	s.gen++
	s.active = true
	s.pending = false
	clear(s.ended)
	s.panel.ok = false // the panel reads its service again
	s.sync(ctx)
	return tea.Batch(
		s.readStates(ctx), s.readMetrics(ctx),
		tick(loaders.StatesEvery, statesTick(s.gen)),
		tick(loaders.MetricsEvery, metricsTick(s.gen)),
		s.follow(ctx),
	)
}

// Suspend ends the round: pending timers and live messages are ignored and
// every subscription stops.
func (s *screen) Suspend(*tui.Ctx) {
	s.gen++
	s.active = false
	s.pending = false
	clear(s.ended)
	for key, sub := range s.subs {
		sub.stop()
		delete(s.subs, key)
	}
	s.liveErr, s.liveConnErr = "", false
}

func statesTick(gen uint64) func(time.Time) tea.Msg {
	return func(time.Time) tea.Msg { return statesTickMsg{gen} }
}

func metricsTick(gen uint64) func(time.Time) tea.Msg {
	return func(time.Time) tea.Msg { return metricsTickMsg{gen} }
}

func (s *screen) readStates(ctx *tui.Ctx) tea.Cmd {
	if ctx.API == nil || ctx.Store == nil {
		return nil
	}
	targets := loaders.Targets(ctx.Store.Projects(), ctx.Settings)
	if len(targets) == 0 {
		return nil
	}
	return loaders.States(ctx.API, targets, ctx.Clock())
}

func (s *screen) readMetrics(ctx *tui.Ctx) tea.Cmd {
	if ctx.API == nil || ctx.Store == nil {
		return nil
	}
	targets := loaders.Targets(ctx.Store.Projects(), ctx.Settings)
	if len(targets) == 0 {
		return nil
	}
	return loaders.Metrics(ctx.API, targets, ctx.Clock())
}

type wantedSub struct {
	doc  string
	vars map[string]any
}

// sync makes the subscriptions match the store: the instances of every
// running deployment and every deployment in progress. New ones start before
// old ones stop, so the socket stays up across a change. One that ended
// (dropped from s.subs) is made again, unless the server completed it since
// the last poll tick.
func (s *screen) sync(ctx *tui.Ctx) {
	if !s.active || ctx.Live == nil || ctx.Send == nil || ctx.Store == nil {
		return
	}
	wanted := map[string]wantedSub{}
	for _, in := range ctx.Store.Instances(envIDs(ctx)) {
		st, ok := ctx.Store.State(in.Place.EnvID, in.Place.ServiceID)
		if !ok {
			continue
		}
		if st.ActiveID != "" {
			wanted["instances:"+st.ActiveID] = wantedSub{instancesDoc, map[string]any{"input": map[string]any{"deploymentId": st.ActiveID}}}
		}
		for _, id := range st.InProgress {
			wanted["deployment:"+id] = wantedSub{deploymentDoc, map[string]any{"id": id}}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(wanted)) {
		if _, ok := s.subs[key]; !ok && !s.ended[key] {
			s.subs[key] = s.subscribe(ctx, key, wanted[key])
		}
	}
	for key, sub := range s.subs {
		if _, ok := wanted[key]; !ok {
			sub.stop()
			delete(s.subs, key)
		}
	}
}

// subscribe forwards the subscription's events through ctx.Send, tagged with
// its key and a fresh id (a callback can still run after stop).
func (s *screen) subscribe(ctx *tui.Ctx, key string, w wantedSub) *liveSub {
	s.subSeq++
	id, send := s.subSeq, ctx.Send
	stop := ctx.Live.Subscribe(w.doc, w.vars,
		func(json.RawMessage) { send(liveEventMsg{key: key, id: id}) },
		func(msg string, why railway.LiveFail) { send(liveFailMsg{key: key, id: id, text: msg, why: why}) })
	return &liveSub{id: id, stop: stop}
}

// held reports whether a live message comes from a subscription still held.
func (s *screen) held(key string, id uint64) bool {
	sub, ok := s.subs[key]
	return s.active && ok && sub.id == id
}

// liveEvent starts or extends a burst: the states are read LiveDebounce
// after its newest event, or liveMaxWait after its first.
func (s *screen) liveEvent() tea.Cmd {
	var cmds []tea.Cmd
	gen := s.gen
	if !s.pending {
		s.pending = true
		s.burst++
		burst := s.burst
		cmds = append(cmds, tick(liveMaxWait, func(time.Time) tea.Msg { return debounceMsg{gen: gen, burst: burst, maxWait: true} }))
	}
	s.debSeq++
	burst, seq := s.burst, s.debSeq
	cmds = append(cmds, tick(loaders.LiveDebounce, func(time.Time) tea.Msg { return debounceMsg{gen: gen, burst: burst, seq: seq} }))
	return tea.Batch(cmds...)
}

// Capturing reports whether a filter, a confirmation or an input takes every
// key.
func (s *screen) Capturing() bool { return s.filter.Editing() || s.confirm != nil || s.input != nil }

func (s *screen) Keys() []tui.KeyHint {
	if s.filter.Editing() {
		return []tui.KeyHint{
			{Key: "enter", Label: "keep filter", Always: true},
			{Key: "esc", Label: "clear", Always: true},
		}
	}
	if s.confirm != nil {
		return []tui.KeyHint{{Key: "y", Label: "restart", Always: true}, {Key: "n", Label: "cancel", Always: true}}
	}
	if s.input != nil {
		return []tui.KeyHint{
			{Key: "enter", Label: "ok", Always: true},
			{Key: "esc", Label: "cancel", Always: true},
		}
	}
	return []tui.KeyHint{
		{Key: "enter", Label: "variables", Rank: 1},
		{Key: "d", Label: "redeploy", Rank: 2},
		{Key: "R", Label: "restart now", Rank: 3},
		{Key: "l", Label: "limits", Rank: 4},
		{Key: "z", Label: "disk", Rank: 5},
		{Key: "m", Label: "domains", Rank: 6},
		{Key: "t", Label: "logs", Rank: 7},
		{Key: "[ ]", Label: "resolution", Rank: 8},
		{Key: "r", Label: "refresh", Rank: 9},
		{Key: "/", Label: "filter", Rank: 10, Optional: true},
		{Key: "esc", Label: "back", Always: true},
	}
}

// Note counts the services and those down (failed or offline).
func (s *screen) Note(ctx *tui.Ctx) string {
	if ctx.Views == nil {
		return ""
	}
	rows := ctx.Views.Monitor(ctx.CS, envIDs(ctx))
	down := 0
	for _, r := range rows {
		if r.Status.Kind == monitor.Failed || r.Status.Kind == monitor.Offline {
			down++
		}
	}
	note := plural(len(rows), "service")
	if down > 0 {
		note += " · " + strconv.Itoa(down) + " down"
	}
	return note
}

// Notice is the live socket's trouble, if any.
func (s *screen) Notice(ctx *tui.Ctx) tui.FlashMsg {
	if ctx.Live != nil && ctx.Live.State() == "retrying" {
		return tui.FlashMsg{Kind: '✗', Text: "live updates paused · retrying"}
	}
	if s.liveErr != "" && !(s.liveConnErr && ctx.Live != nil && ctx.Live.State() == "open") {
		// Live's own texts already say "live: "
		return tui.FlashMsg{Kind: '!', Text: "live: " + strings.TrimPrefix(s.liveErr, "live: ")}
	}
	return tui.FlashMsg{}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// readFailure is why the last states or metrics read failed, sanitized; ""
// when both last reads were usable.
func readFailure(ctx *tui.Ctx) string {
	if ctx.Store == nil {
		return ""
	}
	for _, t := range loaders.Targets(ctx.Store.Projects(), ctx.Settings) {
		for _, key := range []string{store.StatesKey(t.ProjectID), store.MetricsKey(t.ProjectID)} {
			if e := ctx.Store.Load(key).Err; e != "" && !strings.HasPrefix(e, "! ") {
				return text.Sanitize(e)
			}
		}
	}
	return ""
}

func envIDs(ctx *tui.Ctx) []string {
	var ids []string
	if ctx.Store == nil {
		return nil
	}
	for _, t := range loaders.Targets(ctx.Store.Projects(), ctx.Settings) {
		for _, e := range t.Environments {
			ids = append(ids, e.ID)
		}
	}
	return ids
}

// visible is the rows matching the filter by project, environment or service
// name, and the count before filtering.
func (s *screen) visible(ctx *tui.Ctx) ([]view.MonitorRow, int) {
	if ctx.Views == nil {
		return nil, 0
	}
	s.filter.Load(ctx)
	all := ctx.Views.Monitor(ctx.CS, envIDs(ctx))
	if !s.filter.Active() {
		return all, len(all)
	}
	var out []view.MonitorRow
	for _, r := range all {
		if s.filter.Match(r.Project + " " + r.Env + " " + r.Service) {
			out = append(out, r)
		}
	}
	return out, len(all)
}

// Update handles msg, then lets the panel follow the cursor.
func (s *screen) Update(ctx *tui.Ctx, msg tea.Msg) (tui.Screen, tea.Cmd) {
	cmd := s.update(ctx, msg)
	return s, tea.Batch(cmd, s.follow(ctx))
}

func (s *screen) update(ctx *tui.Ctx, msg tea.Msg) tea.Cmd {
	if cmd, ok := s.panelUpdate(ctx, msg); ok {
		return cmd
	}
	_, cmd := s.handle(ctx, msg)
	return cmd
}

func (s *screen) handle(ctx *tui.Ctx, msg tea.Msg) (tui.Screen, tea.Cmd) {
	switch m := msg.(type) {
	case statesTickMsg:
		if m.gen != s.gen || !s.active {
			return s, nil
		}
		// completed streams may be made again from this read; their next
		// end reads the states again: at most one extra read a tick
		clear(s.ended)
		return s, tea.Batch(s.readStates(ctx), tick(loaders.StatesEvery, statesTick(s.gen)))
	case metricsTickMsg:
		if m.gen != s.gen || !s.active {
			return s, nil
		}
		return s, tea.Batch(s.readMetrics(ctx), tick(loaders.MetricsEvery, metricsTick(s.gen)))
	case liveEventMsg:
		if !s.held(m.key, m.id) {
			return s, nil
		}
		return s, s.liveEvent()
	case debounceMsg:
		if m.gen != s.gen || !s.active || !s.pending || m.burst != s.burst || (!m.maxWait && m.seq != s.debSeq) {
			return s, nil
		}
		s.pending = false
		return s, s.readStates(ctx)
	case liveFailMsg:
		if !s.held(m.key, m.id) {
			return s, nil
		}
		if m.why == railway.LiveFailEnded {
			// over at Live: made again by the next sync
			s.subs[m.key].stop()
			delete(s.subs, m.key)
			if m.text == railway.LiveStreamEnded {
				// a deployment finished or went: read what it became; not
				// made again before the next poll tick, so a stream the
				// server keeps ending reads once a tick
				s.ended[m.key] = true
				return s, s.liveEvent()
			}
		}
		s.liveErr = text.Sanitize(m.text)
		s.liveConnErr = m.why == railway.LiveFailConn
		return s, nil
	case loaders.StatesMsg:
		// the root has applied it to the store
		if m.Err == nil {
			s.liveErr, s.liveConnErr = "", false
		}
		s.sync(ctx)
		return s, nil
	case components.ConfirmedMsg:
		s.confirm = nil
		return s, tui.Emit(tui.ImmediateMsg{Change: kinds.Restart(s.target.Place, ctx.Now())})
	case components.CancelMsg:
		s.confirm, s.input = nil, nil
		return s, nil
	case components.SubmitMsg:
		if s.input == nil {
			return s, nil
		}
		return s, s.submit(ctx, m.Value)
	case tea.KeyMsg:
		if s.confirm != nil {
			return s, s.confirm.Update(m)
		}
		if s.input != nil {
			return s, s.input.field.Update(m)
		}
		if h, cmd := s.filter.Update(ctx, m); h {
			return s, cmd
		}
		return s.key(ctx, m)
	}
	return s, nil
}

func (s *screen) key(ctx *tui.Ctx, k tea.KeyMsg) (tui.Screen, tea.Cmd) {
	rows, _ := s.visible(ctx)
	h := max(1, ctx.Height-tui.ChromeRows-1-s.extraLines(ctx))
	if s.fits(ctx, len(rows)) {
		h -= sepLines + panelLines
	}
	switch k.Type {
	case tea.KeyUp:
		s.list.Move(len(rows), -1, h)
	case tea.KeyDown:
		s.list.Move(len(rows), 1, h)
	case tea.KeyPgUp:
		s.list.Move(len(rows), -h, h)
	case tea.KeyPgDown:
		s.list.Move(len(rows), h, h)
	case tea.KeyEnter:
		if r, ok := s.current(rows); ok {
			return s, tui.Emit(tui.OpenMsg{Name: "variables", Args: []any{r.Place}})
		}
	case tea.KeyRunes:
		switch string(k.Runes) {
		case "j":
			s.list.Move(len(rows), 1, h)
		case "k":
			s.list.Move(len(rows), -1, h)
		case "r":
			return s, tea.Batch(s.readStates(ctx), s.readMetrics(ctx))
		case "d", "R", "l", "z":
			r, ok := s.current(rows)
			if !ok || ctx.Store == nil {
				return s, nil
			}
			if cmd := readOnly(ctx, r.Place); cmd != nil {
				return s, cmd
			}
			switch string(k.Runes) {
			case "d":
				return s, tui.Emit(tui.PutChangeMsg{Change: kinds.Redeploy(r.Place, ctx.Now())})
			case "R":
				s.target = r
				s.confirm = &components.Confirm{Prompt: "restart " + r.Env + " · " + r.Service + " now?"}
			case "l":
				return s, s.openLimits(ctx, r)
			case "z":
				return s, s.openDisk(ctx, r)
			}
		case "m":
			if r, ok := s.current(rows); ok {
				return s, tui.Emit(tui.OpenMsg{Name: "domains", Args: []any{r.Place}})
			}
		case "t":
			if r, ok := s.current(rows); ok {
				return s, tui.Emit(tui.OpenMsg{Name: "logs", Args: []any{r.Place}})
			}
		case "[", "]":
			if len(rows) == 0 {
				return s, nil // no panel to step
			}
			if !s.fits(ctx, len(rows)) {
				return s, tui.Flash('!', "panel hidden · make the window taller")
			}
			if k.Runes[0] == '[' {
				return s, s.stepLevel(ctx, -1)
			}
			return s, s.stepLevel(ctx, 1)
		}
	}
	return s, nil
}

func (s *screen) current(rows []view.MonitorRow) (view.MonitorRow, bool) {
	if len(rows) == 0 {
		return view.MonitorRow{}, false
	}
	// a filter may have left the cursor past the last shown row
	s.list.Cursor = max(0, min(s.list.Cursor, len(rows)-1))
	return rows[s.list.Cursor], true
}

// padLeft fits str to w cells, right-aligned.
func padLeft(str string, w int) string {
	if n := ansi.StringWidth(str); n < w {
		return strings.Repeat(" ", w-n) + str
	}
	return ansi.Truncate(str, w, "…")
}

const (
	gap   = "  "
	pendW = len("pending")
	cpuW  = 7 // the value columns, icon included (monitor.Layout)
	memW  = 11
	netW  = 13
)

// valueCols are the metric columns' titles and value widths.
var valueCols = [3]struct {
	title string
	width int
}{{"cpu", cpuW}, {"mem", memW}, {"net", netW}}

// segment is the width of a metric column: its chart, if any, and its value.
func segment(cols monitor.Columns, value int) int {
	if cols.Chart > 0 {
		return cols.Chart + 1 + value
	}
	return value
}

// valueColumns is how many of the cpu, mem and net columns fit: with no room
// for charts, the net value goes first, then the mem value, so the pending
// column always shows.
func valueColumns(cols monitor.Columns, width int) int {
	if cols.Chart > 0 {
		return 3
	}
	used := cols.Project + cols.Env + cols.Name + cols.Status + 3*len(gap) + len(gap) + pendW
	shown := 0
	for _, v := range valueCols {
		used += len(gap) + v.width
		if shown > 0 && used > width {
			break
		}
		shown++
	}
	return shown
}

// withPending joins parts and the pending column: only parts are ever cut,
// so the pending column always shows whole.
func withPending(parts []string, pending string, width int) string {
	left := ansi.Truncate(strings.Join(parts, gap), max(0, width-len(gap)-pendW), "…")
	return left + gap + pending
}

// metric is one chart column of a row.
type metric struct {
	icon        string
	values      []float64
	limit       float64 // 0: the series' peak
	width       int     // the value column, icon included
	format      func(float64) string
	scaleToPeak bool // traffic: the colour scale is the peak
}

func (s *screen) View(ctx *tui.Ctx, width, height int) string {
	th := ctx.Theme
	rows, total := s.visible(ctx)
	var lines []string

	// the filter field takes the first line; a failure line and a confirm
	// take the last ones
	if f := s.filter.View(width, len(rows), total); f != "" {
		lines = append(lines, f)
	}
	var tail []string
	if why := readFailure(ctx); why != "" {
		tail = append(tail, th.Danger.Render(ansi.Truncate("✗ cannot read Railway: "+why+" · showing the last read · r retries", width, "…")))
	}
	if s.confirm != nil {
		tail = append(tail, th.Warn.Render(s.confirm.View(width)))
	}
	if s.input != nil {
		tail = append(tail, s.input.field.View(width))
	}
	listH := max(height-1-len(lines)-len(tail), 0) // header row
	var panel []string
	if panelFits(height, len(rows), len(lines)+len(tail)) {
		listH -= sepLines + panelLines
		if r, ok := s.current(rows); ok {
			panel = append([]string{th.Muted.Render(strings.Repeat("─", width))}, s.panelView(ctx, r, width)...)
		}
	}

	if len(rows) == 0 {
		msg := "no services yet · r refresh"
		if s.filter.Active() {
			msg = "no service matches the filter"
		}
		lines = append(lines, th.Muted.Render(msg))
	} else {
		mp, me, mn := 0, 0, 0
		for _, r := range rows {
			mp = max(mp, ansi.StringWidth(text.Sanitize(r.Project)))
			me = max(me, ansi.StringWidth(text.Sanitize(r.Env)))
			mn = max(mn, ansi.StringWidth(text.Sanitize(r.Service)))
		}
		cols := monitor.Layout(width-pendW-len(gap), mp, me, mn)
		shown := valueColumns(cols, width)
		status := "status"
		if cols.StatusIconOnly {
			status = ""
		}
		hdr := []string{tui.Pad("project", cols.Project), tui.Pad("env", cols.Env), tui.Pad("name", cols.Name), tui.Pad(status, cols.Status)}
		for _, v := range valueCols[:shown] {
			hdr = append(hdr, tui.Pad(v.title, segment(cols, v.width)))
		}
		lines = append(lines, th.Muted.Render(withPending(hdr, "pending", width)))
		from, to := s.list.Window(len(rows), listH)
		for i := from; i < to; i++ {
			lines = append(lines, s.row(ctx, rows[i], cols, shown, i == s.list.Cursor, width))
		}
	}
	// a short table pads its window so the panel keeps to the bottom
	if len(panel) > 0 {
		for len(lines) < height-len(panel)-len(tail) {
			lines = append(lines, "")
		}
	}
	lines = append(lines, panel...)
	lines = append(lines, tail...)
	if len(lines) > height {
		lines = lines[:max(height, 0)]
	}
	return strings.Join(lines, "\n")
}

// row draws one service. The selected row is drawn plain in the selection
// style; the others colour the status, the charts and the pending mark.
func (s *screen) row(ctx *tui.Ctx, r view.MonitorRow, cols monitor.Columns, shown int, selected bool, width int) string {
	th := ctx.Theme
	paint := func(st lipgloss.Style, str string) string {
		if selected {
			return str
		}
		return st.Render(str)
	}
	icon := ctx.Icons.Status(r.Status.Icon)
	label := icon
	if !cols.StatusIconOnly {
		label = icon + " " + r.Status.Text
	}
	parts := []string{
		tui.Pad(text.Sanitize(r.Project), cols.Project),
		tui.Pad(text.Sanitize(r.Env), cols.Env),
		tui.Pad(text.Sanitize(r.Service), cols.Name),
		paint(statusStyle(th, r.Status.Kind), tui.Pad(label, cols.Status)),
	}
	for _, m := range []metric{
		{icon: ctx.Icons.CPU(), values: r.CPU, limit: r.CPULimit, width: cpuW, format: monitor.FormatCPU},
		{icon: ctx.Icons.Memory(), values: r.Mem, limit: r.MemLimitGB, width: memW, format: func(v float64) string { return monitor.FormatBytes(v * monitor.GB) }},
		{icon: ctx.Icons.Traffic(), values: r.Net, width: netW, format: monitor.FormatRate, scaleToPeak: true},
	}[:shown] {
		parts = append(parts, chartCell(th, m, cols.Chart, paint))
	}
	pend := strings.Repeat(" ", pendW)
	if r.Pending > 0 {
		pend = paint(th.New, tui.Pad(ctx.Icons.Pending()+" "+strconv.Itoa(r.Pending), pendW))
	}
	line := withPending(parts, pend, width)
	if selected {
		return th.Selected.Render(line)
	}
	return line
}

// chartCell is a one-line chart of the newest points that fit, scaled to
// the series' peak and coloured by level, then the icon and the newest value
// right-aligned. Without data the chart is blank and the value "—".
func chartCell(th tui.Theme, m metric, chartW int, paint func(lipgloss.Style, string) string) string {
	var b strings.Builder
	value := "—"
	if len(m.values) > 0 {
		value = m.format(m.values[len(m.values)-1])
	}
	if chartW > 0 {
		peak := 0.0
		if len(m.values) > 0 {
			peak = slices.Max(m.values)
		}
		limit := m.limit
		if m.scaleToPeak {
			limit = peak
		}
		cells := monitor.Braille(m.values, chartW, 1, peak, limit)[0]
		for i := 0; i < len(cells); {
			step := tui.LevelStep(cells[i].Level)
			j := i
			var run []rune
			for j < len(cells) && tui.LevelStep(cells[j].Level) == step {
				run = append(run, cells[j].R)
				j++
			}
			b.WriteString(paint(th.Level(cells[i].Level), string(run)))
			i = j
		}
		b.WriteString(" ")
	}
	b.WriteString(paint(th.Muted, m.icon) + " " + padLeft(value, m.width-2))
	return b.String()
}

func statusStyle(th tui.Theme, k monitor.Kind) lipgloss.Style {
	switch k {
	case monitor.Online:
		return th.OK
	case monitor.OnlineDeploying, monitor.Deploying:
		return th.Warn
	case monitor.Failed:
		return th.Danger
	}
	return th.Muted
}
