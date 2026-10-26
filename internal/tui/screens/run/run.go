// Package run is the run view: the steps of one apply run with their
// status, merged from the run's journal and its live events. It resumes or
// abandons a failed run, or one left running by a quit or crash.
package run

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/men232/railway-manager/internal/apply"
	"github.com/men232/railway-manager/internal/changeset"
	"github.com/men232/railway-manager/internal/dbops"
	"github.com/men232/railway-manager/internal/kinds"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/tui"
	"github.com/men232/railway-manager/internal/tui/components"
	"github.com/men232/railway-manager/internal/view"
)

type screen struct {
	id      string
	filter  *components.Filter
	list    components.List
	confirm *components.Confirm
	// trust is the ssh host key the confirmation would trust; nil when it
	// asks to abandon.
	trust *dbops.HostKeys
	// canTrust: a failed step waits for an ssh host key, as last drawn.
	canTrust bool

	// live holds the step events seen while on top, newer than or equal
	// to the runs index.
	live map[string]apply.StepRecord
	// top is the first line shown of the step list.
	top int
	// labelW is the width of the steps' words, as last drawn.
	labelW int
}

// New returns the run view of one run.
func New(runID string) tui.Screen {
	return &screen{id: runID, filter: components.NewFilter("run:" + runID), live: map[string]apply.StepRecord{}}
}

// Build is the registry constructor: the only argument is the run id
// (string). It returns nil for any other arguments.
func Build(args ...any) tui.Screen {
	if len(args) != 1 {
		return nil
	}
	id, ok := args[0].(string)
	if !ok || id == "" {
		return nil
	}
	return New(id)
}

func (s *screen) Title() string { return "Run" }

func (s *screen) Init(ctx *tui.Ctx) tea.Cmd {
	s.filter.Load(ctx)
	return tui.Emit(tui.RefreshRunsMsg{})
}

// Resume drops the events seen before the screen was covered (the runs
// index has them by now) and asks for the index again.
func (s *screen) Resume(ctx *tui.Ctx) tea.Cmd {
	clear(s.live)
	return tui.Emit(tui.RefreshRunsMsg{})
}

// Capturing reports whether the abandon confirmation takes every key.
func (s *screen) Capturing() bool { return s.confirm != nil || s.filter.Editing() }

func (s *screen) Keys() []tui.KeyHint {
	if s.confirm != nil {
		verb := "abandon"
		if s.trust != nil {
			verb = "trust"
		}
		yes := tui.KeyHint{Key: "y", Label: verb, Always: true}
		if s.confirm.Word != "" {
			yes = tui.KeyHint{Key: "enter", Label: verb, Always: true}
		}
		return []tui.KeyHint{yes, {Key: "esc", Label: "cancel", Always: true}}
	}
	if s.filter.Editing() {
		return []tui.KeyHint{
			{Key: "enter", Label: "keep filter", Always: true},
			{Key: "esc", Label: "clear", Always: true},
		}
	}
	keys := []tui.KeyHint{
		{Key: "↑↓", Label: "move", Rank: 1, Optional: true},
		{Key: "r", Label: "resume", Rank: 1},
		{Key: "X", Label: "abandon", Rank: 2},
		{Key: "/", Label: "filter", Rank: 3, Optional: true},
		{Key: "esc", Label: "back", Always: true},
	}
	if s.canTrust {
		keys = slices.Insert(keys, 1, tui.KeyHint{Key: "t", Label: "trust ssh key", Rank: 1})
	}
	return keys
}

// hostKeys is the ssh host key a failed database step of the run would not
// connect to, not trusted yet (kinds.UntrustedHost).
func (s *screen) hostKeys(ctx *tui.Ctx) (dbops.HostKeys, bool) {
	for _, st := range s.steps(ctx) {
		if st.rec.Status != apply.StatusFailed {
			continue
		}
		if hk, ok := kinds.UntrustedHost(st.rec); ok && len(hk.Keys) > 0 {
			return hk, true
		}
	}
	return dbops.HostKeys{}, false
}

// trustRisk is said before trusting an ssh host key.
const trustRisk = "compare it with the fingerprint Railway publishes for this host: a wrong key would get the database passwords"

