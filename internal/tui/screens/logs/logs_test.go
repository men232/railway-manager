package logs_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
	"github.com/men232/railway-manager/internal/settings"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/screens/logs"
	"github.com/men232/railway-manager/internal/tui/tuitest"
	"github.com/men232/railway-manager/internal/view"
)

func TestMain(m *testing.M) {
	tuitest.Init()
	os.Exit(m.Run())
}

// secret is a variable value in the store: no frame may show it.
const secret = "fixture-value-logs"

var (
	now   = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	place = domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"}
)

var catalog = railway.Catalog{Projects: []railway.Project{{
	ID: "p1", Name: "shop", Workspace: "ws",
	Environments: []railway.Environment{{ID: "e1", Name: "production", CanAccess: true, ServiceIDs: []string{"s1", "s2"}}},
	Services:     []railway.Service{{ID: "s1", Name: "api"}, {ID: "s2", Name: "worker"}},
}}}

// states: api runs dep-api, worker has nothing running.
var states = []railway.DeploymentState{
	{EnvID: "e1", ServiceID: "s1", ActiveID: "dep-api", ActiveStatus: "SUCCESS", LatestStatus: "SUCCESS"},
	{EnvID: "e1", ServiceID: "s2"},
}

type staticTokens struct{}

func (staticTokens) Get(context.Context) railway.TokenStatus {
	return railway.TokenStatus{Token: "test-token", Source: "env:RAILWAY_TOKEN"}
}
func (s staticTokens) Refresh(ctx context.Context) railway.TokenStatus { return s.Get(ctx) }
func (s staticTokens) Peek() railway.TokenStatus                       { return s.Get(context.Background()) }

func ctx(w, h int) *tui.Ctx {
	st := store.New()
	st.ReplaceCatalog(catalog)
	st.ReplaceVariables("p1", []railway.VarRecord{
		{EnvID: "e1", ServiceID: "s1", Name: "API_KEY", Unrendered: secret, Rendered: secret},
	})
	st.ReplaceStates("p1", []string{"e1"}, states)
	return &tui.Ctx{
		Store: st, CS: &changeset.ChangeSet{},
		Views:    &view.Views{Store: st, Reg: kinds.NewRegistry()},
		Settings: &settings.Settings{Projects: []string{"p1"}, Envs: []string{"e1"}},
		Theme:    tui.NewTheme(tuitest.Profile()), Icons: tui.Icons{},
		Width: w, Height: h, IsTTY: true,
		Now: func() time.Time { return now },
	}
}

// at is the fixture time plus n seconds.
func at(n int) time.Time { return now.Add(time.Duration(n) * time.Second) }

// numbered is n info lines "line 0001".. starting at second from.
func numbered(from, n int) []railway.LogLine {
	out := make([]railway.LogLine, n)
	for i := range n {
		out[i] = railway.LogLine{Timestamp: at(from + i), Severity: "info", Message: fmt.Sprintf("line %04d", from+i)}
	}
	return out
}

type rig struct {
	t    *testing.T
	fake *railwaytest.Fake
	live *railway.Live
	c    *tui.Ctx
	sent chan tea.Msg
	// states counts the states reads the screen asked for.
	states int
}

func newRig(t *testing.T, w, h int) *rig {
	t.Helper()
	f := railwaytest.New(t)
	f.SetStates("p1", states)
	live := railway.NewLive(f.WSURL, staticTokens{}, nil)
	t.Cleanup(live.Close)
	r := &rig{t: t, fake: f, live: live, sent: make(chan tea.Msg, 100)}
	c := ctx(w, h)
	c.API = &railway.Client{T: &railway.Transport{Endpoint: f.URL, Tokens: staticTokens{}}}
	c.Live = live
	c.Send = func(m tea.Msg) { r.sent <- m }
	r.c = c
	return r
}

