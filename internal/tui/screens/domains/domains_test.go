package domains_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/railway/railwaytest"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
	"github.com/men232/railway-manager/internal/tui/screens/domains"
	"github.com/men232/railway-manager/internal/tui/tuitest"
	"github.com/men232/railway-manager/internal/view"
)

func TestMain(m *testing.M) {
	tuitest.Init()
	os.Exit(m.Run())
}

// secret is a variable value in the store: no frame may show it.
const secret = "fixture-value-domains"

var (
	now   = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	place = domain.Place{ProjectID: "p1", EnvID: "e1", ServiceID: "s1"}
)

var catalog = railway.Catalog{Projects: []railway.Project{{
	ID: "p1", Name: "shop", Workspace: "ws",
	Environments: []railway.Environment{{ID: "e1", Name: "production", CanAccess: true, ServiceIDs: []string{"s1"}}},
	Services:     []railway.Service{{ID: "s1", Name: "api"}},
}}}

// fixture: one Railway domain, a verified custom domain with a valid
// certificate, and one still waiting for DNS on the default port.
var fixtureDomains = railway.Domains{
	Service: []railway.ServiceDomain{{ID: "sd-1", Domain: "api-production.up.railway.app", Port: 8080}},
	Custom: []railway.CustomDomain{
		{ID: "cd-1", Domain: "shop.example.com", Port: 8080, Verified: true, CertStatus: "CERTIFICATE_STATUS_TYPE_VALID"},
		{ID: "cd-2", Domain: "www.example.com", CertStatus: "CERTIFICATE_STATUS_TYPE_ISSUING"},
	},
}

func ctx(w, h int) *tui.Ctx {
	st := store.New()
	st.ReplaceCatalog(catalog)
	st.ReplaceVariables("p1", []railway.VarRecord{
		{EnvID: "e1", ServiceID: "s1", Name: "API_KEY", Unrendered: secret, Rendered: secret},
	})
	st.SetDomains(place, fixtureDomains)
	return &tui.Ctx{
		Store: st, CS: &changeset.ChangeSet{},
		Views: &view.Views{Store: st, Reg: kinds.NewRegistry()},
		Theme: tui.NewTheme(tuitest.Profile()), Icons: tui.Icons{},
		Width: w, Height: h, IsTTY: true,
		Now: func() time.Time { return now },
	}
}

func r(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

var (
	enter = tea.KeyMsg{Type: tea.KeyEnter}
	esc   = tea.KeyMsg{Type: tea.KeyEsc}
	down  = tea.KeyMsg{Type: tea.KeyDown}
	up    = tea.KeyMsg{Type: tea.KeyUp}
)

// settle runs cmd and feeds component messages back into the screen,
// returning the intents for the root.
func settle(c *tui.Ctx, s tui.Screen, cmd tea.Cmd) (tui.Screen, []tea.Msg) {
	var out []tea.Msg
	var run func(tea.Cmd)
	run = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		switch m := cmd().(type) {
		case tea.BatchMsg:
			for _, c := range m {
				run(c)
			}
		case components.SubmitMsg, components.CancelMsg:
			var next tea.Cmd
			s, next = s.Update(c, m)
			run(next)
		case nil:
		default:
			out = append(out, m)
		}
	}
	run(cmd)
	return s, out
}

// press sends keys one by one and collects every intent.
func press(c *tui.Ctx, s tui.Screen, keys ...tea.KeyMsg) (tui.Screen, []tea.Msg) {
	var all []tea.Msg
	for _, k := range keys {
		var cmd tea.Cmd
		var ms []tea.Msg
		s, cmd = s.Update(c, k)
		s, ms = settle(c, s, cmd)
		all = append(all, ms...)
	}
	return s, all
}

// typed is the keys that type str.
func typed(str string) []tea.KeyMsg {
	var out []tea.KeyMsg
	for _, ch := range str {
		out = append(out, r(string(ch)))
	}
	return out
}