// info is the run in the runs index; nil when the index lacks it.
func (s *screen) info(ctx *tui.Ctx) *apply.RunInfo {
	for i := range ctx.RunIndex.Runs {
		if ctx.RunIndex.Runs[i].ID == s.id {
			return &ctx.RunIndex.Runs[i]
		}
	}
	return nil
}

type step struct {
	key string
	rec apply.StepRecord
}

// steps merges the journal, the live events and the live errors of
// Ctx.Running, ordered by step key.
func (s *screen) steps(ctx *tui.Ctx) []step {
	recs := map[string]apply.StepRecord{}
	if info := s.info(ctx); info != nil {
		for k, r := range info.Steps {
			recs[k] = r
		}
	}
	for k, r := range s.live {
		recs[k] = r
	}
	if p, ok := ctx.Running[s.id]; ok {
		for k, e := range p.Errors {
			r, known := recs[k]
			if !known {
				continue // not a step of this run's journal
			}
			if r.Error == "" {
				r.Error = e
			}
			recs[k] = r
		}
	}
	out := make([]step, 0, len(recs))
	for k, r := range recs {
		out = append(out, step{k, r})
	}
	slices.SortFunc(out, func(a, b step) int { return strings.Compare(a.key, b.key) })
	return out
}

// header state and counts: live when the run executes here, else journal.
func (s *screen) summary(ctx *tui.Ctx, steps []step) (state string, done, total int) {
	if info := s.info(ctx); info != nil {
		state = info.State
	}
	if p, ok := ctx.Running[s.id]; ok {
		if p.State != "" {
			state = p.State
		}
		if p.Total > 0 {
			return state, p.Done, p.Total
		}
	}
	for _, st := range steps {
		total++
		if st.rec.Status == apply.StatusDone || st.rec.Status == apply.StatusSkipped {
			done++
		}
	}
	return state, done, total
}

// shown is the steps matching the filter, grouped, and how many steps the
// run has before filtering.
func (s *screen) shown(ctx *tui.Ctx) ([]group, int) {
	s.filter.Load(ctx)
	all := s.steps(ctx)
	groups := s.groups(ctx, all)
	if !s.filter.Active() {
		return groups, len(all)
	}
	var out []group
	for _, g := range groups {
		var items []item
		for _, it := range g.items {
			hay := g.title + " " + it.label + " " + it.st.key + " " + firstLine(it.st.rec.Error)
			for _, c := range it.changes {
				hay += " " + c.Target
			}
			if s.filter.Match(hay) {
				items = append(items, it)
			}
		}
		if len(items) > 0 {
			out = append(out, group{title: g.title, items: items})
		}
	}
	return out, len(all)
}

// navigable is the steps of groups in order, the cursor's stops.
func navigable(groups []group) []item {
	var out []item
	for _, g := range groups {
		out = append(out, g.items...)
	}
	return out
}

func (s *screen) Update(ctx *tui.Ctx, msg tea.Msg) (tui.Screen, tea.Cmd) {
	switch m := msg.(type) {
	case components.CancelMsg:
		s.confirm, s.trust = nil, nil
		return s, nil
	case components.ConfirmedMsg:
		if s.confirm == nil {
			return s, nil
		}
		s.confirm = nil
		if hk := s.trust; hk != nil {
			s.trust = nil
			return s, trustCmd(ctx, *hk)
		}
		return s, tui.Emit(tui.AbandonMsg{RunID: s.id})
	case tui.RunEventMsg:
		if m.Event.RunID == s.id {
			rec := s.live[m.Event.StepKey]
			rec.Status, rec.Error = m.Event.Status, m.Event.Error
			s.live[m.Event.StepKey] = rec
		}
		return s, nil
	case tea.KeyMsg:
		if s.confirm != nil {
			return s, s.confirm.Update(m)
		}
		if h, cmd := s.filter.Update(ctx, m); h {
			return s, cmd
		}
		return s.key(ctx, m)
	}
	return s, nil
}

