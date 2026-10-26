package services

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
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
	"github.com/men232/railway-manager/internal/monitor"
	"github.com/men232/railway-manager/internal/railway"
	"github.com/men232/railway-manager/internal/store"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/view"
)

// The panel under the table (plus three charts): a title,
// source, commit, reach, two usage rows, three deployments and three charts
// two lines high. It is panelLines high whatever it shows, under a separator
// line, and shows only when the table keeps at least minTableRows rows
// beside them.
const (
	panelLines   = 15
	sepLines     = 1
	minTableRows = 3
	labelW       = 9
	gridGap      = 4
	deployRows   = 3
	chartRows    = 2
	// minWideChart is the narrowest chart drawn two lines high with its
	// caption beside it; below, the chart is one line and the caption goes
	// under it.
	minWideChart = 16
	// captionW is the room kept right of a two-line chart for its caption:
	// the widest one, "now 1023.9 MB · peak 1023.9 MB · limit 1023.9 MB". It
	// does not follow the data, so the chart width (and with it the span the
	// panel reads) depends on the terminal width alone.
	captionW = len("now ") + 9 + 8 + 9 + 9 + 9 // " · peak " and " · limit " are 8 and 9 cells
)

// chartCells is the width in cells of the panel charts in a panel width
// wide, and how many lines high they are: the panel draws them at this width
// and reads its metrics over the span these cells need (single source).
func chartCells(width int) (cells, rows int) {
	if wide := width - labelW - 2 - captionW; wide >= minWideChart {
		return wide, chartRows
	}
	return max(0, width-labelW), 1
}

// gaugeSteps are the gauge widths tried, widest first, until the usage rows
// fit.
var gaugeSteps = [...]int{10, 5, 0}

// Links are built only from values that match these (and kinds.ValidHost).
var (
	// each segment starts with a letter or digit: no ".", "..", "../.."
	repoRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	shaRE  = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
)

// Messages of the panel: the cursor rested (restSeq), the panel metrics
// poll (pollSeq) and the terminal width rested (resizeSeq), all tagged with
// the screen generation.
type (
	panelRestMsg   struct{ gen, seq uint64 }
	panelPollMsg   struct{ gen, seq uint64 }
	panelResizeMsg struct{ gen, seq uint64 }
)

// panelState follows the service under the cursor.
type panelState struct {
	place   domain.Place
	ok      bool   // place is followed: a rest is scheduled or done
	restSeq uint64 // the newest rest timer
	pollSeq uint64 // the current poll loop
	level   int    // index into loaders.Levels
	// cells is the chart cells (capped at loaders.MaxPanelCells) the newest
	// panel metrics read spans; 0 before the first read of the place.
	cells     int
	resizeSeq uint64 // the newest resize timer
}

func (s *screen) level() loaders.Level { return loaders.Levels[s.panel.level] }

// spanCells is the chart cells a panel width wide draws, as a read spans
// them (capped at loaders.MaxPanelCells).
func spanCells(width int) int {
	cells, _ := chartCells(width)
	return min(cells, loaders.MaxPanelCells)
}

// wantCells is the chart cells the terminal's width needs read.
func wantCells(ctx *tui.Ctx) int { return spanCells(ctx.Width - tui.ChromeCols) }

// drawCells is the cells whose read the panel draws: the newest read's (so
// a resize shows the last data until the new span is read), else the ones
// the width needs.
func (s *screen) drawCells(width int) int {
	if s.panel.cells > 0 {
		return s.panel.cells
	}
	return spanCells(width)
}

// panelFits reports whether a body of height lines has room for the panel,
// its separator and minTableRows rows, given the lines above (filter) and
// below (failure, prompt) the table.
func panelFits(height, nrows, extra int) bool {
	return nrows > 0 && height-extra-1-sepLines-panelLines >= minTableRows
}

// extraLines counts the body lines other than the table and the panel.
func (s *screen) extraLines(ctx *tui.Ctx) int {
	n := 0
	if s.filter.Editing() || s.filter.Active() {
		n++
	}
	if readFailure(ctx) != "" {
		n++
	}
	if s.confirm != nil || s.input != nil {
		n++
	}
	return n
}

