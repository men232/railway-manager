package services_test

import (
	"encoding/json"
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
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/screens/services"
	"github.com/men232/railway-manager/internal/tui/tuitest"
	"github.com/men232/railway-manager/internal/view"
)

var (
	apiPlace    = domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"}
	cronPlace   = domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s4"} // the second row
	fullSHA     = "0123456789abcdef0123456789abcdef01234567"
	gibibyte    = 1024.0 * 1024 * 1024
	mem412MB    = 412.3 * 1024 * 1024 / gibibyte // 412.3 MB in Railway's GB
	panelHeight = 30
)

// panelMetrics are the table series of the api: 0.42 vCPU of 32, 412.3 MB
// of 32 GB, some traffic.
func panelMetrics() []railway.MetricSeries {
	const n = 60
	last := func(v float64, f func(int) float64) func(int) float64 {
		return func(i int) float64 {
			if i == n-1 {
				return v
			}
			return f(i)
		}
	}
	out := []railway.MetricSeries{
		series("e1", "s1", "CPU_USAGE", n, last(0.42, func(i int) float64 { return float64(i%7) * 0.05 })),
		series("e1", "s1", "CPU_LIMIT", n, func(int) float64 { return 32 }),
		series("e1", "s1", "MEMORY_USAGE_GB", n, last(mem412MB, func(i int) float64 { return 0.3 + float64(i)/1000 })),
		series("e1", "s1", "MEMORY_LIMIT_GB", n, func(int) float64 { return 32 }),
		series("e1", "s1", "NETWORK_RX_GB", n, func(i int) float64 { return float64(i%5) / (1024 * 1024) }),
		series("e1", "s1", "NETWORK_TX_GB", n, func(i int) float64 { return float64(i%3) / (1024 * 1024) }),
	}
	return append(out, monMetrics()[6:]...) // the web's
}

// panelSeries is the api's 1h window: 60 points a minute apart.
func panelSeries() []railway.MetricSeries {
	const n = 60
	at := func(m string, f func(int) float64) railway.MetricSeries {
		s := railway.MetricSeries{EnvID: "e1", ServiceID: "s1", Measurement: m}
		for i := range n {
			s.Points = append(s.Points, railway.MetricPoint{TS: now.Unix() - int64(60*(n-1-i)), Value: f(i)})
		}
		return s
	}
	return []railway.MetricSeries{
		at("CPU_USAGE", func(i int) float64 { return 0.1 + float64(i%10)*0.1 }),
		at("CPU_LIMIT", func(int) float64 { return 32 }),
		at("MEMORY_USAGE_GB", func(i int) float64 { return 0.25 + float64(i)/400 }),
		at("MEMORY_LIMIT_GB", func(int) float64 { return 32 }),
		at("NETWORK_RX_GB", func(i int) float64 { return float64(i%4) / 1024 }),
		at("NETWORK_TX_GB", func(i int) float64 { return float64(i%6) / 2048 }),
	}
}

func apiDetail() railway.ServiceDetail {
	return railway.ServiceDetail{
		Regions: map[string]int{"us-west2": 2, "europe-west4": 1},
		Repo:    "acme/shop", Branch: "main", Builder: "NIXPACKS",
		RestartPolicy: "ON_FAILURE", RestartMaxRetries: 3, Healthcheck: "/health", Sleeps: true,
		Deploys: []railway.DeployInfo{
			{ID: "dep-api", Status: "SUCCESS", Reason: "deploy", Creator: "ana", CommitHash: fullSHA,
				CommitMessage: "fix checkout totals\n\nlonger body", CreatedAt: now.Add(-3*time.Hour - 12*time.Minute)},
			{ID: "dep-old", Status: "FAILED", Reason: "redeploy", Creator: "bo", CreatedAt: now.Add(-26 * time.Hour)},
			{ID: "dep-older", Status: "REMOVED", Reason: "deploy", Creator: "ana", CreatedAt: now.Add(-50 * time.Hour)},
		},
	}
}

// panelCtx is the monitor fixture with everything the api's panel shows:
// detail, domains, one volume, the environment config and the 1h series.
func panelCtx(w, h int) *tui.Ctx {
	c := monCtx(w, h)
	st := c.Store
	c.Views = &view.Views{Store: st, Reg: kinds.NewRegistry()}
	st.ReplaceMetrics([]string{"e1"}, panelMetrics())
	st.ReplaceVolumes([]string{"e1"}, []railway.VolumeMount{
		{EnvID: "e1", ServiceID: "s1", VolumeID: "vol-1", MountPath: "/data", SizeMB: 5000, UsedMB: 1200},
	})
	st.SetConfig("e1", railway.EnvConfig{
		Etag:     "etag-1",
		Services: map[string]railway.ServiceConfig{"s1": {}},
		Volumes:  map[string]railway.VolumeConfig{"vol-1": {SizeMB: 5000}},
	})
	st.SetDetail("e1", "s1", apiDetail())
	st.SetDomains(apiPlace, railway.Domains{
		Service: []railway.ServiceDomain{{ID: "sd-1", Domain: "api-production.up.railway.app", Port: 8080}},
		Custom:  []railway.CustomDomain{{ID: "cd-1", Domain: "shop.example.com", Port: 8080, Verified: true}},
	})
	// the 1m level over the span this width draws
	st.SetPanelSeries(apiPlace, 60, services.PanelSpanAt(w-tui.ChromeCols, 60), panelSeries())
	return c
}