func (s *screen) key(ctx *tui.Ctx, k tea.KeyMsg) (tui.Screen, tea.Cmd) {
	groups, _ := s.shown(ctx)
	steps := navigable(groups)
	h := max(1, ctx.Height-tui.ChromeRows-1)
	switch k.Type {
	case tea.KeyUp:
		s.list.Move(len(steps), -1, h)
		return s, nil
	case tea.KeyDown:
		s.list.Move(len(steps), 1, h)
		return s, nil
	case tea.KeyPgUp:
		s.list.Move(len(steps), -h, h)
		return s, nil
	case tea.KeyPgDown:
		s.list.Move(len(steps), h, h)
		return s, nil
	case tea.KeyRunes:
	default:
		return s, nil
	}
	state, _, _ := s.summary(ctx, s.steps(ctx))
	if k := string(k.Runes); (k == "r" || k == "X") && s.elsewhere(ctx) {
		dir := ""
		if ctx.Runs != nil {
			dir = ctx.Runs.Dir
		}
		return s, tui.Flash('!', tui.RunElsewhere(dir, s.id, s.info(ctx).HolderPID))
	}
	switch string(k.Runes) {
	case "j":
		s.list.Move(len(steps), 1, h)
	case "k":
		s.list.Move(len(steps), -1, h)
	case "r":
		if !s.resumable(ctx, state) {
			return s, tui.Flash('!', "only a failed or interrupted run can be resumed")
		}
		return s, tui.Emit(tui.ResumeMsg{RunID: s.id})
	case "t":
		hk, ok := s.hostKeys(ctx)
		if !ok {
			return s, tui.Flash('!', "no ssh host key waits to be trusted")
		}
		s.trust = &hk
		s.confirm = &components.Confirm{Prompt: "trust the ssh host key of " + hk.Host + "?", Word: "trust"}
	case "X":
		s.trust = nil
		if !s.resumable(ctx, state) {
			return s, tui.Flash('!', "only a failed or interrupted run can be abandoned")
		}
		s.confirm = &components.Confirm{Prompt: "abandon run " + s.id + "? written changes stay as they are"}
		// Fail closed: a run the index does not show may have acted.
		if info := s.info(ctx); info == nil || view.DBActed(*info) {
			// An escape for a database gone for good.
			// The risk shows on its own line above the question (View).
			s.confirm = &components.Confirm{Prompt: "abandon run " + s.id + "?", Word: "abandon"}
		}
	}
	return s, nil
}

// trustCmd writes hk to the work dir's known hosts file, off the UI
// goroutine, and says whether it did.
func trustCmd(ctx *tui.Ctx, hk dbops.HostKeys) tea.Cmd {
	if ctx.Runs == nil {
		return tui.Flash('✗', "no work dir to keep the ssh host key in")
	}
	file := dbops.KnownHostsFile(ctx.Runs.Dir)
	return func() tea.Msg {
		if err := dbops.TrustHostKeys(file, hk); err != nil {
			return tui.FlashMsg{Kind: '✗', Text: text.Sanitize("trust the ssh host key: " + err.Error())}
		}
		return tui.FlashMsg{Kind: '✓', Text: text.Sanitize("trusted the ssh host key of " + hk.Host + " · r resumes the run")}
	}
}

// abandonRisk is said before abandoning a run whose database step acted.
const abandonRisk = "the database password may already be new; abandoning leaves places on the old one"

// elsewhere reports whether the run executes in another live app instance
// (its lock is held there): it is neither resumed nor abandoned from here.
func (s *screen) elsewhere(ctx *tui.Ctx) bool {
	if p, ok := ctx.Running[s.id]; ok && p.State != "" {
		return false
	}
	info := s.info(ctx)
	return info != nil && info.Elsewhere
}

// resumable reports whether the run can be resumed or abandoned: it failed,
// or it is running on disk but not executing in this process (the app quit
// or crashed during an attempt). A run another live instance executes is
// refused by the run's lock when the root acts on it.
func (s *screen) resumable(ctx *tui.Ctx, state string) bool {
	switch state {
	case apply.RunFailed:
		return true
	case apply.RunRunning:
		return ctx.Running[s.id].State != apply.RunRunning
	}
	return false
}

// Notice has no standing footer message.
func (*screen) Notice(*tui.Ctx) tui.FlashMsg { return tui.FlashMsg{} }

// Note is the run in the frame's header: id, state, progress, when.
func (s *screen) Note(ctx *tui.Ctx) string {
	all := s.steps(ctx)
	state, done, total := s.summary(ctx, all)
	if state == "" {
		return ""
	}
	if s.elsewhere(ctx) {
		state = "running in another window"
	}
	note := text.Sanitize(s.id) + " · " + state + " · " + strconv.Itoa(done) + "/" + strconv.Itoa(total)
	if info := s.info(ctx); info != nil && !info.Created.IsZero() {
		note += " · " + info.Created.In(time.Local).Format("01-02 15:04")
	}
	return note
}