func (s *screen) fits(ctx *tui.Ctx, nrows int) bool {
	return panelFits(ctx.Height-tui.ChromeRows, nrows, s.extraLines(ctx))
}

// follow starts a rest timer when the panel shows a service it did not
// follow yet; while the panel has no room it follows nothing, so it reads
// again once it shows.
func (s *screen) follow(ctx *tui.Ctx) tea.Cmd {
	if !s.active {
		return nil
	}
	rows, _ := s.visible(ctx)
	r, ok := s.current(rows)
	if !ok || !s.fits(ctx, len(rows)) {
		s.panel.ok = false
		return nil
	}
	if s.panel.ok && r.Place == s.panel.place {
		return nil
	}
	s.panel.place, s.panel.ok = r.Place, true
	s.panel.cells = 0 // the rest reads the span the width needs
	s.panel.restSeq++
	s.panel.pollSeq++ // the last place's poll stops
	gen, seq := s.gen, s.panel.restSeq
	return tick(loaders.DetailDelay, func(time.Time) tea.Msg { return panelRestMsg{gen, seq} })
}

func (s *screen) readDetail(ctx *tui.Ctx) tea.Cmd {
	if ctx.API == nil {
		return nil
	}
	return loaders.Detail(ctx.API, s.panel.place, ctx.Clock())
}

// readPanel reads the panel metrics of the level over the span the current
// width needs: two points a chart cell.
func (s *screen) readPanel(ctx *tui.Ctx) tea.Cmd {
	s.panel.cells = wantCells(ctx)
	if ctx.API == nil {
		return nil
	}
	step := s.level().Step
	return loaders.PanelMetrics(ctx.API, s.panel.place, step, loaders.PanelSpan(step, s.panel.cells), ctx.Clock())
}

// poll starts a new panel metrics loop; earlier ones go stale.
func (s *screen) poll() tea.Cmd {
	s.panel.pollSeq++
	gen, seq := s.gen, s.panel.pollSeq
	return tick(loaders.MetricsEvery, func(time.Time) tea.Msg { return panelPollMsg{gen, seq} })
}

// panelUpdate handles the panel's timers and resizes; ok is false for any
// other message.
func (s *screen) panelUpdate(ctx *tui.Ctx, msg tea.Msg) (tea.Cmd, bool) {
	switch m := msg.(type) {
	case panelRestMsg:
		if m.gen != s.gen || !s.active || !s.panel.ok || m.seq != s.panel.restSeq {
			return nil, true
		}
		return tea.Batch(s.readDetail(ctx), s.readPanel(ctx), s.poll()), true
	case panelPollMsg:
		if m.gen != s.gen || !s.active || !s.panel.ok || m.seq != s.panel.pollSeq {
			return nil, true
		}
		if rows, _ := s.visible(ctx); !s.fits(ctx, len(rows)) {
			s.panel.ok = false // read again once it shows
			return nil, true
		}
		return tea.Batch(s.readPanel(ctx), s.poll()), true
	case tea.WindowSizeMsg:
		// the root has set ctx.Width; a change of chart cells reads again
		// once the resizing rests, and any resize makes the earlier timer
		// stale
		if !s.active || !s.panel.ok || s.panel.cells == 0 {
			return nil, true
		}
		s.panel.resizeSeq++
		if wantCells(ctx) == s.panel.cells {
			return nil, true
		}
		gen, seq := s.gen, s.panel.resizeSeq
		return tick(loaders.DetailDelay, func(time.Time) tea.Msg { return panelResizeMsg{gen, seq} }), true
	case panelResizeMsg:
		if m.gen != s.gen || !s.active || !s.panel.ok || m.seq != s.panel.resizeSeq {
			return nil, true
		}
		if wantCells(ctx) == s.panel.cells {
			return nil, true
		}
		if rows, _ := s.visible(ctx); !s.fits(ctx, len(rows)) {
			s.panel.ok = false // read again once it shows
			return nil, true
		}
		return tea.Batch(s.readPanel(ctx), s.poll()), true
	}
	return nil, false
}