func body(c *tui.Ctx, s tui.Screen) string {
	return s.View(c, c.Width-tui.ChromeCols, c.Height-tui.ChromeRows)
}

func lineWith(out, sub string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}

func TestPanelShowsInfo(t *testing.T) {
	c := panelCtx(150, panelHeight)
	out := ansi.Strip(body(c, services.New()))
	for _, want := range []string{
		"● ONLINE", "api", "shop/production", "europe-west4, us-west2 ×2", "up 3h 12m",
		"acme/shop", "main", "NIXPACKS", "on failure ×3", "/health",
		"0123456", "fix checkout totals", "@ana",
		"api-production.up.railway.app", "shop.example.com",
		"0.42 / 32 vCPU", "412.3 MB / 32.0 GB", "1.2 GB / 5.0 GB  /data", "↓ ", "↑ ",
		"deploy", "3h 12m ago", "redeploy", "@bo",
		"now ", "peak ", "limit ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("panel lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "longer body") {
		t.Error("only the first commit message line shows")
	}
	d := apiDetail()
	d.Cron = "0 3 * * *"
	c.Store.SetDetail("e1", "s1", d)
	if out := ansi.Strip(body(c, services.New())); !strings.Contains(out, "☾ sleeps when idle") || !strings.Contains(out, "cron 0 3 * * *") {
		t.Errorf("sleep and cron items:\n%s", out)
	}
}

// "sleeps when idle" is a setting: a running service that has it is online,
// in the table and in the panel; only a SLEEPING deployment shows ☾.
func TestSleepsWhenIdleIsNotAsleep(t *testing.T) {
	c := panelCtx(150, panelHeight)
	out := ansi.Strip(body(c, services.New()))
	if l := lineWith(out, " api "); !strings.Contains(l, "● ONLINE") {
		t.Errorf("table row: %q", l)
	}
	if l := lineWith(out, "shop/production"); !strings.Contains(l, "● ONLINE") || strings.Contains(l, "SLEEPING") {
		t.Errorf("panel status: %q", l)
	}
	if !strings.Contains(out, "☾ sleeps when idle") {
		t.Errorf("the setting still shows:\n%s", out)
	}
	if l := lineWith(out, " cron "); !strings.Contains(l, "☾ SLEEPING") {
		t.Errorf("a SLEEPING deployment: %q", l)
	}
}

func TestPanelPlainIconsHaveNoPrivateUse(t *testing.T) {
	for _, nerd := range []bool{false, true} {
		// pending changes: their marks are icons too
		c := pendingCtx(t, 150)
		c.Icons = tui.Icons{Nerd: nerd}
		out := body(c, services.New())
		if !nerd && !strings.Contains(ansi.Strip(out), "→ 16 vCPU ●") {
			t.Errorf("plain pending mark missing:\n%s", ansi.Strip(out))
		}
		pua := 0
		for _, r := range out {
			if (r >= 0xE000 && r <= 0xF8FF) || r >= 0xF0000 {
				pua++
			}
		}
		if !nerd && pua != 0 {
			t.Errorf("plain icons: %d private use runes:\n%s", pua, ansi.Strip(out))
		}
		if nerd && pua == 0 {
			t.Error("nerd icons: no Nerd Font glyph at all")
		}
	}
}

func osc8(url, text string) string { return "\x1b]8;;" + url + "\x1b\\" + text + "\x1b]8;;\x1b\\" }

func TestPanelLinks(t *testing.T) {
	c := panelCtx(150, panelHeight)
	out := body(c, services.New())
	for _, url := range []string{
		"https://github.com/acme/shop",
		"https://github.com/acme/shop/commit/" + fullSHA,
		"https://api-production.up.railway.app",
		"https://shop.example.com",
	} {
		if !strings.Contains(out, "\x1b]8;;"+url+"\x1b\\") {
			t.Errorf("no link to %s", url)
		}
	}
	// widths are measured without the escapes: no line is wider than the body
	for _, l := range strings.Split(out, "\n") {
		if w := ansi.StringWidth(l); w > c.Width-tui.ChromeCols {
			t.Errorf("line %d cells wide: %q", w, ansi.Strip(l))
		}
	}

	// a coloured theme keeps the links whole
	cc := panelCtx(150, panelHeight)
	cc.Theme = tui.NewTheme(termenv.TrueColor)
	if out := body(cc, services.New()); !strings.Contains(out, "\x1b]8;;https://github.com/acme/shop\x1b\\") || !strings.Contains(ansi.Strip(out), "acme/shop") {
		t.Errorf("coloured link broken: %q", out)
	}

	d := apiDetail()
	d.Repo = "evil/x\x1b]8;;javascript:"
	d.Deploys[0].CommitHash = "not-a-sha"
	c.Store.SetDetail("e1", "s1", d)
	c.Store.SetDomains(apiPlace, railway.Domains{Custom: []railway.CustomDomain{{ID: "cd-x", Domain: "bad host/x"}}})
	out = body(c, services.New())
	if strings.Contains(out, "\x1b]8;;javascript") || strings.Contains(out, "github.com/evil") || strings.Contains(out, "https://bad") {
		t.Errorf("unsafe values must not become links:\n%q", out)
	}
	if strings.Contains(out, "\x1b]8;;https") {
		t.Errorf("no link expected:\n%q", out)
	}
	if !strings.Contains(ansi.Strip(out), "evil/x") || !strings.Contains(ansi.Strip(out), "not-a-s") {
		t.Errorf("the values still show as plain text:\n%s", ansi.Strip(out))
	}
}