func keys(groups ...[]tea.KeyMsg) []tea.KeyMsg {
	var out []tea.KeyMsg
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func one(ks ...tea.KeyMsg) []tea.KeyMsg { return ks }

func body(c *tui.Ctx, s tui.Screen) string {
	return ansi.Strip(s.View(c, c.Width-tui.ChromeCols, c.Height-tui.ChromeRows))
}

func frame(c *tui.Ctx, s tui.Screen) string {
	return tuitest.Plain(tui.Frame(c, s.Title(), "", s.View(c, c.Width-tui.ChromeCols, c.Height-tui.ChromeRows), tui.Footer{Hints: s.Keys()}))
}

func flashOf(t *testing.T, ms []tea.Msg) tui.FlashMsg {
	t.Helper()
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	f, ok := ms[0].(tui.FlashMsg)
	if !ok {
		t.Fatalf("msg = %#v", ms[0])
	}
	return f
}

func putOf(t *testing.T, ms []tea.Msg) changeset.Change {
	t.Helper()
	if len(ms) != 1 {
		t.Fatalf("msgs = %#v", ms)
	}
	p, ok := ms[0].(tui.PutChangeMsg)
	if !ok {
		t.Fatalf("msg = %#v", ms[0])
	}
	return p.Change
}

func port(t *testing.T, c changeset.Change) int {
	t.Helper()
	var n struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(c.New, &n); err != nil {
		t.Fatal(err)
	}
	return n.Port
}

// must is the change of a constructor that the fixture makes valid.
func must(ch changeset.Change, err error) changeset.Change {
	if err != nil {
		panic(err)
	}
	return ch
}

func put(t *testing.T, c *tui.Ctx, ch changeset.Change) changeset.Change {
	t.Helper()
	if err := c.CS.Put(ch, nil); err != nil {
		t.Fatal(err)
	}
	return ch
}

func newScreen(c *tui.Ctx) tui.Screen {
	s := domains.Build(place)
	s.Init(c)
	return s
}

func TestBuildArgs(t *testing.T) {
	if domains.Build(place) == nil {
		t.Fatal("Build(place) = nil")
	}
	for _, args := range [][]any{nil, {"p1"}, {place, place}} {
		if domains.Build(args...) != nil {
			t.Errorf("Build(%v) should be nil", args)
		}
	}
}

func TestDomainsListsRows(t *testing.T) {
	c := ctx(100, 20)
	out := body(c, newScreen(c))
	for _, want := range [][]string{
		{"api-production.up.railway.app", ":8080", "railway"},
		{"shop.example.com", ":8080", "custom", "verified", "cert valid"},
		{"www.example.com", "default", "custom", "waiting for DNS", "cert issuing"},
	} {
		line := lineWith(out, want[0])
		for _, w := range want[1:] {
			if !strings.Contains(line, w) {
				t.Errorf("row %q lacks %q: %q", want[0], w, line)
			}
		}
	}
	if !strings.Contains(out, "production · api") {
		t.Errorf("no header:\n%s", out)
	}
}

func lineWith(out, sub string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}

func TestDomainsAddCustom(t *testing.T) {
	c := ctx(80, 20)
	s := newScreen(c)
	s, ms := press(c, s, r("a"))
	if len(ms) != 0 || !s.(tui.Capturer).Capturing() {
		t.Fatalf("a: msgs %v, capturing %v", ms, s.(tui.Capturer).Capturing())
	}
	// x, g and q are typed into the field, not taken as keys
	s, ms = press(c, s, keys(typed("Shop2.Example.com"), one(enter))...)
	if len(ms) != 0 || !strings.Contains(body(c, s), "port") {
		t.Fatalf("host: msgs %v\n%s", ms, body(c, s))
	}
	s, ms = press(c, s, keys(typed("3000"), one(enter))...)
	ch := putOf(t, ms)
	if ch.Kind != "domain.custom.add" || ch.Target != "custom-domain:shop2.example.com" || ch.Place != place || port(t, ch) != 3000 {
		t.Errorf("change = %+v (port %d)", ch, port(t, ch))
	}
	if s.(tui.Capturer).Capturing() {
		t.Error("still capturing after the port")
	}

	// empty port: Railway's default
	_, ms = press(c, s, keys(one(r("a")), typed("new.example.com"), one(enter, enter))...)
	if ch := putOf(t, ms); port(t, ch) != 0 {
		t.Errorf("empty port = %d", port(t, ch))
	}
	// a bad port flashes and puts nothing
	_, ms = press(c, s, keys(one(r("a")), typed("new.example.com"), one(enter), typed("99999"), one(enter))...)
	if f := flashOf(t, ms); f.Kind != '✗' || !strings.Contains(f.Text, "port") {
		t.Errorf("bad port flash = %+v", f)
	}
	// esc cancels
	s, ms = press(c, s, r("a"), r("z"), esc)
	if len(ms) != 0 || s.(tui.Capturer).Capturing() {
		t.Errorf("esc: msgs %v, capturing %v", ms, s.(tui.Capturer).Capturing())
	}
}

func TestDomainsAddInvalidHostFlashes(t *testing.T) {
	c := ctx(80, 20)
	s := newScreen(c)
	s, ms := press(c, s, keys(one(r("a")), typed("bad host!"), one(enter))...)
	f := flashOf(t, ms)
	if f.Kind != '✗' || f.Text != "not a valid host name" {
		t.Errorf("flash = %+v", f)
	}
	if s.(tui.Capturer).Capturing() || strings.Contains(body(c, s), "port:") {
		t.Errorf("the port should not be asked after a bad host")
	}
	// a host the service already has: the kind's error too
	_, ms = press(c, s, keys(one(r("a")), typed("Shop.example.com"), one(enter))...)
	if f := flashOf(t, ms); f.Kind != '✗' || f.Text != "this service already has that domain" {
		t.Errorf("flash = %+v", f)
	}
}

func TestDomainsAddRailway(t *testing.T) {
	c := ctx(80, 20)
	s := newScreen(c)
	s, ms := press(c, s, r("g"), enter)
	if ch := putOf(t, ms); ch.Kind != "domain.service.add" || ch.Place != place || port(t, ch) != 0 {
		t.Errorf("default port change = %+v", ch)
	}
	s, ms = press(c, s, keys(one(r("g")), typed("8080"), one(enter))...)
	if ch := putOf(t, ms); port(t, ch) != 8080 {
		t.Errorf("port = %d", port(t, ch))
	}
	_, ms = press(c, s, keys(one(r("g")), typed("http"), one(enter))...)
	if f := flashOf(t, ms); f.Kind != '✗' || !strings.Contains(f.Text, "port") {
		t.Errorf("flash = %+v", f)
	}
}

// cursorTo moves the cursor to the row holding sub: the rows follow the
// header line (no filter, no scrolling in these tests).
func cursorTo(t *testing.T, c *tui.Ctx, s tui.Screen, sub string) tui.Screen {
	t.Helper()
	lines := strings.Split(body(c, s), "\n")
	for i, l := range lines {
		if i > 0 && strings.Contains(l, sub) {
			for range len(lines) {
				s, _ = press(c, s, up)
			}
			for range i - 1 {
				s, _ = press(c, s, down)
			}
			return s
		}
	}
	t.Fatalf("no row %q:\n%s", sub, body(c, s))
	return s
}

func TestDomainsDeletePendingAddRemoves(t *testing.T) {
	c := ctx(80, 20)
	add := put(t, c, must(kinds.AddCustomDomain(c.Store, place, "new.example.com", 3000, now)))
	sadd := put(t, c, kinds.AddServiceDomain(place, 0, now))
	s := newScreen(c)

	s = cursorTo(t, c, s, "new.example.com")
	s, ms := press(c, s, r("x"))
	if len(ms) != 1 || ms[0] != (tui.RemoveChangeMsg{ID: add.ID}) {
		t.Errorf("x on a pending custom add = %#v", ms)
	}
	s = cursorTo(t, c, s, "railway domain")
	_, ms = press(c, s, r("x"))
	if len(ms) != 1 || ms[0] != (tui.RemoveChangeMsg{ID: sadd.ID}) {
		t.Errorf("x on a pending railway add = %#v", ms)
	}
}

func TestDomainsDeleteLive(t *testing.T) {
	c := ctx(80, 20)
	s := newScreen(c)
	s = cursorTo(t, c, s, "api-production")
	s, ms := press(c, s, r("x"))
	if ch := putOf(t, ms); ch.Kind != "domain.service.delete" || ch.Target != "service-domain:sd-1" {
		t.Errorf("service delete = %+v", ch)
	}
	s = cursorTo(t, c, s, "www.example.com")
	_, ms = press(c, s, r("x"))
	del := putOf(t, ms)
	if del.Kind != "domain.custom.delete" || del.Target != "custom-domain:www.example.com" {
		t.Errorf("custom delete = %+v", del)
	}
	// the delete shows struck through with the pending mark; x again drops it
	if err := c.CS.Put(del, nil); err != nil {
		t.Fatal(err)
	}
	if l := lineWith(body(c, s), "www.example.com"); !strings.Contains(l, "✗ www.example.com ●") {
		t.Errorf("pending delete row = %q", l)
	}
	_, ms = press(c, s, r("x"))
	if len(ms) != 1 || ms[0] != (tui.RemoveChangeMsg{ID: del.ID}) {
		t.Errorf("x on a pending delete = %#v", ms)
	}
}

func TestDomainsInRunRefused(t *testing.T) {
	c := ctx(80, 20)
	add := put(t, c, must(kinds.AddCustomDomain(c.Store, place, "new.example.com", 0, now)))
	c.CS.Changes[0].RunID = "20261024-abc"
	s := cursorTo(t, c, newScreen(c), "new.example.com")
	_, ms := press(c, s, r("x"))
	if f := flashOf(t, ms); f.Kind != '✗' || !strings.Contains(f.Text, "in run 20261024-abc") {
		t.Errorf("flash = %+v (change %s)", f, add.ID)
	}
}

func TestDomainsFilter(t *testing.T) {
	c := ctx(80, 20)
	s := newScreen(c)
	s, _ = press(c, s, keys(one(r("/")), typed("shop"), one(enter))...)
	out := body(c, s)
	if !strings.Contains(out, "shop.example.com") || strings.Contains(out, "www.example.com") || !strings.Contains(out, "1/3") {
		t.Errorf("filtered:\n%s", out)
	}
	if c.Filters["domains:"+place.Key()] != "shop" {
		t.Errorf("filters = %v", c.Filters)
	}
	// x acts on the shown row
	_, ms := press(c, s, r("x"))
	if ch := putOf(t, ms); ch.Target != "custom-domain:shop.example.com" {
		t.Errorf("x under the filter = %+v", ch)
	}
	// an add the filter hides says so
	_, ms = press(c, s, keys(one(r("a")), typed("other.example.com"), one(enter, enter))...)
	if len(ms) != 2 {
		t.Fatalf("msgs = %#v", ms)
	}
	if f, ok := ms[1].(tui.FlashMsg); !ok || f.Text != "added · hidden by the filter" {
		t.Errorf("second msg = %#v", ms[1])
	}
}

func TestDomainsLinksAndPlainIcons(t *testing.T) {
	c := ctx(100, 20)
	put(t, c, must(kinds.AddCustomDomain(c.Store, place, "new.example.com", 0, now)))
	s := newScreen(c)
	s, _ = press(c, s, down) // the links are on the rows not under the cursor
	raw := s.View(c, c.Width-tui.ChromeCols, c.Height-tui.ChromeRows)
	if !strings.Contains(raw, "\x1b]8;;https://api-production.up.railway.app\x1b\\") {
		t.Errorf("no link on the railway domain:\n%q", raw)
	}
	if strings.Contains(raw, "\x1b]8;;https://new.example.com") {
		t.Errorf("a pending add is not in Railway yet: no link")
	}
	for _, ru := range raw {
		if ru >= 0xE000 && ru <= 0xF8FF || ru >= 0xF0000 {
			t.Fatalf("plain icons emit a private use rune %U", ru)
		}
	}
	// a host that fails ValidHost is shown, never linked
	c.Store.SetDomains(place, railway.Domains{Custom: []railway.CustomDomain{{ID: "cd-x", Domain: "evil\x1b]8;;javascript:x"}}})
	raw = s.View(c, c.Width-tui.ChromeCols, c.Height-tui.ChromeRows)
	if strings.Contains(raw, "\x1b]8;;javascript") || strings.Contains(raw, "\x1b]8;;https://evil") {
		t.Errorf("unsafe host linked: %q", raw)
	}
}

func TestDomainsStates(t *testing.T) {
	c := ctx(80, 20)
	c.Store = store.New()
	c.Store.ReplaceCatalog(catalog)
	c.Views = &view.Views{Store: c.Store, Reg: kinds.NewRegistry()}
	s := newScreen(c)
	if out := body(c, s); !strings.Contains(out, "reading domains") {
		t.Errorf("before the read:\n%s", out)
	}
	c.Store.SetDomains(place, railway.Domains{})
	if out := body(c, s); !strings.Contains(out, "no domain here · g adds a Railway domain, a a custom one") {
		t.Errorf("empty:\n%s", out)
	}
}

type staticTokens struct{}

func (staticTokens) Get(context.Context) railway.TokenStatus {
	return railway.TokenStatus{Token: "test-token", Source: "env:RAILWAY_TOKEN"}
}
func (s staticTokens) Refresh(ctx context.Context) railway.TokenStatus { return s.Get(ctx) }
func (s staticTokens) Peek() railway.TokenStatus                       { return s.Get(context.Background()) }

func TestDomainsInitReadsDetail(t *testing.T) {
	f := railwaytest.New(t)
	f.SetDomains("e1", "s1", fixtureDomains)
	c := ctx(80, 20)
	c.API = &railway.Client{T: &railway.Transport{Endpoint: f.URL, Tokens: staticTokens{}}}
	cmd := domains.Build(place).Init(c)
	if cmd == nil {
		t.Fatal("Init read nothing")
	}
	m, ok := cmd().(loaders.DetailMsg)
	if !ok || m.Place != place || len(m.Domains.Custom) != 2 {
		t.Errorf("Init = %#v", m)
	}
}

func TestDomainsGoldens(t *testing.T) {
	for _, w := range []int{80, 60} {
		c := ctx(w, 18)
		c.Store.SetDomains(place, railway.Domains{
			Service: fixtureDomains.Service,
			Custom: append(fixtureDomains.Custom, railway.CustomDomain{
				ID: "cd-3", Domain: "checkout-eu-west.storefront.example.com", Port: 3000,
				CertStatus: "CERTIFICATE_STATUS_TYPE_VALID", Verified: true,
			}),
		})
		put(t, c, must(kinds.AddCustomDomain(c.Store, place, "new.example.com", 3000, now)))
		put(t, c, must(kinds.DeleteCustomDomain(c.Store, place, "www.example.com", now)))
		put(t, c, kinds.AddServiceDomain(place, 0, now))
		out := frame(c, newScreen(c))
		tuitest.NoSecrets(t, out, secret)
		tuitest.Golden(t, "domains_"+map[int]string{80: "80", 60: "60"}[w]+".golden", out)
	}
}

// With no room, the columns go first, then the host is cut: the pending
// mark and the +/✗ stay.
func TestDomainsNarrowKeepsPendingMark(t *testing.T) {
	c := ctx(28, 20)
	put(t, c, must(kinds.AddCustomDomain(c.Store, place, "checkout.storefront.example.com", 0, now)))
	put(t, c, must(kinds.DeleteCustomDomain(c.Store, place, "www.example.com", now)))
	s := newScreen(c)
	out := body(c, s)
	if l := lineWith(out, "+ checkout"); !strings.HasSuffix(l, "… ●") {
		t.Errorf("pending add row = %q", l)
	}
	if l := lineWith(out, "✗ www.example.com"); !strings.Contains(l, "✗ www.example.com ●") || strings.Contains(l, "default") {
		t.Errorf("pending delete row = %q", l)
	}
	for _, l := range strings.Split(out, "\n") {
		if w := ansi.StringWidth(l); w > c.Width-tui.ChromeCols {
			t.Errorf("line %q is %d wide", l, w)
		}
	}
}

// After a run applies its changes the store's domains are stale: returning
// to the screen (Resume) and r both read the detail again.
func TestDomainsResumeAndRReadDetail(t *testing.T) {
	f := railwaytest.New(t)
	f.SetDomains("e1", "s1", fixtureDomains)
	c := ctx(80, 20)
	c.API = &railway.Client{T: &railway.Transport{Endpoint: f.URL, Tokens: staticTokens{}}}
	s := domains.Build(place)
	s.Init(c)
	if sp, ok := s.(tui.Suspender); ok {
		sp.Suspend(c)
	}
	// a run adds a host in Railway while another screen is on top
	f.SetDomains("e1", "s1", railway.Domains{
		Service: fixtureDomains.Service,
		Custom:  append(append([]railway.CustomDomain{}, fixtureDomains.Custom...), railway.CustomDomain{ID: "cd-9", Domain: "added.example.com", Port: 3000}),
	})
	r, ok := s.(tui.Resumer)
	if !ok {
		t.Fatal("domains screen is not a Resumer")
	}
	cmd := r.Resume(c)
	if cmd == nil {
		t.Fatal("Resume read nothing")
	}
	if m, ok := cmd().(loaders.DetailMsg); !ok || m.Place != place || len(m.Domains.Custom) != 3 {
		t.Errorf("Resume = %#v", m)
	}

	_, cmd = s.Update(c, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		t.Fatal("r read nothing")
	}
	if m, ok := cmd().(loaders.DetailMsg); !ok || m.Place != place {
		t.Errorf("r = %#v", m)
	}
	var hint bool
	for _, k := range s.Keys() {
		if k.Key == "r" && k.Label == "refresh" {
			hint = true
		}
	}
	if !hint {
		t.Errorf("Keys = %+v, want r refresh", s.Keys())
	}
}

// A failed read while domains show keeps them and says so in the footer.
func TestDomainsFailedRereadNotice(t *testing.T) {
	c := ctx(80, 20)
	s := newScreen(c)
	if n := s.(tui.Noter).Notice(c); n.Text != "" {
		t.Errorf("notice before a failure = %#v", n)
	}
	loaders.DetailMsg{Place: place, Err: errors.New("boom \x1b[31mred"), At: now}.ApplyTo(c.Store)
	n := s.(tui.Noter).Notice(c)
	if n.Kind != '✗' || !strings.HasPrefix(n.Text, "cannot read domains: boom") || !strings.HasSuffix(n.Text, "red · r retries") {
		t.Errorf("notice = %c %q", n.Kind, n.Text)
	}
	if strings.Contains(n.Text, "\x1b") {
		t.Errorf("notice not sanitized: %q", n.Text)
	}
	if out := body(c, s); !strings.Contains(out, "shop.example.com") {
		t.Errorf("the list should stay:\n%s", out)
	}
	// a good read clears it
	loaders.DetailMsg{Place: place, Domains: fixtureDomains, At: now.Add(time.Minute)}.ApplyTo(c.Store)
	if n := s.(tui.Noter).Notice(c); n.Text != "" {
		t.Errorf("notice after a good read = %#v", n)
	}
}

// many is n custom domains, host-00.example.com on.
func many(n int) railway.Domains {
	var d railway.Domains
	for i := range n {
		d.Custom = append(d.Custom, railway.CustomDomain{ID: fmt.Sprintf("cd-%02d", i), Domain: fmt.Sprintf("host-%02d.example.com", i), Verified: true})
	}
	return d
}

// PgDn moves by the rows that show: the filter line takes one.
func TestDomainsPageSizeCountsFilterLine(t *testing.T) {
	c := ctx(80, 20)
	c.Store.SetDomains(place, many(30))
	s := newScreen(c)
	s, _ = press(c, s, keys(typed("/example"), one(enter))...)
	first := func() string {
		for _, l := range strings.Split(body(c, s), "\n") {
			if strings.Contains(l, "host-") {
				return strings.Fields(l)[0]
			}
		}
		return ""
	}
	if got := first(); got != "host-00.example.com" {
		t.Fatalf("first row = %q\n%s", got, body(c, s))
	}
	s, _ = press(c, s, tea.KeyMsg{Type: tea.KeyPgDown})
	// one page down: the cursor is on the row just past the first page, the
	// last row shown, so the window moved by one
	if got := first(); got != "host-01.example.com" {
		t.Errorf("after PgDn the first row = %q\n%s", got, body(c, s))
	}
}

// Even narrower than the prefix and the mark, no row is wider than the body.
func TestDomainsTinyWidthNeverOverflows(t *testing.T) {
	for w := 1; w <= 8; w++ {
		c := ctx(tui.ChromeCols+w, 20)
		put(t, c, must(kinds.AddCustomDomain(c.Store, place, "new.example.com", 0, now)))
		put(t, c, must(kinds.DeleteCustomDomain(c.Store, place, "www.example.com", now)))
		s := newScreen(c)
		for _, l := range strings.Split(body(c, s), "\n") {
			if n := ansi.StringWidth(l); n > w {
				t.Errorf("width %d: line %q is %d wide", w, l, n)
			}
		}
	}
}

// The keys and the view agree on the rows that show: with no room for a
// row, moving the cursor scrolls nothing, so a taller body afterwards still
// starts at the top.
func TestDomainsKeysAndViewShareListHeight(t *testing.T) {
	c := ctx(80, tui.ChromeRows+1) // the header only
	c.Store.SetDomains(place, many(30))
	s := newScreen(c)
	_ = body(c, s)
	s, _ = press(c, s, r("j"), r("j"), r("j"))
	_ = body(c, s)
	c.Height = 20
	out := body(c, s)
	if !strings.Contains(out, "host-00.example.com") {
		t.Errorf("the taller body does not start at the top:\n%s", out)
	}
}