// stepLevel moves the chart level by d and reads it at once.
func (s *screen) stepLevel(ctx *tui.Ctx, d int) tea.Cmd {
	l := s.panel.level + d
	if l < 0 || l >= len(loaders.Levels) {
		return nil
	}
	s.panel.level = l
	if !s.panel.ok {
		return nil
	}
	return tea.Batch(s.readPanel(ctx), s.poll())
}

// panelView draws the panel of row r: exactly panelLines lines, each at
// most width cells.
func (s *screen) panelView(ctx *tui.Ctx, r view.MonitorRow, width int) []string {
	p := &painter{ctx: ctx, th: ctx.Theme, ic: ctx.Icons, width: width}
	lv := s.level()
	d := ctx.Views.Detail(ctx.CS, r.Place, lv.Step, loaders.PanelSpan(lv.Step, s.drawCells(width)))
	pend := placeChanges(ctx.CS, r.Place)

	lines := []string{p.title(r, d, lv.Name)}
	lines = append(lines, p.source(d, ctx.Store.Load(store.DetailKey(r.Place)).Err))
	lines = append(lines, p.commit(r, d))
	lines = append(lines, p.reach(d))
	lines = append(lines, p.usage(r, d, pend)...)
	// two-line charts add the time axis: it takes a deployment row
	rows := deployRows
	if _, chart := chartCells(width); chart == chartRows {
		rows--
	}
	lines = append(lines, p.deploys(d, rows)...)
	lines = append(lines, p.charts(d)...)
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, width, "…")
	}
	for len(lines) < panelLines {
		lines = append(lines, "")
	}
	return lines[:panelLines]
}

func placeChanges(cs *changeset.ChangeSet, p domain.Place) []changeset.Change {
	if cs == nil {
		return nil
	}
	var out []changeset.Change
	for _, c := range cs.Changes {
		if c.Place == p {
			out = append(out, c)
		}
	}
	return out
}

// painter holds what every panel line needs.
type painter struct {
	ctx   *tui.Ctx
	th    tui.Theme
	ic    tui.Icons
	width int
}

func (p *painter) meta(s string) string  { return p.th.Muted.Render(s) }
func (p *painter) label(s string) string { return p.th.Muted.Render(tui.Pad(s, labelW)) }
func (p *painter) bold(s string) string {
	st := lipgloss.NewStyle()
	if p.th.Renderer != nil {
		st = p.th.Renderer.NewStyle()
	}
	return st.Bold(true).Render(s)
}
func (p *painter) sep() string { return p.th.Muted.Render(" · ") }

// icon is a dim glyph and a space, or nothing when the set has none.
func (p *painter) icon(g string) string {
	if g == "" {
		return ""
	}
	return p.th.Muted.Render(g) + " "
}

// link is text linked to url in the accent tone, underlined.
func (p *painter) link(url, txt string) string {
	return tui.LinkStyled(url, p.th.Accent.Underline(true).Render(text.Sanitize(txt)))
}

// line is a label then the items joined by " · ", or "—" without any.
func (p *painter) line(name string, items []string) string {
	if len(items) == 0 {
		return p.label(name) + "—"
	}
	return p.label(name) + strings.Join(items, p.sep())
}

// span is a coarse duration: 45s, 12m, 2h 14m, 3d 4h.
func span(d time.Duration) string {
	sec := max(0, int64(d/time.Second))
	pair := func(big int64, unit string, small int64, smallUnit string) string {
		if small == 0 {
			return strconv.FormatInt(big, 10) + unit
		}
		return strconv.FormatInt(big, 10) + unit + " " + strconv.FormatInt(small, 10) + smallUnit
	}
	switch {
	case sec < 60:
		return strconv.FormatInt(sec, 10) + "s"
	case sec < 3600:
		return strconv.FormatInt(sec/60, 10) + "m"
	case sec < 86400:
		return pair(sec/3600, "h", sec%3600/60, "m")
	}
	return pair(sec/86400, "d", sec%86400/3600, "h")
}

// running is the deployment that runs now, if the detail lists it.
func running(ctx *tui.Ctx, place domain.Place, d view.Detail) (railway.DeployInfo, bool) {
	st, ok := ctx.Store.State(place.EnvID, place.ServiceID)
	if !ok || st.ActiveID == "" {
		return railway.DeployInfo{}, false
	}
	for _, dep := range d.Info.Deploys {
		if dep.ID == st.ActiveID {
			return dep, true
		}
	}
	return railway.DeployInfo{}, false
}