func TestTUILinkStyled(t *testing.T) {
	if got := tui.LinkStyled("https://a.example", "ab"); got != osc8("https://a.example", "ab") {
		t.Errorf("LinkStyled = %q", got)
	}
	if got := tui.LinkStyled("https://a.example\x1b\\x", "t"); got != "t" {
		t.Errorf("a url with a control char is no link: %q", got)
	}
	if got := tui.LinkStyled("https://a.example x", "t"); got != "t" {
		t.Errorf("a url with a space is no link: %q", got)
	}
	if w := ansi.StringWidth(tui.LinkStyled("https://a.example", "abc")); w != 3 {
		t.Errorf("width = %d", w)
	}
}

func pendingCtx(t *testing.T, w int) *tui.Ctx {
	t.Helper()
	c := panelCtx(w, panelHeight)
	cpu, mem := 16.0, int64(16<<30)
	add := func(ch changeset.Change, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.CS.Put(ch, nil); err != nil {
			t.Fatal(err)
		}
	}
	add(kinds.SetLimits(c.Store, apiPlace, &cpu, &mem, now))
	add(kinds.ResizeVolume(c.Store, apiPlace, "vol-1", 10000, now))
	add(kinds.AddCustomDomain(c.Store, apiPlace, "new.example.com", 8080, now))
	add(kinds.DeleteCustomDomain(c.Store, apiPlace, "shop.example.com", now))
	return c
}

func TestPanelPendingInline(t *testing.T) {
	c := pendingCtx(t, 150)
	out := ansi.Strip(body(c, services.New()))
	if !strings.Contains(out, "0.42 / 32 → 16 vCPU ●") {
		t.Errorf("cpu pending:\n%s", out)
	}
}

func TestPanelPendingLimits(t *testing.T) {
	c := pendingCtx(t, 150)
	out := ansi.Strip(body(c, services.New()))
	for _, want := range []string{
		"0.42 / 32 → 16 vCPU ●",
		"412.3 MB / 32.0 GB → 16.0 GB ●",
		"1.2 GB / 5.0 GB → 10.0 GB ● can't be undone",
		"+ new.example.com ●",
		"✗ shop.example.com ●",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("panel lacks %q:\n%s", want, out)
		}
	}
	// the pending delete is no link
	if strings.Contains(body(c, services.New()), "\x1b]8;;https://shop.example.com") {
		t.Error("a domain pending delete should not link")
	}

	// a clear shows the plan default
	c = panelCtx(150, panelHeight)
	ch, err := kinds.SetLimits(c.Store, apiPlace, nil, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CS.Put(ch, nil); err != nil {
		t.Fatal(err)
	}
	out = ansi.Strip(body(c, services.New()))
	if !strings.Contains(out, "0.42 / 32 → default vCPU ●") {
		t.Errorf("clear pending:\n%s", out)
	}
}

func TestPanelHiddenWhenNoRoom(t *testing.T) {
	// body 20 = header + 3 rows + separator + 15 panel lines: shown; one
	// less: not
	c := panelCtx(80, 20+tui.ChromeRows)
	if out := ansi.Strip(body(c, services.New())); !strings.Contains(out, "source") {
		t.Errorf("panel should show at body height 20:\n%s", out)
	} else if n := strings.Count(out, "\n") + 1; n != 20 {
		t.Errorf("body has %d lines, want 20:\n%s", n, out)
	} else if !strings.Contains(out, " web ") || strings.Contains(out, "worker") {
		t.Errorf("the table keeps 3 rows:\n%s", out)
	}
	c = panelCtx(80, 19+tui.ChromeRows)
	out := ansi.Strip(body(c, services.New()))
	if strings.Contains(out, "source") || strings.Contains(out, "deploys") {
		t.Errorf("panel should hide at body height 19:\n%s", out)
	}
	if !strings.Contains(out, "site") {
		t.Errorf("the table alone shows every row it can:\n%s", out)
	}
	if n := len(strings.Split(body(panelCtx(80, 30), services.New()), "\n")); n > 30-tui.ChromeRows {
		t.Errorf("body has %d lines", n)
	}
}

