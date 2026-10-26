package services_test

import (
	"context"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/termenv"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
	"github.com/men232/railway-manager/internal/settings"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/screens/services"
	"github.com/men232/railway-manager/internal/tui/tuitest"
	"github.com/men232/railway-manager/internal/view"
)

type staticTokens struct{}

func (staticTokens) Get(context.Context) railway.TokenStatus {
	return railway.TokenStatus{Token: "test-token", Source: "env:RAILWAY_TOKEN"}
}
func (s staticTokens) Refresh(ctx context.Context) railway.TokenStatus { return s.Get(ctx) }
func (s staticTokens) Peek() railway.TokenStatus                       { return s.Get(context.Background()) }

// monCatalog has every status the table shows: shop/production holds an
// online, a deploying, a failed and a sleeping service; blog is never read.
var monCatalog = railway.Catalog{Projects: []railway.Project{
	{
		ID: "p1", Name: "shop", Workspace: "ws",
		Environments: []railway.Environment{{ID: "e1", Name: "production", CanAccess: true, ServiceIDs: []string{"s1", "s2", "s3", "s4"}}},
		Services:     []railway.Service{{ID: "s1", Name: "api"}, {ID: "s2", Name: "web"}, {ID: "s3", Name: "worker"}, {ID: "s4", Name: "cron"}},
	},
	{
		ID: "p2", Name: "blog", Workspace: "ws",
		Environments: []railway.Environment{{ID: "e3", Name: "prod", CanAccess: true, ServiceIDs: []string{"s5"}}},
		Services:     []railway.Service{{ID: "s5", Name: "site"}},
	},
}}

var monStates = []railway.DeploymentState{
	{EnvID: "e1", ServiceID: "s1", ActiveID: "dep-api", ActiveStatus: "SUCCESS", LatestStatus: "SUCCESS"},
	{EnvID: "e1", ServiceID: "s2", InProgress: []string{"dep-web"}},
	{EnvID: "e1", ServiceID: "s3", ActiveID: "dep-worker", ActiveStatus: "CRASHED", LatestStatus: "CRASHED"},
	{EnvID: "e1", ServiceID: "s4", ActiveID: "dep-cron", ActiveStatus: "SLEEPING", LatestStatus: "SLEEPING"},
}

// series is n points 30 s apart ending at now, valued by f.
func series(env, svc, m string, n int, f func(i int) float64) railway.MetricSeries {
	s := railway.MetricSeries{EnvID: env, ServiceID: svc, Measurement: m}
	end := now.Unix()
	for i := range n {
		s.Points = append(s.Points, railway.MetricPoint{TS: end - int64(30*(n-1-i)), Value: f(i)})
	}
	return s
}

func monMetrics() []railway.MetricSeries {
	const n = 60 // 30 minutes at 30 s
	return []railway.MetricSeries{
		series("e1", "s1", "CPU_USAGE", n, func(i int) float64 { return float64(i%12) * 0.1 }),
		series("e1", "s1", "CPU_LIMIT", n, func(int) float64 { return 2 }),
		series("e1", "s1", "MEMORY_USAGE_GB", n, func(i int) float64 { return 0.25 + float64(i)/400 }),
		series("e1", "s1", "MEMORY_LIMIT_GB", n, func(int) float64 { return 1 }),
		series("e1", "s1", "NETWORK_RX_GB", n, func(i int) float64 { return float64(i%5) / (1024 * 1024) }),
		series("e1", "s1", "NETWORK_TX_GB", n, func(i int) float64 { return float64(i%3) / (1024 * 1024) }),
		series("e1", "s2", "CPU_USAGE", n, func(i int) float64 { return 0.05 }),
		series("e1", "s2", "MEMORY_USAGE_GB", n, func(i int) float64 { return 0.5 }),
	}
}

// monCtx is a monitor context without API or Live: the table renders from the
// store alone.
func monCtx(w, h int) *tui.Ctx {
	st := store.New()
	st.ReplaceCatalog(monCatalog)
	st.ReplaceVariables("p1", []railway.VarRecord{
		{EnvID: "e1", ServiceID: "s1", Name: "DATABASE_URL", Unrendered: secret, Rendered: secret},
	})
	st.ReplaceStates("p1", []string{"e1"}, monStates)
	st.ReplaceMetrics([]string{"e1"}, monMetrics())
	return &tui.Ctx{
		Store: st, CS: &changeset.ChangeSet{},
		Views:    &view.Views{Store: st, Reg: apply.Registry{}},
		Settings: &settings.Settings{Projects: []string{"p1", "p2"}, WriteEnvs: []string{"e1", "e3"}},
		Theme:    tui.NewTheme(tuitest.Profile()),
		Width:    w, Height: h,
		Now: func() time.Time { return now },
	}
}