// title: status, name, place, regions, uptime; the chart level (time per
// point) on the right.
func (p *painter) title(r view.MonitorRow, d view.Detail, level string) string {
	left := statusStyle(p.th, r.Status.Kind).Bold(true).Render(p.ic.Status(r.Status.Icon)+" "+r.Status.Text) +
		"  " + p.bold(text.Sanitize(r.Service)) +
		p.sep() + p.meta(text.Sanitize(r.Project)+"/"+text.Sanitize(r.Env))
	if len(d.Info.Regions) > 0 {
		var regions []string
		for _, name := range slices.Sorted(maps.Keys(d.Info.Regions)) {
			reg := text.Sanitize(name)
			if n := d.Info.Regions[name]; n > 1 {
				reg += " ×" + strconv.Itoa(n)
			}
			regions = append(regions, reg)
		}
		left += p.sep() + p.icon(p.ic.Region()) + p.meta(strings.Join(regions, ", "))
	}
	if dep, ok := running(p.ctx, r.Place, d); ok && !dep.CreatedAt.IsZero() {
		left += p.sep() + p.meta("up "+span(p.ctx.Clock()().Sub(dep.CreatedAt)))
	}
	right := p.meta(level)
	room := p.width - ansi.StringWidth(right) - 2
	left = ansi.Truncate(left, max(0, room), "…")
	return left + strings.Repeat(" ", max(2, p.width-ansi.StringWidth(left)-ansi.StringWidth(right))) + right
}

var restartText = map[string]string{"ON_FAILURE": "on failure", "ALWAYS": "always", "NEVER": "never"}

func (p *painter) source(d view.Detail, loadErr string) string {
	if !d.HasInfo {
		if loadErr != "" && !strings.HasPrefix(loadErr, "! ") {
			return p.label("source") + p.th.Danger.Render(p.ic.Fail()+" cannot read the details: "+text.Sanitize(loadErr))
		}
		return p.line("source", nil)
	}
	in := d.Info
	var items []string
	switch {
	case in.Repo != "":
		repo := p.meta(text.Sanitize(in.Repo))
		if repoRE.MatchString(in.Repo) {
			repo = p.link("https://github.com/"+in.Repo, in.Repo)
		}
		items = append(items, p.icon(p.ic.Repo())+repo)
	case in.Image != "":
		items = append(items, text.Sanitize(in.Image))
	}
	if in.Branch != "" {
		items = append(items, p.icon(p.ic.Branch())+text.Sanitize(in.Branch))
	}
	if in.Builder != "" && in.Repo != "" {
		items = append(items, p.icon(p.ic.Builder())+p.meta(text.Sanitize(in.Builder)))
	}
	if in.RestartPolicy != "" {
		rt, ok := restartText[in.RestartPolicy]
		if !ok {
			rt = strings.ToLower(text.Sanitize(in.RestartPolicy))
		}
		if in.RestartPolicy == "ON_FAILURE" && in.RestartMaxRetries > 0 {
			rt += " ×" + strconv.Itoa(in.RestartMaxRetries)
		}
		items = append(items, p.icon(p.ic.Restart())+p.meta(rt))
	}
	if in.Sleeps {
		items = append(items, p.icon(p.ic.Sleep())+p.meta("sleeps when idle"))
	}
	if in.Cron != "" {
		items = append(items, p.meta("cron "+text.Sanitize(in.Cron)))
	}
	if in.Healthcheck != "" {
		items = append(items, p.icon(p.ic.Health())+p.meta(text.Sanitize(in.Healthcheck)))
	}
	return p.line("source", items)
}