// panelRig is the fake-Railway rig, tall enough for the panel.
func panelRig(t *testing.T) *rig {
	r := newRig(t)
	r.c.Height = 40
	r.fake.SetDetail("e1", "s1", apiDetail())
	r.fake.SetDetail("e1", "s4", railway.ServiceDetail{})
	return r
}

// scheduled splits timers by delay.
func scheduled(timers []services.Scheduled, d time.Duration) []services.Scheduled {
	var out []services.Scheduled
	for _, tm := range timers {
		if tm.D == d {
			out = append(out, tm)
		}
	}
	return out
}

type panelLoads struct {
	details []loaders.DetailMsg
	panels  []loaders.PanelMetricsMsg
}

func panelOf(other []tea.Msg) panelLoads {
	var p panelLoads
	for _, m := range other {
		switch m := m.(type) {
		case loaders.DetailMsg:
			p.details = append(p.details, m)
		case loaders.PanelMetricsMsg:
			p.panels = append(p.panels, m)
		}
	}
	return p
}

// rested inits s, delivers the first reads and the rest timer, and returns
// the panel's poll timer.
func (r *rig) rested(s tui.Screen) (tui.Screen, services.Scheduled) {
	r.t.Helper()
	loads, timers, _ := run(s.Init(r.c))
	for _, m := range loads {
		s, _ = r.deliver(s, m)
	}
	rest := scheduled(timers, loaders.DetailDelay)
	if len(rest) != 1 {
		r.t.Fatalf("Init: rest timers %v", timers)
	}
	s, cmd := r.deliver(s, rest[0].Msg)
	_, tms, other := run(cmd)
	p := panelOf(other)
	if len(p.details) != 1 || len(p.panels) != 1 || p.details[0].Place != apiPlace || p.panels[0].Step != 60 {
		r.t.Fatalf("rest: %#v", p)
	}
	poll := scheduled(tms, loaders.MetricsEvery)
	if len(poll) != 1 {
		r.t.Fatalf("rest: timers %v", tms)
	}
	return s, poll[0]
}

func TestPanelDetailDebounce(t *testing.T) {
	r := panelRig(t)
	s := services.New()
	loads, timers, _ := run(s.Init(r.c))
	for _, m := range loads {
		s, _ = r.deliver(s, m)
	}
	first := scheduled(timers, loaders.DetailDelay)
	if len(first) != 1 {
		t.Fatalf("Init should schedule one detail read: %v", timers)
	}
	// the cursor moves on before it rests
	s, cmd := s.Update(r.c, tea.KeyMsg{Type: tea.KeyDown})
	_, tms, other := run(cmd)
	second := scheduled(tms, loaders.DetailDelay)
	if len(second) != 1 || len(panelOf(other).details) != 0 {
		t.Fatalf("down: timers %v, loads %v", tms, other)
	}
	// the first timer is stale
	s, cmd = r.deliver(s, first[0].Msg)
	if _, tms, other := run(cmd); len(tms) != 0 || len(panelOf(other).details)+len(panelOf(other).panels) != 0 {
		t.Errorf("stale rest read: timers %v other %v", tms, other)
	}
	// the second reads the cron's detail and its 1h series, and polls
	_, cmd = r.deliver(s, second[0].Msg)
	_, tms, other = run(cmd)
	p := panelOf(other)
	if len(p.details) != 1 || p.details[0].Place != cronPlace || len(p.panels) != 1 || p.panels[0].Place != cronPlace || p.panels[0].Step != 60 {
		t.Errorf("rest: %#v", p)
	}
	if len(scheduled(tms, loaders.MetricsEvery)) != 1 {
		t.Errorf("rest should start the panel poll: %v", tms)
	}
	s.(tui.Suspender).Suspend(r.c)
}

func TestPanelLevelKeys(t *testing.T) {
	r := panelRig(t)
	s, _ := r.rested(services.New())
	press := func(k string) []loaders.PanelMetricsMsg {
		var cmd tea.Cmd
		s, cmd = s.Update(r.c, r1(k))
		_, _, other := run(cmd)
		return panelOf(other).panels
	}
	cells := services.ChartCells(r.c.Width - tui.ChromeCols)
	if p := press("]"); len(p) != 1 || p[0].Step != 300 || p[0].Span != loaders.PanelSpan(300, cells) || p[0].Place != apiPlace {
		t.Errorf("] = %#v", p)
	}
	if !strings.HasSuffix(strings.TrimRight(panelTitle(ansi.Strip(body(r.c, s))), " "), " 5m") {
		t.Error("the panel title should name the level")
	}
	if p := press("["); len(p) != 1 || p[0].Step != 60 {
		t.Errorf("[ = %#v", p)
	}
	if p := press("["); len(p) != 0 {
		t.Errorf("[ at 1m = %#v", p)
	}
	for _, st := range []int{300, 900, 3600} {
		if p := press("]"); len(p) != 1 || p[0].Step != st || p[0].Span != loaders.PanelSpan(st, cells) {
			t.Errorf("] = %#v, want step %d", p, st)
		}
	}
	if p := press("]"); len(p) != 0 {
		t.Errorf("] at 1h = %#v", p)
	}
	s.(tui.Suspender).Suspend(r.c)
}