// drive runs cmd and hands every message it yields to s (store updates are
// applied first, as the root does), following the commands s returns.
func (r *rig) drive(s tui.Screen, cmd tea.Cmd) tui.Screen {
	r.t.Helper()
	if cmd == nil {
		return s
	}
	switch m := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range m {
			s = r.drive(s, c)
		}
	case nil:
	default:
		if _, ok := m.(loaders.StatesMsg); ok {
			r.states++
		}
		var next tea.Cmd
		s, next = r.deliver(s, m)
		s = r.drive(s, next)
	}
	return s
}

func (r *rig) deliver(s tui.Screen, msg tea.Msg) (tui.Screen, tea.Cmd) {
	if u, ok := msg.(store.Update); ok {
		u.ApplyTo(r.c.Store)
	}
	return s.Update(r.c, msg)
}

// next waits for a message the live callbacks forwarded.
func (r *rig) next() tea.Msg {
	r.t.Helper()
	select {
	case m := <-r.sent:
		return m
	case <-time.After(5 * time.Second):
		r.t.Fatal("nothing forwarded")
	}
	return nil
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// started inits the screen, applies its first read and waits for its
// subscription.
func (r *rig) started(p domain.Place) tui.Screen {
	r.t.Helper()
	s := logs.Build(p)
	if s == nil {
		r.t.Fatal("Build(place) = nil")
	}
	s = r.drive(s, s.Init(r.c))
	eventually(r.t, "the subscription", func() bool { return len(r.fake.WSSubscribed()) == 1 })
	return s
}

// push sends lines on the newest subscription and delivers what Live
// forwards.
func (r *rig) push(s tui.Screen, lines []railway.LogLine) tui.Screen {
	r.t.Helper()
	subs := r.fake.WSSubscribed()
	out := make([]map[string]any, 0, len(lines))
	for _, l := range lines {
		out = append(out, map[string]any{"timestamp": l.Timestamp.Format(time.RFC3339Nano), "message": l.Message, "severity": l.Severity})
	}
	r.fake.WSPush(subs[len(subs)-1].ID, map[string]any{"deploymentLogs": out})
	s, cmd := r.deliver(s, r.next())
	return r.drive(s, cmd)
}

func (r *rig) key(s tui.Screen, ks ...tea.KeyMsg) tui.Screen {
	for _, k := range ks {
		var cmd tea.Cmd
		s, cmd = r.deliver(s, k)
		s = r.drive(s, cmd)
	}
	return s
}

func rk(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func typed(str string) []tea.KeyMsg {
	var out []tea.KeyMsg
	for _, ch := range str {
		out = append(out, rk(string(ch)))
	}
	return out
}

var (
	up    = tea.KeyMsg{Type: tea.KeyUp}
	home  = tea.KeyMsg{Type: tea.KeyHome}
	enter = tea.KeyMsg{Type: tea.KeyEnter}
)

func render(c *tui.Ctx, s tui.Screen) string {
	return s.View(c, c.Width-tui.ChromeCols, c.Height-tui.ChromeRows)
}

func note(c *tui.Ctx, s tui.Screen) string { return s.(tui.Noter).Note(c) }

func frame(c *tui.Ctx, s tui.Screen) string {
	n := s.(tui.Noter)
	return tui.Frame(c, s.Title(), n.Note(c), render(c, s), tui.Footer{Hints: s.Keys(), Flash: n.Notice(c)})
}

// bodyLines are the log lines shown, after the header.
func bodyLines(c *tui.Ctx, s tui.Screen) []string {
	ls := strings.Split(ansi.Strip(render(c, s)), "\n")
	return ls[1:]
}

func TestBuildArgs(t *testing.T) {
	if logs.Build(place) == nil {
		t.Fatal("Build(place) = nil")
	}
	for _, args := range [][]any{nil, {"p1"}, {place, place}} {
		if logs.Build(args...) != nil {
			t.Errorf("Build(%v) should be nil", args)
		}
	}
}

func TestLogsLoadsThenSubscribes(t *testing.T) {
	r := newRig(t, 80, 20)
	r.fake.SetLogs("dep-api", numbered(0, 3))
	s := r.started(place)
	req, ok := r.fake.LastRequest("DeploymentLogs")
	if !ok || req.Vars["deploymentId"] != "dep-api" || req.Vars["limit"] != float64(500) {
		t.Fatalf("load request = %v (%v)", req.Vars, ok)
	}
	sub := r.fake.WSSubscribed()[0]
	if sub.Query != railway.DeploymentLogsSubscription || sub.Vars["deploymentId"] != "dep-api" {
		t.Errorf("subscription = %+v", sub)
	}
	out := ansi.Strip(render(r.c, s))
	for _, want := range []string{"production · api", "12:00:00 line 0000", "12:00:02 line 0002"} {
		if !strings.Contains(out, want) {
			t.Errorf("view lacks %q:\n%s", want, out)
		}
	}
	// the subscription's backlog overlaps the read: shown once
	s = r.push(s, append(numbered(1, 2), numbered(3, 1)...))
	if got := strings.Count(ansi.Strip(render(r.c, s)), "line 0002"); got != 1 {
		t.Errorf("line 0002 shown %d times", got)
	}
	if !strings.Contains(ansi.Strip(render(r.c, s)), "line 0003") {
		t.Error("new line not shown")
	}
}

func TestLogsSanitizeCapAndCut(t *testing.T) {
	r := newRig(t, 80, 20)
	evil := "\x1b[31mred\x1b[0m \x1b]8;;https://evil.example\x07link\x1b]8;;\x07 \u009b2J\x1b]0;title\x07done\x9b\r\nnext\ttab"
	r.fake.SetLogs("dep-api", []railway.LogLine{{Timestamp: at(0), Severity: "info", Message: evil}})
	s := r.started(place)
	raw := render(r.c, s)
	for _, bad := range []string{"\x1b[31m", "\x1b]", "\x07", "\u009b", "\x9b", "\r", "\t"} {
		if strings.Contains(raw, bad) {
			t.Errorf("view carries %q: %q", bad, raw)
		}
	}
	if !strings.Contains(raw, "red link done") {
		t.Errorf("sanitized text missing: %q", raw)
	}

	// 2500 more lines: the newest 2000 of the 2501 are kept
	s = r.push(s, numbered(1, 2500))
	if got := note(r.c, s); !strings.HasPrefix(got, "2000 lines") {
		t.Errorf("note = %q", got)
	}
	// a 5000-char line pushes out one more and is cut to the width
	long := strings.Repeat("x", 5000)
	s = r.push(s, []railway.LogLine{{Timestamp: at(3000), Severity: "info", Message: long}})
	width := r.c.Width - tui.ChromeCols
	for i, l := range strings.Split(render(r.c, s), "\n") {
		if w := ansi.StringWidth(l); w > width {
			t.Errorf("line %d is %d wide, body %d", i, w, width)
		}
	}
	ls := bodyLines(r.c, s)
	if last := ls[len(ls)-1]; !strings.HasPrefix(last, "12:50:00 xxx") || !strings.HasSuffix(last, "…") {
		t.Errorf("long line = %q", last)
	}
	if got := strings.Count(ansi.Strip(render(r.c, s)), "\n"); got >= r.c.Height-tui.ChromeRows {
		t.Errorf("view has %d lines for a body of %d", got+1, r.c.Height-tui.ChromeRows)
	}
	// a line of wide runes is cut to the width too
	s = r.push(s, []railway.LogLine{{Timestamp: at(3001), Severity: "error", Message: strings.Repeat("界", 100)}})
	for i, l := range strings.Split(render(r.c, s), "\n") {
		if w := ansi.StringWidth(l); w > width {
			t.Errorf("wide: line %d is %d wide, body %d", i, w, width)
		}
	}
	ls = bodyLines(r.c, s)
	if last := ls[len(ls)-1]; !strings.HasPrefix(last, "12:50:01 界界") || !strings.HasSuffix(last, "…") {
		t.Errorf("wide line = %q", last)
	}
	s = r.key(s, home)
	if first := bodyLines(r.c, s)[0]; !strings.Contains(first, "line 0503") {
		t.Errorf("oldest kept = %q, want line 0503", first)
	}
	if strings.Contains(ansi.Strip(render(r.c, s)), "done") {
		t.Error("the evicted first line is still shown")
	}
}

func TestLogsFollowAndFilter(t *testing.T) {
	r := newRig(t, 80, 14)
	lines := numbered(0, 40)
	lines[5].Severity, lines[5].Message = "error", "boom: connection refused"
	lines[30].Severity, lines[30].Message = "warn", "slow query"
	r.fake.SetLogs("dep-api", lines)
	s := r.started(place)
	if got := note(r.c, s); got != "40 lines · following" {
		t.Errorf("note = %q", got)
	}
	ls := bodyLines(r.c, s)
	if last := ls[len(ls)-1]; !strings.Contains(last, "line 0039") {
		t.Errorf("following: last line = %q", last)
	}

	// scrolling up pauses; new lines then do not move the view
	s = r.key(s, up)
	if got := note(r.c, s); got != "40 lines · paused" {
		t.Errorf("note after up = %q", got)
	}
	before := ansi.Strip(render(r.c, s))
	if !strings.Contains(before, "line 0038") || strings.Contains(before, "line 0039") {
		t.Errorf("after up:\n%s", before)
	}
	s = r.push(s, numbered(40, 3))
	if after := ansi.Strip(render(r.c, s)); after != before {
		t.Errorf("paused view moved:\n%s\nwas\n%s", after, before)
	}
	// f follows again: the newest shows
	s = r.key(s, rk("f"))
	if got := note(r.c, s); got != "43 lines · following" {
		t.Errorf("note after f = %q", got)
	}
	ls = bodyLines(r.c, s)
	if last := ls[len(ls)-1]; !strings.Contains(last, "line 0042") {
		t.Errorf("after f: last = %q", last)
	}
	s = r.key(s, rk("f"))
	if got := note(r.c, s); got != "43 lines · paused" {
		t.Errorf("note after second f = %q", got)
	}
	s = r.key(s, rk("f"))

	// the filter keeps matching lines; its text is saved under the place
	s = r.key(s, append(append([]tea.KeyMsg{rk("/")}, typed("BOOM")...), enter)...)
	if got := r.c.Filters["logs:"+place.Key()]; got != "BOOM" {
		t.Errorf("saved filter = %q", got)
	}
	out := ansi.Strip(render(r.c, s))
	if !strings.Contains(out, "boom: connection refused") || strings.Contains(out, "line 00") {
		t.Errorf("filtered:\n%s", out)
	}
	if !strings.Contains(out, "1/43") {
		t.Errorf("filter count missing:\n%s", out)
	}
	// a new matching line shows while following
	s = r.push(s, []railway.LogLine{{Timestamp: at(50), Severity: "error", Message: "boom again"}})
	if !strings.Contains(ansi.Strip(render(r.c, s)), "boom again") {
		t.Error("new matching line not shown")
	}
	s = r.key(s, rk("/"), tea.KeyMsg{Type: tea.KeyEsc})
	s = r.key(s, append(append([]tea.KeyMsg{rk("/")}, typed("nothing like it")...), enter)...)
	if !strings.Contains(ansi.Strip(render(r.c, s)), "no line matches the filter") {
		t.Errorf("no-match state:\n%s", ansi.Strip(render(r.c, s)))
	}

	// a screen opened again restores the filter
	again := logs.Build(place)
	_ = again.Init(r.c) // its read is not run
	if out := ansi.Strip(render(r.c, again)); !strings.Contains(out, "/ nothing like it") {
		t.Errorf("filter not restored:\n%s", out)
	}
}

func TestLogsSeverityTones(t *testing.T) {
	c := ctx(80, 14)
	c.Theme = tui.NewTheme(termenv.ANSI)
	r := newRig(t, 80, 14)
	r.c.Theme = c.Theme
	r.fake.SetLogs("dep-api", []railway.LogLine{
		{Timestamp: at(0), Severity: "error", Message: "bad"},
		{Timestamp: at(1), Severity: "warn", Message: "meh"},
		{Timestamp: at(2), Severity: "info", Message: "fine"},
	})
	s := r.started(place)
	raw := strings.Split(render(r.c, s), "\n")
	if !strings.Contains(raw[1], c.Theme.Danger.Render("bad")) {
		t.Errorf("error line not in Danger: %q", raw[1])
	}
	if !strings.Contains(raw[2], c.Theme.Warn.Render("meh")) {
		t.Errorf("warn line not in Warn: %q", raw[2])
	}
	if strings.Contains(raw[3], c.Theme.Danger.Render("fine")) || strings.Contains(raw[3], c.Theme.Warn.Render("fine")) {
		t.Errorf("info line toned: %q", raw[3])
	}
}

func TestLogsSuspendUnsubscribes(t *testing.T) {
	r := newRig(t, 80, 20)
	r.fake.SetLogs("dep-api", numbered(0, 2))
	s := r.started(place)
	id := r.fake.WSSubscribed()[0].ID

	// an event already forwarded before Suspend is dropped after it
	r.fake.WSPush(id, map[string]any{"deploymentLogs": []map[string]any{{"timestamp": at(5).Format(time.RFC3339), "message": "late", "severity": "info"}}})
	late := r.next()
	s.(tui.Suspender).Suspend(r.c)
	eventually(t, "the complete", func() bool {
		for _, c := range r.fake.WSCompletes() {
			if c == id {
				return true
			}
		}
		return false
	})
	s, cmd := r.deliver(s, late)
	s = r.drive(s, cmd)
	if strings.Contains(ansi.Strip(render(r.c, s)), "late") {
		t.Error("a line from the stopped subscription was shown")
	}
	eventually(t, "the socket to close", func() bool { return r.live.State() == "idle" })

	// Resume reads and subscribes again; the old subscription's messages
	// still count for nothing
	s = r.drive(s, s.(tui.Resumer).Resume(r.c))
	eventually(t, "a new subscription", func() bool { return len(r.fake.WSSubscribed()) == 2 })
	s, cmd = r.deliver(s, late)
	s = r.drive(s, cmd)
	if strings.Contains(ansi.Strip(render(r.c, s)), "late") {
		t.Error("a stale line counted after Resume")
	}
	s = r.push(s, []railway.LogLine{{Timestamp: at(9), Severity: "info", Message: "fresh"}})
	if !strings.Contains(ansi.Strip(render(r.c, s)), "fresh") {
		t.Error("the new subscription's line is not shown")
	}
}

// A stream the server ends reads the states once; it is not made again for
// the same deployment, only for a new one.
func TestLogsStreamEndedNoResubscribeLoop(t *testing.T) {
	r := newRig(t, 80, 20)
	r.fake.SetLogs("dep-api", numbered(0, 2))
	s := r.started(place)
	r.fake.WSComplete(r.fake.WSSubscribed()[0].ID)
	s, cmd := r.deliver(s, r.next())
	s = r.drive(s, cmd)
	if r.states != 1 {
		t.Fatalf("states reads after the end = %d, want 1", r.states)
	}
	if n := s.(tui.Noter).Notice(r.c); !strings.Contains(n.Text, "stream ended") {
		t.Errorf("notice = %+v", n)
	}
	time.Sleep(50 * time.Millisecond)
	if got := len(r.fake.WSSubscribed()); got != 1 {
		t.Fatalf("resubscribed the ended stream: %d subscriptions", got)
	}
	if !strings.Contains(ansi.Strip(render(r.c, s)), "line 0001") {
		t.Error("lines dropped after the end")
	}

	// a redeploy: the next states read finds dep-api2, read and followed
	r.fake.SetStates("p1", []railway.DeploymentState{{EnvID: "e1", ServiceID: "s1", ActiveID: "dep-api2", ActiveStatus: "SUCCESS"}})
	r.fake.SetLogs("dep-api2", []railway.LogLine{{Timestamp: at(100), Severity: "info", Message: "booted"}})
	s = r.drive(s, loaders.States(r.c.API, []railway.DiscoveryTarget{{ProjectID: "p1", Environments: []railway.Environment{{ID: "e1"}}}}, r.c.Now))
	eventually(t, "the new deployment's subscription", func() bool { return len(r.fake.WSSubscribed()) == 2 })
	if sub := r.fake.WSSubscribed()[1]; sub.Vars["deploymentId"] != "dep-api2" {
		t.Errorf("second subscription = %+v", sub)
	}
	out := ansi.Strip(render(r.c, s))
	if !strings.Contains(out, "booted") || strings.Contains(out, "line 0001") {
		t.Errorf("after the redeploy:\n%s", out)
	}
	if n := s.(tui.Noter).Notice(r.c); n.Text != "" {
		t.Errorf("notice after the new deployment = %+v", n)
	}
}

func TestLogsLiveFailSanitized(t *testing.T) {
	r := newRig(t, 80, 20)
	r.fake.SetLogs("dep-api", numbered(0, 1))
	s := r.started(place)
	r.fake.WSPushErrors(r.fake.WSSubscribed()[0].ID, "bad \x1b]0;x\x07thing")
	s, _ = r.deliver(s, r.next())
	n := s.(tui.Noter).Notice(r.c)
	if !strings.Contains(n.Text, "bad thing") || strings.ContainsAny(n.Text, "\x1b\x07") {
		t.Errorf("notice = %q", n.Text)
	}
}

func TestLogsNothingRunning(t *testing.T) {
	c := ctx(80, 14)
	s := logs.Build(domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s2"})
	if cmd := s.Init(c); cmd != nil {
		if m := cmd(); m != nil {
			t.Errorf("Init without a deployment and without an API yielded %#v", m)
		}
	}
	if out := ansi.Strip(render(c, s)); !strings.Contains(out, "! nothing running") {
		t.Errorf("view:\n%s", out)
	}
}

func TestLogsReadFailure(t *testing.T) {
	r := newRig(t, 80, 14)
	r.fake.FailNext("DeploymentLogs", "boom \x1b[2Jhere")
	s := logs.Build(place)
	s = r.drive(s, s.Init(r.c))
	out := ansi.Strip(render(r.c, s))
	if !strings.Contains(out, "✗ cannot read the logs") || !strings.Contains(out, "boom here") {
		t.Errorf("view:\n%s", out)
	}
	if strings.Contains(render(r.c, s), "\x1b[2J") {
		t.Error("escape in the failure line")
	}
	// r reads again
	r.fake.SetLogs("dep-api", numbered(0, 1))
	s = r.key(s, rk("r"))
	if !strings.Contains(ansi.Strip(render(r.c, s)), "line 0000") {
		t.Errorf("after r:\n%s", ansi.Strip(render(r.c, s)))
	}
}

func goldenLines() []railway.LogLine {
	return []railway.LogLine{
		{Timestamp: at(0), Severity: "info", Message: "Starting Container"},
		{Timestamp: at(1), Severity: "info", Message: "> shop-api@1.4.2 start"},
		{Timestamp: at(2), Severity: "info", Message: "listening on :8080"},
		{Timestamp: at(14), Severity: "warn", Message: "slow request GET /orders took 2380ms (threshold 1000ms) user=42 region=eu-west"},
		{Timestamp: at(15), Severity: "info", Message: "GET /health 200 1ms"},
		{Timestamp: at(31), Severity: "error", Message: "upstream payments: connect ECONNREFUSED 10.0.4.12:5432 — retrying in 5s with backoff"},
		{Timestamp: at(36), Severity: "info", Message: "reconnected to payments"},
	}
}

func TestLogsGoldens(t *testing.T) {
	for _, w := range []int{80, 60} {
		r := newRig(t, w, 16)
		r.fake.SetLogs("dep-api", goldenLines())
		s := r.started(place)
		out := frame(r.c, s)
		tuitest.NoSecrets(t, ansi.Strip(out), secret)
		for _, ru := range out {
			if ru >= 0xe000 && ru <= 0xf8ff || ru >= 0xf0000 {
				t.Errorf("plain icons emitted a Private Use rune %U", ru)
			}
		}
		tuitest.Golden(t, fmt.Sprintf("logs_%d.golden", w), out)
	}
}

// Closing a modal (Suspend, then Resume) keeps the lines, the pause and the
// position; only the stream is made again.
func TestLogsResumeKeepsHistory(t *testing.T) {
	r := newRig(t, 80, 14)
	r.fake.SetLogs("dep-api", numbered(0, 40))
	s := r.started(place)
	s = r.push(s, numbered(40, 3))
	s = r.key(s, up, up, up)
	before, beforeNote := ansi.Strip(render(r.c, s)), note(r.c, s)
	if beforeNote != "43 lines · paused" {
		t.Fatalf("note = %q", beforeNote)
	}
	s.(tui.Suspender).Suspend(r.c)
	s = r.drive(s, s.(tui.Resumer).Resume(r.c))
	eventually(t, "the stream made again", func() bool { return len(r.fake.WSSubscribed()) == 2 })
	if after := ansi.Strip(render(r.c, s)); after != before {
		t.Errorf("view after Resume:\n%s\nwas\n%s", after, before)
	}
	if got := note(r.c, s); got != beforeNote {
		t.Errorf("note after Resume = %q", got)
	}
	// the new stream's backlog repeats kept lines: only the new one counts
	s = r.push(s, append(numbered(38, 5), numbered(43, 1)...))
	if got := note(r.c, s); got != "44 lines · paused" {
		t.Errorf("note after the backlog = %q", got)
	}
	// r starts over: the 40 lines Railway has
	s = r.key(s, rk("r"))
	if got := note(r.c, s); !strings.HasPrefix(got, "40 lines") {
		t.Errorf("note after r = %q", got)
	}
}

// The backlog is matched line by line: a line of another replica older than
// the newest read line is new, and a line without a time is not shown twice.
func TestLogsBacklogDedupe(t *testing.T) {
	r := newRig(t, 80, 20)
	read := numbered(0, 5)
	read[2] = railway.LogLine{Severity: "info", Message: "no time"}
	r.fake.SetLogs("dep-api", read)
	s := r.started(place)
	s = r.push(s, []railway.LogLine{
		read[2], read[3], read[4],
		{Timestamp: at(1), Severity: "info", Message: "other replica"},
		{Timestamp: at(9), Severity: "info", Message: "newer"},
	})
	out := ansi.Strip(render(r.c, s))
	for _, want := range []string{"other replica", "newer"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q dropped:\n%s", want, out)
		}
	}
	for _, once := range []string{"no time", "line 0003", "line 0004"} {
		if n := strings.Count(out, once); n != 1 {
			t.Errorf("%q shown %d times", once, n)
		}
	}
}

// A failed states read while finding the deployment is said, and r retries.
func TestLogsStatesReadFails(t *testing.T) {
	r := newRig(t, 80, 14)
	st := store.New()
	st.ReplaceCatalog(catalog)
	r.c.Store = st
	r.fake.FailNext("ServiceStates", "nope \x1b[2Jdown")
	r.fake.SetLogs("dep-api", numbered(0, 1))
	s := logs.Build(place)
	s = r.drive(s, s.Init(r.c))
	out := ansi.Strip(render(r.c, s))
	if !strings.Contains(out, "✗ cannot read the deployment: ") || !strings.Contains(out, "nope down") ||
		!strings.Contains(out, "r reads again") || strings.Contains(out, "nothing running") {
		t.Errorf("view:\n%s", out)
	}
	s = r.key(s, rk("r"))
	if !strings.Contains(ansi.Strip(render(r.c, s)), "line 0000") {
		t.Errorf("after r:\n%s", ansi.Strip(render(r.c, s)))
	}
}

// The Logs screen's states read has its own load key: its failure is not the
// Services screen's, and a project-wide states failure is not the Logs one.
func TestLogsStatesOwnLoadKey(t *testing.T) {
	r := newRig(t, 80, 14)
	st := store.New()
	st.ReplaceCatalog(catalog)
	r.c.Store = st
	r.fake.FailNext("ServiceStates", "nope")
	s := logs.Build(place)
	s = r.drive(s, s.Init(r.c))
	if l := st.Load(store.StatesKey("p1")); l.Err != "" {
		t.Errorf("project-wide load = %+v", l)
	}
	if l := st.Load(store.PlaceStatesKey(place)); l.Err == "" {
		t.Errorf("logs load = %+v", l)
	}

	r2 := newRig(t, 80, 14)
	st2 := store.New()
	st2.ReplaceCatalog(catalog)
	r2.c.Store = st2
	s2 := logs.Build(place)
	r2.fake.SetLogs("dep-api", numbered(0, 1))
	r2.fake.FailNext("ServiceStates", "services poll down")
	// the Services screen's poll fails while Logs waits for its own read
	s2.Init(r2.c)
	s2, _ = r2.deliver(s2, loaders.StatesMsg{ProjectID: "p1", EnvIDs: []string{"e1"}, Err: errors.New("services poll down"), At: time.Now()})
	if out := ansi.Strip(render(r2.c, s2)); strings.Contains(out, "services poll down") {
		t.Errorf("project-wide failure shown on Logs:\n%s", out)
	}
}

// A lost connection is reported while the socket is down and is over once
// it is open again.
func TestLogsLostConnectionNoticeClearsOnReopen(t *testing.T) {
	r := newRig(t, 80, 20)
	r.fake.SetLogs("dep-api", numbered(0, 1))
	s := r.started(place)
	r.fake.WSDrop()
	s, _ = r.deliver(s, r.next())
	if n := s.(tui.Noter).Notice(r.c); n.Text == "" {
		t.Errorf("no notice while the socket is down (state %s)", r.live.State())
	}
	eventually(t, "reopen", func() bool { return r.live.State() == "open" && len(r.fake.WSSubscribed()) == 2 })
	if n := s.(tui.Noter).Notice(r.c); n.Text != "" {
		t.Errorf("notice after reconnect = %+v", n)
	}
}

// A connection failure is judged by what it is, not by the socket's state
// when the screen handles it: one handled after the socket reopened leaves
// no notice, while an error of the subscription itself still shows.
func TestLogsConnectionFailureHandledAfterReopen(t *testing.T) {
	r := newRig(t, 80, 20)
	r.fake.SetLogs("dep-api", numbered(0, 1))
	s := r.started(place)
	r.fake.WSDrop()
	eventually(t, "reopen", func() bool { return r.live.State() == "open" && len(r.fake.WSSubscribed()) == 2 })
	s, _ = r.deliver(s, r.next())
	if n := s.(tui.Noter).Notice(r.c); n.Text != "" {
		t.Errorf("notice for a connection failure handled after reopen = %+v", n)
	}
	r.fake.WSPushErrors(r.fake.WSSubscribed()[1].ID, "rate limited")
	s, _ = r.deliver(s, r.next())
	if n := s.(tui.Noter).Notice(r.c); !strings.Contains(n.Text, "rate limited") {
		t.Errorf("subscription error notice = %+v", n)
	}
}