// commit: the short hash, the first message line cut so @author stays.
func (p *painter) commit(r view.MonitorRow, d view.Detail) string {
	if !d.HasInfo {
		return p.line("commit", nil)
	}
	shown, ok := running(p.ctx, r.Place, d)
	if !ok && len(d.Info.Deploys) > 0 {
		shown, ok = d.Info.Deploys[0], true
	}
	if !ok || shown.CommitHash == "" {
		return p.line("commit", nil)
	}
	short := text.Sanitize(shown.CommitHash)
	if rs := []rune(short); len(rs) > 7 {
		short = string(rs[:7])
	}
	hash := short
	if shaRE.MatchString(shown.CommitHash) && repoRE.MatchString(d.Info.Repo) {
		hash = p.link("https://github.com/"+d.Info.Repo+"/commit/"+shown.CommitHash, short)
	}
	head := p.label("commit") + p.icon(p.ic.Commit()) + hash
	tail := ""
	if shown.Creator != "" {
		tail = p.sep() + p.meta("@"+text.Sanitize(shown.Creator))
	}
	msg, _, _ := strings.Cut(shown.CommitMessage, "\n")
	msg = strings.TrimSpace(text.Sanitize(msg))
	if msg == "" {
		return head + tail
	}
	room := p.width - ansi.StringWidth(head) - ansi.StringWidth(tail) - 3
	if room < 2 {
		return head + tail
	}
	return head + p.sep() + ansi.Truncate(msg, room, "…") + tail
}

// reach: each domain, linked; pending adds and deletes in their tones.
func (p *painter) reach(d view.Detail) string {
	// pending adds and deletes first: a long line cuts the others
	var items, pending []string
	mark := p.th.New.Render(p.ic.Pending())
	for _, dm := range d.Domains {
		host := text.Sanitize(dm.Host)
		switch {
		case dm.Pending != nil && dm.ID == "":
			if host == "" {
				host = "railway domain"
			}
			pending = append(pending, p.th.New.Render("+ "+host)+" "+mark)
		case dm.Pending != nil:
			pending = append(pending, p.th.Danger.Strikethrough(true).Render("✗ "+host)+" "+mark)
		case kinds.ValidHost(dm.Host):
			items = append(items, p.icon(p.ic.Globe())+p.link("https://"+dm.Host, dm.Host))
		default:
			items = append(items, p.icon(p.ic.Globe())+host)
		}
	}
	items = append(pending, items...)
	if len(items) == 0 {
		if !d.HasInfo {
			return p.line("reach", nil)
		}
		items = append(items, p.meta("no public domain"))
	}
	return p.line("reach", items)
}

// meter is one usage cell before layout: the icon (none for the network,
// which leads with ↓), a gauge level, the newest value, what it is out of,
// a pending new limit or size with its mark, and a tail.
type meter struct {
	icon   string
	level  float64
	gauge  bool   // level is known
	value  string // right-aligned to its column
	limit  string // "" when unknown
	change string // a pending new limit or size, or ""
	tone   lipgloss.Style
	unit   string
	note   string // after the pending mark, in the change's tone
	tail   string // muted; dropped first when short
	plain  string // the whole text after the value (network, no volume)
}

func last(v []float64) (float64, bool) {
	if len(v) == 0 {
		return 0, false
	}
	return v[len(v)-1], true
}

func trimNum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// volumeSize is decimal: Railway's 50000 MB is 50.0 GB.
func volumeSize(mb float64) string {
	if mb >= 1000 {
		return fmt.Sprintf("%.1f GB", mb/1000)
	}
	return fmt.Sprintf("%.1f MB", mb)
}

// after is what follows the value: " / limit → new unit ● note  tail". In
// compact form a pending cell drops the value (and the tail): "limit → new
// unit ●" still says what changes.
func (p *painter) after(m meter, compact bool) string {
	var b strings.Builder
	if m.limit != "" {
		if compact && m.change != "" {
			b.WriteString(p.meta(m.limit))
		} else {
			b.WriteString(p.meta(" / " + m.limit))
		}
	}
	if m.change != "" {
		b.WriteString(m.tone.Render(" → " + m.change))
	}
	if m.unit != "" {
		b.WriteString(p.meta(" " + m.unit))
	}
	if m.change != "" {
		b.WriteString(" " + m.tone.Render(p.ic.Pending()))
		if m.note != "" {
			b.WriteString(m.tone.Render(" " + m.note))
		}
	}
	if m.tail != "" && !compact {
		b.WriteString(p.meta("  " + m.tail))
	}
	b.WriteString(m.plain)
	return b.String()
}