func (s *screen) View(ctx *tui.Ctx, width, height int) string {
	th := ctx.Theme
	all := s.steps(ctx)
	state, _, _ := s.summary(ctx, all)
	groups, nAll := s.shown(ctx)
	steps := navigable(groups)
	if s.info(ctx) == nil && len(all) == 0 && state == "" {
		switch {
		case slices.Contains(ctx.RunIndex.Corrupt, s.id):
			msg := "! run " + text.Sanitize(s.id) + " unavailable: its files cannot be read"
			if ctx.Runs != nil {
				msg += " · " + tui.RunRecovery(ctx.Runs.Dir, s.id)
			}
			return th.Warn.Render(msg)
		case !ctx.RunIndex.Loaded:
			return th.Muted.Render("reading runs…")
		}
		return th.Muted.Render("run " + text.Sanitize(s.id) + " not found")
	}
	_, s.canTrust = s.hostKeys(ctx)
	var lines []string
	if f := s.filter.View(width, len(steps), nAll); f != "" {
		lines = append(lines, f)
	}
	s.list.Cursor = min(s.list.Cursor, max(0, len(steps)-1))

	// the selected step in full, under the list; the abandon question
	var bottom []string
	if s.confirm != nil {
		switch {
		case s.trust != nil:
			bottom = append(bottom, th.Warn.Render(ansi.Truncate(text.Sanitize(s.trust.Host)+" presents "+text.Sanitize(strings.Join(s.trust.Fingerprints, ", ")), width, "…")))
			bottom = append(bottom, th.Danger.Render(ansi.Truncate(ctx.Icons.Warn()+" "+trustRisk, width, "…")))
		case s.confirm.Word != "":
			bottom = append(bottom, th.Danger.Render(ansi.Truncate(ctx.Icons.Warn()+" "+abandonRisk, width, "…")))
		}
		bottom = append(bottom, s.confirm.View(width))
	} else if len(steps) > 0 {
		bottom = s.detail(ctx, steps[s.list.Cursor], width)
	}

	notes := s.notes(ctx, all)
	// the steps' words share a column as wide as the longest, up to half
	s.labelW = 0
	for _, it := range steps {
		s.labelW = max(s.labelW, ansi.StringWidth(text.Sanitize(it.label)))
	}
	s.labelW = min(s.labelW, max(12, width*3/5))
	var rows []string
	cursorRow := 0
	n := 0
	for gi, g := range groups {
		if gi > 0 {
			rows = append(rows, "")
		}
		rows = append(rows, th.Muted.Render("▾ ")+th.Accent2.Bold(true).Render(text.Sanitize(g.title)))
		for _, it := range g.items {
			if n == s.list.Cursor {
				cursorRow = len(rows)
			}
			rows = append(rows, s.row(ctx, it, notes[it.st.key], n == s.list.Cursor, width))
			rows = append(rows, s.changeRows(ctx, it, width)...)
			n++
		}
	}
	if len(steps) == 0 {
		msg := "no steps"
		if s.filter.Active() {
			msg = "no step matches the filter"
		}
		rows = append(rows, th.Muted.Render(msg))
	}
	listH := max(height-len(lines)-len(bottom), 1)
	if len(rows) > listH {
		// scroll to keep the cursor's step shown, with its group heading
		// when it can be
		s.top = min(max(s.top, cursorRow-listH+1), cursorRow)
		if cursorRow-s.top < 2 {
			s.top = max(0, cursorRow-1)
		}
		s.top = min(max(s.top, 0), len(rows)-listH)
		rows = rows[s.top : s.top+listH]
	} else {
		s.top = 0
		for len(rows) < listH {
			rows = append(rows, "")
		}
	}
	lines = append(lines, rows...)
	lines = append(lines, bottom...)
	if len(lines) > height {
		lines = lines[:max(height, 0)]
	}
	return strings.Join(lines, "\n")
}

// group is a heading and the steps under it.
type group struct {
	title string
	items []item
}

// item is a step as shown: its words, the place it acts at, and the
// changes it writes.
type item struct {
	st      step
	label   string
	place   string
	changes []apply.FrozenChange
}

