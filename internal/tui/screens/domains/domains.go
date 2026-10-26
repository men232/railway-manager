// Package domains is the Domains screen: the Railway and custom domains of
// one service with what the change set does to them. It adds a Railway
// domain (g) or a custom one (a) and deletes the cursor's domain (x), each as
// a change; deleting a domain that is only a pending add drops that change.
// It reads the domains on open, on return (Resume) and on r; a failed read
// while domains show stands in the footer.
package domains

import (
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/domain"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/loaders"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
	"github.com/men232/railway-manager/internal/view"
)

// step is which input is open.
type step int

const (
	stepNone        step = iota
	stepRailwayPort      // g: the port of a Railway domain
	stepHost             // a: the custom host
	stepCustomPort       // a: then its port
)

// chartStep is the panel chart step whose Detail the screen reads (no
// span: the domains do not depend on the charts).
const chartStep = 60

// portError is the kinds' wording for a bad port.
const portError = "port must be 1–65535, or empty for the default"

type screen struct {
	place  domain.Place
	filter *components.Filter
	list   components.List
	step   step
	host   string // the custom host, once given
	field  *components.TextInput
}

// New returns the domains screen of a service.
func New(p domain.Place) tui.Screen {
	return &screen{place: p, filter: components.NewFilter("domains:" + p.Key())}
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

// Init restores the filter and reads the service's detail and domains.
func (s *screen) Init(ctx *tui.Ctx) tea.Cmd {
	s.filter.Load(ctx)
	return s.read(ctx)
}

// Resume reads the domains again: a run may have applied changes while
// another screen was on top, leaving the store's domains stale.
func (s *screen) Resume(ctx *tui.Ctx) tea.Cmd { return s.read(ctx) }

// read is the service's detail and domains, nil without an API.
func (s *screen) read(ctx *tui.Ctx) tea.Cmd {
	if ctx.API == nil {
		return nil
	}
	now := ctx.Now
	if now == nil {
		now = time.Now
	}
	return loaders.Detail(ctx.API, s.place, now)
}

func (s *screen) Title() string { return "Domains" }

// Capturing reports whether an input takes every key.
func (s *screen) Capturing() bool { return s.step != stepNone || s.filter.Editing() }

func (s *screen) Keys() []tui.KeyHint {
	if s.step != stepNone {
		return []tui.KeyHint{
			{Key: "enter", Label: "confirm", Always: true},
			{Key: "esc", Label: "cancel", Always: true},
		}
	}
	if s.filter.Editing() {
		return []tui.KeyHint{
			{Key: "enter", Label: "keep filter", Always: true},
			{Key: "esc", Label: "clear", Always: true},
		}
	}
	return []tui.KeyHint{
		{Key: "g", Label: "railway domain", Rank: 1},
		{Key: "a", Label: "custom domain", Rank: 2},
		{Key: "x", Label: "delete", Rank: 3},
		{Key: "r", Label: "refresh", Rank: 4, Optional: true},
		{Key: "/", Label: "filter", Rank: 5, Optional: true},
		{Key: "esc", Label: "back", Always: true},
	}
}

// Note is empty: the header names the service.
func (s *screen) Note(*tui.Ctx) string { return "" }

// Notice is a failed read while domains show (the list stays): with none,
// the body says it instead.
func (s *screen) Notice(ctx *tui.Ctx) tui.FlashMsg {
	if ctx.Store == nil {
		return tui.FlashMsg{}
	}
	if _, ok := ctx.Store.Domains(s.place); !ok {
		return tui.FlashMsg{}
	}
	e := ctx.Store.Load(store.DetailKey(s.place)).Err
	if e == "" || strings.HasPrefix(e, "! ") {
		return tui.FlashMsg{}
	}
	return tui.FlashMsg{Kind: '✗', Text: "cannot read domains: " + text.Sanitize(e) + " · r retries"}
}

// label is the row's host as shown: a pending Railway domain has none yet.
func label(d view.DomainView) string {
	if d.Host == "" && !d.Custom {
		return "railway domain"
	}
	return d.Host
}

func kindName(d view.DomainView) string {
	if d.Custom {
		return "custom"
	}
	return "railway"
}

// filtered is the rows matching the filter and the count before filtering.
func (s *screen) filtered(ctx *tui.Ctx) ([]view.DomainView, int) {
	if ctx.Views == nil {
		return nil, 0
	}
	s.filter.Load(ctx)
	all := ctx.Views.Detail(ctx.CS, s.place, chartStep, 0).Domains
	if !s.filter.Active() {
		return all, len(all)
	}
	var out []view.DomainView
	for _, d := range all {
		if s.filter.Match(label(d) + " " + kindName(d)) {
			out = append(out, d)
		}
	}
	return out, len(all)
}

func (s *screen) current(ctx *tui.Ctx) (view.DomainView, bool) {
	rows, _ := s.filtered(ctx)
	if len(rows) == 0 {
		return view.DomainView{}, false
	}
	// a filter may have left the cursor past the last shown row
	s.list.Cursor = max(0, min(s.list.Cursor, len(rows)-1))
	return rows[s.list.Cursor], true
}

// change is the pending change with the id, or nil.
func change(ctx *tui.Ctx, id string) *changeset.Change {
	if ctx.CS == nil || id == "" {
		return nil
	}
	for i := range ctx.CS.Changes {
		if ctx.CS.Changes[i].ID == id {
			return &ctx.CS.Changes[i]
		}
	}
	return nil
}

func (s *screen) closeInput() { s.step, s.host, s.field = stepNone, "", nil }

func (s *screen) Update(ctx *tui.Ctx, msg tea.Msg) (tui.Screen, tea.Cmd) {
	if ctx.Store == nil {
		return s, nil
	}
	switch m := msg.(type) {
	case components.CancelMsg:
		s.closeInput()
		return s, nil
	case components.SubmitMsg:
		return s, s.submit(ctx, strings.TrimSpace(m.Value))
	case tea.KeyMsg:
		if s.step != stepNone {
			return s, s.field.Update(m)
		}
		if h, cmd := s.filter.Update(ctx, m); h {
			return s, cmd
		}
		return s, s.key(ctx, m)
	}
	return s, nil
}

// parsePort reads a port answer: empty is Railway's default (0).
func parsePort(v string) (int, bool) {
	if v == "" {
		return 0, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		return 0, false
	}
	return n, true
}

func (s *screen) submit(ctx *tui.Ctx, value string) tea.Cmd {
	switch s.step {
	case stepHost:
		// the kind checks the host (and that the service lacks it) now, so
		// a bad one is said before the port is asked
		if _, err := kinds.AddCustomDomain(ctx.Store, s.place, value, 0, ctx.Now()); err != nil {
			s.closeInput()
			return tui.Flash('✗', err.Error())
		}
		s.step, s.host = stepCustomPort, value
		s.field = &components.TextInput{Label: "port"}
		return nil
	case stepRailwayPort, stepCustomPort:
		st, host := s.step, s.host
		s.closeInput()
		port, ok := parsePort(value)
		if !ok {
			return tui.Flash('✗', portError)
		}
		var c changeset.Change
		if st == stepRailwayPort {
			c = kinds.AddServiceDomain(s.place, port, ctx.Now())
		} else {
			var err error
			if c, err = kinds.AddCustomDomain(ctx.Store, s.place, host, port, ctx.Now()); err != nil {
				return tui.Flash('✗', err.Error())
			}
		}
		put := tui.Emit(tui.PutChangeMsg{Change: c})
		shown := "railway domain railway"
		if st == stepCustomPort {
			shown = strings.ToLower(host) + " custom"
		}
		if s.filter.Active() && !s.filter.Match(shown) {
			return tea.Batch(put, tui.Flash('!', "added · hidden by the filter"))
		}
		return put
	}
	s.closeInput()
	return nil
}

func (s *screen) key(ctx *tui.Ctx, k tea.KeyMsg) tea.Cmd {
	rows, _ := s.filtered(ctx)
	// the rows View shows; a page is at least one row
	h := s.listHeight(ctx.Height - tui.ChromeRows)
	move := func(d int) { s.list.Move(len(rows), d, h) }
	page := max(1, h)
	switch k.Type {
	case tea.KeyUp:
		move(-1)
		return nil
	case tea.KeyDown:
		move(1)
		return nil
	case tea.KeyPgUp:
		move(-page)
		return nil
	case tea.KeyPgDown:
		move(page)
		return nil
	case tea.KeyRunes:
	default:
		return nil
	}
	switch string(k.Runes) {
	case "j":
		move(1)
	case "k":
		move(-1)
	case "g", "a":
		if s.place.Shared() {
			return tui.Flash('✗', "a domain belongs to a service")
		}
		if string(k.Runes) == "g" {
			s.step, s.field = stepRailwayPort, &components.TextInput{Label: "port"}
		} else {
			s.step, s.field = stepHost, &components.TextInput{Label: "host"}
		}
	case "x":
		return s.delete(ctx)
	case "r":
		return s.read(ctx)
	}
	return nil
}

// delete deletes the cursor's domain; a row with a pending change drops
// that change instead (a pending add goes away, a pending delete is undone).
func (s *screen) delete(ctx *tui.Ctx) tea.Cmd {
	row, ok := s.current(ctx)
	if !ok {
		return nil
	}
	if c := change(ctx, row.ChangeID); c != nil {
		if c.RunID != "" {
			return tui.Flash('✗', "in run "+c.RunID)
		}
		return tui.Emit(tui.RemoveChangeMsg{ID: c.ID})
	}
	var c changeset.Change
	var err error
	if row.Custom {
		c, err = kinds.DeleteCustomDomain(ctx.Store, s.place, row.Host, ctx.Now())
	} else {
		c, err = kinds.DeleteServiceDomain(ctx.Store, s.place, row.ID, ctx.Now())
	}
	if err != nil {
		return tui.Flash('✗', err.Error())
	}
	return tui.Emit(tui.PutChangeMsg{Change: c})
}

// listHeight is the rows left for domains in a body of the given height:
// the header takes one, and the filter line and an open prompt one each.
func (s *screen) listHeight(body int) int {
	h := body - 1
	if s.filter.Editing() || s.filter.Active() {
		h--
	}
	if s.field != nil {
		h--
	}
	return max(h, 0)
}

func (s *screen) View(ctx *tui.Ctx, width, height int) string {
	if ctx.Store == nil {
		return ""
	}
	th := ctx.Theme
	rows, total := s.filtered(ctx)
	header := th.Accent.Render(text.Sanitize(ctx.EnvName(s.place.EnvID) + " · " + ctx.Store.ServiceName(s.place.ServiceID)))
	lines := []string{ansi.Truncate(header, width, "…")}
	if f := s.filter.View(width, len(rows), total); f != "" {
		lines = append(lines, f)
	}
	prompt := ""
	if s.field != nil {
		prompt = s.field.View(width)
	}
	listH := s.listHeight(height)

	if len(rows) == 0 {
		lines = append(lines, s.empty(ctx))
	} else {
		cs := make([]cells, len(rows))
		for i, d := range rows {
			cs[i] = s.cells(ctx, d)
		}
		l := layout(cs, width)
		from, to := s.list.Window(len(rows), listH)
		for i := from; i < to; i++ {
			lines = append(lines, s.row(ctx, rows[i], cs[i], l, i == s.list.Cursor, width))
		}
	}
	if prompt != "" {
		lines = append(lines, prompt)
	}
	if len(lines) > height {
		lines = lines[:max(height, 0)]
	}
	return strings.Join(lines, "\n")
}

// empty is the line shown when no row is.
func (s *screen) empty(ctx *tui.Ctx) string {
	th := ctx.Theme
	if s.filter.Active() {
		return th.Muted.Render("no domain matches the filter")
	}
	if _, ok := ctx.Store.Domains(s.place); !ok {
		if e := ctx.Store.Load(store.DetailKey(s.place)).Err; e != "" {
			return th.Danger.Render("✗ cannot read the domains: " + text.Sanitize(e))
		}
		return th.Muted.Render("reading domains")
	}
	return th.Muted.Render("no domain here · g adds a Railway domain, a a custom one")
}

// cells are one row's column texts, plain and sanitized.
type cells struct {
	prefix, host, mark string // the host cell: "+ " / "✗ ", the host, " ●"
	port, kind         string
	status, cert       string
}

func (c cells) hostW() int {
	return ansi.StringWidth(c.prefix) + ansi.StringWidth(c.host) + ansi.StringWidth(c.mark)
}

// certText is Railway's certificate status in words:
// CERTIFICATE_STATUS_TYPE_VALID → "cert valid".
func certText(s string) string {
	s = strings.TrimPrefix(strings.ToUpper(s), "CERTIFICATE_STATUS_TYPE_")
	if s == "" {
		return ""
	}
	return "cert " + strings.ToLower(strings.ReplaceAll(s, "_", " "))
}

func (s *screen) cells(ctx *tui.Ctx, d view.DomainView) cells {
	c := cells{host: text.Sanitize(label(d)), kind: kindName(d), port: "default"}
	if d.Port != 0 {
		c.port = ":" + strconv.Itoa(d.Port)
	}
	if d.Pending != nil {
		c.mark = " " + ctx.Icons.Pending()
		if ch := change(ctx, d.ChangeID); ch != nil && ch.RunID != "" {
			c.mark = " " + ctx.Icons.Run()
		}
		if d.ID == "" {
			c.prefix = "+ "
		} else {
			c.prefix = "✗ "
		}
	}
	if d.Custom && d.ID != "" {
		c.status = "waiting for DNS"
		if d.Verified {
			c.status = "verified"
		}
		c.cert = certText(text.Sanitize(d.CertStatus))
	}
	return c
}

// columns are the optional columns, in the order they are dropped when the
// width is short: the kind first (a custom domain also shows its status, a
// Railway one its up.railway.app host), the DNS status last.
var columns = []struct {
	name string
	get  func(cells) string
}{
	{name: "kind", get: func(c cells) string { return c.kind }},
	{name: "cert", get: func(c cells) string { return c.cert }},
	{name: "port", get: func(c cells) string { return c.port }},
	{name: "status", get: func(c cells) string { return c.status }},
}

// lay is the column widths: 0 means the column is not shown.
type lay struct {
	host                     int
	port, kind, status, cert int
}

const gap = 2

// layout fits the columns into width: the host column takes its widest
// cell, and the optional columns are dropped (kind, cert, port, status)
// until the row fits; only then is the host cut.
func layout(rows []cells, width int) lay {
	var l lay
	for _, r := range rows {
		l.host = max(l.host, r.hostW())
	}
	widths := map[string]int{}
	for _, col := range columns {
		for _, r := range rows {
			widths[col.name] = max(widths[col.name], ansi.StringWidth(col.get(r)))
		}
	}
	sum := func() int {
		n := l.host
		for _, w := range widths {
			if w > 0 {
				n += gap + w
			}
		}
		return n
	}
	for _, col := range columns {
		if sum() <= width {
			break
		}
		widths[col.name] = 0
	}
	l.port, l.kind, l.status, l.cert = widths["port"], widths["kind"], widths["status"], widths["cert"]
	if s := sum(); s > width {
		l.host = max(1, l.host-(s-width))
	}
	return l
}

// row draws one domain. The selected row is plain text under the selection
// style; the others use tones, and a domain in Railway with a valid host is
// a link. The pending mark is never cut: a short host column cuts the host.
func (s *screen) row(ctx *tui.Ctx, d view.DomainView, c cells, l lay, selected bool, width int) string {
	th := ctx.Theme
	room := max(0, l.host-ansi.StringWidth(c.prefix)-ansi.StringWidth(c.mark))
	host := c.host
	if ansi.StringWidth(host) > room {
		host = ansi.Truncate(host, room, "…")
	}
	fill := strings.Repeat(" ", max(0, l.host-ansi.StringWidth(c.prefix+host+c.mark)))
	type col struct {
		text string
		w    int
		tone lipgloss.Style
	}
	statusTone := th.OK
	if c.status != "verified" {
		statusTone = th.Warn
	}
	cols := []col{
		{c.port, l.port, th.Muted},
		{c.kind, l.kind, th.Muted},
		{c.status, l.status, statusTone},
		{c.cert, l.cert, th.Muted},
	}

	if selected {
		plain := c.prefix + host + c.mark + fill
		for _, cl := range cols {
			if cl.w > 0 {
				plain += strings.Repeat(" ", gap) + tui.Pad(cl.text, cl.w)
			}
		}
		return th.Selected.Render(ansi.Truncate(strings.TrimRight(plain, " "), width, "…"))
	}

	var cell string
	switch {
	case d.Pending != nil && d.ID == "":
		cell = th.New.Render(c.prefix+host) + th.New.Render(c.mark)
	case d.Pending != nil:
		cell = th.Danger.Strikethrough(true).Render(c.prefix+host) + th.New.Render(c.mark)
	case kinds.ValidHost(d.Host) && host == c.host:
		cell = tui.LinkStyled("https://"+d.Host, th.Accent.Underline(true).Render(host))
	default:
		cell = host
	}
	// only the columns up to the last non-empty one, the last unpadded,
	// so a line ends at its text
	last := -1
	for i, cl := range cols {
		if cl.w > 0 && cl.text != "" {
			last = i
		}
	}
	line := cell
	if last >= 0 {
		line += fill
	}
	for i, cl := range cols[:last+1] {
		if cl.w == 0 {
			continue
		}
		t := tui.Pad(cl.text, cl.w)
		if i == last {
			t = cl.text
		}
		line += strings.Repeat(" ", gap) + cl.tone.Render(t)
	}
	// narrower than the prefix and the mark: they are cut too
	return ansi.Truncate(line, width, "")
}