func (p *painter) usage(r view.MonitorRow, d view.Detail, pend []changeset.Change) []string {
	var newCPU, newMem string
	for _, c := range pend {
		if cpu, mem, ok := kinds.PendingLimits(c); ok {
			newCPU, newMem = "default", "default"
			if cpu != nil {
				newCPU = trimNum(*cpu)
			}
			if mem != nil {
				newMem = monitor.FormatBytes(float64(*mem))
			}
		}
	}

	cpuLimit := r.CPULimit
	if cpuLimit == 0 && d.Limits.CPU > 0 {
		cpuLimit = d.Limits.CPU
	}
	cpu := meter{icon: p.ic.CPU(), value: "—", change: newCPU, tone: p.th.New}
	cpuNow, hasCPU := last(r.CPU)
	if hasCPU {
		cpu.value = monitor.FormatCPU(cpuNow)
	}
	if cpuLimit > 0 {
		cpu.limit = trimNum(cpuLimit)
		cpu.level, cpu.gauge = cpuNow/cpuLimit, hasCPU
	}
	if hasCPU || cpuLimit > 0 || newCPU != "" {
		cpu.unit = "vCPU"
	}

	memLimit := r.MemLimitGB * monitor.GB
	if memLimit == 0 && d.Limits.MemBytes > 0 {
		memLimit = d.Limits.MemBytes
	}
	mem := meter{icon: p.ic.Memory(), value: "—", change: newMem, tone: p.th.New}
	memNow, hasMem := last(r.Mem)
	if hasMem {
		mem.value = monitor.FormatBytes(memNow * monitor.GB)
	}
	if memLimit > 0 {
		mem.limit = monitor.FormatBytes(memLimit)
		mem.level, mem.gauge = memNow*monitor.GB/memLimit, hasMem
	}

	disk := meter{icon: p.ic.Disk(), plain: p.meta("no volume"), tone: p.th.Warn}
	if len(d.Volumes) > 0 {
		v := d.Volumes[0]
		disk.plain = ""
		disk.value, disk.limit = volumeSize(v.UsedMB), volumeSize(v.SizeMB)
		if v.SizeMB > 0 {
			disk.level, disk.gauge = v.UsedMB/v.SizeMB, true
		}
		for _, c := range pend {
			if id, mb, ok := kinds.PendingVolumeSize(c); ok && id == v.ID {
				disk.change, disk.note = volumeSize(float64(mb)), "can't be undone"
			}
		}
		disk.tail = text.Sanitize(v.MountPath)
	}

	net := meter{value: "—"}
	rx := p.ctx.Store.Series(r.Place.EnvID, r.Place.ServiceID, "NETWORK_RX_GB")
	tx := p.ctx.Store.Series(r.Place.EnvID, r.Place.ServiceID, "NETWORK_TX_GB")
	if in, out, ok := monitor.NewestRates(rx, tx); ok {
		net.value = monitor.FormatRate(in)
		net.plain = p.meta("  ↑ ") + monitor.FormatRate(out)
	}

	labels := [2]string{p.label("usage"), p.label("")}
	fit := func(rows [2]string) bool {
		return ansi.StringWidth(rows[0]) <= p.width && ansi.StringWidth(rows[1]) <= p.width
	}
	var rows [2]string
	// a grid: values right-aligned in two columns, gauges as wide as fit
	for _, g := range gaugeSteps {
		left := p.cells([2]meter{cpu, disk}, g)
		right := p.cells([2]meter{mem, net}, g)
		lw := max(ansi.StringWidth(left[0]), ansi.StringWidth(left[1])) + gridGap
		for k := range rows {
			rows[k] = labels[k] + left[k] + strings.Repeat(" ", lw-ansi.StringWidth(left[k])) + right[k]
		}
		if fit(rows) {
			return rows[:]
		}
	}
	// short of room: each row on its own, pending cells compact
	rows[0] = labels[0] + p.cell(cpu, false) + "  " + p.cell(mem, false)
	if ansi.StringWidth(rows[0]) > p.width {
		rows[0] = labels[0] + p.cell(cpu, true) + "  " + p.cell(mem, true)
	}
	rows[1] = p.diskRow(labels[1], disk, net)
	return rows[:]
}