func renderMon(c *tui.Ctx, s tui.Screen) string {
	note, f := "", tui.Footer{Hints: s.Keys()}
	if n, ok := s.(tui.Noter); ok {
		note, f.Flash = n.Note(c), n.Notice(c)
	}
	return tuitest.Plain(tui.Frame(c, s.Title(), note, s.View(c, c.Width-tui.ChromeCols, c.Height-tui.ChromeRows), f))
}

func TestServicesMonitorGoldens(t *testing.T) {
	for _, w := range []int{80, 60, 150} {
		c := monCtx(w, 18)
		c.CS.Put(changeset.Change{ID: "c1", Kind: "service.redeploy", Place: domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"}, Target: "redeploy", Created: now}, nil)
		s := services.New()
		out := renderMon(c, s)
		tuitest.NoSecrets(t, out, secret)
		for _, want := range []string{"api", "web", "worker", "cron", "site"} {
			if !strings.Contains(out, want) {
				t.Errorf("%d: row %s missing", w, want)
			}
		}
		tuitest.Golden(t, "services_monitor_"+itoa(w)+".golden", out)
	}
}

func itoa(n int) string {
	return map[int]string{60: "60", 80: "80", 150: "150"}[n]
}

func TestServicesMonitorStatusAndNote(t *testing.T) {
	c := monCtx(150, 18)
	out := renderMon(c, services.New())
	for _, want := range []string{"● ONLINE", "◌ DEPLOYING", "✗ OFFLINE / FAILED", "☾ SLEEPING", "◇ reading", "5 services · 1 down"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	// a service with no metrics shows — in its value cells
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "worker") && strings.Count(l, "—") != 3 {
			t.Errorf("worker row = %q", l)
		}
	}
}

// rig is a monitor context on a fake Railway with a live socket.
type rig struct {
	t    *testing.T
	fake *railwaytest.Fake
	live *railway.Live
	c    *tui.Ctx
	sent chan tea.Msg
}

func newRig(t *testing.T) *rig {
	t.Helper()
	f := railwaytest.New(t)
	f.SetStates("p1", monStates)
	f.SetMetrics("p1", monMetrics())
	live := railway.NewLive(f.WSURL, staticTokens{}, nil)
	t.Cleanup(live.Close)
	r := &rig{t: t, fake: f, live: live, sent: make(chan tea.Msg, 100)}
	c := monCtx(80, 20)
	c.Settings.Projects = []string{"p1"}
	c.API = &railway.Client{T: &railway.Transport{Endpoint: f.URL, Tokens: staticTokens{}}}
	c.Live = live
	c.Send = func(m tea.Msg) { r.sent <- m }
	r.c = c
	return r
}

// run executes cmd and sorts what it yields: loads (StatesMsg, MetricsMsg),
// timers, and anything else.
func run(cmd tea.Cmd) (loads []tea.Msg, timers []services.Scheduled, other []tea.Msg) {
	var walk func(tea.Cmd)
	walk = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		switch m := cmd().(type) {
		case tea.BatchMsg:
			for _, c := range m {
				walk(c)
			}
		case loaders.StatesMsg, loaders.MetricsMsg:
			loads = append(loads, m)
		case services.Scheduled:
			timers = append(timers, m)
		case nil:
		default:
			other = append(other, m)
		}
	}
	walk(cmd)
	return
}

func countStates(loads []tea.Msg) int {
	n := 0
	for _, m := range loads {
		if _, ok := m.(loaders.StatesMsg); ok {
			n++
		}
	}
	return n
}