func TestPanelMetricsStaleDropped(t *testing.T) {
	r := panelRig(t)
	s, poll := r.rested(services.New())

	// the current poll reads the window again and re-arms
	s, cmd := r.deliver(s, poll.Msg)
	_, tms, other := run(cmd)
	if p := panelOf(other).panels; len(p) != 1 || p[0].Step != 60 || len(scheduled(tms, loaders.MetricsEvery)) != 1 {
		t.Fatalf("poll: %#v %v", p, tms)
	}
	poll = scheduled(tms, loaders.MetricsEvery)[0]

	// a window change starts a new poll; the old one goes stale
	s, cmd = s.Update(r.c, r1("]"))
	_, tms, _ = run(cmd)
	next := scheduled(tms, loaders.MetricsEvery)
	if len(next) != 1 {
		t.Fatalf("] timers %v", tms)
	}
	s, cmd = r.deliver(s, poll.Msg)
	if _, tms, other := run(cmd); len(tms) != 0 || len(panelOf(other).panels) != 0 {
		t.Errorf("stale poll: %v %v", tms, other)
	}
	s, cmd = r.deliver(s, next[0].Msg)
	if _, _, other := run(cmd); len(panelOf(other).panels) != 1 || panelOf(other).panels[0].Step != 300 {
		t.Errorf("current poll: %v", other)
	}

	// moving to another service stops the api's poll
	s, cmd = s.Update(r.c, tea.KeyMsg{Type: tea.KeyDown})
	_, tms, _ = run(cmd)
	if len(scheduled(tms, loaders.DetailDelay)) != 1 {
		t.Fatalf("down: %v", tms)
	}
	s, cmd = r.deliver(s, next[0].Msg)
	if _, tms, other := run(cmd); len(tms) != 0 || len(panelOf(other).panels) != 0 {
		t.Errorf("poll for another place: %v %v", tms, other)
	}
	s.(tui.Suspender).Suspend(r.c)

	// a result for another level does not show in this one
	c := panelCtx(150, panelHeight)
	other6h := []railway.MetricSeries{{EnvID: "e1", ServiceID: "s1", Measurement: "CPU_USAGE",
		Points: []railway.MetricPoint{{TS: now.Unix(), Value: 9.87}}}}
	loaders.PanelMetricsMsg{Place: apiPlace, Step: 300, Span: services.PanelSpanAt(150-tui.ChromeCols, 300), Series: other6h, At: now}.ApplyTo(c.Store)
	if out := ansi.Strip(body(c, services.New())); strings.Contains(out, "9.87") {
		t.Errorf("a 5m result shows in the 1m panel:\n%s", out)
	}
}

// typeIn replaces an input's text and submits it.
func typeIn(t *testing.T, c *tui.Ctx, s tui.Screen, text string) (tui.Screen, []tea.Msg) {
	t.Helper()
	s, _ = s.Update(c, tea.KeyMsg{Type: tea.KeyCtrlU})
	if text != "" {
		s, _ = s.Update(c, r(text))
	}
	s, cmd := s.Update(c, tea.KeyMsg{Type: tea.KeyEnter})
	return settle(t, c, s, cmd)
}