// diskRow is the disk and net row short of room: it drops the mount path,
// then the net cell, then the disk's current value, so the pending part
// ("→ new ● can't be undone") always shows whole.
func (p *painter) diskRow(label string, disk, net meter) string {
	bare := disk
	bare.tail = ""
	for _, row := range []string{
		label + p.cell(disk, false) + "  " + p.cell(net, false),
		label + p.cell(bare, false) + "  " + p.cell(net, false),
		label + p.cell(bare, false),
	} {
		if ansi.StringWidth(row) <= p.width {
			return row
		}
	}
	return label + p.cell(bare, true)
}

// cell draws one meter without alignment.
func (p *painter) cell(m meter, compact bool) string {
	lead := p.meta("↓ ")
	if m.icon != "" {
		lead = p.icon(m.icon)
	}
	if compact && m.change != "" {
		return lead + p.after(m, true)
	}
	return lead + m.value + p.after(m, compact)
}

// cells draws a column of two meters, values right-aligned to the wider,
// gauges g cells wide (a meter without a level keeps the gauge's room).
func (p *painter) cells(ms [2]meter, g int) [2]string {
	vw := max(ansi.StringWidth(ms[0].value), ansi.StringWidth(ms[1].value))
	anyGauge := g > 0 && (ms[0].gauge || ms[1].gauge)
	var out [2]string
	for k, m := range ms {
		var b strings.Builder
		if m.icon != "" {
			b.WriteString(p.icon(m.icon))
		} else {
			b.WriteString(p.meta("↓ "))
		}
		if anyGauge {
			if m.gauge {
				b.WriteString(p.gauge(m.level, g) + " ")
			} else if m.icon != "" && m.value != "" {
				b.WriteString(strings.Repeat(" ", g+1))
			}
		}
		if m.value != "" {
			b.WriteString(padLeft(m.value, vw))
		}
		b.WriteString(p.after(m, false))
		out[k] = b.String()
	}
	return out
}

// gauge is how full, in g cells of a thin line coloured by level; the empty
// part is faint.
func (p *painter) gauge(level float64, g int) string {
	filled := min(g, max(0, int(level*float64(g)+0.5)))
	return p.th.Level(level).Render(strings.Repeat("━", filled)) + p.th.Faint.Render(strings.Repeat("─", g-filled))
}

func (p *painter) deployIcon(status string) string {
	switch status {
	case "SUCCESS":
		return p.th.OK.Render(p.ic.DeployOK())
	case "FAILED", "CRASHED":
		return p.th.Danger.Render(p.ic.DeployFailed())
	case "SLEEPING":
		return p.th.Muted.Render(p.ic.Sleep())
	case "REMOVED", "REMOVING", "SKIPPED":
		return p.th.Muted.Render(p.ic.Todo())
	}
	return p.th.Warn.Render(p.ic.DeployRunning())
}

// deploys: the newest n, as a small table, n lines high.
func (p *painter) deploys(d view.Detail, n int) []string {
	deps := d.Info.Deploys[:min(n, len(d.Info.Deploys))]
	out := make([]string, 0, n)
	if len(deps) == 0 {
		out = append(out, p.line("deploys", nil))
	}
	reason := func(dep railway.DeployInfo) string {
		if dep.Reason != "" {
			return text.Sanitize(dep.Reason)
		}
		return strings.ToLower(text.Sanitize(dep.Status))
	}
	ago := func(dep railway.DeployInfo) string {
		if dep.CreatedAt.IsZero() {
			return ""
		}
		return span(p.ctx.Clock()().Sub(dep.CreatedAt)) + " ago"
	}
	rw, aw := 0, 0
	for _, dep := range deps {
		rw = max(rw, ansi.StringWidth(reason(dep)))
		aw = max(aw, ansi.StringWidth(ago(dep)))
	}
	for k, dep := range deps {
		lbl := p.label("")
		if k == 0 {
			lbl = p.label("deploys")
		}
		l := lbl + p.deployIcon(dep.Status) + " " + tui.Pad(reason(dep), rw+2)
		if dep.Creator != "" {
			l += p.meta(tui.Pad(ago(dep), aw+2)) + p.meta("@"+text.Sanitize(dep.Creator))
		} else {
			l += p.meta(ago(dep))
		}
		out = append(out, l)
	}
	for len(out) < n {
		out = append(out, "")
	}
	return out
}