// groups orders the steps: the database steps by database, then each
// environment's (its writes, then domains, redeploys and restarts) in the
// order the run's changes name them, then any other.
func (s *screen) groups(ctx *tui.Ctx, steps []step) []group {
	info := s.info(ctx)
	var frozen []apply.FrozenChange
	if info != nil {
		frozen = info.Frozen
	}
	var envOrder []string
	for _, c := range frozen {
		if !slices.Contains(envOrder, c.Place.EnvID) {
			envOrder = append(envOrder, c.Place.EnvID)
		}
	}
	byKey := map[string]*group{}
	var dbKeys, envKeys []string
	var other []item
	envName := func(id string) string {
		if ctx.Store == nil {
			return id
		}
		return ctx.EnvName(id)
	}
	svcName := func(id string) string {
		switch {
		case id == "" || id == "shared":
			return "shared"
		case ctx.Store == nil:
			return id
		}
		return ctx.Store.ServiceName(id)
	}
	place := func(p string) (env, svc string) {
		env, svc, _ = strings.Cut(p, "/")
		return env, svc
	}
	add := func(key, title string, it item) {
		g := byKey[key]
		if g == nil {
			g = &group{title: title}
			byKey[key] = g
			if strings.HasPrefix(key, "db:") {
				dbKeys = append(dbKeys, key)
			} else {
				envKeys = append(envKeys, key)
			}
		}
		g.items = append(g.items, it)
	}
	for _, st := range steps {
		kind, rest, _ := strings.Cut(st.key, ":")
		switch kind {
		case "preflight", "dbpw", "dbverify":
			p, user, _ := strings.Cut(rest, ":")
			env, svc := place(p)
			label := map[string]string{
				"preflight": "check the database can be reached",
				"dbpw":      "change the password in the database",
				"dbverify":  "log in with the new password",
			}[kind]
			title := "database · " + envName(env) + " · " + svcName(svc) + " · " + user + " password"
			add("db:"+rest, title, item{st: st, label: label, place: envName(env) + " · " + svcName(svc)})
		case "patch":
			var cs []apply.FrozenChange
			for _, c := range frozen {
				if c.Place.EnvID == rest && patched(c.Kind) {
					cs = append(cs, c)
				}
			}
			label := "write " + tui.Plural(len(cs), "change")
			if len(cs) > 0 && allVariables(cs) {
				label = "write " + tui.Plural(len(cs), "variable")
			}
			add("env:"+rest, envName(rest), item{st: st, label: label, place: envName(rest), changes: cs})
		case "redeploy", "restart":
			env, svc := place(rest)
			add("env:"+env, envName(env), item{st: st, label: kind + " " + svcName(svc), place: envName(env) + " · " + svcName(svc)})
		case "domain":
			// "<env>/<service>/<target>"
			parts := append(strings.SplitN(rest, "/", 3), "", "")
			env, p, target := parts[0], parts[1], parts[2]
			var cs []apply.FrozenChange
			for _, c := range frozen {
				if strings.HasPrefix(c.Kind, "domain.") && "domain:"+c.Place.Key()+"/"+c.Target == st.key {
					cs = append(cs, c)
				}
			}
			host := target
			if _, h, ok := strings.Cut(target, ":"); ok {
				host = h
			}
			add("env:"+env, envName(env), item{st: st, label: "domain " + host + " at " + svcName(p), place: envName(env) + " · " + svcName(p), changes: cs})
		default:
			other = append(other, item{st: st, label: st.key})
		}
	}
	// environments in the order the changes name them
	rank := func(key string) int {
		if i := slices.Index(envOrder, strings.TrimPrefix(key, "env:")); i >= 0 {
			return i
		}
		return len(envOrder)
	}
	slices.SortStableFunc(envKeys, func(a, b string) int { return rank(a) - rank(b) })
	slices.Sort(dbKeys)
	stepRank := map[string]int{"patch": 0, "domain": 1, "redeploy": 2, "restart": 3}
	var out []group
	for _, k := range append(dbKeys, envKeys...) {
		g := byKey[k]
		if !strings.HasPrefix(k, "db:") {
			slices.SortStableFunc(g.items, func(a, b item) int {
				ka, _, _ := strings.Cut(a.st.key, ":")
				kb, _, _ := strings.Cut(b.st.key, ":")
				return stepRank[ka] - stepRank[kb]
			})
		} else {
			order := map[string]int{"preflight": 0, "dbpw": 1, "dbverify": 2}
			slices.SortStableFunc(g.items, func(a, b item) int {
				ka, _, _ := strings.Cut(a.st.key, ":")
				kb, _, _ := strings.Cut(b.st.key, ":")
				return order[ka] - order[kb]
			})
		}
		out = append(out, *g)
	}
	if len(other) > 0 {
		out = append(out, group{title: "other", items: other})
	}
	return out
}