func limitsNew(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLimitsKeyPutsChange(t *testing.T) {
	c := panelCtx(80, panelHeight)
	s := services.New()
	s, _ = s.Update(c, r("l"))
	if !s.(tui.Capturer).Capturing() {
		t.Fatal("l should open an input")
	}
	out := ansi.Strip(body(c, s))
	if !strings.Contains(out, "cpu (vCPU): 32") {
		t.Errorf("cpu input prefilled with the effective limit:\n%s", out)
	}
	s, msgs := typeIn(t, c, s, "16")
	if len(msgs) != 0 || !strings.Contains(ansi.Strip(body(c, s)), "memory (GB): 32.0") {
		t.Fatalf("memory input: msgs %v\n%s", msgs, ansi.Strip(body(c, s)))
	}
	s, msgs = typeIn(t, c, s, "16")
	if len(msgs) != 1 {
		t.Fatalf("msgs = %#v", msgs)
	}
	pm, ok := msgs[0].(tui.PutChangeMsg)
	if !ok || pm.Change.Kind != "limits.set" || pm.Change.Place != apiPlace {
		t.Fatalf("msg = %#v", msgs[0])
	}
	if n := limitsNew(t, pm.Change.New); n["cpu"] != 16.0 || n["memoryBytes"] != float64(16<<30) {
		t.Errorf("new = %v", n)
	}
	if s.(tui.Capturer).Capturing() {
		t.Error("the input should close")
	}

	// a bad number flashes and puts nothing
	s, _ = s.Update(c, r("l"))
	s, _ = typeIn(t, c, s, "lots")
	_, msgs = typeIn(t, c, s, "1")
	if len(msgs) != 1 {
		t.Fatalf("msgs = %#v", msgs)
	}
	if f, ok := msgs[0].(tui.FlashMsg); !ok || f.Kind != '✗' || !strings.Contains(f.Text, "cpu") {
		t.Errorf("flash = %#v", msgs[0])
	}

	// esc puts nothing
	s, _ = s.Update(c, r("l"))
	s, cmd := s.Update(c, tea.KeyMsg{Type: tea.KeyEsc})
	s, msgs = settle(t, c, s, cmd)
	if len(msgs) != 0 || s.(tui.Capturer).Capturing() {
		t.Errorf("esc: %v", msgs)
	}
}

// enter submits an input as it stands.
func enter(t *testing.T, c *tui.Ctx, s tui.Screen) (tui.Screen, []tea.Msg) {
	t.Helper()
	s, cmd := s.Update(c, tea.KeyMsg{Type: tea.KeyEnter})
	return settle(t, c, s, cmd)
}

// only is the one message a step put out.
func only(t *testing.T, msgs []tea.Msg) tea.Msg {
	t.Helper()
	if len(msgs) != 1 {
		t.Fatalf("msgs = %#v, want one", msgs)
	}
	return msgs[0]
}

// An answer left as prefilled keeps the exact value, not its rounded text.
func TestLimitsUntouchedAnswerKeepsExactValue(t *testing.T) {
	// the override: 8.04 GB (1024³) shows as 8.0
	c := panelCtx(80, panelHeight)
	cpu, mem := 4.0, int64(8_632_884_511)
	c.Store.SetConfig("e1", railway.EnvConfig{
		Services: map[string]railway.ServiceConfig{"s1": {CPU: &cpu, MemoryBytes: &mem}},
		Volumes:  map[string]railway.VolumeConfig{"vol-1": {SizeMB: 5000}},
	})
	s := services.New()
	s, _ = s.Update(c, r("l"))
	s, _ = typeIn(t, c, s, "2")
	if out := ansi.Strip(body(c, s)); !strings.Contains(out, "memory (GB): 8.0") {
		t.Fatalf("memory prefill:\n%s", out)
	}
	_, msgs := enter(t, c, s)
	pm, ok := only(t, msgs).(tui.PutChangeMsg)
	if !ok {
		t.Fatalf("msg = %#v", msgs[0])
	}
	if n := limitsNew(t, pm.Change.New); n["cpu"] != 2.0 || n["memoryBytes"] != float64(8_632_884_511) {
		t.Errorf("override: new = %v", n)
	}

	// no override: Railway's 32 GB limit shows as 32.0
	c = panelCtx(80, panelHeight)
	s = services.New()
	s, _ = s.Update(c, r("l"))
	s, _ = typeIn(t, c, s, "16")
	_, msgs = enter(t, c, s)
	pm, ok = only(t, msgs).(tui.PutChangeMsg)
	if !ok {
		t.Fatalf("msg = %#v", msgs[0])
	}
	if n := limitsNew(t, pm.Change.New); n["cpu"] != 16.0 || n["memoryBytes"] != float64(32<<30) {
		t.Errorf("reported limit: new = %v", n)
	}

	// cpu untouched, memory changed: the cpu keeps its value
	c = panelCtx(80, panelHeight)
	s = services.New()
	s, _ = s.Update(c, r("l"))
	s, _ = enter(t, c, s)
	_, msgs = typeIn(t, c, s, "16")
	pm, ok = only(t, msgs).(tui.PutChangeMsg)
	if !ok {
		t.Fatalf("msg = %#v", msgs[0])
	}
	if n := limitsNew(t, pm.Change.New); n["cpu"] != 32.0 || n["memoryBytes"] != float64(16<<30) {
		t.Errorf("cpu untouched: new = %v", n)
	}
}

// A 5050 MB volume prefills a rounded size; left as is it is no shrink.
func TestResizeUntouchedIsUnchanged(t *testing.T) {
	c := panelCtx(80, panelHeight)
	c.Store.ReplaceVolumes([]string{"e1"}, []railway.VolumeMount{
		{EnvID: "e1", ServiceID: "s1", VolumeID: "vol-1", MountPath: "/data", SizeMB: 5050, UsedMB: 1200},
	})
	c.Store.SetConfig("e1", railway.EnvConfig{
		Services: map[string]railway.ServiceConfig{"s1": {}},
		Volumes:  map[string]railway.VolumeConfig{"vol-1": {SizeMB: 5050}},
	})
	s := services.New()
	s, _ = s.Update(c, r("z"))
	s, msgs := enter(t, c, s)
	if f, ok := only(t, msgs).(tui.FlashMsg); !ok || f.Kind != '!' || f.Text != "size unchanged" {
		t.Errorf("flash = %#v", msgs[0])
	}
	if s.(tui.Capturer).Capturing() {
		t.Error("the input should close")
	}
}

// Without the environment settings l says so at once, before any prompt.
func TestLimitsKeyNeedsConfig(t *testing.T) {
	c := monCtx(80, panelHeight) // no environment config read
	s, cmd := services.New().Update(c, r("l"))
	if s.(tui.Capturer).Capturing() {
		t.Error("no input should open")
	}
	_, msgs := settle(t, c, s, cmd)
	if f, ok := only(t, msgs).(tui.FlashMsg); !ok || f.Kind != '!' || !strings.Contains(f.Text, "environment settings not loaded yet") {
		t.Errorf("flash = %#v", msgs[0])
	}
}

func TestLimitsClearWithEmptyInputs(t *testing.T) {
	c := panelCtx(80, panelHeight)
	cpu, mem := 4.0, int64(8<<30)
	c.Store.SetConfig("e1", railway.EnvConfig{
		Services: map[string]railway.ServiceConfig{"s1": {CPU: &cpu, MemoryBytes: &mem}},
		Volumes:  map[string]railway.VolumeConfig{"vol-1": {SizeMB: 5000}},
	})
	s := services.New()
	s, _ = s.Update(c, r("l"))
	if out := ansi.Strip(body(c, s)); !strings.Contains(out, "cpu (vCPU): 4") {
		t.Errorf("prefill is the override:\n%s", out)
	}
	s, _ = typeIn(t, c, s, "")
	if out := ansi.Strip(body(c, s)); !strings.Contains(out, "memory (GB): 8.0") {
		t.Errorf("memory prefill is the override:\n%s", out)
	}
	_, msgs := typeIn(t, c, s, "")
	if len(msgs) != 1 {
		t.Fatalf("msgs = %#v", msgs)
	}
	pm, ok := msgs[0].(tui.PutChangeMsg)
	if !ok || string(pm.Change.New) != `{"cpu":null,"memoryBytes":null}` {
		t.Errorf("msg = %#v", msgs[0])
	}
}

func TestResizeKeyRefusesShrink(t *testing.T) {
	c := panelCtx(80, panelHeight)
	s := services.New()
	s, _ = s.Update(c, r("z"))
	if !s.(tui.Capturer).Capturing() || !strings.Contains(ansi.Strip(body(c, s)), "size (GB): 5.0") {
		t.Fatalf("z should open the size input:\n%s", ansi.Strip(body(c, s)))
	}
	s, msgs := typeIn(t, c, s, "3")
	if len(msgs) != 1 {
		t.Fatalf("msgs = %#v", msgs)
	}
	if f, ok := msgs[0].(tui.FlashMsg); !ok || f.Kind != '✗' || f.Text != "a volume can only grow" {
		t.Errorf("flash = %#v", msgs[0])
	}
	s, _ = s.Update(c, r("z"))
	_, msgs = typeIn(t, c, s, "10")
	pm, ok := only(t, msgs).(tui.PutChangeMsg)
	if !ok || pm.Change.Kind != "volume.size" || string(pm.Change.New) != `{"volumeId":"vol-1","sizeMB":10000}` {
		t.Errorf("msg = %#v", msgs[0])
	}
}

func TestResizeKeyVolumeCount(t *testing.T) {
	c := panelCtx(80, panelHeight)
	s := down(t, c, services.New(), 1) // cron: no volume
	_, cmd := s.Update(c, r("z"))
	if f, ok := cmd().(tui.FlashMsg); !ok || f.Kind != '!' || f.Text != "no volume on this service" {
		t.Errorf("no volume: %#v", f)
	}
	c.Store.ReplaceVolumes([]string{"e1"}, []railway.VolumeMount{
		{EnvID: "e1", ServiceID: "s1", VolumeID: "vol-1", MountPath: "/data", SizeMB: 5000},
		{EnvID: "e1", ServiceID: "s1", VolumeID: "vol-2", MountPath: "/logs", SizeMB: 5000},
	})
	_, cmd = services.New().Update(c, r("z"))
	if f, ok := cmd().(tui.FlashMsg); !ok || f.Kind != '!' || f.Text != "several volumes · not supported yet" {
		t.Errorf("several: %#v", f)
	}
}

func TestPanelOpenKeys(t *testing.T) {
	c := panelCtx(80, panelHeight)
	for _, k := range []struct{ key, screen string }{{"m", "domains"}, {"t", "logs"}} {
		_, cmd := services.New().Update(c, r(k.key))
		om, ok := cmd().(tui.OpenMsg)
		if !ok || om.Name != k.screen || len(om.Args) != 1 || om.Args[0] != apiPlace {
			t.Errorf("%s: %#v", k.key, om)
		}
	}
}

func TestPanelGoldens(t *testing.T) {
	for _, w := range []int{80, 60} {
		c := pendingCtx(t, w)
		out := renderMon(c, services.New())
		tuitest.NoSecrets(t, out, secret)
		tuitest.Golden(t, "services_panel_"+itoa(w)+".golden", out)
	}
	// the worker: no metrics, no volume, no deployment, no detail
	c := panelCtx(60, panelHeight)
	s := down(t, c, services.New(), 3)
	out := renderMon(c, s)
	tuitest.NoSecrets(t, out, secret)
	if l := lineWith(out, "usage"); !strings.Contains(l, "—") {
		t.Errorf("usage without data: %q", l)
	}
	if !strings.Contains(out, "no volume") {
		t.Errorf("no volume:\n%s", out)
	}
	tuitest.Golden(t, "services_panel_empty_60.golden", out)
}

// A repo segment must start with a letter or digit: a dot path such as
// ../.. would make the link leave github.com/<owner>/<repo>.
func TestPanelRepoLinkNeedsAlnumSegments(t *testing.T) {
	for _, tc := range []struct {
		repo string
		link bool
	}{
		{"acme/shop", true},
		{"acme/shop.go", true},
		{"a-1/_x", false},
		{"../..", false},
		{"./x", false},
		{"acme/..", false},
		{"acme/.", false},
		{".acme/x", false},
		{"-acme/x", false},
	} {
		c := panelCtx(150, panelHeight)
		c.Theme = tui.NewTheme(termenv.TrueColor)
		d := apiDetail()
		d.Repo = tc.repo
		c.Store.SetDetail("e1", "s1", d)
		out := body(c, services.New())
		if got := strings.Contains(out, "\x1b]8;;https://github.com/"); got != tc.link {
			t.Errorf("%q: link = %v, want %v", tc.repo, got, tc.link)
		}
	}
}

// A pending disk row short of room drops the mount path, then the net cell,
// then the current value; the pending part always shows whole.
func TestPanelDiskRowDropOrder(t *testing.T) {
	stage := func(l string) int {
		switch {
		case strings.Contains(l, "/data") && strings.Contains(l, "↓"):
			return 0
		case strings.Contains(l, "↓"):
			return 1
		case strings.Contains(l, "1.2 GB /"):
			return 2
		}
		return 3
	}
	seen, prev := map[int]bool{}, 0
	for w := 150; w >= 50; w-- {
		c := pendingCtx(t, w)
		out := ansi.Strip(body(c, services.New()))
		l := lineWith(out, "can't be undone")
		if !strings.Contains(l, "5.0 GB → 10.0 GB ● can't be undone") {
			t.Fatalf("width %d: pending part cut: %q", w, l)
		}
		if n := ansi.StringWidth(l); n > w-tui.ChromeCols {
			t.Errorf("width %d: row is %d wide", w, n)
		}
		st := stage(l)
		if st < prev {
			t.Errorf("width %d: stage %d after %d: %q", w, st, prev, l)
		}
		if w == 80 && st != 2 {
			t.Errorf("width 80: %q", l)
		}
		seen[st], prev = true, st
	}
	for st := range 4 {
		if !seen[st] {
			t.Errorf("stage %d never shown", st)
		}
	}
}

// [ and ] while the panel has no room change nothing and say why, once per
// press.
func TestPanelLevelKeysWhileHidden(t *testing.T) {
	c := panelCtx(80, 19+tui.ChromeRows)
	s := services.New()
	for _, k := range []string{"]", "]", "["} {
		var cmd tea.Cmd
		s, cmd = s.Update(c, r1(k))
		_, _, other := run(cmd)
		if len(other) != 1 {
			t.Fatalf("%s: messages %#v", k, other)
		}
		if f, ok := other[0].(tui.FlashMsg); !ok || f.Kind != '!' || f.Text != "panel hidden · make the window taller" {
			t.Errorf("%s: %#v", k, other[0])
		}
	}
	c.Height = panelHeight
	if title := strings.TrimRight(panelTitle(ansi.Strip(body(c, s))), " "); !strings.HasSuffix(title, " 1m") {
		t.Errorf("the level changed while hidden: %q", title)
	}
}

// Without the environment settings z says so at once, before any prompt.
func TestResizeKeyNeedsConfig(t *testing.T) {
	c := monCtx(80, panelHeight) // no environment config read
	c.Store.ReplaceVolumes([]string{"e1"}, []railway.VolumeMount{
		{EnvID: "e1", ServiceID: "s1", VolumeID: "vol-1", MountPath: "/data", SizeMB: 5000},
	})
	s, cmd := services.New().Update(c, r("z"))
	if s.(tui.Capturer).Capturing() {
		t.Error("no input should open")
	}
	_, msgs := settle(t, c, s, cmd)
	if f, ok := only(t, msgs).(tui.FlashMsg); !ok || f.Kind != '!' || f.Text != "environment settings not loaded yet · r refresh" {
		t.Errorf("flash = %#v", msgs[0])
	}
}

// A filter that leaves few rows keeps the panel at the bottom of the body:
// the table pads its window with blank lines instead of the panel rising.
func TestPanelStaysAtBottomUnderFilter(t *testing.T) {
	c := panelCtx(150, 40)
	var s tui.Screen = services.New()
	s, _ = s.Update(c, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	s, _ = s.Update(c, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("api")})
	out := ansi.Strip(body(c, s))
	if strings.Contains(out, "worker") {
		t.Fatalf("not filtered:\n%s", out)
	}
	lines := strings.Split(out, "\n")
	if h := c.Height - tui.ChromeRows; len(lines) != h {
		t.Fatalf("body is %d lines, want %d:\n%s", len(lines), h, out)
	}
	sep := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "───") {
			sep = i
		}
	}
	if want := len(lines) - 16; sep != want {
		t.Errorf("separator at line %d, want %d (panel at the bottom):\n%s", sep, want, out)
	}
}