// chartFormats prints a chart's values and its limit.
var chartFormats = map[string]struct{ value, limit func(float64) string }{
	"cpu": {monitor.FormatCPU, trimNum},
	"mem": {func(v float64) string { return monitor.FormatBytes(v * monitor.GB) }, func(v float64) string { return monitor.FormatBytes(v * monitor.GB) }},
	"net": {monitor.FormatRate, nil},
}

func (p *painter) caption(c view.ChartView) string {
	f := chartFormats[c.Name]
	nowV, peak, limit := "—", "—", "—"
	if c.HasData {
		nowV, peak = f.value(c.Last), f.value(c.Peak)
	}
	line := "now " + nowV + " · peak " + peak
	if f.limit != nil {
		if c.Limit > 0 {
			limit = f.limit(c.Limit)
		}
		line += " · limit " + limit
	}
	return line
}

// charts: cpu, mem and net, two lines high with the caption beside, or one
// line with the caption under it when the width is short (chartCells).
func (p *painter) charts(d view.Detail) []string {
	cells, rows := chartCells(p.width)
	var out []string
	for _, c := range d.Charts {
		cp := p.meta(p.caption(c))
		lines := p.chart(c, cells, rows)
		if rows == chartRows {
			out = append(out, p.label(c.Name)+lines[0]+"  "+cp, p.label("")+lines[1])
			continue
		}
		out = append(out, p.label(c.Name)+lines[0], p.label("")+cp)
	}
	if rows == chartRows && d.Step > 0 {
		// one time axis for the three: they share width, step and span
		out = append(out, p.label("")+p.meta(axis(d.Step, cells)))
	}
	return out
}

// axisGrid is the cells between two axis ticks.
const axisGrid = 5

// axis is the time axis under charts cells wide, two points of step seconds
// a cell, the newest on the last cell: ticks every axisGrid cells back from
// the newest, each label (-20m, -1h40, -60h) centred on its tick, an
// even-width label leaning right. Labels go on every tick when the widest
// fits with two blanks between, else on every second, third... tick, so the
// spacing stays even; a label that would leave the chart is dropped. "now"
// follows the chart in the captions' column.
func axis(step, cells int) string {
	if cells <= 0 || step <= 0 {
		return ""
	}
	cellD := 2 * time.Duration(step) * time.Second
	ticks := (cells - 1) / axisGrid
	widest := 0
	for i := 1; i <= ticks; i++ {
		widest = max(widest, len(ago(time.Duration(i*axisGrid)*cellD)))
	}
	every := 1
	for widest+2 > every*axisGrid {
		every++
	}
	line := []rune(strings.Repeat(" ", cells))
	for i := every; i <= ticks; i += every {
		label := []rune(ago(time.Duration(i*axisGrid) * cellD))
		at := cells - 1 - i*axisGrid - (len(label)-1)/2
		if at < 0 || at+len(label) > cells {
			continue
		}
		copy(line[at:], label)
	}
	return string(line) + "  now"
}

// ago is an axis offset back from now: -45m, -2h, -1h15.
func ago(d time.Duration) string {
	m := int(d / time.Minute)
	switch {
	case m < 60:
		return fmt.Sprintf("-%dm", m)
	case m%60 == 0:
		return fmt.Sprintf("-%dh", m/60)
	}
	return fmt.Sprintf("-%dh%02d", m/60, m%60)
}

// chart draws a braille chart scaled to the series' peak, coloured by level.
func (p *painter) chart(c view.ChartView, width, rows int) []string {
	cells := monitor.Braille(c.Values, width, rows, c.Peak, c.Limit)
	out := make([]string, rows)
	for l, line := range cells {
		var b strings.Builder
		for i := 0; i < len(line); {
			step := tui.LevelStep(line[i].Level)
			j := i
			var run []rune
			for j < len(line) && tui.LevelStep(line[j].Level) == step {
				run = append(run, line[j].R)
				j++
			}
			b.WriteString(p.th.Level(line[i].Level).Render(string(run)))
			i = j
		}
		out[l] = b.String()
	}
	return out
}