// patched reports whether a change of kind is written by its
// environment's patch step (not by its own steps or an action).
func patched(kind string) bool {
	switch {
	case kind == "db.password", kind == "db.keyfile", kind == "variable.keep",
		kind == "service.redeploy", kind == "service.restart",
		strings.HasPrefix(kind, "domain."):
		return false
	}
	return true
}

func allVariables(cs []apply.FrozenChange) bool {
	for _, c := range cs {
		if !strings.HasPrefix(c.Kind, "variable.") {
			return false
		}
	}
	return true
}

// changeRows are the lines of the changes a step writes: the service, the
// name and what was done, never a value; a change the run skipped or
// failed says so.
func (s *screen) changeRows(ctx *tui.Ctx, it item, width int) []string {
	th := ctx.Theme
	info := s.info(ctx)
	svcW, nameW := 0, 0
	svc := func(c apply.FrozenChange) string {
		switch {
		case c.Place.Shared():
			return "shared"
		case ctx.Store == nil:
			return text.Sanitize(c.Place.ServiceID)
		}
		return text.Sanitize(ctx.Store.ServiceName(c.Place.ServiceID))
	}
	for _, c := range it.changes {
		svcW = max(svcW, ansi.StringWidth(svc(c)))
		nameW = max(nameW, ansi.StringWidth(text.Sanitize(c.Target)))
	}
	// the columns take what they need, up to a third of the line each
	svcW, nameW = min(svcW, max(12, width/3)), min(nameW, max(12, width/3))
	var out []string
	for _, c := range it.changes {
		what := text.Sanitize(c.Overlay.Short)
		if what == "" {
			what = verb(c.Kind) // a run scrubbed before overlays were kept
		}
		tone := th.New
		switch c.Overlay.Tone {
		case "danger":
			tone = th.Danger
		case "warn":
			tone = th.Warn
		}
		l := "      " + th.Muted.Render(tui.Pad(svc(c), svcW)) + "  " + tui.Pad(text.Sanitize(c.Target), nameW) + "  " + tone.Render(what)
		if info != nil {
			switch o := info.Outcomes[c.ID]; o.Status {
			case apply.StatusSkipped:
				l += "  " + th.Muted.Render(ctx.Icons.Skipped()+" skipped"+reason(o.Reason))
			case apply.StatusFailed:
				l += "  " + th.Danger.Render(ctx.Icons.Fail()+" failed"+reason(o.Reason))
			}
		}
		out = append(out, ansi.Truncate(l, width, "…"))
	}
	return out
}

// verb is what a change of kind does, for one with no overlay.
func verb(kind string) string {
	switch kind {
	case "variable.set":
		return "set"
	case "variable.delete":
		return "deleted"
	case "limits.set":
		return "limits changed"
	case "volume.size":
		return "volume resized"
	case "domain.service.add", "domain.custom.add":
		return "domain added"
	case "domain.service.delete", "domain.custom.delete":
		return "domain removed"
	}
	return ""
}

func reason(r string) string {
	if r = firstLine(r); r != "" {
		return ": " + r
	}
	return ""
}

// detail is the selected step in full: its words, where, when, and its
// error wrapped, under a thin rule.
func (s *screen) detail(ctx *tui.Ctx, it item, width int) []string {
	th := ctx.Theme
	glyph, tone := s.glyph(ctx, it.st.rec.Status)
	head := tone.Render(glyph) + " " + text.Sanitize(it.label)
	if it.place != "" {
		head += th.Muted.Render(" · " + text.Sanitize(it.place))
	}
	if !it.st.rec.At.IsZero() {
		head += th.Muted.Render(" · " + it.st.rec.At.In(time.Local).Format("15:04:05"))
	}
	out := []string{th.Faint.Render(strings.Repeat("─", width)), ansi.Truncate(head, width, "…")}
	if e := text.Sanitize(strings.ReplaceAll(it.st.rec.Error, "\n", " ")); e != "" {
		wrapped := strings.Split(ansi.Wordwrap(e, max(8, width-2), " "), "\n")
		if len(wrapped) > 4 {
			wrapped = append(wrapped[:3], "…")
		}
		for _, l := range wrapped {
			out = append(out, "  "+th.Danger.Render(ansi.Truncate(l, width-2, "…")))
		}
	}
	return out
}