// deliver applies store updates as the root does, then hands msg to s.
func (r *rig) deliver(s tui.Screen, msg tea.Msg) (tui.Screen, tea.Cmd) {
	if u, ok := msg.(store.Update); ok {
		u.ApplyTo(r.c.Store)
	}
	return s.Update(r.c, msg)
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

func subscribedIDs(f *railwaytest.Fake) []string {
	var ids []string
	for _, s := range f.WSSubscribed() {
		ids = append(ids, s.ID)
	}
	return ids
}

// started resumes s, applies its first reads and waits for the four
// subscriptions: the running api, worker and cron deployments and the web
// deploy in progress.
func (r *rig) started(s tui.Screen) (tui.Screen, []services.Scheduled) {
	r.t.Helper()
	loads, timers, _ := run(s.Init(r.c))
	if countStates(loads) != 1 || len(loads) != 2 || len(timers) != 2 {
		r.t.Fatalf("Init: loads %d (states %d), timers %d", len(loads), countStates(loads), len(timers))
	}
	for _, m := range loads {
		s, _ = r.deliver(s, m)
	}
	eventually(r.t, "four subscriptions", func() bool { return len(r.fake.WSSubscribed()) == 4 })
	return s, timers
}

func TestServicesSubscribesRunningAndInProgress(t *testing.T) {
	r := newRig(t)
	s, _ := r.started(services.New())
	docOf := func(sub railwaytest.WSSub) string {
		switch {
		case strings.Contains(sub.Query, "deploymentInstanceExecutions"):
			in, _ := sub.Vars["input"].(map[string]any)
			return "instances:" + in["deploymentId"].(string)
		case strings.Contains(sub.Query, "deployment("):
			return "deployment:" + sub.Vars["id"].(string)
		}
		return "?"
	}
	var docs []string
	byDoc := map[string]string{}
	for _, sub := range r.fake.WSSubscribed() {
		docs = append(docs, docOf(sub))
		byDoc[docOf(sub)] = sub.ID
	}
	slices.Sort(docs)
	if !slices.Equal(docs, []string{"deployment:dep-web", "instances:dep-api", "instances:dep-cron", "instances:dep-worker"}) {
		t.Errorf("subscriptions = %v", docs)
	}

	// the deploy finished: its subscription goes, the new running one comes
	r.fake.SetStates("p1", []railway.DeploymentState{
		{EnvID: "e1", ServiceID: "s1", ActiveID: "dep-api", ActiveStatus: "SUCCESS"},
		{EnvID: "e1", ServiceID: "s2", ActiveID: "dep-web", ActiveStatus: "SUCCESS"},
	})
	_, cmd := s.Update(r.c, r1("r"))
	loads, _, _ := run(cmd)
	for _, m := range loads {
		s, _ = r.deliver(s, m)
	}
	eventually(t, "resync", func() bool {
		subs := r.fake.WSSubscribed()
		done := r.fake.WSCompletes()
		slices.Sort(done)
		want := []string{byDoc["deployment:dep-web"], byDoc["instances:dep-cron"], byDoc["instances:dep-worker"]}
		slices.Sort(want)
		return len(subs) == 5 && docOf(subs[4]) == "instances:dep-web" && slices.Equal(done, want)
	})
	s.(tui.Suspender).Suspend(r.c)
}

func TestServicesSuspendStopsPollingAndSubscriptions(t *testing.T) {
	r := newRig(t)
	s, first := r.started(services.New())

	// Resume again while on top: the first loop's ticks start nothing.
	loads, second, _ := run(s.(tui.Resumer).Resume(r.c))
	if len(loads) != 2 || len(second) != 2 {
		t.Fatalf("Resume: loads %d timers %d", len(loads), len(second))
	}
	for _, tm := range first {
		_, cmd := r.deliver(s, tm.Msg)
		if l, tms, _ := run(cmd); len(l) != 0 || len(tms) != 0 {
			t.Errorf("stale tick after Resume: loads %d timers %d", len(l), len(tms))
		}
	}
	// the current loop's ticks read and re-arm
	var third []services.Scheduled
	for _, tm := range second {
		_, cmd := r.deliver(s, tm.Msg)
		l, tms, _ := run(cmd)
		if len(l) != 1 || len(tms) != 1 || tms[0].D != tm.D {
			t.Errorf("tick %v: loads %d timers %v", tm.D, len(l), tms)
		}
		third = append(third, tms...)
	}
	if got := len(r.fake.WSSubscribed()); got != 4 {
		t.Errorf("Resume twice: %d subscriptions sent, want 4", got)
	}

	s.(tui.Suspender).Suspend(r.c)
	for _, tm := range third {
		_, cmd := r.deliver(s, tm.Msg)
		if l, tms, _ := run(cmd); len(l) != 0 || len(tms) != 0 {
			t.Errorf("tick after Suspend: loads %d timers %d", len(l), len(tms))
		}
	}
	// every subscription was completed: Live holds none
	eventually(t, "completes", func() bool {
		done := r.fake.WSCompletes()
		slices.Sort(done)
		ids := subscribedIDs(r.fake)
		slices.Sort(ids)
		return slices.Equal(done, ids)
	})
	// a late states read while suspended subscribes nothing
	loads, _, _ = run(loaders.States(r.c.API, loaders.Targets(r.c.Store.Projects(), r.c.Settings), r.c.Now))
	for _, m := range loads {
		s, _ = r.deliver(s, m)
	}
	time.Sleep(50 * time.Millisecond)
	if got := len(r.fake.WSSubscribed()); got != 4 {
		t.Errorf("subscribed while suspended: %d", got)
	}
	eventually(t, "socket idle", func() bool { return r.live.State() == "idle" })
}

func TestServicesLiveEventDebounced(t *testing.T) {
	r := newRig(t)
	s, _ := r.started(services.New())
	id := r.fake.WSSubscribed()[0].ID
	for range 3 {
		r.fake.WSPush(id, map[string]any{"deploymentInstanceExecutions": map[string]any{"id": "i1", "status": "RUNNING"}})
	}
	var timers []services.Scheduled
	for range 3 {
		select {
		case m := <-r.sent:
			_, cmd := r.deliver(s, m)
			l, tms, _ := run(cmd)
			if len(l) != 0 {
				t.Errorf("an event read at once")
			}
			for _, tm := range tms {
				if tm.D != 2*time.Second { // the burst's max wait
					timers = append(timers, tm)
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatal("event not forwarded")
		}
	}
	if len(timers) != 3 {
		t.Fatalf("timers = %d", len(timers))
	}
	reads := 0
	for _, tm := range timers {
		if tm.D != loaders.LiveDebounce {
			t.Errorf("debounce = %v", tm.D)
		}
		_, cmd := r.deliver(s, tm.Msg)
		l, _, _ := run(cmd)
		reads += countStates(l)
		if len(l) != countStates(l) {
			t.Errorf("an event read metrics")
		}
	}
	if reads != 1 {
		t.Errorf("states reads = %d, want 1", reads)
	}
}

func TestServicesLiveFailSanitized(t *testing.T) {
	r := newRig(t)
	s, _ := r.started(services.New())
	id := r.fake.WSSubscribed()[0].ID
	r.fake.WSPushErrors(id, "bad \x1b[31mthing")
	select {
	case m := <-r.sent:
		s, _ = r.deliver(s, m)
	case <-time.After(5 * time.Second):
		t.Fatal("fail not forwarded")
	}
	n := s.(tui.Noter).Notice(r.c)
	if strings.Contains(n.Text, "\x1b") || !strings.Contains(n.Text, "bad") || !strings.Contains(n.Text, "thing") {
		t.Errorf("notice = %q", n.Text)
	}
}

func TestServicesLiveRetryingNotice(t *testing.T) {
	r := newRig(t)
	var once sync.Once
	block := make(chan struct{})
	t.Cleanup(func() { once.Do(func() { close(block) }) })
	r.live.Sleep = func(ctx context.Context, d time.Duration) error {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return ctx.Err()
	}
	s, _ := r.started(services.New())
	r.fake.WSDrop()
	eventually(t, "retrying", func() bool { return r.live.State() == "retrying" })
	n := s.(tui.Noter).Notice(r.c)
	if n.Kind != '✗' || n.Text != "live updates paused · retrying" {
		t.Errorf("notice = %c %q", n.Kind, n.Text)
	}
	if out := renderMon(r.c, s); !strings.Contains(out, "✗ live updates paused · retrying") {
		t.Errorf("footer:\n%s", out)
	}
	s.(tui.Suspender).Suspend(r.c)
}

func TestServicesFailedReadKeepsData(t *testing.T) {
	r := newRig(t)
	s, _ := r.started(services.New())
	before := renderMon(r.c, s)
	if !strings.Contains(before, "1.10") {
		t.Fatalf("no data before:\n%s", before)
	}
	r.fake.FailNext("ServiceStates", "boom")
	_, cmd := s.Update(r.c, r1("r"))
	loads, _, _ := run(cmd)
	if countStates(loads) != 1 || len(loads) != 2 {
		t.Fatalf("r: loads %d", len(loads))
	}
	for _, m := range loads {
		s, _ = r.deliver(s, m)
	}
	out := renderMon(r.c, s)
	if !strings.Contains(out, "✗ cannot read Railway: ") || !strings.Contains(out, "boom") || !strings.Contains(out, "· showing the last read · r retries") {
		t.Errorf("no failure line:\n%s", out)
	}
	for _, want := range []string{"api", "web", "worker", "cron"} {
		if !strings.Contains(out, want) {
			t.Errorf("row %s lost:\n%s", want, out)
		}
	}
	if got := s.(tui.Noter).Note(r.c); got != "4 services · 1 down" {
		t.Errorf("note = %q", got)
	}
	s.(tui.Suspender).Suspend(r.c)
}

func r1(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func TestThemeLevelSteps(t *testing.T) {
	th := tui.NewTheme(termenv.TrueColor)
	if th.Level(0).Render("x") == th.Level(1).Render("x") {
		t.Error("low and high levels share a colour")
	}
	if th.Level(0.01).Render("x") != th.Level(0).Render("x") {
		t.Error("levels within one step differ")
	}
	if th.Level(0.5).Render("x") == th.Level(0.25).Render("x") {
		t.Error("distinct steps share a colour")
	}
}

// At 60 columns the charts, then the net and mem values give way; the
// pending column and its header always show and no row is cut with "…".
func TestServicesNarrowKeepsPending(t *testing.T) {
	c := monCtx(60, 18)
	c.CS.Put(changeset.Change{ID: "c1", Kind: "service.redeploy", Place: domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"}, Target: "redeploy", Created: now}, nil)
	out := renderMon(c, services.New())
	lines := strings.Split(out, "\n")
	var hdr, api string
	for _, l := range lines {
		switch {
		case strings.Contains(l, "project"):
			hdr = l
		case strings.Contains(l, " api "):
			api = l
		}
	}
	if !strings.Contains(hdr, "pending") || !strings.Contains(hdr, "mem") || strings.Contains(hdr, "net") {
		t.Errorf("header = %q", hdr)
	}
	if !strings.Contains(api, "● 1") || !strings.Contains(api, "407.0 MB") {
		t.Errorf("api row = %q", api)
	}
	for _, l := range lines {
		if strings.Contains(l, "shop") || strings.Contains(l, "blog") {
			if strings.Contains(strings.ReplaceAll(l, "pr…", ""), "…") {
				t.Errorf("row cut: %q", l)
			}
		}
	}
	// narrower still: mem goes too, pending stays
	c = monCtx(50, 18)
	c.CS.Put(changeset.Change{ID: "c1", Kind: "service.redeploy", Place: domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"}, Target: "redeploy", Created: now}, nil)
	out = renderMon(c, services.New())
	if !strings.Contains(out, "pending") || !strings.Contains(out, "● 1") || strings.Contains(out, " mem ") {
		t.Errorf("50 cols:\n%s", out)
	}
}

// brailleRuns are the lengths of the runs of Braille cells in a line.
func brailleRuns(l string) []int {
	var runs []int
	n := 0
	for _, r := range l + " " {
		if r >= 0x2800 && r <= 0x28FF {
			n++
			continue
		}
		if n > 0 {
			runs = append(runs, n)
		}
		n = 0
	}
	return runs
}

// A table chart narrower than 8 cells is not drawn: at 99 columns the charts
// would get 7, so all three go and env and the status label take the room;
// at 100 all three show with 8.
func TestServicesChartMinimumWidth(t *testing.T) {
	// with every name whole, the charts reach 8 cells at 107 columns
	out := renderMon(monCtx(106, 18), services.New())
	api := lineWith(out, " api ")
	if runs := brailleRuns(api); len(runs) != 0 {
		t.Errorf("106: charts %v in %q", runs, api)
	}
	if !strings.Contains(api, "production") || !strings.Contains(api, "● ONLINE") {
		t.Errorf("106: env and status label take the room: %q", api)
	}
	out = renderMon(monCtx(107, 18), services.New())
	api = lineWith(out, " api ")
	if runs := brailleRuns(api); !slices.Equal(runs, []int{8, 8, 8}) {
		t.Errorf("107: charts %v in %q", runs, api)
	}
	if !strings.Contains(api, "production") {
		t.Errorf("107: env squeezed by the charts: %q", api)
	}
}

// Charts never squeeze the env name: as the terminal widens, the env text
// only grows (at 100 columns it is whole, with or without charts).
func TestServicesEnvNeverShrinksAsWidthGrows(t *testing.T) {
	envAt := func(w int) string {
		api := lineWith(renderMon(monCtx(w, 18), services.New()), " api ")
		f := strings.Fields(api)
		if len(f) < 3 {
			t.Fatalf("%d: api row %q", w, api)
		}
		return f[2]
	}
	prev := ""
	for w := 60; w <= 200; w++ {
		env := envAt(w)
		if utf8.RuneCountInString(env) < utf8.RuneCountInString(prev) {
			t.Errorf("width %d: env %q, shorter than %q at %d", w, env, prev, w-1)
		}
		prev = env
	}
	if env := envAt(100); env != "production" {
		t.Errorf("100: env %q, want production", env)
	}
}

// At 80 columns: no charts, the whole env name, the status icon and all
// three values.
func TestServices80Columns(t *testing.T) {
	out := renderMon(monCtx(80, 18), services.New())
	api := lineWith(out, " api ")
	if runs := brailleRuns(api); len(runs) != 0 {
		t.Errorf("charts %v in %q", runs, api)
	}
	for _, want := range []string{"production", "●", "1.10", "407.0 MB", "204.8 B/s"} {
		if !strings.Contains(api, want) {
			t.Errorf("api row lacks %q: %q", want, api)
		}
	}
	if strings.Contains(api, "ONLINE") {
		t.Errorf("status label at 80: %q", api)
	}
}

func TestServicesPlainReadingIconIsNotEllipsis(t *testing.T) {
	if got := (tui.Icons{}).Status("…"); got == "…" || got == "" {
		t.Errorf("plain reading icon = %q", got)
	}
	if got := (tui.Icons{Nerd: true}).Status("…"); got == "…" || got == "" {
		t.Errorf("nerd reading icon = %q", got)
	}
}

func TestLevelStep(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want int
	}{{0, 0}, {-1, 0}, {math.NaN(), 0}, {1, 8}, {2, 8}, {0.0625, 1}, {0.06, 0}, {0.5, 4}} {
		if got := tui.LevelStep(tc.in); got != tc.want {
			t.Errorf("LevelStep(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
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

// A second Resume keeps the subscriptions, and their events still count.
func TestServicesResumeTwiceKeepsLiveEvents(t *testing.T) {
	r := newRig(t)
	s, _ := r.started(services.New())
	run(s.(tui.Resumer).Resume(r.c))
	r.fake.WSPush(r.fake.WSSubscribed()[0].ID, map[string]any{"deploymentInstanceExecutions": map[string]any{"id": "i1", "status": "RUNNING"}})
	_, cmd := r.deliver(s, r.next())
	_, tms, _ := run(cmd)
	if !slices.ContainsFunc(tms, func(tm services.Scheduled) bool { return tm.D == loaders.LiveDebounce }) {
		t.Errorf("event after a second Resume: timers %v", tms)
	}
	s.(tui.Suspender).Suspend(r.c)
}

// A stream the server completed is no error: it reads the states (debounced).
// When it is made again: TestServicesStreamEndedNoResubscribeLoop.
func TestServicesStreamEndedReadsStates(t *testing.T) {
	r := newRig(t)
	s, _ := r.started(services.New())
	r.fake.WSComplete(r.fake.WSSubscribed()[0].ID)
	s, cmd := r.deliver(s, r.next())
	if n := s.(tui.Noter).Notice(r.c); n.Text != "" {
		t.Errorf("notice = %q", n.Text)
	}
	_, tms, _ := run(cmd)
	reads := 0
	for _, tm := range tms {
		_, cmd := r.deliver(s, tm.Msg)
		l, _, _ := run(cmd)
		reads += countStates(l)
		for _, m := range l {
			s, _ = r.deliver(s, m)
		}
	}
	if reads != 1 {
		t.Fatalf("states reads = %d, want 1 (timers %v)", reads, tms)
	}
	s.(tui.Suspender).Suspend(r.c)
}

// A connection failure is no subscription failure: Live resends every
// subscription, so the next sync makes none again. The notice says "live: "
// once.
func TestServicesReconnectKeepsSubscriptions(t *testing.T) {
	r := newRig(t)
	release := make(chan struct{})
	r.live.Sleep = func(ctx context.Context, d time.Duration) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s, _ := r.started(services.New())
	r.fake.WSRejectInits(4403, "nope")
	r.fake.WSDrop()
	release <- struct{}{}
	// the lost connection, then the rejected one, each to all four
	for range 8 {
		s, _ = r.deliver(s, r.next())
	}
	// still down: the failure shows
	if n := s.(tui.Noter).Notice(r.c); n.Text == "" {
		t.Errorf("no notice while the socket is down (state %s)", r.live.State())
	}
	r.fake.WSRejectInits(0, "")
	release <- struct{}{}
	eventually(t, "resent", func() bool { return r.live.State() == "open" && len(r.fake.WSSubscribed()) == 8 })
	// the socket is open again: the connection failure is over
	if n := s.(tui.Noter).Notice(r.c); n.Text != "" {
		t.Errorf("notice after reconnect = %q", n.Text)
	}
	_, cmd := s.Update(r.c, r1("r"))
	loads, _, _ := run(cmd)
	for _, m := range loads {
		s, _ = r.deliver(s, m)
	}
	time.Sleep(50 * time.Millisecond)
	if got := len(r.fake.WSSubscribed()); got != 8 {
		t.Errorf("subscriptions sent = %d, want 8", got)
	}
	if got := r.fake.WSCompletes(); len(got) != 0 {
		t.Errorf("completes = %v", got)
	}
	s.(tui.Suspender).Suspend(r.c)
}

// Events closer than the debounce cannot postpone the read past 2 s.
func TestServicesLiveDebounceMaxWait(t *testing.T) {
	r := newRig(t)
	s, _ := r.started(services.New())
	id := r.fake.WSSubscribed()[0].ID
	var timers []services.Scheduled
	for range 3 {
		r.fake.WSPush(id, map[string]any{"deploymentInstanceExecutions": map[string]any{"id": "i1", "status": "RUNNING"}})
		_, cmd := r.deliver(s, r.next())
		_, tms, _ := run(cmd)
		timers = append(timers, tms...)
	}
	var maxWait []services.Scheduled
	for _, tm := range timers {
		if tm.D == 2*time.Second {
			maxWait = append(maxWait, tm)
		}
	}
	if len(maxWait) != 1 {
		t.Fatalf("max-wait timers = %d (%v)", len(maxWait), timers)
	}
	_, cmd := r.deliver(s, maxWait[0].Msg)
	if l, _, _ := run(cmd); countStates(l) != 1 {
		t.Errorf("max wait: states reads = %d", countStates(l))
	}
	// the burst is read: its last debounce timer reads nothing more
	_, cmd = r.deliver(s, timers[len(timers)-1].Msg)
	if l, _, _ := run(cmd); len(l) != 0 {
		t.Errorf("debounce after max wait read again")
	}
	s.(tui.Suspender).Suspend(r.c)
}

// A stream the server completes at once is not made again before the next
// poll tick, and its end reads the states at most once per poll tick.
func TestServicesStreamEndedNoResubscribeLoop(t *testing.T) {
	r := newRig(t)
	s, timers := r.started(services.New())
	apiSubs := func() []string {
		var ids []string
		for _, sub := range r.fake.WSSubscribed() {
			if in, ok := sub.Vars["input"].(map[string]any); ok && in["deploymentId"] == "dep-api" {
				ids = append(ids, sub.ID)
			}
		}
		return ids
	}
	// apply runs cmd's loads into the store and the screen, and counts the
	// states reads.
	apply := func(cmd tea.Cmd) (reads int, timers []services.Scheduled) {
		loads, tms, _ := run(cmd)
		for _, m := range loads {
			s, _ = r.deliver(s, m)
		}
		return countStates(loads), tms
	}
	// endAPI completes the newest dep-api subscription and delivers the
	// timers its end schedules.
	endAPI := func() int {
		ids := apiSubs()
		r.fake.WSComplete(ids[len(ids)-1])
		var cmd tea.Cmd
		s, cmd = r.deliver(s, r.next())
		_, tms := apply(cmd)
		reads := 0
		for _, tm := range tms {
			var c tea.Cmd
			s, c = r.deliver(s, tm.Msg)
			n, _ := apply(c)
			reads += n
		}
		return reads
	}

	if n := endAPI(); n != 1 {
		t.Fatalf("first end: states reads = %d, want 1", n)
	}
	for range 3 { // reads before the poll tick make nothing again
		_, cmd := s.Update(r.c, r1("r"))
		apply(cmd)
	}
	time.Sleep(50 * time.Millisecond)
	if got := len(apiSubs()); got != 1 {
		t.Fatalf("remade before the tick: %d dep-api subscriptions", got)
	}

	// each poll tick makes it again once, and its end reads once more
	for tick := 1; tick <= 3; tick++ {
		var next []services.Scheduled
		for _, tm := range timers {
			if tm.D == loaders.StatesEvery {
				var cmd tea.Cmd
				s, cmd = r.deliver(s, tm.Msg)
				_, tms := apply(cmd)
				next = append(next, tms...)
			}
		}
		timers = next
		eventually(t, "remade after the tick", func() bool { return len(apiSubs()) == 1+tick })

		// it ends again at once: one read, not made again before the tick
		if n := endAPI(); n != 1 {
			t.Errorf("tick %d: end reads = %d, want 1", tick, n)
		}
		for range 3 {
			_, cmd := s.Update(r.c, r1("r"))
			apply(cmd)
		}
		time.Sleep(50 * time.Millisecond)
		if got := len(apiSubs()); got != 1+tick {
			t.Errorf("tick %d: dep-api subscriptions = %d, want %d", tick, got, 1+tick)
		}
	}
	s.(tui.Suspender).Suspend(r.c)
}

// A deploy whose stream ended early (still in progress) and that really
// finishes after the next poll tick is read at once (debounced), not at the
// poll after.
func TestServicesRealFinishAfterTransientEnd(t *testing.T) {
	r := newRig(t)
	s, timers := r.started(services.New())
	webSubs := func() []string {
		var ids []string
		for _, sub := range r.fake.WSSubscribed() {
			if sub.Vars["id"] == "dep-web" {
				ids = append(ids, sub.ID)
			}
		}
		return ids
	}
	apply := func(cmd tea.Cmd) (reads int, tms []services.Scheduled) {
		loads, tms, _ := run(cmd)
		for _, m := range loads {
			s, _ = r.deliver(s, m)
		}
		return countStates(loads), tms
	}
	// endWeb completes the newest dep-web stream and counts the states reads
	// its debounce makes.
	endWeb := func() int {
		ids := webSubs()
		r.fake.WSComplete(ids[len(ids)-1])
		var cmd tea.Cmd
		s, cmd = r.deliver(s, r.next())
		_, tms := apply(cmd)
		reads := 0
		for _, tm := range tms {
			var c tea.Cmd
			s, c = r.deliver(s, tm.Msg)
			n, _ := apply(c)
			reads += n
		}
		return reads
	}

	// a transient end: the deploy is still in progress
	if n := endWeb(); n != 1 {
		t.Fatalf("transient end: reads = %d, want 1", n)
	}
	for _, tm := range timers {
		if tm.D == loaders.StatesEvery {
			var cmd tea.Cmd
			s, cmd = r.deliver(s, tm.Msg)
			apply(cmd)
		}
	}
	eventually(t, "remade after the tick", func() bool { return len(webSubs()) == 2 })

	// it really finishes: its end reads at once and finds it running
	r.fake.SetStates("p1", []railway.DeploymentState{
		{EnvID: "e1", ServiceID: "s1", ActiveID: "dep-api", ActiveStatus: "SUCCESS", LatestStatus: "SUCCESS"},
		{EnvID: "e1", ServiceID: "s2", ActiveID: "dep-web", ActiveStatus: "SUCCESS", LatestStatus: "SUCCESS"},
	})
	if n := endWeb(); n != 1 {
		t.Fatalf("real finish: reads = %d, want 1", n)
	}
	st, _ := r.c.Store.State("e1", "s2")
	if st.ActiveID != "dep-web" || len(st.InProgress) != 0 {
		t.Errorf("state after the finish = %#v", st)
	}
	s.(tui.Suspender).Suspend(r.c)
}

// A connection failure is judged by what it is, not by the socket's state
// when the screen handles it: failures handled after the socket reopened
// leave no notice, while an error of a subscription itself still shows.
func TestServicesConnectionFailureHandledAfterReopen(t *testing.T) {
	r := newRig(t)
	s, _ := r.started(services.New())
	r.fake.WSDrop()
	eventually(t, "resent", func() bool { return r.live.State() == "open" && len(r.fake.WSSubscribed()) == 8 })
	for range 4 {
		s, _ = r.deliver(s, r.next())
	}
	if n := s.(tui.Noter).Notice(r.c); n.Text != "" {
		t.Errorf("notice for a connection failure handled after reopen = %q", n.Text)
	}
	r.fake.WSPushErrors(r.fake.WSSubscribed()[4].ID, "rate limited")
	s, _ = r.deliver(s, r.next())
	if n := s.(tui.Noter).Notice(r.c); !strings.Contains(n.Text, "rate limited") {
		t.Errorf("subscription error notice = %q", n.Text)
	}
	s.(tui.Suspender).Suspend(r.c)
}