// glyph is a step status's icon and tone.
func (s *screen) glyph(ctx *tui.Ctx, status string) (string, lipgloss.Style) {
	th := ctx.Theme
	switch status {
	case apply.StatusDone:
		return ctx.Icons.Ok(), th.OK
	case apply.StatusFailed:
		return ctx.Icons.Fail(), th.Danger
	case apply.StatusStarted:
		return ctx.Icons.Run(), th.Warn
	case apply.StatusBlocked:
		return ctx.Icons.Blocked(), th.Muted
	case apply.StatusSkipped:
		return ctx.Icons.Skipped(), th.Muted
	}
	return ctx.Icons.Todo(), th.Muted
}

// notes are what a database step did, by step key, never a value: a dbpw step
// that acted reads "password changed", with "not verified" while its dbverify
// step is not done; a step's change outcome reasons (a KEYFILE verify's
// "repaired: …") on its dbverify step; a step's journal Data warning (ssh
// tunnels left behind). Sanitized, first lines only.
func (s *screen) notes(ctx *tui.Ctx, steps []step) map[string]string {
	recs := make(map[string]apply.StepRecord, len(steps))
	for _, st := range steps {
		recs[st.key] = st.rec
	}
	info := s.info(ctx)
	out := map[string]string{}
	add := func(key, note string) {
		if note == "" {
			return
		}
		if out[key] != "" {
			out[key] += " · "
		}
		out[key] += note
	}
	for _, st := range steps {
		if sfx, ok := strings.CutPrefix(st.key, "dbpw:"); ok && st.rec.Acted {
			note := "password changed"
			if v := recs["dbverify:"+sfx]; v.Status != apply.StatusDone && v.Status != apply.StatusSkipped {
				note += " · not verified"
			}
			add(st.key, note)
		}
		if sfx, ok := strings.CutPrefix(st.key, "dbverify:"); ok && info != nil {
			group := changeset.PasswordGroupPrefix + sfx
			for _, c := range info.Frozen {
				if c.Group == group {
					add(st.key, firstLine(info.Outcomes[c.ID].Reason))
				}
			}
		}
		if len(st.rec.Data) > 0 {
			var d struct {
				Warning string `json:"warning"`
			}
			if json.Unmarshal(st.rec.Data, &d) == nil {
				add(st.key, firstLine(d.Warning))
			}
		}
	}
	return out
}

// row is a step's line: its status, its words, and at the right its error,
// its notes, or when it finished (a patch: its patch id too).
func (s *screen) row(ctx *tui.Ctx, it item, note string, selected bool, width int) string {
	th := ctx.Theme
	glyph, tone := s.glyph(ctx, it.st.rec.Status)
	label := text.Sanitize(it.label)
	right, rtone := "", th.Muted
	switch {
	case it.st.rec.Error != "":
		right, rtone = firstLine(it.st.rec.Error), th.Danger
		if it.st.rec.Status == apply.StatusSkipped {
			rtone = th.Muted
		}
	case note != "":
		right, rtone = note, th.Warn
	case it.st.rec.Status == apply.StatusDone && !it.st.rec.At.IsZero():
		right = it.st.rec.At.In(time.Local).Format("15:04:05")
		if id := it.st.rec.PatchID; id != "" {
			right = "patch " + text.Sanitize(id[:min(7, len(id))]) + " · " + right
		}
	}
	labelW := max(s.labelW, 1)
	if selected {
		return ansi.Truncate(th.Accent.Render("▌ ")+th.Accent.Bold(true).Render(glyph+" "+tui.Pad(label, labelW))+"  "+th.Accent.Render(right), width, "…")
	}
	return ansi.Truncate("  "+tone.Render(glyph)+" "+tui.Pad(label, labelW)+"  "+rtone.Render(right), width, "…")
}

// firstLine is the first line of an error, sanitized.
func firstLine(e string) string {
	first, _, _ := strings.Cut(strings.ReplaceAll(e, "\r\n", "\n"), "\n")
	return text.Sanitize(first)
}
